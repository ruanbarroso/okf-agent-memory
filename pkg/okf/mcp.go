package okf

import (
	"bufio"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var MCPVersion = "0.2.0"

// SetMCPVersion sets the version string advertised in the MCP initialize handshake.
func SetMCPVersion(v string) {
	if v != "" {
		MCPVersion = v
	}
}

type jsonRPCRequest struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method"`
	Params  json.RawMessage  `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Result  any              `json:"result,omitempty"`
	Error   *rpcError        `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mcpToolCallParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

type mcpServer struct {
	bundleDir string
	rootDir   string
	writer    io.Writer
	mu        sync.Mutex

	// Optional Git sync state (see SyncProvider). All zero values are inert: a
	// server over a plain, Git-less directory never touches any of it.
	syncMu      sync.Mutex
	syncTimer   *time.Timer
	syncPending map[string]string // bundle dir -> pending commit summary
}

// RunMCPServer runs the Model Context Protocol stdio server for an OKF bundle.
func RunMCPServer(bundleDir string) error {
	return RunMCPServerIO(bundleDir, os.Stdin, os.Stdout)
}

const maxMCPLineLength = 4 * 1024 * 1024 // 4MB maximum JSON-RPC message size

func readBoundedLine(r *bufio.Reader, maxLen int) ([]byte, bool, error) {
	var line []byte
	for {
		chunk, isPrefix, err := r.ReadLine()
		if err != nil {
			if err == io.EOF && len(line) > 0 {
				return line, false, nil
			}
			return nil, false, err
		}
		line = append(line, chunk...)
		if len(line) > maxLen {
			for isPrefix && err == nil {
				_, isPrefix, err = r.ReadLine()
			}
			return nil, true, nil
		}
		if !isPrefix {
			break
		}
	}
	return line, false, nil
}

// RunMCPServerIO runs the MCP server on the provided reader and writer.
func RunMCPServerIO(bundleDir string, in io.Reader, out io.Writer) error {
	rootDir := os.Getenv("OKF_MCP_ROOT")
	if rootDir == "" {
		if bundleDir != "" && bundleDir != "." {
			cleanBundle := filepath.Clean(bundleDir)
			rootDir = filepath.Dir(cleanBundle)
			if rootDir == "" {
				rootDir = "."
			}
		} else {
			rootDir = "."
		}
	}

	absRoot, err := filepath.Abs(rootDir)
	if err == nil {
		if evalRoot, err := filepath.EvalSymlinks(absRoot); err == nil {
			absRoot = evalRoot
		}
		rootDir = absRoot
	}

	s := &mcpServer{
		bundleDir: bundleDir,
		rootDir:   rootDir,
		writer:    out,
	}
	// Last-chance flush: anything a tool wrote in the final moments of a
	// session still gets validated, committed and pushed before exit.
	defer s.FlushSync()

	reader := bufio.NewReader(in)
	for {
		line, tooLarge, err := readBoundedLine(reader, maxMCPLineLength)
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if tooLarge {
			rawNull := json.RawMessage("null")
			s.sendError(&rawNull, -32700, "Parse error: request exceeds 4MB maximum size")
			continue
		}

		trimmed := strings.TrimSpace(string(line))
		if len(trimmed) == 0 {
			continue
		}

		var req jsonRPCRequest
		if err := json.Unmarshal(line, &req); err != nil {
			rawNull := json.RawMessage("null")
			s.sendError(&rawNull, -32700, "Parse error")
			continue
		}

		s.handleRequest(req)
	}
}

func (s *mcpServer) sendResponse(id *json.RawMessage, result any) {
	if id == nil || string(*id) == "null" {
		// Per JSON-RPC 2.0 and MCP spec, never send a response to a notification
		return
	}
	resp := jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	}
	data, _ := json.Marshal(resp)
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.writer.Write(append(data, '\n'))
}

func (s *mcpServer) sendError(id *json.RawMessage, code int, message string) {
	if (id == nil || string(*id) == "null") && code != -32700 {
		// Per JSON-RPC 2.0 and MCP spec, never reply to a notification
		return
	}
	resp := jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error: &rpcError{
			Code:    code,
			Message: message,
		},
	}
	data, _ := json.Marshal(resp)
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.writer.Write(append(data, '\n'))
}

