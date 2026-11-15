package okf_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/okf-memory/okf-agent-memory/pkg/okf"
)

func TestResolveScopedConceptBareIDFallback(t *testing.T) {
	root := t.TempDir()
	project, user, system, vendor := filepath.Join(root, "project"), filepath.Join(root, "user"), filepath.Join(root, "system"), filepath.Join(root, "vendor")
	for _, dir := range []string{project, user, system, filepath.Join(vendor, "example")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "index.md"), []byte("# Index\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, entry := range []struct{ dir, id, title string }{
		{project, "shared", "project"},
		{user, "shared", "user"},
		{system, "shared", "system"},
		{user, "user-only", "user"},
		{system, "user-only", "system"},
		{system, "system-only", "system"},
		{filepath.Join(vendor, "example"), "vendor-only", "vendor"},
	} {
		if err := os.WriteFile(filepath.Join(entry.dir, entry.id+".md"), []byte("---\ntype: Note\ntitle: "+entry.title+"\n---\n# Note\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		id    string
		scope okf.Scope
		fail  bool
	}{
		{"shared", okf.ScopeProject, false},
		{"user-only", okf.ScopeUser, false},
		{"system-only.md", okf.ScopeSystem, false},
		{"user:shared", okf.ScopeUser, false},
		{"system:shared", okf.ScopeSystem, false},
		{"@example/vendor-only", okf.ScopeVendor, false},
		{"okf://project/shared", okf.ScopeProject, false},
		{"vendor-only", "", true},
		{"missing", "", true},
		{"user:system-only", "", true},
		{"okf://project/user-only", "", true},
		{"../shared", "", true},
	} {
		t.Run(tc.id, func(t *testing.T) {
			got, err := okf.ResolveScopedConcept(tc.id, project, vendor, user, system)
			if tc.fail {
				if err == nil {
					t.Fatalf("expected lookup to fail, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Scope != tc.scope || got.Title != string(tc.scope) {
				t.Fatalf("got scope %q title %q, want %q", got.Scope, got.Title, tc.scope)
			}
		})
	}
	t.Run("missing optional user bundle", func(t *testing.T) {
		got, err := okf.ResolveScopedConcept("system-only", project, vendor, filepath.Join(root, "absent"), system)
		if err != nil {
			t.Fatal(err)
		}
		if got.Scope != okf.ScopeSystem {
			t.Fatalf("scope = %q", got.Scope)
		}
	})
	t.Run("missing project bundle", func(t *testing.T) {
		got, err := okf.ResolveScopedConcept("shared", filepath.Join(root, "absent"), vendor, user, system)
		if err != nil {
			t.Fatal(err)
		}
		if got.Scope != okf.ScopeUser {
			t.Fatalf("scope = %q", got.Scope)
		}
	})
	t.Run("invalid user bundle is not a miss", func(t *testing.T) {
		invalid := filepath.Join(root, "not-a-directory")
		if err := os.WriteFile(invalid, []byte("invalid"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := okf.ResolveScopedConcept("system-only", project, vendor, invalid, system); err == nil {
			t.Fatal("expected user bundle load error")
		}
	})

	t.Run("broken project bundle is not a miss", func(t *testing.T) {
		if err := os.Symlink("gone.md", filepath.Join(project, "broken.md")); err != nil {
			t.Fatal(err)
		}
		if _, err := okf.ResolveScopedConcept("shared", project, vendor, user, system); err == nil {
			t.Fatal("expected project bundle load error")
		}
	})
}
