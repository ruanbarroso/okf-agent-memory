package sync_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/okf-memory/okf-agent-memory/pkg/sync"
	"github.com/okf-memory/okf-agent-memory/pkg/vault"
)

// End-to-end: a real Push against an in-process hub must not persist sync state
// through a .okf symlink that leaves the bundle, and must report the failure.
func TestEngine_PushRefusesSyncStateOutsideBundle(t *testing.T) {
	server := sync.NewServer("")
	root := t.TempDir()
	bundle, outside := filepath.Join(root, "bundle"), filepath.Join(root, "outside")
	for _, d := range []string{bundle, outside} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(bundle, "index.md"), []byte("# Knowledge Index\nokf_version: 0.2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(bundle, ".okf")); err != nil {
		t.Fatal(err)
	}
	engine := sync.NewEngine(newTestClient(server.Handler(), "token-1"), "boundary-vault", testVaultKey(9), bundle)
	_, err := engine.Push(context.Background(), vault.CommitAuthor{ClientID: "client-1", Agent: "agent"}, "push")
	if err == nil || !strings.Contains(err.Error(), "outside bundle") {
		t.Fatalf("expected push to report refused state write, got %v", err)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("sync state escaped bundle: %d outside entries", len(entries))
	}
}
