package okf_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/okf-memory/okf-agent-memory/pkg/okf"
)

// TestSaveConceptFromSymlinkedWorkingDirectory guards the false positive where a bundle
// reached through a symlinked working directory (macOS /tmp -> /private/tmp) was refused as
// an escape: the root stayed relative while the target resolved to the real path.
func TestSaveConceptFromSymlinkedWorkingDirectory(t *testing.T) {
	realParent := t.TempDir()
	writeSymlinkTestBundle(t, filepath.Join(realParent, "knowledge"))
	linkParent := filepath.Join(t.TempDir(), "link")
	symlinkOrSkip(t, realParent, linkParent)

	t.Chdir(linkParent)

	b, err := okf.LoadBundle("knowledge")
	if err != nil {
		t.Fatalf("load through symlinked cwd: %v", err)
	}
	c, ok := b.Concepts["note"]
	if !ok {
		t.Fatalf("expected concept note, got %d", len(b.Concepts))
	}
	c.Description = "edited from a symlinked working directory"
	if err := okf.SaveConcept("knowledge", c, okf.SaveOptions{Actor: "test"}); err != nil {
		t.Fatalf("save through symlinked cwd must not be refused as a traversal: %v", err)
	}
	if _, err := os.Stat(filepath.Join(realParent, "knowledge", "note.md")); err != nil {
		t.Fatalf("concept not written at its real path: %v", err)
	}
}
