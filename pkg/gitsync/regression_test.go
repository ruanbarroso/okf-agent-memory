package gitsync

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Regressions found running sync across a real fleet (2026-10-18).

// rootBundle is a standalone bundle that IS the repository (bundleRel "."),
// the layout every fleet machine uses, with one locally committed concept.
func rootBundle(t *testing.T) (bundle string, cfg *InitResult) {
	t.Helper()
	bundle = newBundle(t, filepath.Join(t.TempDir(), "memory"))
	cfg = mustInit(t, bundle)
	identity(t, cfg.RepoRoot)
	saveConcept(t, bundle, "decisions/base", "Base Decision")
	if res, err := AutoPublish(bundle, "base"); err != nil || res == nil || res.State != "local_committed" {
		t.Fatalf("base commit: %+v err=%v", res, err)
	}
	return bundle, cfg
}

// The first porcelain line starts with a blank (index column). Trimming the
// whole output before slicing line[3:] ate the first character of the
// alphabetically first path: "bugfixes/index.md" was reported as
// "ugfixes/index.md".
func TestDirtyPathsKeepsFirstCharacter(t *testing.T) {
	requireGit(t)
	bundle, cfg := rootBundle(t)
	for _, f := range []string{"aaa.md", "bbb.md", "ação.md"} {
		if err := os.WriteFile(filepath.Join(bundle, f), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitIn(t, cfg.RepoRoot, "add", "--", "aaa.md", "bbb.md", "ação.md")
	gitIn(t, cfg.RepoRoot, "commit", "-q", "-m", "files")
	if err := os.Remove(filepath.Join(bundle, "aaa.md")); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"bbb.md", "ação.md"} {
		if err := os.WriteFile(filepath.Join(bundle, f), []byte("changed\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitIn(t, cfg.RepoRoot, "mv", "decisions/base.md", "decisions/renamed.md")

	got, err := dirtyPaths(cfg.RepoRoot, []string{"."})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"aaa.md", "ação.md", "bbb.md", "decisions/renamed.md"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("dirtyPaths = %q, want %q", got, want)
	}
}

// okf never deletes concept files, so a file that vanished from disk was
// removed by something else. Publish must not turn that into a deletion for
// every agent; it reports the file instead. A deliberate `git rm` still goes.
func TestPublishNeverStagesUnstagedDeletions(t *testing.T) {
	requireGit(t)
	bundle, cfg := rootBundle(t)
	saveConcept(t, bundle, "bugfixes/kept", "Kept")
	saveConcept(t, bundle, "bugfixes/removed", "Removed")
	if res, err := AutoPublish(bundle, "two more"); err != nil || res == nil || res.State != "local_committed" {
		t.Fatalf("setup commit: %+v err=%v", res, err)
	}

	if err := os.Remove(filepath.Join(bundle, "bugfixes", "kept.md")); err != nil {
		t.Fatal(err)
	}
	gitIn(t, cfg.RepoRoot, "rm", "-q", "bugfixes/removed.md")
	saveConcept(t, bundle, "decisions/new", "New")

	res, err := AutoPublish(bundle, "new concept")
	if err != nil || res == nil {
		t.Fatalf("publish: %+v err=%v", res, err)
	}
	if res.State != "local_committed" {
		t.Fatalf("expected local_committed, got %+v", res)
	}
	if len(res.Skipped) != 1 || res.Skipped[0] != "bugfixes/kept.md" {
		t.Fatalf("Skipped = %q, want only bugfixes/kept.md", res.Skipped)
	}
	tree := gitIn(t, cfg.RepoRoot, "ls-tree", "-r", "--name-only", "HEAD")
	if !strings.Contains(tree, "bugfixes/kept.md") {
		t.Fatal("a file deleted outside okf was committed as a deletion")
	}
	if strings.Contains(tree, "bugfixes/removed.md") {
		t.Fatal("an explicit git rm must still be published")
	}
	if !strings.Contains(tree, "decisions/new.md") {
		t.Fatal("the new concept was not committed")
	}
}

const conflictedBody = "<<<<<<< HEAD\nours\n=======\ntheirs\n>>>>>>> 39ab9a3 (okf: update knowledge)\n"

// A merge left half-resolved must never be committed by publish.
func TestPublishRefusesConflictMarkersInWorkingTree(t *testing.T) {
	requireGit(t)
	bundle, cfg := rootBundle(t)
	before := gitIn(t, cfg.RepoRoot, "rev-parse", "HEAD")

	logPath := filepath.Join(bundle, "log.md")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, append([]byte(conflictedBody), data...), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := AutoPublish(bundle, "markers")
	if err != nil || res == nil {
		t.Fatalf("publish: %+v err=%v", res, err)
	}
	if res.State != "conflict" || !containsFold(res.Conflicts, "log.md") {
		t.Fatalf("expected conflict on log.md, got %+v", res)
	}
	if after := gitIn(t, cfg.RepoRoot, "rev-parse", "HEAD"); after != before {
		t.Fatal("a commit was created from a tree with conflict markers")
	}
	if staged := gitIn(t, cfg.RepoRoot, "diff", "--cached", "--name-only"); staged != "" {
		t.Fatalf("a refused publish must leave nothing staged, found %q", staged)
	}
}

// A concept that merely documents a conflict, quoting a single marker, is
// ordinary content and must publish.
func TestPublishAllowsLoneQuotedMarker(t *testing.T) {
	requireGit(t)
	bundle, _ := rootBundle(t)
	if err := os.WriteFile(filepath.Join(bundle, "decisions", "base.md"),
		[]byte("---\ntype: Fact\ntitle: Base\ndescription: d\n---\n\nThe file started with\n<<<<<<< HEAD\nand nothing else.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := AutoPublish(bundle, "docs")
	if err != nil || res == nil || res.State != "local_committed" {
		t.Fatalf("a lone quoted marker must not block publish, got %+v err=%v", res, err)
	}
}

// Markers that already reached a local commit (a rebase continued by hand)
// must not be pushed.
func TestPublishRefusesConflictMarkersInUnpublishedCommits(t *testing.T) {
	requireGit(t)
	remote, a, _ := twoAgentFixture(t)
	bundle := filepath.Join(a, "knowledge")
	remoteHead := gitIn(t, remote, "rev-parse", "main")

	if err := os.WriteFile(filepath.Join(bundle, "decisions", "base.md"),
		append([]byte("---\ntype: Fact\ntitle: Base\ndescription: d\n---\n\n"), []byte(conflictedBody)...), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, a, "commit", "-q", "-am", "hand-resolved rebase, markers left in")

	res, err := AutoPublish(bundle, "push it")
	if err != nil || res == nil {
		t.Fatalf("publish: %+v err=%v", res, err)
	}
	if res.State != "conflict" || !containsFold(res.Conflicts, "knowledge/decisions/base.md") {
		t.Fatalf("expected conflict on the marked concept, got %+v", res)
	}
	if got := gitIn(t, remote, "rev-parse", "main"); got != remoteHead {
		t.Fatal("commits with conflict markers reached the remote")
	}
}

// A rebase stopped on a conflict detaches HEAD, which the branch check does
// not catch; publishing there would commit the half-resolved tree.
func TestPublishRefusesDuringRebase(t *testing.T) {
	requireGit(t)
	bundle, cfg := rootBundle(t)
	gitDir := gitIn(t, cfg.RepoRoot, "rev-parse", "--absolute-git-dir")
	if err := os.MkdirAll(filepath.Join(gitDir, "rebase-merge"), 0o755); err != nil {
		t.Fatal(err)
	}
	before := gitIn(t, cfg.RepoRoot, "rev-parse", "HEAD")
	saveConcept(t, bundle, "decisions/during", "During Rebase")

	res, err := AutoPublish(bundle, "during rebase")
	if err != nil || res == nil {
		t.Fatalf("publish: %+v err=%v", res, err)
	}
	if res.State != "in_progress" {
		t.Fatalf("expected in_progress, got %+v", res)
	}
	if after := gitIn(t, cfg.RepoRoot, "rev-parse", "HEAD"); after != before {
		t.Fatal("publish committed during a rebase")
	}
}

func TestRedactSecrets(t *testing.T) {
	cases := []struct{ in, want string }{
		{"fatal: unable to access 'https://x-access-token:ghp_AbC123@github.com/o/r.git/'",
			"fatal: unable to access 'https://***@github.com/o/r.git/'"},
		{"https://user:pass@example.com/repo", "https://***@example.com/repo"},
		{"token ghp_Secret123 leaked", "token ghp_*** leaked"},
		{"bearer github_pat_11ABC_def end", "bearer github_pat_*** end"},
		{"git@github.com:o/r.git rejected", "git@github.com:o/r.git rejected"},
	}
	for _, c := range cases {
		if got := redactSecrets(c.in); got != c.want {
			t.Errorf("redactSecrets(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Credentials embedded in a remote URL must not leak through git errors.
func TestGitErrorsRedactRemoteCredentials(t *testing.T) {
	requireGit(t)
	_, cfg := rootBundle(t)
	gitIn(t, cfg.RepoRoot, "remote", "add", "origin", "https://x-access-token:ghp_LeakMe123@127.0.0.1:1/o/r.git")
	_, err := gitRunQuiet(cfg.RepoRoot, 20e9, "ls-remote", "https://x-access-token:ghp_LeakMe123@127.0.0.1:1/o/r.git")
	if err == nil {
		t.Skip("ls-remote unexpectedly succeeded")
	}
	if strings.Contains(err.Error(), "LeakMe123") {
		t.Fatalf("token leaked into error: %v", err)
	}
}

func TestRemoteErrorExplainsRejectedCredential(t *testing.T) {
	auth := errors.New("remote: Invalid username or token. Password authentication is not supported for Git operations.\nfatal: Authentication failed for 'https://github.com/o/r.git/'")
	if msg := remoteError("push to", "origin", auth).Error(); !strings.Contains(msg, "authentication rejected") {
		t.Fatalf("auth failure must say so, got %q", msg)
	}
	other := errors.New("fatal: unable to access: Could not resolve host: github.com")
	if msg := remoteError("push to", "origin", other).Error(); strings.Contains(msg, "authentication rejected") {
		t.Fatalf("network failure misreported as auth: %q", msg)
	}
}

// Fleet bundles live at the repository root. Concurrent writes there touch
// log.md and directory index.md files; both must auto-merge instead of
// failing closed and leaving a rebase for someone to finish by hand.
func TestRootBundleDirectoryIndexConflictsAutoMerge(t *testing.T) {
	requireGit(t)
	base := t.TempDir()
	remote := filepath.Join(base, "remote.git")
	gitIn(t, base, "init", "--bare", "-b", "main", filepath.Base(remote))

	a := newBundle(t, filepath.Join(base, "alpha"))
	cfgA := mustInit(t, a)
	identity(t, cfgA.RepoRoot)
	saveConcept(t, a, "verifications/base", "Base")
	gitIn(t, a, "remote", "add", "origin", filepath.ToSlash(remote))
	if res, err := AutoPublish(a, "base"); err != nil || res == nil || res.State != "pushed" {
		t.Fatalf("initial publish: %+v err=%v", res, err)
	}

	b := filepath.Join(base, "beta")
	gitIn(t, base, "clone", "-q", filepath.ToSlash(remote), "beta")
	identity(t, b)

	saveConcept(t, a, "verifications/from-alpha", "From Alpha")
	if res, err := AutoPublish(a, "alpha"); err != nil || res == nil || res.State != "pushed" {
		t.Fatalf("alpha publish: %+v err=%v", res, err)
	}
	saveConcept(t, b, "verifications/from-beta", "From Beta")
	res, err := AutoPublish(b, "beta")
	if err != nil || res == nil {
		t.Fatalf("beta publish: %+v err=%v", res, err)
	}
	if res.State != "pushed" || !res.Rebased {
		t.Fatalf("directory index conflict in a root bundle must auto-merge, got %+v", res)
	}
	tree := gitIn(t, remote, "ls-tree", "-r", "--name-only", "main")
	for _, want := range []string{"verifications/from-alpha.md", "verifications/from-beta.md"} {
		if !strings.Contains(tree, want) {
			t.Fatalf("remote missing %s:\n%s", want, tree)
		}
	}
	idx := gitIn(t, remote, "show", "main:verifications/index.md")
	if strings.Contains(idx, "<<<<<<<") || !strings.Contains(idx, "from-alpha.md") || !strings.Contains(idx, "from-beta.md") {
		t.Fatalf("merged index is wrong:\n%s", idx)
	}
}
