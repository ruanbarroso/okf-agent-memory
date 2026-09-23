package gitsync

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/okf-memory/okf-agent-memory/pkg/okf"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available on PATH")
	}
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_EDITOR=true", "GIT_PAGER=cat")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s failed: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func identity(t *testing.T, dir string) {
	t.Helper()
	gitIn(t, dir, "config", "user.email", "test@example.com")
	gitIn(t, dir, "config", "user.name", "Test Agent")
}

func newBundle(t *testing.T, dir string) string {
	t.Helper()
	if err := okf.InitBundle(dir); err != nil {
		t.Fatalf("InitBundle: %v", err)
	}
	return dir
}

func saveConcept(t *testing.T, bundle, id, title string) {
	t.Helper()
	c := &okf.Concept{
		ID:          id,
		Path:        id + ".md",
		Type:        "Fact",
		Title:       title,
		Description: "Test concept " + title,
		Body:        "# " + title + "\n\nBody of " + title + ".",
	}
	if err := okf.SaveConcept(bundle, c, true, true, true, "agent/test"); err != nil {
		t.Fatalf("SaveConcept(%s): %v", id, err)
	}
}

func mustInit(t *testing.T, bundle string) *InitResult {
	t.Helper()
	res, err := Init(bundle, "origin", "", "", true, true)
	if err != nil {
		t.Fatalf("gitsync.Init: %v", err)
	}
	return res
}

// twoAgentFixture builds the canonical multi-agent topology:
// a bare remote, agent A (its own clone) that has published once, and agent B
// (a fresh clone) ready to write.
func twoAgentFixture(t *testing.T) (remote, a, b string) {
	t.Helper()
	base := t.TempDir()
	remote = filepath.Join(base, "remote.git")
	gitIn(t, base, "init", "--bare", "-b", "main", filepath.Base(remote))

	a = filepath.Join(base, "alpha")
	if err := os.MkdirAll(a, 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, a, "init", "-b", "main")
	identity(t, a)

	bundleA := newBundle(t, filepath.Join(a, "knowledge"))
	saveConcept(t, bundleA, "decisions/base", "Base Decision")

	_ = mustInit(t, bundleA)
	gitIn(t, a, "remote", "add", "origin", filepath.ToSlash(remote))

	// Sync performs the initial commit and push itself — no manual git.
	res, err := AutoPublish(bundleA, "initial publish")
	if err != nil || res == nil || res.State != "pushed" {
		t.Fatalf("initial publish failed: res=%+v err=%v", res, err)
	}

	b = filepath.Join(base, "beta")
	gitIn(t, base, "clone", filepath.ToSlash(remote), "beta")
	identity(t, b)
	return remote, a, b
}

// ---------------------------------------------------------------------------
// Pure-function tests
// ---------------------------------------------------------------------------

