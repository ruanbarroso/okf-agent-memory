// Package gitsync implements OPTIONAL, automatic Git synchronization for OKF
// bundles, with no backend of any kind: the user's own Git remote (GitHub or
// otherwise) is the transport, the coordination point, and the history.
//
// Design contract:
//
//   - WITHOUT sync enabled, everything behaves exactly as before: a plain
//     directory with no Git repository is a fully valid memory. Nothing
//     changes, nothing is required.
//   - WITH sync enabled (.okf-sync.json inside the bundle + a Git repo),
//     synchronization is AUTOMATIC: writes are validated, committed and pushed
//     without being asked, and the bundle is refreshed from the remote at
//     session start.
//   - Concurrency between multiple agents is optimistic: fetch + rebase retry
//     on push rejection, structured auto-merge for log.md, and fail-closed
//     manual resolution for real concept conflicts. Never a silent overwrite,
//     never a force push to the published branch.
package gitsync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/okf-memory/okf-agent-memory/pkg/okf"
	okfsync "github.com/okf-memory/okf-agent-memory/pkg/sync"
)

// ConfigFileName is the per-bundle sync configuration file. It lives inside
// the bundle directory (like .okf-vault.json), is skipped by bundle loading,
// and travels with the repository so every agent cloning the repo inherits
// the same sync policy.
const ConfigFileName = ".okf-sync.json"

// EnvDisable completely disables automatic sync behavior when set to a
// truthy value, regardless of configuration. Kill switch for debugging.
const EnvDisable = "OKF_SYNC_DISABLE"

// EnvAgentID overrides the configured agent identity, so several machines can
// share one committed config while still signing their own commits.
const EnvAgentID = "OKF_SYNC_AGENT_ID"

// MaxPushRetries is how many fetch+rebase cycles a rejected push may go
// through before giving up and reporting a conflict.
const MaxPushRetries = 3

// Config controls optional Git synchronization for one bundle.
type Config struct {
	Enabled  bool   `json:"enabled"`
	Remote   string `json:"remote"`    // Git remote name (default "origin")
	Branch   string `json:"branch"`    // published branch (default "main")
	AgentID  string `json:"agent_id"`  // identity used in commit messages
	AutoPull bool   `json:"auto_pull"` // refresh from remote at session start
	AutoPush bool   `json:"auto_push"` // commit+push automatically after writes
	// StrictValidate gates the producer gate (orphans, broken links) in
	// addition to structural conformance. Off by default because a fresh
	// concept is routinely an orphan until it gets linked.
	StrictValidate bool     `json:"strict_validate"`
	DebounceMS     int      `json:"debounce_ms"` // MCP write batching window
	ExtraPaths     []string `json:"extra_paths"` // extra repo-relative paths allowed to be staged (e.g. "AGENTS.md")
}

// DefaultConfig returns the configuration written by `okf sync init`.
func DefaultConfig() *Config {
	return &Config{
		Enabled:        true,
		Remote:         "origin",
		Branch:         "main",
		AgentID:        "agent/local",
		AutoPull:       true,
		AutoPush:       true,
		StrictValidate: false,
		DebounceMS:     1500,
		ExtraPaths:     nil,
	}
}

// LoadConfig reads the sync configuration for a bundle. The second return
// value reports whether a config file exists at all.
func LoadConfig(bundleDir string) (*Config, bool, error) {
	cfgPath := filepath.Join(bundleDir, ConfigFileName)
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("gitsync: failed to read %s: %w", cfgPath, err)
	}
	var cfg Config
	if err := jsonUnmarshalStrict(data, &cfg); err != nil {
		return nil, true, fmt.Errorf("gitsync: failed to parse %s: %w", cfgPath, err)
	}
	if cfg.Remote == "" {
		cfg.Remote = "origin"
	}
	if cfg.Branch == "" {
		cfg.Branch = "main"
	}
	if cfg.AgentID == "" {
		cfg.AgentID = "agent/local"
	}
	if cfg.DebounceMS < 0 {
		cfg.DebounceMS = 0
	}
	return &cfg, true, nil
}

// SaveConfig writes the sync configuration into the bundle directory.
func SaveConfig(bundleDir string, cfg *Config) error {
	if cfg == nil {
		return errors.New("gitsync: cannot save nil config")
	}
	if cfg.Remote == "" {
		cfg.Remote = "origin"
	}
	if cfg.Branch == "" {
		cfg.Branch = "main"
	}
	if cfg.AgentID == "" {
		cfg.AgentID = "agent/local"
	}
	if cfg.DebounceMS < 0 {
		cfg.DebounceMS = 0
	}
	data, err := jsonMarshalIndent(cfg)
	if err != nil {
		return err
	}
	cfgPath := filepath.Join(bundleDir, ConfigFileName)
	// #nosec G306 -- sync config is non-secret, project-shared policy
	if err := os.WriteFile(cfgPath, data, 0o644); err != nil {
		return fmt.Errorf("gitsync: failed to write %s: %w", cfgPath, err)
	}
	return nil
}

