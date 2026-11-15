package okf

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMCPRelativeNestedBundlePath starts the server as `okf mcp docs/knowledge`
// does, from the folder above docs/, with OKF_MCP_ROOT unset.
func TestMCPRelativeNestedBundlePath(t *testing.T) {
	t.Setenv("OKF_MCP_ROOT", "")
	tmpDir := t.TempDir()
	bundle := filepath.Join(tmpDir, "docs", "knowledge")
	if err := os.MkdirAll(filepath.Join(bundle, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(bundle, "index.md"), []byte("---\nokf_version: \"0.2\"\n---\n# Root\n"), 0o644)
	_ = os.WriteFile(filepath.Join(bundle, "log.md"), []byte("# Log\n"), 0o644)
	_ = os.WriteFile(filepath.Join(bundle, "notes", "example.md"),
		[]byte("---\ntype: Note\ntitle: Nested bundle note\n---\n\n# Note\n"), 0o644)
	t.Chdir(tmpDir)

	responses := runMCPConversation(t, filepath.Join("docs", "knowledge"), []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"okf_show","arguments":{"concept_id":"notes/example"}}}`,
	})
	if len(responses) != 1 || responses[0].Error != nil {
		t.Fatalf("unexpected responses: %+v", responses)
	}
	result, _ := json.Marshal(responses[0].Result)
	if strings.Contains(string(result), `"isError":true`) || !strings.Contains(string(result), "Nested bundle note") {
		t.Fatalf("okf_show over a relative nested bundle path failed: %s", result)
	}
}
