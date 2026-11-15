package registry_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/okf-memory/okf-agent-memory/pkg/registry"
)

// Registry manifests and bundle archives are untrusted input. These tests use
// only the client's public API (Resolve, DownloadAndExtract).

func serveBody(body []byte) roundTripFunc {
	return func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body))}, nil
	}
}

type tarEntry struct {
	name, body string
	typeflag   byte
}

// orderedTarGz builds an archive with entries in the given order (the order
// matters for the all-or-nothing checks).
func orderedTarGz(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for _, e := range entries {
		typeflag := e.typeflag
		if typeflag == 0 {
			typeflag = tar.TypeReg
		}
		hdr := &tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.body)), Typeflag: typeflag}
		if typeflag == tar.TypeDir {
			hdr.Mode, hdr.Size = 0o755, 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func extractInto(t *testing.T, archive []byte, targetDir string) error {
	t.Helper()
	client := registry.NewClient("https://registry.test")
	client.HTTPClient.Transport = serveBody(archive)
	return client.DownloadAndExtract(&registry.BundleManifest{ID: "acme/rules", Version: "1.0.0", DownloadURL: "https://registry.test/b.tar.gz"}, targetDir)
}

func listFiles(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && p != dir {
			rel, _ := filepath.Rel(dir, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	return out
}

func TestClient_ResolveRefusesUnsafeManifestID(t *testing.T) {
	for _, id := range []string{"../../../outside/planted", "acme/../../x", "/etc/okf", `acme\..\x`, "", "acme//rules", "./acme", "acme/."} {
		t.Run(id, func(t *testing.T) {
			client := registry.NewClient("https://registry.test")
			client.HTTPClient.Transport = serveBody([]byte(`{"id":` + strconv.Quote(id) + `,"version":"1.0.0","download_url":"/b.tar.gz"}`))
			m, err := client.Resolve("plant")
			if err == nil {
				t.Fatalf("expected manifest with id %q to be refused, got %+v", id, m)
			}
			if !strings.Contains(err.Error(), "unsafe bundle id "+strconv.Quote(id)) {
				t.Fatalf("error must name the offending id %q: %v", id, err)
			}
		})
	}
}

func TestClient_ResolveAcceptsScopedAndNestedManifestIDs(t *testing.T) {
	for _, id := range []string{"nextjs-15", "peter/django-5-rules", "@peter/django-5-rules", "acme/tools/linter", "weird..name"} {
		client := registry.NewClient("https://registry.test")
		client.HTTPClient.Transport = serveBody([]byte(`{"id":` + strconv.Quote(id) + `,"version":"1.0.0","download_url":"/b.tar.gz"}`))
		m, err := client.Resolve("x")
		if err != nil || m.ID != id {
			t.Fatalf("legitimate id %q must resolve unchanged: %+v, %v", id, m, err)
		}
	}
}

func TestClient_ResolveGitURLRefusesTraversal(t *testing.T) {
	client := registry.NewClient("https://registry.test")
	if m, err := client.Resolve("github.com/../outside@v1.0.0"); err == nil {
		t.Fatalf("expected git URL with a .. owner to be refused, got %+v", m)
	}
}

func TestDownloadAndExtract_RefusesEscapingEntries(t *testing.T) {
	for _, name := range []string{"../escape.md", "a/../../escape.md", "/abs/escape.md"} {
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			target := filepath.Join(base, "vendor", "acme", "rules")
			// The escaping entry comes last: nothing ahead of it may be written.
			err := extractInto(t, orderedTarGz(t, tarEntry{name: "index.md", body: "# ok"}, tarEntry{name: name, body: "pwn"}), target)
			if files := listFiles(t, base); len(files) != 3 { // vendor, vendor/acme, vendor/acme/rules
				t.Fatalf("nothing may be extracted from a refused archive, found %v", files)
			}
			if err == nil || !strings.Contains(err.Error(), "path traversal attempt") {
				t.Fatalf("expected archive entry %q to be refused, got %v", name, err)
			}
		})
	}
}

func TestDownloadAndExtract_RefusesWritingThroughPreexistingSymlink(t *testing.T) {
	t.Run("directory symlink", func(t *testing.T) {
		base := t.TempDir()
		target, outside := filepath.Join(base, "vendor", "rules"), filepath.Join(base, "outside")
		if err := os.MkdirAll(target, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(outside, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(target, "sub")); err != nil {
			t.Fatal(err)
		}
		err := extractInto(t, orderedTarGz(t, tarEntry{name: "index.md", body: "# ok"}, tarEntry{name: "sub/planted.md", body: "pwn"}), target)
		if files := listFiles(t, outside); len(files) != 0 {
			t.Fatalf("outside directory written: %v", files)
		}
		if err == nil || !strings.Contains(err.Error(), `archive entry "sub/planted.md"`) {
			t.Fatalf("expected write through directory symlink to be refused, got %v", err)
		}
		if _, err := os.Stat(filepath.Join(target, "index.md")); !os.IsNotExist(err) {
			t.Fatalf("archive must not be half-extracted: %v", err)
		}
	})
	t.Run("file symlink", func(t *testing.T) {
		base := t.TempDir()
		target, victim := filepath.Join(base, "vendor", "rules"), filepath.Join(base, "victim.md")
		if err := os.MkdirAll(target, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(victim, []byte("user data"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(victim, filepath.Join(target, "index.md")); err != nil {
			t.Fatal(err)
		}
		err := extractInto(t, orderedTarGz(t, tarEntry{name: "index.md", body: "pwn"}), target)
		if b, _ := os.ReadFile(victim); string(b) != "user data" {
			t.Fatalf("outside file overwritten: %q", b)
		}
		if err == nil || !strings.Contains(err.Error(), `archive entry "index.md"`) {
			t.Fatalf("expected write through file symlink to be refused, got %v", err)
		}
	})
	t.Run("directory entry through symlink", func(t *testing.T) {
		base := t.TempDir()
		target, outside := filepath.Join(base, "vendor", "rules"), filepath.Join(base, "outside")
		if err := os.MkdirAll(target, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(outside, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(target, "sub")); err != nil {
			t.Fatal(err)
		}
		// index.md first, so the archive has no single root wrapper to strip.
		err := extractInto(t, orderedTarGz(t, tarEntry{name: "index.md", body: "# ok"}, tarEntry{name: "sub/nested/", typeflag: tar.TypeDir}), target)
		if files := listFiles(t, outside); len(files) != 0 {
			t.Fatalf("outside directory written: %v", files)
		}
		if err == nil {
			t.Fatal("expected directory entry through symlink to be refused")
		}
	})
}

// Control: a symlink that stays inside the target directory, a symlinked target
// directory, and ordinary nested entries keep extracting.
func TestDownloadAndExtract_LegitimateLayoutsStillExtract(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real-rules")
	if err := os.MkdirAll(filepath.Join(real, "shared"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(real, "shared"), filepath.Join(real, "alias")); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(base, "rules")
	if err := os.Symlink(real, target); err != nil {
		t.Fatal(err)
	}
	err := extractInto(t, orderedTarGz(t,
		tarEntry{name: "index.md", body: "# ok"},
		tarEntry{name: "decisions/deep/adr.md", body: "adr"},
		tarEntry{name: "alias/note.md", body: "note"},
		tarEntry{name: "weird..name.md", body: "dots"},
	), target)
	if err != nil {
		t.Fatalf("legitimate archive refused: %v", err)
	}
	for p, want := range map[string]string{"index.md": "# ok", "decisions/deep/adr.md": "adr", "shared/note.md": "note", "weird..name.md": "dots"} {
		if b, err := os.ReadFile(filepath.Join(real, filepath.FromSlash(p))); err != nil || string(b) != want {
			t.Fatalf("expected %s = %q, got %q (%v)", p, want, b, err)
		}
	}
}
