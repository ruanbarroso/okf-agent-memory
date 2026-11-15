package registry_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/okf-memory/okf-agent-memory/pkg/registry"
)

func TestValidateBundleID(t *testing.T) {
	for _, id := range []string{
		"nextjs-15",
		"peter/django-5-rules",
		"@peter/django-5-rules",
		"acme/tools/linter",
		"weird..name",
		"a.b/c.d",
		"Ünïcode bundle/name",
	} {
		if err := registry.ValidateBundleID(id); err != nil {
			t.Errorf("legitimate id %q refused: %v", id, err)
		}
	}

	for _, tc := range []struct{ id, reason string }{
		{"", "empty id"},
		{"..", "parent directory traversal"},
		{"../outside", "parent directory traversal"},
		{"../../../outside-reg/planted", "parent directory traversal"},
		{"acme/../../x", "parent directory traversal"},
		{"acme/..", "parent directory traversal"},
		{"/etc/okf", "absolute path"},
		{"/", "absolute path"},
		{"acme//rules", "non-canonical path"},
		{"acme/", "non-canonical path"},
		{"./acme", "non-canonical path"},
		{"acme/./rules", "non-canonical path"},
		{".", "non-canonical path"},
		{`acme\rules`, "backslash"},
		{`..\outside`, "backslash"},
		{"acme\x00rules", "NUL byte"},
	} {
		err := registry.ValidateBundleID(tc.id)
		if err == nil {
			t.Errorf("unsafe id %q accepted", tc.id)
			continue
		}
		if !strings.Contains(err.Error(), strconv.Quote(tc.id)) || !strings.Contains(err.Error(), tc.reason) {
			t.Errorf("id %q: error %q must name the value and the reason %q", tc.id, err, tc.reason)
		}
	}
}

