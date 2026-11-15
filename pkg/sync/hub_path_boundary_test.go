package sync_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/okf-memory/okf-agent-memory/pkg/sync"
	"github.com/okf-memory/okf-agent-memory/pkg/vault"
)

// Regression tests: tree paths supplied through the hub (by any vault peer
// holding the key) must never make Pull or Sync write or delete outside the
// resolved bundle root. Escaping trees are refused with an error before any
// file is touched; legitimate in-bundle paths keep working.

const hubPathVault = "hub-path-vault"

var hubPathAuthor = vault.CommitAuthor{ClientID: "client-1", Agent: "agent"}

type hubPathFixture struct {
	server  *sync.Server
	key     []byte
	bundle  string
	outside string
}

func newHubPathFixture(t *testing.T) *hubPathFixture {
	t.Helper()
	root := t.TempDir()
	fx := &hubPathFixture{
		server:  sync.NewServer(""),
		key:     testVaultKey(11),
		bundle:  filepath.Join(root, "bundle"),
		outside: filepath.Join(root, "outside"),
	}
	for _, d := range []string{fx.bundle, fx.outside} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return fx
}

func (fx *hubPathFixture) engine(bundle string) *sync.Engine {
	return sync.NewEngine(newTestClient(fx.server.Handler(), "token"), hubPathVault, fx.key, bundle)
}

// publish plays a key-holding peer: it uploads a tree with exactly the given
// path -> content entries and advances the hub head to a commit on top of it.
func (fx *hubPathFixture) publish(t *testing.T, entries map[string]string) {
	t.Helper()
	ctx := context.Background()
	client := newTestClient(fx.server.Handler(), "peer")
	put := func(plain []byte) string {
		env, err := vault.EncryptPayload(plain, fx.key)
		if err != nil {
			t.Fatal(err)
		}
		h := vault.HashBlob(env)
		if err := client.PutBlob(ctx, hubPathVault, h, env); err != nil {
			t.Fatal(err)
		}
		return h
	}
	tree := vault.NewTree()
	for p, content := range entries {
		tree.Entries[p] = vault.TreeEntry{
			BlobHash:      put([]byte(content)),
			Size:          int64(len(content)),
			PlaintextHash: vault.HashPlaintext([]byte(content)),
			ModifiedAt:    time.Now().UTC().Format(time.RFC3339),
		}
	}
	treeBytes, err := tree.Serialize()
	if err != nil {
		t.Fatal(err)
	}
	head, err := client.GetHead(ctx, hubPathVault)
	if err != nil {
		t.Fatal(err)
	}
	var parent *string
	if head.HeadCommit != "" {
		parent = &head.HeadCommit
	}
	commitBytes, err := vault.NewCommit(parent, put(treeBytes), vault.CommitAuthor{ClientID: "peer", Agent: "peer"}, "peer").Serialize()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Commit(ctx, hubPathVault, put(commitBytes), parent); err != nil {
		t.Fatal(err)
	}
}

// files lists regular files and symlinks under dir, relative and slash-separated.
func files(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func expectRefused(t *testing.T, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("expected hub path refusal, got %v", err)
	}
}

func TestEngine_PullRefusesParentTraversalPath(t *testing.T) {
	fx := newHubPathFixture(t)
	fx.publish(t, map[string]string{
		"notes/legit.md":      "legit",
		"../outside/pwn.md":   "pwned",
		"a/../../outside/x":   "pwned",
		"../outside/sub/y.md": "pwned",
	})
	_, err := fx.engine(fx.bundle).Pull(context.Background())
	expectRefused(t, err)
	if got := files(t, fx.outside); len(got) != 0 {
		t.Fatalf("pull wrote outside the bundle: %v", got)
	}
	if got := files(t, fx.bundle); len(got) != 0 {
		t.Fatalf("refused pull must not be half-applied, bundle holds %v", got)
	}
}

