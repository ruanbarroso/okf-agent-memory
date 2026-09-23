package main

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/okf-memory/okf-agent-memory/pkg/gitsync"
	"github.com/okf-memory/okf-agent-memory/pkg/okf"
)

// The MCP automation contract: with sync enabled, a concept created through
// the MCP tool must reach the remote without any explicit publish — the
// shutdown flush (which backs the debounce) is enough to carry it out.
func TestMCPServerAutoSyncPublishesOnShutdown(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available on PATH")
	}

	gitCmd := func(t *testing.T, dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(cmd.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_EDITOR=true")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s in %s failed: %v\n%s", strings.Join(args, " "), dir, err, out)
		}
		return strings.TrimSpace(string(out))
	}

	base := t.TempDir()
	remote := filepath.Join(base, "remote.git")
	gitCmd(t, base, "init", "--bare", "-b", "main", filepath.Base(remote))

	// Standalone bundle: the repository is the bundle directory itself.
	bundle := filepath.Join(base, "memory")
	if err := okf.InitBundle(bundle); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, bundle, "init", "-b", "main")
	gitCmd(t, bundle, "config", "user.email", "test@example.com")
	gitCmd(t, bundle, "config", "user.name", "Test Agent")
	gitCmd(t, bundle, "remote", "add", "origin", filepath.ToSlash(remote))

	cfg := gitsync.DefaultConfig()
	cfg.AgentID = "agent/mcp-test"
	cfg.AutoPull = false // keep the session-start goroutine out of the test
	// The debounce never fires during the test: only the shutdown flush runs,
	// which is exactly the path under test.
	cfg.DebounceMS = 60000
	if err := gitsync.SaveConfig(bundle, cfg); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"okf_create","arguments":{"concept_id":"decisions/mcp-created","type":"Fact","title":"MCP Created","description":"Created through the MCP tool with sync enabled."}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"okf_sync_status","arguments":{}}}`,
		"", // EOF: the server flushes and exits
	}, "\n")

	if err := RunMCPServerIO(bundle, strings.NewReader(in), &out); err != nil {
		t.Fatalf("RunMCPServerIO: %v", err)
	}

	payload := out.String()
	if !strings.Contains(payload, "Successfully created concept decisions/mcp-created.md") {
		t.Fatalf("create tool did not succeed:\n%s", payload)
	}
	if !strings.Contains(payload, `"sync_enabled":true`) {
		t.Fatalf("okf_sync_status should report sync enabled:\n%s", payload)
	}

	// The write must have reached the remote with zero explicit publishing.
	shown := gitCmd(t, bundle, "show", "origin/main:decisions/mcp-created.md")
	if !strings.Contains(shown, "title: MCP Created") {
		t.Fatalf("remote missing the MCP-created concept, got:\n%s", shown)
	}
	remoteLog := gitCmd(t, bundle, "show", "origin/main:log.md")
	if !strings.Contains(remoteLog, "decisions/mcp-created.md") {
		t.Fatalf("remote log missing the creation entry:\n%s", remoteLog)
	}
}

// Without a sync config, the MCP server must behave exactly as before: the
// tool result carries no sync machinery and nothing Git-related happens.
func TestMCPServerWithoutSyncUnchanged(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available on PATH")
	}
	bundle := t.TempDir()
	if err := okf.InitBundle(bundle); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"okf_create","arguments":{"concept_id":"decisions/plain","type":"Fact","title":"Plain","description":"No sync involved."}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"okf_sync_status","arguments":{}}}`,
		"",
	}, "\n")

	if err := RunMCPServerIO(bundle, strings.NewReader(in), &out); err != nil {
		t.Fatalf("RunMCPServerIO: %v", err)
	}
	payload := out.String()
	if !strings.Contains(payload, "Successfully created concept decisions/plain.md") {
		t.Fatalf("create tool did not succeed:\n%s", payload)
	}
	if !strings.Contains(payload, `"sync_enabled":false`) {
		t.Fatalf("okf_sync_status should report sync disabled for a Git-less bundle:\n%s", payload)
	}
}