func (s *mcpServer) sendToolResult(id *json.RawMessage, text string, structured any, isError bool) {
	res := map[string]any{
		"content": []map[string]string{
			{"type": "text", "text": text},
		},
	}
	if structured != nil && !isError {
		res["structuredContent"] = structured
	}
	if isError {
		res["isError"] = true
	}
	s.sendResponse(id, res)
}

func (s *mcpServer) handleRequest(req jsonRPCRequest) {
	isNotification := req.ID == nil || string(*req.ID) == "null"

	switch req.Method {
	case "initialize":
		s.sendResponse(req.ID, map[string]any{
			"protocolVersion": "2024-11-05",
			"serverInfo": map[string]string{
				"name":    "okf-agent-memory",
				"version": MCPVersion,
			},
			"capabilities": map[string]any{
				"tools": map[string]bool{
					"listChanged": false,
				},
				"resources": map[string]bool{
					"listChanged": false,
				},
				"prompts": map[string]bool{
					"listChanged": false,
				},
			},
		})
		// Session start: bring the bundle up to date with the remote in the
		// background so a long session works from fresh knowledge. Best
		// effort only — a failing network must never block the handshake.
		go s.autoSyncRefresh()

	case "notifications/initialized", "initialized":
		// Standard lifecycle notification after initialize handshake; no response.
		return

	case "notifications/cancelled", "cancelled":
		// Notification indicating client canceled a request; no response.
		return

	case "ping":
		s.sendResponse(req.ID, map[string]any{})

	case "resources/list":
		s.sendResponse(req.ID, map[string]any{
			"resources": []any{},
		})

	case "prompts/list":
		s.sendResponse(req.ID, map[string]any{
			"prompts": []any{},
		})

	case "tools/list":
		s.sendResponse(req.ID, map[string]any{
			"tools": GetMCPTools(),
		})

	case "tools/call":
		s.handleToolCall(req)

	default:
		if isNotification || strings.HasPrefix(req.Method, "notifications/") {
			// Per JSON-RPC 2.0 & MCP, ignore unknown notifications silently
			return
		}
		s.sendError(req.ID, -32601, "Method not found")
	}
}

//go:embed schemas/tools.json
var embeddedToolsJSON []byte

var cachedMCPTools []map[string]any

func init() {
	if err := json.Unmarshal(embeddedToolsJSON, &cachedMCPTools); err != nil {
		panic(fmt.Sprintf("okf: corrupt embedded schemas/tools.json: %v", err))
	}
}

// GetMCPTools returns the cached tool definitions with input and output schemas.
func GetMCPTools() []map[string]any {
	return cachedMCPTools
}

func (s *mcpServer) resolveBundleDir(callParams mcpToolCallParams) (string, error) {
	target, err := getStringArg(callParams.Arguments, "bundle", 1000, false)
	if err != nil {
		return "", err
	}
	target = strings.TrimSpace(target)

	if target == "" {
		if s.bundleDir != "" && s.bundleDir != "." {
			target = s.bundleDir
		} else if info, err := os.Stat("knowledge"); err == nil && info.IsDir() {
			target = "knowledge"
		} else {
			target = "."
		}
	}

	normTarget := strings.ReplaceAll(target, "\\", "/")

	// Confinement check: if s.rootDir is configured, target must stay within s.rootDir
	if s.rootDir != "" {
		absRoot, err := filepath.EvalSymlinks(s.rootDir)
		if err != nil {
			absRoot, err = filepath.Abs(s.rootDir)
			if err != nil {
				return "", fmt.Errorf("invalid server root directory: %w", err)
			}
		} else {
			absRoot, _ = filepath.Abs(absRoot)
		}

		var absTarget string
		if IsAbsPath(normTarget) {
			absTarget = normTarget
		} else {
			absTarget = filepath.Join(s.rootDir, filepath.FromSlash(normTarget))
		}

		// Walk up to find the closest ancestor that exists and evaluate its symlinks
		curr := absTarget
		var missingParts []string
		for {
			_, lstatErr := os.Lstat(curr)
			if lstatErr == nil {
				break
			}
			missingParts = append([]string{filepath.Base(curr)}, missingParts...)
			parent := filepath.Dir(curr)
			if parent == curr {
				break
			}
			curr = parent
		}

		realCurr, err := filepath.EvalSymlinks(curr)
		if err != nil {
			return "", fmt.Errorf("failed to resolve bundle path %q: %w", target, err)
		}
		realCurr, err = filepath.Abs(realCurr)
		if err != nil {
			return "", err
		}

		parts := append([]string{realCurr}, missingParts...)
		realTarget := filepath.Join(parts...)

		rel, err := filepath.Rel(absRoot, realTarget)
		normRel := strings.ReplaceAll(rel, "\\", "/")
		if err != nil || rel == ".." || normRel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || strings.HasPrefix(normRel, "../") {
			return "", fmt.Errorf("path traversal denied: bundle directory %q escapes server root %q", target, s.rootDir)
		}

		return realTarget, nil
	}

	return target, nil
}