func TestEngine_PullRefusesAbsolutePath(t *testing.T) {
	fx := newHubPathFixture(t)
	abs := filepath.ToSlash(filepath.Join(fx.outside, "pwn-abs.md"))
	fx.publish(t, map[string]string{abs: "pwned"})
	_, err := fx.engine(fx.bundle).Pull(context.Background())
	expectRefused(t, err)
	if got := files(t, fx.outside); len(got) != 0 {
		t.Fatalf("pull wrote outside the bundle: %v", got)
	}
	// filepath.Join would otherwise re-root the absolute path under the bundle.
	if got := files(t, fx.bundle); len(got) != 0 {
		t.Fatalf("absolute hub path was written (re-rooted) inside the bundle: %v", got)
	}
}

func TestEngine_PullRefusesWritingThroughSymlinkToOutside(t *testing.T) {
	fx := newHubPathFixture(t)
	if err := os.Symlink(fx.outside, filepath.Join(fx.bundle, "linked")); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(fx.outside, "victim.md")
	if err := os.WriteFile(victim, []byte("user data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(fx.bundle, "note.md")); err != nil {
		t.Fatal(err)
	}

	for name, entries := range map[string]map[string]string{
		"directory symlink": {"linked/pwn.md": "pwned"},
		"file symlink":      {"note.md": "overwritten"},
	} {
		t.Run(name, func(t *testing.T) {
			fx.publish(t, entries)
			_, err := fx.engine(fx.bundle).Pull(context.Background())
			expectRefused(t, err)
			if got := files(t, fx.outside); len(got) != 1 || got[0] != "victim.md" {
				t.Fatalf("pull wrote outside the bundle: %v", got)
			}
			if data, _ := os.ReadFile(victim); string(data) != "user data" {
				t.Fatalf("outside file modified through symlink: %q", data)
			}
		})
	}
}

func TestEngine_PullWritesLegitimateInBundlePaths(t *testing.T) {
	fx := newHubPathFixture(t)
	// In-bundle directory symlink and a symlinked bundle root must keep working.
	if err := os.MkdirAll(filepath.Join(fx.bundle, "real"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(fx.bundle, "real"), filepath.Join(fx.bundle, "alias")); err != nil {
		t.Fatal(err)
	}
	rootLink := filepath.Join(filepath.Dir(fx.bundle), "bundle-link")
	if err := os.Symlink(fx.bundle, rootLink); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"index.md":              "# index",
		"notes/deep/nested.md":  "nested",
		"alias/through-link.md": "via in-bundle symlink",
		"weird..name.md":        "dots inside a name are fine",
		"spaced name/ü.md":      "unicode",
	}
	fx.publish(t, want)
	res, err := fx.engine(rootLink).Pull(context.Background())
	if err != nil {
		t.Fatalf("legitimate pull refused: %v", err)
	}
	if len(res.UpdatedFiles) != len(want) {
		t.Fatalf("updated %d files, want %d", len(res.UpdatedFiles), len(want))
	}
	for p, content := range want {
		data, err := os.ReadFile(filepath.Join(fx.bundle, filepath.FromSlash(p)))
		if err != nil || string(data) != content {
			t.Fatalf("%s: got %q, %v", p, data, err)
		}
	}
	if data, _ := os.ReadFile(filepath.Join(fx.bundle, "real", "through-link.md")); string(data) != "via in-bundle symlink" {
		t.Fatalf("in-bundle symlink target not written: %q", data)
	}
	if got := files(t, fx.outside); len(got) != 0 {
		t.Fatalf("unexpected outside writes %v", got)
	}

	// Deleting through the same engine path set keeps working too.
	fx.publish(t, map[string]string{"index.md": "# index"})
	res, err = fx.engine(rootLink).Pull(context.Background())
	if err != nil {
		t.Fatalf("legitimate delete pull refused: %v", err)
	}
	if len(res.DeletedFiles) != len(want)-1 {
		t.Fatalf("deleted %v", res.DeletedFiles)
	}
	if _, err := os.Stat(filepath.Join(fx.bundle, "notes", "deep", "nested.md")); !os.IsNotExist(err) {
		t.Fatalf("in-bundle delete did not happen: %v", err)
	}
}

func TestEngine_PullRefusesHiddenPaths(t *testing.T) {
	for _, p := range []string{".okf-vault.json", ".okf/sync-state.json", ".git/hooks/post-checkout", "notes/.hidden.md"} {
		t.Run(p, func(t *testing.T) {
			fx := newHubPathFixture(t)
			fx.publish(t, map[string]string{p: "injected"})
			_, err := fx.engine(fx.bundle).Pull(context.Background())
			expectRefused(t, err)
			if _, err := os.Lstat(filepath.Join(fx.bundle, filepath.FromSlash(p))); !os.IsNotExist(err) {
				t.Fatalf("hidden hub path %s was written: %v", p, err)
			}
		})
	}
}

func TestEngine_PullRefusesDeletingThroughSymlinkToOutside(t *testing.T) {
	fx := newHubPathFixture(t)
	fx.publish(t, map[string]string{"docs/x.md": "doc"})
	if _, err := fx.engine(fx.bundle).Pull(context.Background()); err != nil {
		t.Fatal(err)
	}
	// docs/ is later replaced by a symlink to a directory outside the bundle
	// that happens to hold a file with the same name.
	if err := os.RemoveAll(filepath.Join(fx.bundle, "docs")); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(fx.outside, "x.md")
	if err := os.WriteFile(victim, []byte("user data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(fx.outside, filepath.Join(fx.bundle, "docs")); err != nil {
		t.Fatal(err)
	}
	fx.publish(t, map[string]string{"index.md": "# index"})
	_, err := fx.engine(fx.bundle).Pull(context.Background())
	expectRefused(t, err)
	if data, _ := os.ReadFile(victim); string(data) != "user data" {
		t.Fatalf("pull deleted/modified a file outside the bundle: %q", data)
	}
}

func TestEngine_SyncRefusesEscapingRemotePath(t *testing.T) {
	fx := newHubPathFixture(t)
	index := "# Knowledge Index\nokf_version: 0.2\n"
	if err := os.WriteFile(filepath.Join(fx.bundle, "index.md"), []byte(index), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.engine(fx.bundle).Push(context.Background(), hubPathAuthor, "first"); err != nil {
		t.Fatal(err)
	}
	fx.publish(t, map[string]string{"index.md": index, "../outside/pwn-sync.md": "pwned"})
	peerHead, err := newTestClient(fx.server.Handler(), "peer").GetHead(context.Background(), hubPathVault)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fx.bundle, "local.md"), []byte("local edit"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = fx.engine(fx.bundle).Sync(context.Background(), hubPathAuthor, "sync")
	expectRefused(t, err)
	if got := files(t, fx.outside); len(got) != 0 {
		t.Fatalf("sync wrote outside the bundle: %v", got)
	}
	after, err := newTestClient(fx.server.Handler(), "peer").GetHead(context.Background(), hubPathVault)
	if err != nil {
		t.Fatal(err)
	}
	if after.HeadCommit != peerHead.HeadCommit {
		t.Fatalf("refused sync still committed a merge on top of the escaping tree")
	}
}

func TestEngine_PullIgnoresPoisonedSyncStateDeleteBase(t *testing.T) {
	fx := newHubPathFixture(t)
	victim := filepath.Join(fx.outside, "victim.md")
	if err := os.WriteFile(victim, []byte("user data"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Sync state left by a binary that trusted hub paths: its tree (the next
	// pull's delete base) names a path outside the bundle.
	if err := os.MkdirAll(filepath.Join(fx.bundle, ".okf"), 0o700); err != nil {
		t.Fatal(err)
	}
	state := `{"version":1,"vault_id":"` + hubPathVault + `","head":"stale-head","tree":{"version":1,"entries":{"../outside/victim.md":{"blob_hash":"b","size":9,"plaintext_hash":"p"}}}}`
	if err := os.WriteFile(filepath.Join(fx.bundle, ".okf", "sync-state.json"), []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}
	fx.publish(t, map[string]string{"index.md": "# index"})
	if _, err := fx.engine(fx.bundle).Pull(context.Background()); err != nil {
		t.Fatalf("pull with ignored poisoned state failed: %v", err)
	}
	if data, _ := os.ReadFile(victim); string(data) != "user data" {
		t.Fatalf("pull deleted a file outside the bundle via the stored delete base: %q", data)
	}
}
