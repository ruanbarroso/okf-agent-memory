package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestShowBareIDReportsScope(t *testing.T) {
	root := t.TempDir()
	project, user := filepath.Join(root, "project"), filepath.Join(root, "user")
	t.Setenv("OKF_USER_DIR", user)
	t.Setenv("OKF_SYSTEM_DIR", filepath.Join(root, "system"))
	for _, dir := range []string{project, user} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "index.md"), []byte("# Index\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	raw := "---\ntype: Note\ntitle: User note\n---\n# Note\n"
	if err := os.WriteFile(filepath.Join(user, "note.md"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	out := captureOutput(func() { cmdShow([]string{"note", project, "--json"}) })
	var result map[string]any
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatal(err)
	}
	if result["scope"] != "user" || result["title"] != "User note" {
		t.Fatalf("unexpected output: %s", out)
	}
	out = captureOutput(func() { cmdShow([]string{"note", project}) })
	if !strings.Contains(out, "Scope:       user") {
		t.Fatalf("missing scope: %s", out)
	}
	out = captureOutput(func() { cmdShow([]string{"note", project, "--raw"}) })
	if out != raw {
		t.Fatalf("raw content changed: %q", out)
	}
}