func getStringArg(args map[string]any, key string, maxLen int, required bool) (string, error) {
	val, ok := args[key]
	if !ok || val == nil {
		if required {
			return "", fmt.Errorf("missing required argument '%s'", key)
		}
		return "", nil
	}
	strVal, ok := val.(string)
	if !ok {
		return "", fmt.Errorf("argument '%s' must be a string", key)
	}
	if len(strVal) > maxLen {
		return "", fmt.Errorf("argument '%s' exceeds maximum length of %d bytes", key, maxLen)
	}
	return strVal, nil
}

func getTagsArg(args map[string]any) ([]string, error) {
	value, ok := args["tags"].([]any)
	if !ok {
		return nil, fmt.Errorf("argument 'tags' must be an array of strings")
	}
	if len(value) > 100 {
		return nil, fmt.Errorf("argument 'tags' exceeds maximum of 100 items")
	}
	tags := make([]string, 0, len(value))
	for _, item := range value {
		tag, ok := item.(string)
		if !ok || len(tag) > 50 || strings.TrimSpace(tag) == "" {
			return nil, fmt.Errorf("each tag must be a non-empty string of at most 50 bytes")
		}
		tags = append(tags, strings.TrimSpace(tag))
	}
	return tags, nil
}

func getMutationStatus(args map[string]any) (string, error) {
	status, err := getStringArg(args, "status", 1000, true)
	if err != nil {
		return "", err
	}
	if !IsValidConceptStatus(status) {
		return "", fmt.Errorf("argument 'status' must be draft, stable, or deprecated")
	}
	return status, nil
}