// ActiveConfig returns the effective configuration if sync is enabled for
// this bundle right now (config present, enabled, not killed by env, Git
// repository present). Any other situation returns (nil, reason, nil): the
// caller must treat that as a silent no-op, which is what keeps plain
// no-Git directories working unchanged.
func ActiveConfig(bundleDir string) (*Config, string, error) {
	if envDisabled() {
		return nil, "disabled by " + EnvDisable, nil
	}
	cfg, found, err := LoadConfig(bundleDir)
	if err != nil {
		return nil, "", err
	}
	if !found {
		return nil, "no " + ConfigFileName + " in bundle", nil
	}
	if !cfg.Enabled {
		return nil, "sync disabled in config", nil
	}
	repoRoot, err := DiscoverRepo(bundleDir)
	if err != nil {
		return nil, "", err
	}
	if repoRoot == "" {
		return nil, "not inside a Git repository", nil
	}
	if aid := strings.TrimSpace(os.Getenv(EnvAgentID)); aid != "" {
		cfg.AgentID = aid
	}
	return cfg, "", nil
}

func envDisabled() bool {
	v := strings.TrimSpace(os.Getenv(EnvDisable))
	if v == "" {
		return false
	}
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Git discovery and command plumbing
// ---------------------------------------------------------------------------

// DiscoverRepo walks up from dir looking for a .git entry (directory for
// normal repositories, file for worktrees). Returns "" when there is none.
func DiscoverRepo(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	curr := abs
	for {
		if _, err := os.Stat(filepath.Join(curr, ".git")); err == nil {
			return curr, nil
		}
		parent := filepath.Dir(curr)
		if parent == curr {
			return "", nil
		}
		curr = parent
	}
}

// gitAvailable reports whether a git binary is on PATH.
func gitAvailable() bool {
	_, err := exec.LookPath("git")
	return err == nil
}

// gitRun executes git in repoRoot and returns combined stdout+stderr output.
// GIT_TERMINAL_PROMPT=0 keeps automation from hanging on credential prompts.
func gitRun(repoRoot string, timeout time.Duration, args ...string) (string, error) {
	if !gitAvailable() {
		return "", errors.New("gitsync: git binary not found on PATH")
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_EDITOR=true",
		"GIT_PAGER=cat",
		"LC_ALL=C",
	)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(buf.String())
		if ctx.Err() == context.DeadlineExceeded {
			return msg, fmt.Errorf("gitsync: git %s timed out after %s", strings.Join(args, " "), timeout)
		}
		return msg, fmt.Errorf("gitsync: git %s failed: %s", strings.Join(args, " "), msg)
	}
	return strings.TrimSpace(buf.String()), nil
}

func gitRunQuiet(repoRoot string, timeout time.Duration, args ...string) (string, error) {
	return gitRun(repoRoot, timeout, args...)
}

