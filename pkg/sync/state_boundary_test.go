package sync

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/okf-memory/okf-agent-memory/pkg/vault"
)

// Regression tests: sync state I/O must stay inside the resolved bundle root.
// A .okf directory or state-file symlink that leaves the bundle is refused with
// an error; symlinks that stay inside the bundle keep working.

func boundaryEngine(bundle string) *Engine {
	return &Engine{BundlePath: bundle, VaultID: "fixture", cachedHead: "fixture-head", cachedTree: vault.NewTree()}
}

func boundaryDirs(t *testing.T) (bundle, outside string) {
	t.Helper()
	root := t.TempDir()
	bundle, outside = filepath.Join(root, "bundle"), filepath.Join(root, "outside")
	for _, d := range []string{bundle, outside} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return bundle, outside
}

func outsideEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestSyncStateRefusesOkfDirectorySymlinkOutsideBundle(t *testing.T) {
	bundle, outside := boundaryDirs(t)
	if err := os.Symlink(outside, filepath.Join(bundle, syncStateDirName)); err != nil {
		t.Fatal(err)
	}
	err := boundaryEngine(bundle).saveState()
	if err == nil || !strings.Contains(err.Error(), "outside bundle") {
		t.Fatalf("expected outside-bundle refusal, got %v", err)
	}
	if names := outsideEntries(t, outside); len(names) != 0 {
		t.Fatalf("state I/O escaped bundle, outside dir now holds %v", names)
	}
}

func TestSyncStateRefusesDanglingOkfSymlinkOutsideBundle(t *testing.T) {
	bundle, outside := boundaryDirs(t)
	target := filepath.Join(outside, "not-yet-created")
	if err := os.Symlink(target, filepath.Join(bundle, syncStateDirName)); err != nil {
		t.Fatal(err)
	}
	if err := boundaryEngine(bundle).saveState(); err == nil {
		t.Fatal("expected refusal for dangling .okf symlink leaving the bundle")
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("directory created outside bundle: %v", err)
	}
}

func TestSyncStateFileSymlinkOutsideBundleIsNeitherWrittenNorRead(t *testing.T) {
	bundle, outside := boundaryDirs(t)
	if err := os.MkdirAll(filepath.Join(bundle, syncStateDirName), 0o700); err != nil {
		t.Fatal(err)
	}
	outsideState := filepath.Join(outside, "foreign-state.json")
	tree, err := vault.NewTree().Serialize()
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(syncState{Version: currentSyncStateVersion, VaultID: "fixture", Head: "foreign-head", Tree: tree})
	if err != nil {
		t.Fatal(err)
	}
	foreign := string(encoded)
	if err := os.WriteFile(outsideState, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideState, filepath.Join(bundle, syncStateDirName, syncStateFileName)); err != nil {
		t.Fatal(err)
	}
	reader := &Engine{BundlePath: bundle, VaultID: "fixture", cachedTree: vault.NewTree()}
	reader.loadState()
	if reader.cachedHead != "" {
		t.Fatalf("state was read through an escaping symlink: head=%q", reader.cachedHead)
	}
	if err := boundaryEngine(bundle).saveState(); err == nil || !strings.Contains(err.Error(), "outside bundle") {
		t.Fatalf("expected outside-bundle refusal, got %v", err)
	}
	if data, _ := os.ReadFile(outsideState); string(data) != foreign {
		t.Fatalf("outside file modified: %s", data)
	}
}

func TestSyncStateAllowsSymlinksThatStayInsideBundle(t *testing.T) {
	bundle, outside := boundaryDirs(t)
	real := filepath.Join(bundle, ".state-real")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(bundle, syncStateDirName)); err != nil {
		t.Fatal(err)
	}
	if err := boundaryEngine(bundle).saveState(); err != nil {
		t.Fatalf("in-bundle symlink refused: %v", err)
	}
	if _, err := os.Stat(filepath.Join(real, syncStateFileName)); err != nil {
		t.Fatalf("state not written inside bundle: %v", err)
	}
	reader := &Engine{BundlePath: bundle, VaultID: "fixture", cachedTree: vault.NewTree()}
	reader.loadState()
	if reader.cachedHead != "fixture-head" {
		t.Fatalf("state round-trip failed: head=%q", reader.cachedHead)
	}
	if names := outsideEntries(t, outside); len(names) != 0 {
		t.Fatalf("unexpected outside writes %v", names)
	}
}

func TestSyncStateUnderSymlinkedBundleRootStaysInResolvedRoot(t *testing.T) {
	bundle, _ := boundaryDirs(t)
	link := filepath.Join(filepath.Dir(bundle), "bundle-link")
	if err := os.Symlink(bundle, link); err != nil {
		t.Fatal(err)
	}
	if err := boundaryEngine(link).saveState(); err != nil {
		t.Fatalf("symlinked bundle root refused: %v", err)
	}
	if _, err := os.Stat(filepath.Join(bundle, syncStateDirName, syncStateFileName)); err != nil {
		t.Fatalf("state not written inside resolved bundle: %v", err)
	}
}