func (s *mcpServer) handleToolCall(req jsonRPCRequest) {
	var callParams mcpToolCallParams
	if err := json.Unmarshal(req.Params, &callParams); err != nil {
		s.sendError(req.ID, -32602, "Invalid params")
		return
	}

	if callParams.Arguments == nil {
		callParams.Arguments = make(map[string]any)
	}

	bundleDir, err := s.resolveBundleDir(callParams)
	if err != nil {
		s.sendToolResult(req.ID, fmt.Sprintf("Path traversal denied: %v", err), nil, true)
		return
	}

	b, err := LoadBundle(bundleDir)
	if err != nil {
		if callParams.Name != "okf_search" && callParams.Name != "okf_show" {
			s.sendToolResult(req.ID, fmt.Sprintf("Failed to load bundle from %q: %v", bundleDir, err), nil, true)
			return
		}
	}

	switch callParams.Name {
	case "okf_search":
		query, err := getStringArg(callParams.Arguments, "query", 10000, false)
		if err != nil {
			s.sendToolResult(req.ID, fmt.Sprintf("Invalid arguments: %v", err), nil, true)
			return
		}
		scope, err := getStringArg(callParams.Arguments, "scope", 100, false)
		if err != nil {
			s.sendToolResult(req.ID, fmt.Sprintf("Invalid arguments: %v", err), nil, true)
			return
		}
		forPath, err := getStringArg(callParams.Arguments, "for_path", 1000, false)
		if err != nil {
			s.sendToolResult(req.ID, fmt.Sprintf("Invalid arguments: %v", err), nil, true)
			return
		}
		filter, err := getStringArg(callParams.Arguments, "filter", 1000, false)
		if err != nil {
			s.sendToolResult(req.ID, fmt.Sprintf("Invalid arguments: %v", err), nil, true)
			return
		}
		staleWithinStr, err := getStringArg(callParams.Arguments, "stale_within", 100, false)
		if err != nil {
			s.sendToolResult(req.ID, fmt.Sprintf("Invalid arguments: %v", err), nil, true)
			return
		}
		var staleWithin time.Duration
		if staleWithinStr != "" {
			d, err := ParseRelativeDuration(staleWithinStr)
			if err != nil {
				s.sendToolResult(req.ID, fmt.Sprintf("Invalid stale_within duration: %v", err), nil, true)
				return
			}
			staleWithin = d
		}
		limit := 10
		if l, ok := callParams.Arguments["limit"].(float64); ok {
			if l > 0 && l <= 100 {
				limit = int(l)
			} else if l > 100 {
				limit = 100
			}
		}

		vendorRoot := filepath.Join(".okf", "vendor")
		if s.rootDir != "" {
			cand := filepath.Join(s.rootDir, ".okf", "vendor")
			if info, err := os.Stat(cand); err == nil && info.IsDir() {
				vendorRoot = cand
			}
		}

		results, err := SearchLayered(LayeredSearchOptions{
			BundleDir:  bundleDir,
			VendorRoot: vendorRoot,
			UserDir:    ResolveUserDir(),
			SystemDir:  ResolveSystemDir(),
			Scope:      scope,
			SearchOpts: SearchOptions{
				Query:       query,
				TargetPath:  forPath,
				Limit:       limit,
				Filter:      filter,
				StaleWithin: staleWithin,
			},
		})
		if err != nil {
			s.sendToolResult(req.ID, fmt.Sprintf("Search error: %v", err), nil, true)
			return
		}
		if results == nil {
			results = []SearchResult{}
		}
		resJSON, _ := json.Marshal(results)
		envelope := map[string]any{"results": results}
		s.sendToolResult(req.ID, string(resJSON), envelope, false)

	case "okf_show":
		conceptID, err := getStringArg(callParams.Arguments, "concept_id", 1000, true)
		if err != nil {
			s.sendToolResult(req.ID, fmt.Sprintf("Invalid arguments: %v", err), nil, true)
			return
		}
		conceptID = strings.TrimSpace(conceptID)

		vendorRoot := filepath.Join(".okf", "vendor")
		if s.rootDir != "" {
			cand := filepath.Join(s.rootDir, ".okf", "vendor")
			if info, err := os.Stat(cand); err == nil && info.IsDir() {
				vendorRoot = cand
			}
		}

		res, err := ResolveScopedConcept(conceptID, bundleDir, vendorRoot, ResolveUserDir(), ResolveSystemDir())
		if err != nil {
			s.sendToolResult(req.ID, fmt.Sprintf("%v", err), nil, true)
			return
		}
		resJSON, _ := json.Marshal(res.Concept)
		s.sendToolResult(req.ID, string(resJSON), res.Concept, false)

	case "okf_validate":
		strict := true
		if sVal, ok := callParams.Arguments["strict"].(bool); ok {
			strict = sVal
		}
		stale := false
		if stVal, ok := callParams.Arguments["stale"].(bool); ok {
			stale = stVal
		}
		staleWithinStr, err := getStringArg(callParams.Arguments, "stale_within", 100, false)
		if err != nil {
			s.sendToolResult(req.ID, fmt.Sprintf("Invalid arguments: %v", err), nil, true)
			return
		}
		var staleWithin time.Duration
		if staleWithinStr != "" {
			d, err := ParseRelativeDuration(staleWithinStr)
			if err != nil {
				s.sendToolResult(req.ID, fmt.Sprintf("Invalid stale_within duration: %v", err), nil, true)
				return
			}
			staleWithin = d
		}
		res := Validate(b, ValidateOptions{Strict: strict, Drift: true, Stale: stale, StaleWithin: staleWithin})
		if res.Errors == nil {
			res.Errors = []string{}
		}
		if res.Warnings == nil {
			res.Warnings = []string{}
		}
		if res.GateFindings == nil {
			res.GateFindings = []string{}
		}
		if res.BrokenLinks == nil {
			res.BrokenLinks = []BrokenLink{}
		}
		if res.Orphans == nil {
			res.Orphans = []string{}
		}
		resJSON, _ := json.Marshal(res)
		s.sendToolResult(req.ID, string(resJSON), res, false)

	case "okf_create":
		conceptID, err := getStringArg(callParams.Arguments, "concept_id", 1000, true)
		if err != nil {
			s.sendToolResult(req.ID, fmt.Sprintf("Invalid arguments: %v", err), nil, true)
			return
		}
		conceptID = strings.TrimSpace(conceptID)
		if err := ValidateConceptID(conceptID); err != nil {
			s.sendToolResult(req.ID, fmt.Sprintf("Invalid concept_id: %v", err), nil, true)
			return
		}
		conceptType, err := getStringArg(callParams.Arguments, "type", 1000, true)
		if err != nil {
			s.sendToolResult(req.ID, fmt.Sprintf("Invalid arguments: %v", err), nil, true)
			return
		}
		conceptType = strings.TrimSpace(conceptType)
		if conceptType == "" {
			s.sendToolResult(req.ID, "Invalid type: concept type cannot be empty or whitespace", nil, true)
			return
		}
		title, err := getStringArg(callParams.Arguments, "title", 1000, true)
		if err != nil {
			s.sendToolResult(req.ID, fmt.Sprintf("Invalid arguments: %v", err), nil, true)
			return
		}
		if strings.TrimSpace(title) == "" {
			s.sendToolResult(req.ID, "Invalid title: concept title cannot be empty or whitespace", nil, true)
			return
		}
		desc, err := getStringArg(callParams.Arguments, "description", 1000, false)
		if err != nil {
			s.sendToolResult(req.ID, fmt.Sprintf("Invalid arguments: %v", err), nil, true)
			return
		}
		body, err := getStringArg(callParams.Arguments, "body", 1024*1024, false)
		if err != nil {
			s.sendToolResult(req.ID, fmt.Sprintf("Invalid arguments: %v", err), nil, true)
			return
		}
		status := "stable"
		if _, exists := callParams.Arguments["status"]; exists {
			status, err = getMutationStatus(callParams.Arguments)
			if err != nil {
				s.sendToolResult(req.ID, fmt.Sprintf("Invalid arguments: %v", err), nil, true)
				return
			}
		}
		var tags []string
		if _, exists := callParams.Arguments["tags"]; exists {
			tags, err = getTagsArg(callParams.Arguments)
			if err != nil {
				s.sendToolResult(req.ID, fmt.Sprintf("Invalid arguments: %v", err), nil, true)
				return
			}
		}

		cleanID := strings.TrimSuffix(conceptID, ".md")
		c := &Concept{
			ID:          cleanID,
			Path:        cleanID + ".md",
			Type:        conceptType,
			Title:       strings.TrimSpace(title),
			Description: desc,
			Body:        body,
			Status:      status,
			Tags:        tags,
		}

		if err := SaveConcept(bundleDir, c, SaveOptions{
			IsNew:     true,
			AutoLog:   true,
			AutoIndex: true,
			Actor:     "agent/mcp",
		}); err != nil {
			s.sendToolResult(req.ID, fmt.Sprintf("Failed to save concept: %v", err), nil, true)
			return
		}
		s.scheduleSyncPublish(bundleDir, "create concept "+c.ID)

		msg := fmt.Sprintf("Successfully created concept %s in %s", c.Path, bundleDir)
		structured := map[string]any{
			"success":    true,
			"concept_id": c.ID,
			"path":       c.Path,
			"bundle":     bundleDir,
			"message":    msg,
		}
		s.sendToolResult(req.ID, msg, structured, false)

	case "okf_update":
		conceptID, err := getStringArg(callParams.Arguments, "concept_id", 1000, true)
		if err != nil {
			s.sendToolResult(req.ID, fmt.Sprintf("Invalid arguments: %v", err), nil, true)
			return
		}
		conceptID = strings.TrimSpace(conceptID)
		if err := ValidateConceptID(conceptID); err != nil {
			s.sendToolResult(req.ID, fmt.Sprintf("Invalid concept_id: %v", err), nil, true)
			return
		}
		cleanID := strings.TrimSuffix(conceptID, ".md")
		c, ok := b.Concepts[cleanID]
		if !ok {
			s.sendToolResult(req.ID, fmt.Sprintf("Concept '%s' not found in %s", cleanID, bundleDir), nil, true)
			return
		}

		// Work on a copy to prevent in-memory concept corruption if validation or disk write fails
		updated := *c
		if _, exists := callParams.Arguments["type"]; exists {
			conceptType, err := getStringArg(callParams.Arguments, "type", 1000, true)
			if err != nil {
				s.sendToolResult(req.ID, fmt.Sprintf("Invalid arguments: %v", err), nil, true)
				return
			}
			if strings.TrimSpace(conceptType) == "" {
				s.sendToolResult(req.ID, "Invalid type: concept type cannot be empty or whitespace", nil, true)
				return
			}
			updated.Type = strings.TrimSpace(conceptType)
		}
		if _, exists := callParams.Arguments["status"]; exists {
			status, err := getMutationStatus(callParams.Arguments)
			if err != nil {
				s.sendToolResult(req.ID, fmt.Sprintf("Invalid arguments: %v", err), nil, true)
				return
			}
			updated.Status = status
		}
		if _, exists := callParams.Arguments["tags"]; exists {
			tags, err := getTagsArg(callParams.Arguments)
			if err != nil {
				s.sendToolResult(req.ID, fmt.Sprintf("Invalid arguments: %v", err), nil, true)
				return
			}
			updated.Tags = tags
		}

		if _, exists := callParams.Arguments["title"]; exists {
			title, err := getStringArg(callParams.Arguments, "title", 1000, true)
			if err != nil {
				s.sendToolResult(req.ID, fmt.Sprintf("Invalid arguments: %v", err), nil, true)
				return
			}
			if strings.TrimSpace(title) == "" {
				s.sendToolResult(req.ID, "Invalid title: concept title cannot be empty or whitespace", nil, true)
				return
			}
			updated.Title = strings.TrimSpace(title)
		}
		if _, exists := callParams.Arguments["description"]; exists {
			desc, err := getStringArg(callParams.Arguments, "description", 1000, false)
			if err != nil {
				s.sendToolResult(req.ID, fmt.Sprintf("Invalid arguments: %v", err), nil, true)
				return
			}
			updated.Description = desc
		}
		if _, exists := callParams.Arguments["body"]; exists {
			body, err := getStringArg(callParams.Arguments, "body", 1024*1024, false)
			if err != nil {
				s.sendToolResult(req.ID, fmt.Sprintf("Invalid arguments: %v", err), nil, true)
				return
			}
			updated.Body = body
		}

		if err := SaveConcept(bundleDir, &updated, SaveOptions{
			IsNew:     false,
			AutoLog:   true,
			AutoIndex: true,
			Actor:     "agent/mcp",
		}); err != nil {
			s.sendToolResult(req.ID, fmt.Sprintf("Failed to update concept: %v", err), nil, true)
			return
		}
		s.scheduleSyncPublish(bundleDir, "update concept "+cleanID)

		*c = updated

		msg := fmt.Sprintf("Successfully updated concept %s in %s", c.Path, bundleDir)
		structured := map[string]any{
			"success":    true,
			"concept_id": c.ID,
			"path":       c.Path,
			"bundle":     bundleDir,
			"message":    msg,
		}
		s.sendToolResult(req.ID, msg, structured, false)

	case "okf_relate":
		srcID, err := getStringArg(callParams.Arguments, "source_id", 1000, true)
		if err != nil {
			s.sendToolResult(req.ID, fmt.Sprintf("Invalid arguments: %v", err), nil, true)
			return
		}
		tgtID, err := getStringArg(callParams.Arguments, "target_id", 1000, true)
		if err != nil {
			s.sendToolResult(req.ID, fmt.Sprintf("Invalid arguments: %v", err), nil, true)
			return
		}
		desc, err := getStringArg(callParams.Arguments, "description", 1000, false)
		if err != nil {
			s.sendToolResult(req.ID, fmt.Sprintf("Invalid arguments: %v", err), nil, true)
			return
		}

		if err := RelateConcepts(bundleDir, srcID, tgtID, desc, "agent/mcp"); err != nil {
			s.sendToolResult(req.ID, fmt.Sprintf("Failed to relate concepts: %v", err), nil, true)
			return
		}
		s.scheduleSyncPublish(bundleDir, "relate "+srcID+" -> "+tgtID)

		msg := fmt.Sprintf("Successfully linked '%s' -> '%s' in %s", srcID, tgtID, bundleDir)
		structured := map[string]any{
			"success":   true,
			"source_id": srcID,
			"target_id": tgtID,
			"bundle":    bundleDir,
			"message":   msg,
		}
		s.sendToolResult(req.ID, msg, structured, false)

	case "okf_sync_status":
		res, err := activeSync().Status(bundleDir)
		if err != nil {
			s.sendToolResult(req.ID, fmt.Sprintf("Sync status failed: %v", err), nil, true)
			return
		}
		resJSON, _ := json.Marshal(res)
		s.sendToolResult(req.ID, string(resJSON), res, false)

	case "okf_sync_refresh":
		state, res, err := activeSync().Refresh(bundleDir)
		if err != nil {
			s.sendToolResult(req.ID, fmt.Sprintf("Sync refresh failed: %v", err), nil, true)
			return
		}
		resJSON, _ := json.Marshal(res)
		s.sendToolResult(req.ID, string(resJSON), res, state == "conflict")

	case "okf_sync_publish":
		message, err := getStringArg(callParams.Arguments, "message", 500, false)
		if err != nil {
			s.sendToolResult(req.ID, fmt.Sprintf("Invalid arguments: %v", err), nil, true)
			return
		}
		if strings.TrimSpace(message) == "" {
			message = "update knowledge"
		}
		state, res, err := activeSync().Publish(bundleDir, message)
		if err != nil {
			s.sendToolResult(req.ID, fmt.Sprintf("Sync publish failed: %v", err), nil, true)
			return
		}
		isBad := state == "conflict" || state == "validate_failed" || state == "wrong_branch"
		resJSON, _ := json.Marshal(res)
		s.sendToolResult(req.ID, string(resJSON), res, isBad)

	default:
		s.sendToolResult(req.ID, fmt.Sprintf("Unknown tool: %s", callParams.Name), nil, true)
	}
}