func TestConfigRoundtrip(t *testing.T) {
	dir := t.TempDir()
	cfg, found, err := LoadConfig(dir)
	if err != nil || found || cfg != nil {
		t.Fatalf("missing config must be a silent (nil,false,nil): got (%+v,%v,%v)", cfg, found, err)
	}

	want := DefaultConfig()
	want.AgentID = "agent/tester"
	want.DebounceMS = 500
	if err := SaveConfig(dir, want); err != nil {
		t.Fatal(err)
	}
	got, found, err := LoadConfig(dir)
	if err != nil || !found || got == nil {
		t.Fatalf("LoadConfig after save: found=%v err=%v", found, err)
	}
	if got.AgentID != "agent/tester" || got.DebounceMS != 500 || !got.Enabled ||
		got.Remote != "origin" || got.Branch != "main" || !got.AutoPull || !got.AutoPush {
		t.Fatalf("config roundtrip lost fields: %+v", got)
	}

	// Zero fields fall back to defaults.
	if err := os.WriteFile(filepath.Join(dir, ConfigFileName), []byte(`{"enabled":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, _, err = LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Remote != "origin" || got.Branch != "main" || got.AgentID != "agent/local" {
		t.Fatalf("defaults not applied to sparse config: %+v", got)
	}
}

func TestUnionLines(t *testing.T) {
	a := "# Index\n\n- [A](a.md) — alpha\n"
	b := "# Index\n\n- [A](a.md) — alpha\n- [B](b.md) — beta\n"
	got := string(unionLines(a, b))
	if !strings.Contains(got, "[A](a.md)") || !strings.Contains(got, "[B](b.md)") {
		t.Fatalf("union lost a side:\n%s", got)
	}
	if strings.Count(got, "[A](a.md)") != 1 {
		t.Fatalf("union duplicated a line:\n%s", got)
	}
	if !strings.HasSuffix(got, "\n") || strings.HasSuffix(got, "\n\n") {
		t.Fatalf("union has bad trailing whitespace: %q", got)
	}
}

func TestIsIndexRel(t *testing.T) {
	cases := []struct {
		bundle, path string
		want         bool
	}{
		{"knowledge", "knowledge/index.md", true},
		{"knowledge", "knowledge/decisions/index.md", true},
		{"knowledge", "knowledge/decisions/adr.md", false},
		{"knowledge", "knowledge/log.md", false},
		{"knowledge", "other/index.md", false},
		{".", "index.md", true},
		{".", "decisions/index.md", false},
	}
	for _, c := range cases {
		if got := isIndexRel(c.bundle, c.path); got != c.want {
			t.Errorf("isIndexRel(%q,%q) = %v, want %v", c.bundle, c.path, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Contract: without sync enabled, a Git-less directory behaves exactly as
// before. Nothing is required, nothing happens.
// ---------------------------------------------------------------------------

func TestNoGitOperationUnchanged(t *testing.T) {
	requireGit(t)
	bundle := newBundle(t, filepath.Join(t.TempDir(), "memory"))
	saveConcept(t, bundle, "decisions/plain", "Plain Decision")

	// Kill switch must also silence an enabled config.
	t.Setenv(EnvDisable, "1")

	res, err := AutoPublish(bundle, "should be a no-op")
	if res != nil || err != nil {
		t.Fatalf("AutoPublish without sync must be a silent no-op, got res=%+v err=%v", res, err)
	}

	cfg, reason, err := ActiveConfig(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if cfg != nil {
		t.Fatalf("ActiveConfig must be nil when disabled, got %+v", cfg)
	}
	if reason == "" {
		t.Fatal("ActiveConfig must explain why sync is off")
	}

	st, err := Status(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if st.SyncEnabled {
		t.Fatalf("status must report sync off, got %+v", st)
	}

	pub, err := Publish(bundle, "explicit publish is also local-only")
	if err != nil {
		t.Fatal(err)
	}
	if pub.State != "local_only" {
		t.Fatalf("explicit publish without sync must be local_only, got %+v", pub)
	}
}

// ---------------------------------------------------------------------------
// Contract: with sync enabled, publishing is automatic and honest.
// ---------------------------------------------------------------------------

func TestInitCreatesStandaloneRepoAndPublishes(t *testing.T) {
	requireGit(t)
	root := t.TempDir()
	bundle := newBundle(t, filepath.Join(root, "memory"))

	cfg, err := Init(bundle, "origin", "", "", true, true)
	if err != nil {
		t.Fatal(err)
	}
	identity(t, cfg.RepoRoot)

	if !cfg.RepoCreated {
		t.Fatalf("expected a standalone repo to be created at %s", bundle)
	}
	if cfg.RepoRoot != bundle {
		t.Fatalf("repo for a non-'knowledge' bundle must be the bundle itself: %s", cfg.RepoRoot)
	}
	if _, err := os.Stat(filepath.Join(bundle, ConfigFileName)); err != nil {
		t.Fatalf("config file not written: %v", err)
	}

	st, err := Status(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if !st.SyncEnabled {
		t.Fatalf("status must show sync enabled: %+v", st)
	}

	// A write now publishes automatically (no remote yet: local commit).
	saveConcept(t, bundle, "decisions/first", "First Decision")
	res, err := AutoPublish(bundle, "create concept decisions/first")
	if err != nil {
		t.Fatal(err)
	}
	if res == nil || res.State != "local_committed" {
		t.Fatalf("expected local_committed, got %+v", res)
	}
	if got := gitIn(t, cfg.RepoRoot, "rev-list", "--count", "HEAD"); got != "1" {
		t.Fatalf("expected exactly 1 commit, got %s", got)
	}

	// Refresh without a remote is an explicit, non-fatal no-op.
	rr, err := Refresh(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if rr.State != "no_remote" {
		t.Fatalf("expected refresh to report no_remote, got %+v", rr)
	}
}

func TestEnableDisable(t *testing.T) {
	requireGit(t)
	bundle := newBundle(t, filepath.Join(t.TempDir(), "memory"))
	_ = mustInit(t, bundle)

	saveConcept(t, bundle, "decisions/x", "X")
	if err := SetEnabled(bundle, false); err != nil {
		t.Fatal(err)
	}
	res, err := AutoPublish(bundle, "disabled means no-op")
	if res != nil || err != nil {
		t.Fatalf("disabled sync must disable automation, got %+v %v", res, err)
	}
	if err := SetEnabled(bundle, true); err != nil {
		t.Fatal(err)
	}
	res, err = AutoPublish(bundle, "re-enabled publishes")
	if err != nil {
		t.Fatal(err)
	}
	if res == nil || res.State != "local_committed" {
		t.Fatalf("re-enabled sync must publish, got %+v", res)
	}
}

// Two agents publishing different concepts in the same bundle must both
// land on the remote: the push rejection resolves through fetch+rebase with
// log.md and index.md conflicts auto-merged.
func TestTwoAgentsDifferentConceptsAutoMerge(t *testing.T) {
	requireGit(t)
	_, a, b := twoAgentFixture(t)
	bundleA := filepath.Join(a, "knowledge")
	bundleB := filepath.Join(b, "knowledge")

	// The config traveled with the repository: B never ran 'okf sync init'.
	if _, found, _ := LoadConfig(bundleB); !found {
		t.Fatal("sync config should have arrived in B via git clone")
	}

	// Agent A publishes a concept in decisions/.
	saveConcept(t, bundleA, "decisions/adr-a", "Decision A")
	resA, err := AutoPublish(bundleA, "create concept decisions/adr-a")
	if err != nil {
		t.Fatal(err)
	}
	if resA == nil || resA.State != "pushed" {
		t.Fatalf("A publish: %+v", resA)
	}

	// Agent B (stale, unaware of A) publishes a concept in architecture/.
	// log.md was appended by both -> rebase conflict -> semantic union.
	saveConcept(t, bundleB, "architecture/component-b", "Component B")
	resB, err := AutoPublish(bundleB, "create concept architecture/component-b")
	if err != nil {
		t.Fatal(err)
	}
	if resB == nil || resB.State != "pushed" {
		t.Fatalf("B publish should rebase and push, got %+v (err=%v)", resB, err)
	}
	if !resB.Rebased {
		t.Fatalf("B publish should have rebased over A's commit: %+v", resB)
	}

	// Both entries must exist on the remote, and A must see B's work after fetch.
	gitIn(t, a, "fetch", "origin")
	remoteLog := gitIn(t, a, "show", "origin/main:knowledge/log.md")
	if !strings.Contains(remoteLog, "decisions/adr-a") {
		t.Fatalf("remote log missing A's entry:\n%s", remoteLog)
	}
	if !strings.Contains(remoteLog, "architecture/component-b") {
		t.Fatalf("remote log missing B's entry (log merge dropped a side):\n%s", remoteLog)
	}
	remoteIndex := gitIn(t, a, "show", "origin/main:knowledge/architecture/index.md")
	if !strings.Contains(remoteIndex, "component-b") {
		t.Fatalf("remote architecture index missing B's concept:\n%s", remoteIndex)
	}
}

// Two agents editing the SAME concept file is a real knowledge conflict:
// sync must fail closed — report it, abort the rebase, keep the local commit,
// publish nothing broken.
func TestConceptConflictFailsClosed(t *testing.T) {
	requireGit(t)
	_, a, b := twoAgentFixture(t)
	bundleA := filepath.Join(a, "knowledge")
	bundleB := filepath.Join(b, "knowledge")
	remotePath := "knowledge/decisions/base.md"

	// rewriteBody edits the concept body while preserving the frontmatter
	// SaveConcept wrote — the way a real concept edit happens.
	rewriteBody := func(bundle, from, to string) {
		t.Helper()
		p := filepath.Join(bundle, "decisions", "base.md")
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		content := strings.Replace(string(data), from, to, 1)
		if content == string(data) {
			t.Fatalf("marker %q not found in %s", from, p)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// A rewrites the concept body and publishes.
	rewriteBody(bundleA, "Body of Base Decision.", "A was here.")
	resA, err := AutoPublish(bundleA, "rewrite base by A")
	if err != nil || resA == nil || resA.State != "pushed" {
		t.Fatalf("A publish: %+v err=%v", resA, err)
	}

	// B, still on the old base, rewrites the same concept differently.
	rewriteBody(bundleB, "Body of Base Decision.", "B was here.")
	resB, err := AutoPublish(bundleB, "rewrite base by B")
	if err != nil {
		t.Fatal(err)
	}
	if resB == nil || resB.State != "conflict" {
		t.Fatalf("B publish must end in conflict, got %+v", resB)
	}
	if len(resB.Conflicts) == 0 || !containsFold(resB.Conflicts, remotePath) {
		t.Fatalf("conflict result must name the disputed file, got %+v", resB.Conflicts)
	}

	// B's local commit survives, unpublished.
	if ahead := gitIn(t, b, "rev-list", "--count", "origin/main..HEAD"); ahead != "1" {
		t.Fatalf("B's local commit should survive the abort, ahead=%s", ahead)
	}
	remoteBody := gitIn(t, b, "show", "origin/main:"+remotePath)
	if !strings.Contains(remoteBody, "A was here.") || strings.Contains(remoteBody, "B was here.") {
		t.Fatalf("remote must hold A's version only, got:\n%s", remoteBody)
	}

	// After B resolves by hand (take the remote version, keep both facts) and
	// writes again, sync recovers and publishes the resolution.
	gitIn(t, b, "reset", "--hard", "origin/main")
	rewriteBody(bundleB, "A was here.", "A was here. B was here.")
	resFix, err := AutoPublish(bundleB, "resolution")
	if err != nil {
		t.Fatal(err)
	}
	if resFix == nil || resFix.State != "pushed" {
		t.Fatalf("publish after manual resolution should push, got %+v", resFix)
	}
}

// A clean tree with unpushed local commits (conflict resolved by hand, an
// aborted publish, a fixed validation) must still publish: "publish" means
// make local memory public, not "commit whatever is dirty right now".
func TestPublishPushesUnpushedCommits(t *testing.T) {
	requireGit(t)
	base := t.TempDir()
	remote := filepath.Join(base, "remote.git")
	gitIn(t, base, "init", "--bare", "-b", "main", filepath.Base(remote))

	a := filepath.Join(base, "alpha")
	if err := os.MkdirAll(a, 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, a, "init", "-b", "main")
	identity(t, a)
	bundle := newBundle(t, filepath.Join(a, "knowledge"))
	saveConcept(t, bundle, "decisions/x", "Decision X")
	_ = mustInit(t, bundle)

	// No remote yet: the first publish commits locally.
	res, err := AutoPublish(bundle, "first write")
	if err != nil || res == nil || res.State != "local_committed" {
		t.Fatalf("first publish: %+v err=%v", res, err)
	}

	// Remote appears later; the tree is clean but the commit is unpushed.
	gitIn(t, a, "remote", "add", "origin", filepath.ToSlash(remote))
	res, err = AutoPublish(bundle, "nothing dirty, one commit waiting")
	if err != nil {
		t.Fatal(err)
	}
	if res == nil || res.State != "pushed" {
		t.Fatalf("clean tree with unpushed commit must push, got %+v", res)
	}
	if got := gitIn(t, remote, "log", "--oneline"); !strings.Contains(got, "first write") {
		t.Fatalf("remote missing the earlier commit:\n%s", got)
	}

	// A third publish with nothing dirty and nothing ahead is an honest no-op.
	res, err = AutoPublish(bundle, "truly nothing")
	if err != nil {
		t.Fatal(err)
	}
	if res == nil || res.State != "nothing_to_do" {
		t.Fatalf("expected nothing_to_do, got %+v", res)
	}
}

// A structurally broken bundle must never be committed or pushed.
func TestPublishValidationGate(t *testing.T) {
	requireGit(t)
	root := t.TempDir()
	bundle := newBundle(t, filepath.Join(root, "memory"))
	cfg := mustInit(t, bundle)
	identity(t, cfg.RepoRoot)

	saveConcept(t, bundle, "decisions/good", "Good Decision")
	if res, err := AutoPublish(bundle, "good write"); err != nil || res == nil || res.State != "local_committed" {
		t.Fatalf("good write should commit, got %+v err=%v", res, err)
	}

	// Break the bundle: a concept file with no frontmatter at all.
	if err := os.WriteFile(filepath.Join(bundle, "decisions", "broken.md"),
		[]byte("no frontmatter, just text\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := AutoPublish(bundle, "bad write")
	if err != nil {
		t.Fatal(err)
	}
	if res == nil || res.State != "validate_failed" {
		t.Fatalf("broken bundle must end validate_failed, got %+v", res)
	}
	if len(res.Validation) == 0 {
		t.Fatal("validate_failed must carry findings")
	}
	if got := gitIn(t, cfg.RepoRoot, "rev-list", "--count", "HEAD"); got != "1" {
		t.Fatalf("no commit may exist for a broken bundle, found %s", got)
	}
}

func containsFold(list []string, want string) bool {
	for _, s := range list {
		if strings.Contains(strings.ToLower(s), strings.ToLower(want)) {
			return true
		}
	}
	return false
}