// vendorWorkspace returns a resolved workspace path and a sibling outside dir.
func vendorWorkspace(t *testing.T) (ws, outside string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ws, outside = filepath.Join(base, "ws"), filepath.Join(base, "outside")
	for _, d := range []string{ws, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return ws, outside
}

func mustMkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, p, body string) {
	t.Helper()
	mustMkdir(t, filepath.Dir(p))
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	mustMkdir(t, filepath.Dir(link))
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func TestResolveVendorDir(t *testing.T) {
	t.Run("legitimate ids, vendor root not yet created", func(t *testing.T) {
		ws, _ := vendorWorkspace(t)
		for _, id := range []string{"nextjs-15", "@peter/django-5-rules", "acme/tools/linter"} {
			got, err := registry.ResolveVendorDir(ws, id)
			want := filepath.Join(ws, ".okf", "vendor", filepath.FromSlash(id))
			if err != nil || got != want {
				t.Fatalf("ResolveVendorDir(%q) = %q, %v; want %q", id, got, err, want)
			}
		}
	})
	t.Run("in-vendor symlink is followed", func(t *testing.T) {
		ws, _ := vendorWorkspace(t)
		mustMkdir(t, filepath.Join(ws, ".okf", "vendor", "real"))
		mustSymlink(t, filepath.Join(ws, ".okf", "vendor", "real"), filepath.Join(ws, ".okf", "vendor", "alias"))
		got, err := registry.ResolveVendorDir(ws, "alias/rules")
		if err != nil || got != filepath.Join(ws, ".okf", "vendor", "real", "rules") {
			t.Fatalf("got %q, %v", got, err)
		}
	})
	t.Run("unsafe id", func(t *testing.T) {
		ws, _ := vendorWorkspace(t)
		if _, err := registry.ResolveVendorDir(ws, "../../outside"); err == nil || !strings.Contains(err.Error(), `"../../outside"`) {
			t.Fatalf("expected unsafe id refusal, got %v", err)
		}
	})
	t.Run("symlink in vendor root escapes", func(t *testing.T) {
		ws, outside := vendorWorkspace(t)
		mustSymlink(t, outside, filepath.Join(ws, ".okf", "vendor", "evil"))
		if _, err := registry.ResolveVendorDir(ws, "evil/planted"); err == nil || !strings.Contains(err.Error(), "outside") {
			t.Fatalf("expected containment refusal, got %v", err)
		}
	})
	t.Run("id resolving to the vendor root itself", func(t *testing.T) {
		ws, _ := vendorWorkspace(t)
		mustSymlink(t, ".", filepath.Join(ws, ".okf", "vendor", "self"))
		if _, err := registry.ResolveVendorDir(ws, "self"); err == nil {
			t.Fatal("an id must never resolve to the vendor root itself")
		}
	})
	t.Run("vendor root outside workspace", func(t *testing.T) {
		for _, link := range []string{".okf", ".okf/vendor"} {
			ws, outside := vendorWorkspace(t)
			mustSymlink(t, outside, filepath.Join(ws, filepath.FromSlash(link)))
			if _, err := registry.ResolveVendorDir(ws, "acme/rules"); err == nil || !strings.Contains(err.Error(), "outside workspace") {
				t.Fatalf("%s -> outside: expected vendor root refusal, got %v", link, err)
			}
		}
	})
	t.Run("vendor root symlinked inside workspace", func(t *testing.T) {
		ws, _ := vendorWorkspace(t)
		mustMkdir(t, filepath.Join(ws, "shared-vendor"))
		mustSymlink(t, filepath.Join(ws, "shared-vendor"), filepath.Join(ws, ".okf", "vendor"))
		got, err := registry.ResolveVendorDir(ws, "acme/rules")
		if err != nil || got != filepath.Join(ws, "shared-vendor", "acme", "rules") {
			t.Fatalf("got %q, %v", got, err)
		}
	})
}

func TestRemoveVendorDir(t *testing.T) {
	t.Run("removes bundle and empty parent, keeps vendor root and siblings", func(t *testing.T) {
		ws, _ := vendorWorkspace(t)
		v := filepath.Join(ws, ".okf", "vendor")
		mustWrite(t, filepath.Join(v, "solo", "rules", "index.md"), "x")
		mustWrite(t, filepath.Join(v, "acme", "a", "index.md"), "x")
		mustWrite(t, filepath.Join(v, "acme", "b", "index.md"), "x")
		mustWrite(t, filepath.Join(v, "top", "index.md"), "x")
		for _, id := range []string{"solo/rules", "acme/a", "top"} {
			if err := registry.RemoveVendorDir(ws, id); err != nil {
				t.Fatalf("RemoveVendorDir(%q): %v", id, err)
			}
		}
		for p, want := range map[string]bool{"solo": false, "acme/a": false, "acme/b/index.md": true, "top": false, "": true} {
			if got := exists(filepath.Join(v, filepath.FromSlash(p))); got != want {
				t.Errorf("vendor/%s exists=%v, want %v", p, got, want)
			}
		}
	})
	t.Run("refuses deleting through a symlink swapped in after install", func(t *testing.T) {
		ws, outside := vendorWorkspace(t)
		mustWrite(t, filepath.Join(outside, "rules", "keep.md"), "user data")
		mustSymlink(t, outside, filepath.Join(ws, ".okf", "vendor", "acme"))
		err := registry.RemoveVendorDir(ws, "acme/rules")
		if err == nil || !strings.Contains(err.Error(), `bundle id "acme/rules"`) {
			t.Fatalf("expected containment refusal, got %v", err)
		}
		if !exists(filepath.Join(outside, "rules", "keep.md")) {
			t.Fatal("outside directory deleted")
		}
	})
	t.Run("refuses when the vendor root escapes the workspace", func(t *testing.T) {
		ws, outside := vendorWorkspace(t)
		mustWrite(t, filepath.Join(outside, "vendor", "acme", "rules", "keep.md"), "user data")
		mustSymlink(t, outside, filepath.Join(ws, ".okf"))
		if err := registry.RemoveVendorDir(ws, "acme/rules"); err == nil {
			t.Fatal("expected vendor root refusal")
		}
		if !exists(filepath.Join(outside, "vendor", "acme", "rules", "keep.md")) {
			t.Fatal("outside directory deleted")
		}
	})
	t.Run("a bundle that is itself a symlink is unlinked, not followed", func(t *testing.T) {
		ws, outside := vendorWorkspace(t)
		mustWrite(t, filepath.Join(outside, "keep.md"), "user data")
		mustSymlink(t, outside, filepath.Join(ws, ".okf", "vendor", "evil"))
		if err := registry.RemoveVendorDir(ws, "evil"); err != nil {
			t.Fatalf("RemoveVendorDir: %v", err)
		}
		if exists(filepath.Join(ws, ".okf", "vendor", "evil")) || !exists(filepath.Join(outside, "keep.md")) {
			t.Fatal("expected only the link to be removed")
		}
	})
	t.Run("unsafe id", func(t *testing.T) {
		ws, outside := vendorWorkspace(t)
		mustWrite(t, filepath.Join(outside, "keep.md"), "user data")
		if err := registry.RemoveVendorDir(ws, "../../outside"); err == nil {
			t.Fatal("expected unsafe id refusal")
		}
		if !exists(filepath.Join(outside, "keep.md")) {
			t.Fatal("outside directory deleted")
		}
	})
}

func TestStaging(t *testing.T) {
	t.Run("lives outside the vendor root", func(t *testing.T) {
		ws, _ := vendorWorkspace(t)
		s, err := registry.NewStaging(ws)
		if err != nil {
			t.Fatal(err)
		}
		if filepath.Dir(s.Dir) != filepath.Join(ws, ".okf") {
			t.Fatalf("staging dir %q must be created directly under .okf", s.Dir)
		}
		if err := s.Discard(); err != nil || exists(s.Dir) {
			t.Fatalf("discard: err=%v, still exists=%v", err, exists(s.Dir))
		}
	})

	t.Run("discard refuses a symlink swapped in for the staging dir", func(t *testing.T) {
		ws, outside := vendorWorkspace(t)
		mustWrite(t, filepath.Join(outside, "keep.md"), "user data")
		s, err := registry.NewStaging(ws)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(s.Dir); err != nil {
			t.Fatal(err)
		}
		mustSymlink(t, outside, s.Dir)
		if err := s.Discard(); err == nil {
			t.Fatal("discard through a swapped-in symlink must be refused")
		}
		if !exists(filepath.Join(outside, "keep.md")) {
			t.Fatal("outside file deleted through staging symlink")
		}
	})

	t.Run("commit installs and replaces a previous installation", func(t *testing.T) {
		ws, _ := vendorWorkspace(t)
		dest := filepath.Join(ws, ".okf", "vendor", "acme", "rules")
		mustWrite(t, filepath.Join(dest, "old.md"), "old")
		s, err := registry.NewStaging(ws)
		if err != nil {
			t.Fatal(err)
		}
		mustWrite(t, filepath.Join(s.Dir, "index.md"), "new")
		if err := s.Commit(ws, "acme/rules"); err != nil {
			t.Fatal(err)
		}
		if !exists(filepath.Join(dest, "index.md")) || exists(filepath.Join(dest, "old.md")) {
			t.Fatal("previous installation must be replaced, not merged")
		}
		entries, _ := os.ReadDir(filepath.Join(ws, ".okf"))
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".vendor-") {
				t.Fatalf("leftover staging entry %q", e.Name())
			}
		}
	})

	t.Run("commit replaces a symlinked installation without following it", func(t *testing.T) {
		ws, outside := vendorWorkspace(t)
		mustWrite(t, filepath.Join(outside, "keep.md"), "user data")
		dest := filepath.Join(ws, ".okf", "vendor", "linked")
		mustSymlink(t, outside, dest)
		s, err := registry.NewStaging(ws)
		if err != nil {
			t.Fatal(err)
		}
		mustWrite(t, filepath.Join(s.Dir, "index.md"), "new")
		if err := s.Commit(ws, "linked"); err != nil {
			t.Fatal(err)
		}
		if fi, err := os.Lstat(dest); err != nil || !fi.IsDir() {
			t.Fatalf("symlink must be replaced by the staged directory: %v %v", fi, err)
		}
		if !exists(filepath.Join(outside, "keep.md")) {
			t.Fatal("outside file deleted through the replaced symlink")
		}
	})

	t.Run("commit refuses an escaping id", func(t *testing.T) {
		ws, outside := vendorWorkspace(t)
		s, err := registry.NewStaging(ws)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Commit(ws, "../../outside/planted"); err == nil {
			t.Fatal("escaping id must be refused")
		}
		if exists(filepath.Join(outside, "planted")) {
			t.Fatal("staging moved outside the vendor root")
		}
	})
}