// ---------------------------------------------------------------------------
// Optional Git sync automation
//
// Everything below is inert when sync is not enabled for the bundle: a plain
// directory without Git (or with sync off) behaves exactly as before. When a
// config enables sync, writes are batched with a debounce window and then
// validated, committed and pushed automatically; results go to stderr so the
// JSON-RPC channel on stdout is never polluted.
// ---------------------------------------------------------------------------

// SyncProvider is the optional Git sync backend for the MCP server. It is
// injected (see SetSyncProvider) instead of imported because pkg/gitsync
// depends on this package; without a provider every sync hook is inert.
type SyncProvider interface {
	// Status reports the sync state of bundleDir as a JSON-serializable value.
	Status(bundleDir string) (any, error)
	// Refresh pulls remote changes; state is e.g. "refreshed" or "conflict".
	Refresh(bundleDir string) (state string, result any, err error)
	// Publish validates, commits and pushes bundle changes.
	Publish(bundleDir, summary string) (state string, result any, err error)
	// Auto reports whether sync is active for bundleDir and its automation
	// settings. A nil error with enabled=false means "plain local memory".
	Auto(bundleDir string) (enabled, autoPull, autoPush bool, debounce time.Duration, err error)
	// LogPublish writes a human-readable publish outcome (stderr in practice).
	LogPublish(w io.Writer, result any)
}