// toSlashPath converts an OS path to the forward-slash form Git expects in
// pathspecs on every platform.
func toSlashPath(p string) string {
	if runtime.GOOS == "windows" {
		return strings.ReplaceAll(p, `\`, "/")
	}
	return p
}

// relFromRepo computes the repo-relative, slash-separated path of target.
func relFromRepo(repoRoot, target string) (string, error) {
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(repoRoot, absTarget)
	if err != nil {
		return "", err
	}
	return toSlashPath(rel), nil
}

// remoteConfigured reports whether the configured remote exists in the repo.
func remoteConfigured(repoRoot, remote string) bool {
	out, err := gitRunQuiet(repoRoot, 15*time.Second, "remote")
	if err != nil {
		return false
	}
	for _, r := range strings.Fields(out) {
		if r == remote {
			return true
		}
	}
	return false
}

// currentBranch returns the checked-out branch, or "" on a yet-unborn HEAD.
func currentBranch(repoRoot string) string {
	out, err := gitRunQuiet(repoRoot, 10*time.Second, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return ""
	}
	if out == "HEAD" { // detached
		return ""
	}
	return out
}

// headExists reports whether the repository has at least one commit.
func headExists(repoRoot string) bool {
	_, err := gitRunQuiet(repoRoot, 10*time.Second, "rev-parse", "--verify", "HEAD")
	return err == nil
}

// dirtyPaths lists repo-relative paths with uncommitted changes under the
// given pathspecs only.
func dirtyPaths(repoRoot string, pathspecs []string) ([]string, error) {
	args := []string{"status", "--porcelain", "--"}
	args = append(args, pathspecs...)
	out, err := gitRunQuiet(repoRoot, 15*time.Second, args...)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if len(line) < 4 {
			continue
		}
		// Rename entries look like "R  old -> new": keep the new path.
		entry := strings.TrimSpace(line[3:])
		if idx := strings.Index(entry, " -> "); idx >= 0 {
			entry = entry[idx+4:]
		}
		if entry != "" {
			paths = append(paths, entry)
		}
	}
	sort.Strings(paths)
	return paths, nil
}

// validateBundle loads and validates the bundle, applying the configured gate.
func validateBundle(bundleDir string, strict bool) ([]string, bool, error) {
	b, err := okf.LoadBundle(bundleDir)
	if err != nil {
		// A load failure means the bundle is structurally broken.
		return []string{err.Error()}, false, nil
	}
	res := okf.Validate(b, okf.ValidateOptions{Strict: strict, Drift: true})
	var findings []string
	findings = append(findings, res.Errors...)
	if strict {
		findings = append(findings, res.GateFindings...)
		for _, bl := range res.BrokenLinks {
			findings = append(findings, fmt.Sprintf("broken link %s -> %s (%s)", bl.SourceConcept, bl.TargetHref, bl.Reason))
		}
		findings = append(findings, res.Orphans...)
	}
	passed := res.IsConformant && (!strict || res.GatePassed)
	return findings, passed, nil
}

// ---------------------------------------------------------------------------
// Status / Refresh / Publish
// ---------------------------------------------------------------------------

// StatusResult is a full report of the sync state of a bundle.
type StatusResult struct {
	SyncEnabled bool     `json:"sync_enabled"`
	Reason      string   `json:"reason,omitempty"`
	RepoRoot    string   `json:"repo_root,omitempty"`
	BundleRel   string   `json:"bundle_rel,omitempty"`
	Git         bool     `json:"git_available"`
	Remote      string   `json:"remote,omitempty"`
	RemoteSet   bool     `json:"remote_set"`
	Branch      string   `json:"branch,omitempty"`
	OnBranch    string   `json:"on_branch,omitempty"`
	HeadExists  bool     `json:"head_exists"`
	Dirty       []string `json:"dirty"`
	Ahead       int      `json:"ahead"`
	Behind      int      `json:"behind"`
	Config      *Config  `json:"config,omitempty"`
}

// Status inspects a bundle's sync state without changing anything.
func Status(bundleDir string) (*StatusResult, error) {
	res := &StatusResult{Dirty: []string{}}
	res.Git = gitAvailable()
	if !res.Git {
		res.Reason = "git binary not found on PATH"
		return res, nil
	}
	if envDisabled() {
		res.Reason = "disabled by " + EnvDisable
	}
	repoRoot, err := DiscoverRepo(bundleDir)
	if err != nil {
		return nil, err
	}
	if repoRoot == "" {
		if res.Reason == "" {
			res.Reason = "not inside a Git repository"
		}
		return res, nil
	}
	res.RepoRoot = repoRoot
	res.BundleRel, _ = relFromRepo(repoRoot, bundleDir)
	res.HeadExists = headExists(repoRoot)
	res.OnBranch = currentBranch(repoRoot)

	cfg, found, err := LoadConfig(bundleDir)
	if err != nil {
		return nil, err
	}
	if !found {
		if res.Reason == "" {
			res.Reason = "no " + ConfigFileName + " in bundle (sync off, plain local memory)"
		}
		return res, nil
	}
	res.Config = cfg
	if !cfg.Enabled {
		if res.Reason == "" {
			res.Reason = "sync disabled in config"
		}
		return res, nil
	}
	if res.Reason != "" { // env kill switch wins
		return res, nil
	}

	res.SyncEnabled = true
	res.Remote = cfg.Remote
	res.RemoteSet = remoteConfigured(repoRoot, cfg.Remote)
	res.Branch = cfg.Branch

	pathspecs, err := stagePathspecs(repoRoot, bundleDir, cfg)
	if err != nil {
		return nil, err
	}
	dirty, err := dirtyPaths(repoRoot, pathspecs)
	if err != nil {
		return nil, err
	}
	res.Dirty = dirty

	if res.RemoteSet && res.HeadExists && res.OnBranch == cfg.Branch {
		if out, err := gitRunQuiet(repoRoot, 15*time.Second, "rev-list", "--left-right", "--count",
			cfg.Remote+"/"+cfg.Branch+"...HEAD"); err == nil {
			fields := strings.Fields(out)
			if len(fields) == 2 {
				fmt.Sscanf(fields[0], "%d", &res.Behind)
				fmt.Sscanf(fields[1], "%d", &res.Ahead)
			}
		}
	}
	return res, nil
}

// stagePathspecs builds the exact list of paths sync is allowed to stage:
// the bundle itself plus configured extras, sanitized against escapes.
func stagePathspecs(repoRoot, bundleDir string, cfg *Config) ([]string, error) {
	bundleRel, err := relFromRepo(repoRoot, bundleDir)
	if err != nil {
		return nil, err
	}
	specs := []string{bundleRel}
	for _, extra := range cfg.ExtraPaths {
		extra = toSlashPath(strings.TrimSpace(extra))
		if extra == "" {
			continue
		}
		if filepath.IsAbs(extra) || strings.HasPrefix(extra, "..") || strings.HasPrefix(extra, "~") {
			return nil, fmt.Errorf("gitsync: extra_paths entry %q must be a repo-relative path", extra)
		}
		specs = append(specs, extra)
	}
	return specs, nil
}

// RefreshResult reports what a refresh did.
type RefreshResult struct {
	State   string `json:"state"` // refreshed | no_remote | nothing_new | local_only
	Message string `json:"message,omitempty"`
	// Files changed by the refresh on disk, when known.
	Updated int `json:"updated"`
}

// Refresh pulls the configured branch from the configured remote into the
// local checkout, safely: fetch, then fast-forward or rebase --autostash.
// It never merges conflicting content silently.
func Refresh(bundleDir string) (*RefreshResult, error) {
	cfg, reason, err := ActiveConfig(bundleDir)
	if err != nil {
		return nil, err
	}
	if cfg == nil {
		return &RefreshResult{State: "local_only", Message: reason}, nil
	}
	repoRoot, _ := DiscoverRepo(bundleDir)
	return refreshRepo(repoRoot, cfg)
}

func refreshRepo(repoRoot string, cfg *Config) (*RefreshResult, error) {
	if !remoteConfigured(repoRoot, cfg.Remote) {
		return &RefreshResult{State: "no_remote",
			Message: fmt.Sprintf("remote %q not configured; memory stays local until you add it", cfg.Remote)}, nil
	}
	if _, err := gitRunQuiet(repoRoot, 60*time.Second, "fetch", cfg.Remote, cfg.Branch); err != nil {
		return nil, fmt.Errorf("gitsync: fetch from %s/%s failed: %w", cfg.Remote, cfg.Branch, err)
	}
	if !headExists(repoRoot) {
		// Fresh repository: adopt the remote branch as-is.
		if _, err := gitRunQuiet(repoRoot, 15*time.Second, "merge", "--ff-only", "FETCH_HEAD"); err != nil {
			return nil, fmt.Errorf("gitsync: cannot fast-forward to remote branch: %w", err)
		}
		return &RefreshResult{State: "refreshed"}, nil
	}
	before, err := gitRunQuiet(repoRoot, 10*time.Second, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	// Rebase our local commits (if any) on top of the fetched head.
	// --autostash preserves unrelated dirty state; conflicts abort cleanly.
	if _, err := gitRunQuiet(repoRoot, 60*time.Second, "rebase", "--autostash", "FETCH_HEAD"); err != nil {
		if isRebaseConflict(err) {
			_, _ = gitRunQuiet(repoRoot, 30*time.Second, "rebase", "--abort")
			return nil, fmt.Errorf("gitsync: refresh conflict with %s/%s; run 'okf sync refresh' after resolving, or resolve manually with git", cfg.Remote, cfg.Branch)
		}
		return nil, fmt.Errorf("gitsync: rebase onto %s/%s failed: %w", cfg.Remote, cfg.Branch, err)
	}
	after, _ := gitRunQuiet(repoRoot, 10*time.Second, "rev-parse", "HEAD")
	if before == after {
		return &RefreshResult{State: "nothing_new"}, nil
	}
	return &RefreshResult{State: "refreshed"}, nil
}

func isRebaseConflict(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "conflict") ||
		strings.Contains(msg, "could not apply") ||
		strings.Contains(msg, "needs merge")
}

// PublishResult reports the outcome of a publish attempt.
type PublishResult struct {
	State string `json:"state"`
	// pushed | local_committed | local_only | nothing_to_do | conflict |
	// validate_failed | wrong_branch

	Commit    string   `json:"commit,omitempty"`
	Branch    string   `json:"branch,omitempty"`
	Conflicts []string `json:"conflicts,omitempty"`
	// Validation findings, when state == validate_failed.
	Validation []string `json:"validation,omitempty"`
	Rebased    bool     `json:"rebased"`
	Message    string   `json:"message,omitempty"`
}

// Publish validates the bundle, commits bundle changes and pushes them to
// the configured remote. It is the one-shot "make my memory public" call:
// everything automatic sync does happens through here.
//
// Safety contract:
//   - only the bundle pathspec (+ configured extras) is ever staged;
//   - the bundle must pass validation before the commit is created;
//   - a rejected push triggers fetch + rebase with a bounded retry budget;
//   - log.md conflicts are auto-merged structurally; anything else aborts
//     the rebase and is reported for manual resolution;
//   - the published branch is NEVER force-pushed.
func Publish(bundleDir, summary string) (*PublishResult, error) {
	cfg, reason, err := ActiveConfig(bundleDir)
	if err != nil {
		return nil, err
	}
	if cfg == nil {
		return &PublishResult{State: "local_only", Message: reason}, nil
	}
	repoRoot, _ := DiscoverRepo(bundleDir)
	if summary == "" {
		summary = "update knowledge"
	}
	return publishRepo(repoRoot, bundleDir, cfg, summary)
}

func publishRepo(repoRoot, bundleDir string, cfg *Config, summary string) (*PublishResult, error) {
	if branch := currentBranch(repoRoot); branch != "" && branch != cfg.Branch {
		return &PublishResult{
			State:  "wrong_branch",
			Branch: branch,
			Message: fmt.Sprintf("checked out on %q but sync publishes to %q; switch branches or update the branch in %s",
				branch, cfg.Branch, ConfigFileName),
		}, nil
	}

	pathspecs, err := stagePathspecs(repoRoot, bundleDir, cfg)
	if err != nil {
		return nil, err
	}

	justCommitted := false
	dirty, err := dirtyPaths(repoRoot, pathspecs)
	if err != nil {
		return nil, err
	}

	if len(dirty) > 0 {
		// Gate 1: the bundle must validate before anything is committed.
		findings, passed, err := validateBundle(bundleDir, cfg.StrictValidate)
		if err != nil {
			return nil, err
		}
		if !passed {
			return &PublishResult{
				State:      "validate_failed",
				Branch:     cfg.Branch,
				Validation: findings,
				Message:    "bundle does not pass validation; fix the findings before syncing",
			}, nil
		}

		// Stage exactly the allowed paths and commit.
		args := []string{"add", "--"}
		args = append(args, pathspecs...)
		if _, err := gitRunQuiet(repoRoot, 30*time.Second, args...); err != nil {
			return nil, err
		}
		staged, err := stagedFiles(repoRoot, pathspecs)
		if err != nil {
			return nil, err
		}
		if len(staged) > 0 {
			commitMsg := fmt.Sprintf("okf: %s\n\nSynced-by: %s", summary, cfg.AgentID)
			if _, err := gitRunQuiet(repoRoot, 30*time.Second, "commit", "-m", commitMsg); err != nil {
				return nil, err
			}
			justCommitted = true
		}
	}

	commit := ""
	if justCommitted {
		var err error
		commit, err = gitRunQuiet(repoRoot, 10*time.Second, "rev-parse", "--short", "HEAD")
		if err != nil {
			return nil, err
		}
	}

	result := &PublishResult{Commit: commit, Branch: cfg.Branch}

	if !remoteConfigured(repoRoot, cfg.Remote) {
		if justCommitted {
			result.State = "local_committed"
			result.Message = fmt.Sprintf("committed %s locally; no remote %q configured yet", commit, cfg.Remote)
		} else {
			result.State = "nothing_to_do"
		}
		return result, nil
	}

	// A clean tree is not the same as "nothing to publish": local commits
	// may exist unpublished (a previously aborted publish, a conflict
	// resolved by hand, a validation failure that was fixed). Publishing
	// means making local memory public, so unpushed commits go out too.
	if !justCommitted {
		ahead := 0
		if headExists(repoRoot) {
			if _, err := gitRunQuiet(repoRoot, 10*time.Second, "rev-parse", "--verify", "--quiet", cfg.Remote+"/"+cfg.Branch); err == nil {
				if out, err := gitRunQuiet(repoRoot, 10*time.Second, "rev-list", "--count", cfg.Remote+"/"+cfg.Branch+"..HEAD"); err == nil {
					fmt.Sscanf(strings.TrimSpace(out), "%d", &ahead)
				}
			} else {
				// No remote-tracking ref: HEAD holds commits the tracker has
				// never seen. Attempt the push — it either creates the
				// branch, fast-forwards, or is rejected and handled.
				ahead = 1
			}
		}
		if ahead == 0 {
			return &PublishResult{State: "nothing_to_do", Branch: cfg.Branch}, nil
		}
	}

	// Push with optimistic concurrency: on rejection, fetch + rebase + retry.
	for attempt := 1; attempt <= MaxPushRetries; attempt++ {
		_, err := gitRunQuiet(repoRoot, 90*time.Second, "push", cfg.Remote, "HEAD:refs/heads/"+cfg.Branch)
		if err == nil {
			result.State = "pushed"
			return result, nil
		}
		if strings.Contains(err.Error(), "Everything up-to-date") {
			result.State = "pushed"
			return result, nil
		}
		if !isPushRejection(err) {
			return nil, fmt.Errorf("gitsync: push to %s failed: %w", cfg.Remote, err)
		}
		// Someone else published first. Fetch, rebase on top, validate again.
		if _, err := gitRunQuiet(repoRoot, 60*time.Second, "fetch", cfg.Remote, cfg.Branch); err != nil {
			return nil, fmt.Errorf("gitsync: fetch during conflict recovery failed: %w", err)
		}
		if err := rebaseOnto(repoRoot, bundleDir, cfg, result); err != nil {
			return nil, err
		}
		if result.State == "conflict" || result.State == "validate_failed" {
			// Fail closed: the local commit survived (rebased locally or
			// left where it was), but nothing broken gets pushed.
			return result, nil
		}
	}
	return nil, fmt.Errorf("gitsync: push still rejected after %d fetch+rebase attempts; resolve manually with git and retry", MaxPushRetries)
}

func stagedFiles(repoRoot string, pathspecs []string) ([]string, error) {
	args := []string{"diff", "--cached", "--name-only", "--"}
	args = append(args, pathspecs...)
	out, err := gitRunQuiet(repoRoot, 15*time.Second, args...)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			files = append(files, l)
		}
	}
	return files, nil
}

var pushRejectionRe = regexp.MustCompile(`(?i)non-fast-forward|fetch first|rejected|failed to push some refs|stale info`)

func isPushRejection(err error) bool {
	return err != nil && pushRejectionRe.MatchString(err.Error())
}

// rebaseOnto replays the local commit on top of FETCH_HEAD, auto-merging
// log.md conflicts structurally. On any other conflict it aborts the rebase
// and reports the files for manual resolution; the local commit survives on
// the local branch, unpublished.
func rebaseOnto(repoRoot, bundleDir string, cfg *Config, result *PublishResult) error {
	_, err := gitRunQuiet(repoRoot, 60*time.Second, "rebase", "FETCH_HEAD")
	if err == nil {
		// Replay succeeded; re-validate what we are about to publish.
		findings, passed, err := validateBundle(bundleDir, cfg.StrictValidate)
		if err != nil {
			return err
		}
		if !passed {
			result.State = "validate_failed"
			result.Validation = findings
			result.Message = "merged remote changes broke validation; publish aborted, local commits preserved"
			return nil
		}
		commit, _ := gitRunQuiet(repoRoot, 10*time.Second, "rev-parse", "--short", "HEAD")
		result.Commit = commit
		result.Rebased = true
		return nil
	}
	if !isRebaseConflict(err) {
		_, _ = gitRunQuiet(repoRoot, 30*time.Second, "rebase", "--abort")
		return fmt.Errorf("gitsync: rebase onto %s/%s failed: %w", cfg.Remote, cfg.Branch, err)
	}

	conflicts, mergeErr := conflictedPaths(repoRoot)
	if mergeErr != nil {
		_, _ = gitRunQuiet(repoRoot, 30*time.Second, "rebase", "--abort")
		return mergeErr
	}

	bundleRel, _ := relFromRepo(repoRoot, bundleDir)
	logRel := toSlashPath(filepath.Join(bundleRel, "log.md"))

	// Auto-mergeable conflicts, in order of safety:
	//
	//  1. log.md — both sides only ever append entries; a semantic union
	//     (MergeLogContent, same as hub sync) is provably what both wanted.
	//  2. index.md — machine-maintained directory listings; a line-level
	//     union keeps every concept listed. A union that resurrects a line
	//     or keeps a stale one is caught by re-validation before the push.
	//
	// Anything else aborts the rebase and is reported for manual resolution.
	if len(conflicts) > 0 {
		allMergeable := true
		for _, c := range conflicts {
			if c != logRel && !isIndexRel(bundleRel, c) {
				allMergeable = false
				break
			}
		}
		if allMergeable {
			if err := autoMergeRebaseConflicts(repoRoot, bundleRel, logRel, conflicts); err != nil {
				_, _ = gitRunQuiet(repoRoot, 30*time.Second, "rebase", "--abort")
				return err
			}
			findings, passed, err := validateBundle(bundleDir, cfg.StrictValidate)
			if err != nil {
				return err
			}
			if !passed {
				result.State = "validate_failed"
				result.Validation = findings
				result.Message = "merged remote changes broke validation; publish aborted, local commits preserved"
				return nil
			}
			commit, _ := gitRunQuiet(repoRoot, 10*time.Second, "rev-parse", "--short", "HEAD")
			result.Commit = commit
			result.Rebased = true
			return nil
		}
	}

	// Fail closed: undo the rebase, keep the local commit, report the conflict.
	_, _ = gitRunQuiet(repoRoot, 30*time.Second, "rebase", "--abort")
	result.State = "conflict"
	result.Conflicts = conflicts
	result.Message = "remote changed the same concept(s); resolve with git (rebase onto " +
		cfg.Remote + "/" + cfg.Branch + "), validate, then run 'okf sync publish' again"
	return nil
}

// conflictedPaths lists repo-relative paths with unresolved merge markers.
func conflictedPaths(repoRoot string) ([]string, error) {
	out, err := gitRunQuiet(repoRoot, 15*time.Second, "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			paths = append(paths, l)
		}
	}
	return paths, nil
}

// mergeLogConflict reads both sides of a conflicted log.md from the index and
// merges them with the same semantic union used by hub sync.
func mergeLogConflict(repoRoot, logRel string) ([]byte, bool) {
	// During rebase, stage 2 is the side being rebased ONTO (upstream), stage
	// 3 is the commit being replayed (ours). Union order is irrelevant.
	ours, err1 := gitRunQuiet(repoRoot, 15*time.Second, "show", ":3:"+logRel)
	theirs, err2 := gitRunQuiet(repoRoot, 15*time.Second, "show", ":2:"+logRel)
	if err1 != nil || err2 != nil {
		return nil, false
	}
	merged, err := okfsync.MergeLogContent([]byte(ours), []byte(theirs))
	if err != nil {
		return nil, false
	}
	return merged, true
}

// isIndexRel reports whether p is an index.md inside the bundle — either the
// bundle root index or a directory index.
func isIndexRel(bundleRel, p string) bool {
	if !strings.HasSuffix(p, "index.md") {
		return false
	}
	if bundleRel == "." || bundleRel == "" {
		return p == "index.md"
	}
	return p == bundleRel+"/index.md" ||
		(strings.HasPrefix(p, bundleRel+"/") && strings.HasSuffix(p, "/index.md"))
}

// autoMergeRebaseConflicts resolves every conflicted file with the strategy
// for its kind, stages the results and continues the rebase. It returns an
// error suitable for surfacing after the caller aborts the rebase.
func autoMergeRebaseConflicts(repoRoot, bundleRel, logRel string, conflicts []string) error {
	for _, rel := range conflicts {
		var merged []byte
		var ok bool
		switch {
		case rel == logRel:
			merged, ok = mergeLogConflict(repoRoot, rel)
			if !ok {
				return fmt.Errorf("gitsync: failed to merge %s semantically", rel)
			}
		default: // only index.md reaches here (caller verified)
			merged, ok = mergeIndexConflict(repoRoot, rel)
			if !ok {
				return fmt.Errorf("gitsync: failed to merge %s as a line union", rel)
			}
		}
		target := filepath.Join(repoRoot, filepath.FromSlash(rel))
		if err := os.WriteFile(target, merged, 0o644); err != nil {
			return fmt.Errorf("gitsync: failed to write merged %s: %w", rel, err)
		}
		if _, err := gitRunQuiet(repoRoot, 15*time.Second, "add", "--", rel); err != nil {
			return fmt.Errorf("gitsync: failed to stage merged %s: %w", rel, err)
		}
	}
	if _, err := gitRunQuiet(repoRoot, 30*time.Second, "rebase", "--continue"); err != nil {
		return fmt.Errorf("gitsync: rebase continue failed: %w", err)
	}
	return nil
}

// mergeIndexConflict reads both sides of a conflicted index.md and merges
// them as a line-level union: side A in order, then side B's unseen lines,
// blank lines preserved only where they separate content.
func mergeIndexConflict(repoRoot, rel string) ([]byte, bool) {
	// During rebase, stage 3 is the commit being replayed, stage 2 the
	// upstream side. For a listing union the order does not matter.
	a, err1 := gitRunQuiet(repoRoot, 15*time.Second, "show", ":3:"+rel)
	b, err2 := gitRunQuiet(repoRoot, 15*time.Second, "show", ":2:"+rel)
	if err1 != nil || err2 != nil {
		return nil, false
	}
	return unionLines(a, b), true
}

// unionLines merges two texts line by line: every distinct content line of
// both sides survives, in order (A first, then B's additions), with blank
// lines kept only between content lines.
func unionLines(a, b string) []byte {
	seen := make(map[string]struct{})
	var out []string
	prevBlank := true
	emit := func(line string) {
		t := strings.TrimRight(line, "\r")
		trimmed := strings.TrimSpace(t)
		if trimmed == "" {
			if prevBlank {
				return
			}
			prevBlank = true
			out = append(out, "")
			return
		}
		if _, dup := seen[trimmed]; dup {
			return
		}
		seen[trimmed] = struct{}{}
		prevBlank = false
		out = append(out, t)
	}
	for _, l := range strings.Split(a, "\n") {
		emit(l)
	}
	for _, l := range strings.Split(b, "\n") {
		emit(l)
	}
	// Trim a trailing blank introduced by split, keep a final newline.
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	return []byte(strings.Join(out, "\n") + "\n")
}

// ---------------------------------------------------------------------------
// Initialization
// ---------------------------------------------------------------------------

// InitResult reports what `okf sync init` did.
type InitResult struct {
	RepoCreated bool   `json:"repo_created"`
	RepoRoot    string `json:"repo_root"`
	Branch      string `json:"branch"`
	RemoteSet   bool   `json:"remote_set"`
	Message     string `json:"message,omitempty"`
}

// Init enables sync for a bundle: ensures a Git repository exists (creating
// one when the bundle is standalone), writes the config, and reports the
// next steps. It never touches remotes: those stay plain Git configuration.
func Init(bundleDir, remote, branch, agentID string, autoPull, autoPush bool) (*InitResult, error) {
	if !gitAvailable() {
		return nil, errors.New("gitsync: git binary not found on PATH; install Git or keep using purely local memory")
	}
	if remote == "" {
		remote = "origin"
	}

	// Decide where the repository should live when none exists: a bundle
	// named "knowledge/" is almost always a subdirectory of a project, so
	// the project root is the repository; anything else IS the repository.
	repoRoot, err := DiscoverRepo(bundleDir)
	if err != nil {
		return nil, err
	}
	created := false
	if repoRoot == "" {
		initTarget, err := filepath.Abs(bundleDir)
		if err != nil {
			return nil, err
		}
		if filepath.Base(initTarget) == "knowledge" {
			initTarget = filepath.Dir(initTarget)
		}
		if err := os.MkdirAll(initTarget, 0o755); err != nil {
			return nil, err
		}
		if _, err := gitRunQuiet(initTarget, 30*time.Second, "init", "-b", "main"); err != nil {
			return nil, fmt.Errorf("gitsync: git init failed in %s: %w", initTarget, err)
		}
		repoRoot = initTarget
		created = true
	}

	if branch == "" {
		branch = currentBranch(repoRoot)
		if branch == "" {
			branch = "main"
		}
	}
	if agentID == "" {
		hostname, _ := os.Hostname()
		if hostname != "" {
			agentID = "agent/" + hostname
		} else {
			agentID = "agent/local"
		}
	}

	cfg, found, err := LoadConfig(bundleDir)
	if err != nil {
		return nil, err
	}
	if !found || cfg == nil {
		cfg = DefaultConfig()
	}
	cfg.Enabled = true
	cfg.Branch = branch
	cfg.AgentID = agentID
	if autoPull != cfg.AutoPull {
		cfg.AutoPull = autoPull
	}
	if autoPush != cfg.AutoPush {
		cfg.AutoPush = autoPush
	}
	// Keep an explicitly requested remote name even when the remote itself
	// is not configured yet; Git fails loudly and locally otherwise.
	if remote != "" {
		cfg.Remote = remote
	}
	if err := SaveConfig(bundleDir, cfg); err != nil {
		return nil, err
	}

	res := &InitResult{
		RepoCreated: created,
		RepoRoot:    repoRoot,
		Branch:      branch,
		RemoteSet:   remoteConfigured(repoRoot, cfg.Remote),
	}
	if res.RemoteSet {
		res.Message = "sync enabled; changes will validate, commit and push automatically"
	} else {
		res.Message = fmt.Sprintf("sync enabled for local Git; add a remote to publish: git remote add %s <url>", cfg.Remote)
	}
	return res, nil
}

// SetEnabled flips the enabled flag in the bundle's sync config.
func SetEnabled(bundleDir string, enabled bool) error {
	cfg, found, err := LoadConfig(bundleDir)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("gitsync: no %s in %s; run 'okf sync init' first", ConfigFileName, bundleDir)
	}
	cfg.Enabled = enabled
	return SaveConfig(bundleDir, cfg)
}

// AutoPublish is the automation entry point used after every successful
// write (CLI create/update/relate, MCP tools). With sync disabled or
// unavailable it is a silent no-op; with sync enabled it publishes and
// returns the result so callers can surface the state honestly.
func AutoPublish(bundleDir, summary string) (*PublishResult, error) {
	cfg, _, err := LoadConfig(bundleDir)
	if err != nil {
		return nil, err
	}
	if cfg == nil || !cfg.Enabled || !cfg.AutoPush || envDisabled() {
		return nil, nil
	}
	return Publish(bundleDir, summary)
}

// ---------------------------------------------------------------------------
// JSON helpers (stdlib only; the package has zero external dependencies)
// ---------------------------------------------------------------------------

func jsonMarshalIndent(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func jsonUnmarshalStrict(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(v); err != nil {
		return err
	}
	return nil
}
