package registry

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const DefaultRegistryURL = "https://registry.okf-memory.dev"

type BundleManifest struct {
	ID          string `json:"id"`
	Version     string `json:"version"`
	Hash        string `json:"hash"`
	DownloadURL string `json:"download_url"`
}

type Client struct {
	BaseURL    string
	HTTPClient *http.Client
}

func NewClient(baseURL string) *Client {
	if baseURL == "" {
		baseURL = DefaultRegistryURL
	}
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		HTTPClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

func (c *Client) Resolve(slugOrURL string) (*BundleManifest, error) {
	if strings.HasPrefix(slugOrURL, "github.com/") || strings.HasPrefix(slugOrURL, "https://") {
		return c.resolveGitURL(slugOrURL)
	}

	cleanSlug := strings.Trim(strings.TrimSpace(slugOrURL), "/")
	target := strings.TrimPrefix(cleanSlug, "@")

	var slug, version string
	if idx := strings.Index(target, "@"); idx != -1 {
		slug = target[:idx]
		version = target[idx+1:]
	} else {
		slug = target
	}

	var candidateEndpoints []string
	if version != "" {
		candidateEndpoints = append(candidateEndpoints,
			fmt.Sprintf("%s/bundles/%s-%s.json", c.BaseURL, slug, version),
			fmt.Sprintf("%s/bundles/@%s-%s.json", c.BaseURL, slug, version),
		)
	} else {
		candidateEndpoints = append(candidateEndpoints,
			fmt.Sprintf("%s/bundles/%s.json", c.BaseURL, slug),
			fmt.Sprintf("%s/bundles/@%s.json", c.BaseURL, slug),
		)
	}

	fetchManifest := func(endpoint string) (*BundleManifest, int, error) {
		req, err := http.NewRequest("GET", endpoint, nil)
		if err != nil {
			return nil, 0, err
		}
		req.Header.Set("User-Agent", "okf-agent-memory-cli")
		resp, err := c.HTTPClient.Do(req)
		if err != nil {
			return nil, 0, err
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode == http.StatusOK {
			var m BundleManifest
			if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
				return nil, resp.StatusCode, fmt.Errorf("malformed registry manifest: %w", err)
			}
			if err := ValidateBundleID(m.ID); err != nil {
				return nil, resp.StatusCode, fmt.Errorf("refusing registry manifest %s: %w", endpoint, err)
			}
			if strings.HasPrefix(m.DownloadURL, "/") {
				m.DownloadURL = c.BaseURL + m.DownloadURL
			}
			return &m, http.StatusOK, nil
		}
		return nil, resp.StatusCode, nil
	}

	for _, ep := range candidateEndpoints {
		m, status, err := fetchManifest(ep)
		if err != nil {
			return nil, err
		}
		if status == http.StatusOK && m != nil {
			return m, nil
		}
	}

	// If versioned endpoint was 404, fallback to base bundles/<slug>.json
	if version != "" {
		baseCandidates := []string{
			fmt.Sprintf("%s/bundles/%s.json", c.BaseURL, slug),
			fmt.Sprintf("%s/bundles/@%s.json", c.BaseURL, slug),
		}
		for _, baseEp := range baseCandidates {
			m, status, err := fetchManifest(baseEp)
			if err != nil {
				return nil, err
			}
			if status == http.StatusOK && m != nil {
				if strings.TrimPrefix(m.Version, "v") != strings.TrimPrefix(version, "v") {
					return nil, fmt.Errorf("bundle %q version %s not found in registry (latest is %s)", slug, version, m.Version)
				}
				return m, nil
			}
		}
		return nil, fmt.Errorf("bundle %q (version %s) not found in registry %s", slug, version, c.BaseURL)
	}

	return nil, fmt.Errorf("bundle %q not found in registry %s", cleanSlug, c.BaseURL)
}

func (c *Client) resolveGitURL(rawURL string) (*BundleManifest, error) {
	tag := ""
	if idx := strings.Index(rawURL, "@"); idx != -1 {
		tag = rawURL[idx+1:]
		rawURL = rawURL[:idx]
	}

	parsedURL := rawURL
	if !strings.HasPrefix(parsedURL, "http://") && !strings.HasPrefix(parsedURL, "https://") {
		parsedURL = "https://" + parsedURL
	}
	u, err := url.Parse(parsedURL)
	if err != nil {
		return nil, fmt.Errorf("invalid Git URL: %w", err)
	}

	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 {
		return nil, fmt.Errorf("git URL must follow github.com/owner/repo format")
	}
	owner, repo := parts[0], parts[1]
	id := fmt.Sprintf("%s/%s", owner, repo)
	if err := ValidateBundleID(id); err != nil {
		return nil, fmt.Errorf("invalid Git URL %q: %w", rawURL, err)
	}

	var archiveURL string
	version := tag
	if tag != "" {
		archiveURL = fmt.Sprintf("https://github.com/%s/%s/archive/refs/tags/%s.tar.gz", owner, repo, tag)
	} else {
		version = "main"
		archiveURL = fmt.Sprintf("https://github.com/%s/%s/archive/refs/heads/main.tar.gz", owner, repo)
	}

	return &BundleManifest{
		ID:          id,
		Version:     version,
		Hash:        "",
		DownloadURL: archiveURL,
	}, nil
}

func (c *Client) DownloadAndExtract(manifest *BundleManifest, targetDir string) error {
	req, err := http.NewRequest("GET", manifest.DownloadURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "okf-agent-memory-cli")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to download bundle: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bundle download failed with HTTP %s", resp.Status)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("error reading bundle stream: %w", err)
	}

	calculated := fmt.Sprintf("sha256:%x", sha256.Sum256(data))
	if manifest.Hash != "" {
		if !strings.EqualFold(calculated, manifest.Hash) {
			return fmt.Errorf("checksum mismatch: expected %s, got %s", manifest.Hash, calculated)
		}
	} else {
		manifest.Hash = calculated
	}

	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return fmt.Errorf("cannot create vendor dir: %w", err)
	}
	// Archive entries are untrusted input. Every destination must resolve
	// (through symlinks already present in the vendor directory) strictly
	// inside the resolved target directory.
	root, err := resolvePath(targetDir, true)
	if err != nil {
		return fmt.Errorf("cannot resolve vendor dir: %w", err)
	}

	grPre, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("failed to decompress gzip: %w", err)
	}
	defer func() { _ = grPre.Close() }()

	// Pre-scan headers to determine if bundle uses standard DMAA knowledge/ directory
	// and whether files are wrapped in a single root directory (e.g. GitHub archive repo-main/).
	// Entries that would escape are refused here, before anything is written.
	hasKnowledgeDir := false
	rootWrapper := ""
	firstRootChecked := false
	var extracted []string

	trPre := tar.NewReader(grPre)
	for {
		hdr, err := trPre.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("tar pre-scan error: %w", err)
		}
		cleanName := filepath.ToSlash(filepath.Clean(hdr.Name))
		if cleanName == "." {
			continue
		}
		if cleanName == ".." || strings.HasPrefix(cleanName, "../") || strings.HasPrefix(cleanName, "/") || filepath.IsAbs(hdr.Name) || filepath.VolumeName(hdr.Name) != "" {
			return fmt.Errorf("path traversal attempt: archive entry %q escapes the bundle directory", hdr.Name)
		}
		if hdr.Typeflag == tar.TypeDir || hdr.Typeflag == tar.TypeReg {
			extracted = append(extracted, cleanName)
		}

		parts := strings.Split(cleanName, "/")
		if !firstRootChecked {
			if len(parts) > 1 || hdr.Typeflag == tar.TypeDir {
				rootWrapper = parts[0]
			}
			firstRootChecked = true
		} else if rootWrapper != "" && parts[0] != rootWrapper {
			rootWrapper = ""
		}

		if cleanName == "knowledge/index.md" || strings.HasSuffix(cleanName, "/knowledge/index.md") {
			hasKnowledgeDir = true
		}
	}

	// destination maps an archive entry to its path relative to targetDir, or
	// reports false for entries that are not extracted.
	destination := func(cleanName string) (string, bool) {
		var relDest string
		if hasKnowledgeDir {
			// Extract only contents of knowledge/ directory; skip all non-knowledge repo files
			idx := strings.Index(cleanName, "/knowledge/")
			if idx != -1 {
				relDest = cleanName[idx+len("/knowledge/"):]
			} else if strings.HasPrefix(cleanName, "knowledge/") {
				relDest = cleanName[len("knowledge/"):]
			} else {
				return "", false
			}
		} else {
			// Pure bundle fallback: strip single root wrapper (e.g. repo-main/) if present
			if rootWrapper != "" {
				if cleanName == rootWrapper {
					return "", false
				}
				if strings.HasPrefix(cleanName, rootWrapper+"/") {
					relDest = cleanName[len(rootWrapper)+1:]
				} else {
					relDest = cleanName
				}
			} else {
				relDest = cleanName
			}
		}

		relDest = filepath.Clean(relDest)
		if relDest == "" || relDest == "." || relDest == ".." || strings.HasPrefix(relDest, "..") || filepath.IsAbs(relDest) {
			return "", false
		}
		return relDest, true
	}

	// containedDest checks the lexical join and then the symlink-resolved
	// destination, which is what the write goes to.
	containedDest := func(relDest string) (string, error) {
		destPath := filepath.Join(targetDir, filepath.FromSlash(relDest))
		cleanDestPath := filepath.Clean(destPath)
		cleanTargetDir := filepath.Clean(targetDir)
		if !strings.HasPrefix(cleanDestPath, cleanTargetDir+string(filepath.Separator)) && cleanDestPath != cleanTargetDir {
			return "", fmt.Errorf("path traversal attempt: %s", relDest)
		}
		resolved, err := containedPath(root, filepath.Join(root, filepath.FromSlash(relDest)), true, fmt.Sprintf("archive entry %q", relDest))
		if err != nil {
			return "", fmt.Errorf("path traversal attempt: %w", err)
		}
		return resolved, nil
	}

	// Resolve every destination before writing anything, so one escaping entry
	// refuses the whole archive instead of leaving it half-extracted.
	for _, name := range extracted {
		if relDest, ok := destination(name); ok {
			if _, err := containedDest(relDest); err != nil {
				return err
			}
		}
	}

	gr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("failed to decompress gzip: %w", err)
	}
	defer func() { _ = gr.Close() }()

	tr := tar.NewReader(gr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("tar extract error: %w", err)
		}

		cleanName := filepath.ToSlash(filepath.Clean(hdr.Name))
		if cleanName == "." || cleanName == ".." || strings.HasPrefix(cleanName, "../") {
			continue
		}

		relDest, ok := destination(cleanName)
		if !ok {
			continue
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			cleanDestPath, err := containedDest(relDest)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(cleanDestPath, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			cleanDestPath, err := containedDest(relDest)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(cleanDestPath), 0o755); err != nil {
				return err
			}
			// #nosec G304 -- cleanDestPath is resolved and strictly verified to reside within the resolved target dir
			outFile, err := os.OpenFile(cleanDestPath, os.O_CREATE|os.O_RDWR|os.O_TRUNC, hdr.FileInfo().Mode())
			if err != nil {
				return err
			}
			// #nosec G110 -- bounded extraction limit (50MB) mitigates decompression bombs
			const maxExtractBytes = 50 * 1024 * 1024
			if _, err := io.Copy(outFile, io.LimitReader(tr, maxExtractBytes)); err != nil {
				_ = outFile.Close()
				return err
			}
			_ = outFile.Close()
		}
	}

	return nil
}