var (
	syncProviderMu sync.RWMutex
	syncProvider   SyncProvider
)

// SetSyncProvider installs the Git sync backend used by MCP sync tools and
// automatic publish/refresh. Passing nil restores the inert default.
func SetSyncProvider(p SyncProvider) {
	syncProviderMu.Lock()
	defer syncProviderMu.Unlock()
	syncProvider = p
}

func activeSync() SyncProvider {
	syncProviderMu.RLock()
	defer syncProviderMu.RUnlock()
	if syncProvider == nil {
		return noSync{}
	}
	return syncProvider
}

// noSync is the default provider: sync is never enabled.
type noSync struct{}

func (noSync) Status(string) (any, error) {
	return map[string]any{"sync_enabled": false, "reason": "sync support not linked into this binary"}, nil
}
func (noSync) Refresh(string) (string, any, error) {
	return "local_only", map[string]any{"state": "local_only"}, nil
}
func (noSync) Publish(string, string) (string, any, error) {
	return "local_only", map[string]any{"state": "local_only"}, nil
}
func (noSync) Auto(string) (bool, bool, bool, time.Duration, error) {
	return false, false, false, 0, nil
}
func (noSync) LogPublish(io.Writer, any) {}

// scheduleSyncPublish queues an automatic publish for bundleDir after a
// successful write. Rapid successive writes collapse into one commit.
func (s *mcpServer) scheduleSyncPublish(bundleDir, summary string) {
	enabled, _, autoPush, debounce, err := activeSync().Auto(bundleDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "okf sync: %v\n", err)
		return
	}
	if !enabled || !autoPush {
		return
	}

	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	if s.syncPending == nil {
		s.syncPending = make(map[string]string)
	}
	if _, exists := s.syncPending[bundleDir]; exists {
		// A second write to the same bundle turns the batch into a generic
		// summary; the commit details live in log.md anyway.
		s.syncPending[bundleDir] = "update knowledge"
	} else {
		s.syncPending[bundleDir] = summary
	}

	if debounce <= 0 {
		debounce = 50 * time.Millisecond
	}
	if s.syncTimer != nil {
		s.syncTimer.Reset(debounce)
		return
	}
	s.syncTimer = time.AfterFunc(debounce, s.publishPending)
}

// publishPending drains the debounce queue and publishes each dirty bundle.
func (s *mcpServer) publishPending() {
	pending := s.takeSyncPending()
	s.publishSyncMap(pending)
}

// FlushSync stops the debounce timer and publishes anything still pending.
// Called on server shutdown so a session's final writes are not lost.
func (s *mcpServer) FlushSync() {
	s.syncMu.Lock()
	if s.syncTimer != nil {
		s.syncTimer.Stop()
		s.syncTimer = nil
	}
	pending := s.syncPending
	s.syncPending = nil
	s.syncMu.Unlock()
	s.publishSyncMap(pending)
}

func (s *mcpServer) takeSyncPending() map[string]string {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	pending := s.syncPending
	s.syncPending = nil
	s.syncTimer = nil
	return pending
}

func (s *mcpServer) publishSyncMap(pending map[string]string) {
	p := activeSync()
	for bundleDir, summary := range pending {
		_, res, err := p.Publish(bundleDir, summary)
		if err != nil {
			fmt.Fprintf(os.Stderr, "okf sync: publish failed for %s: %v (the write is safe locally)\n", bundleDir, err)
			continue
		}
		p.LogPublish(os.Stderr, res)
	}
}

// autoSyncRefresh brings the bundle up to date at session start when
// auto_pull is on. Best effort: errors are logged, never fatal.
func (s *mcpServer) autoSyncRefresh() {
	bundleDir, err := s.resolveBundleDir(mcpToolCallParams{})
	if err != nil {
		return
	}
	p := activeSync()
	enabled, autoPull, _, _, err := p.Auto(bundleDir)
	if err != nil || !enabled || !autoPull {
		return
	}
	state, _, err := p.Refresh(bundleDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "okf sync: session refresh failed: %v\n", err)
		return
	}
	if state == "refreshed" {
		fmt.Fprintln(os.Stderr, "okf sync: bundle refreshed from remote")
	}
}
