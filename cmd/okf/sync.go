package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/okf-memory/okf-agent-memory/pkg/gitsync"
)

// cmdSync implements the `okf sync` command group: optional Git-backed
// synchronization that is invisible without a config and automatic with one.
//
// Subcommands:
//
//	init [bundle]     enable sync (creates a Git repo when standalone)
//	status [bundle]   report sync state
//	refresh [bundle]  pull remote changes (fetch + ff/rebase)
//	publish [bundle]  validate + commit + push bundle changes
//	enable [bundle]   re-enable sync after a disable
//	disable [bundle]  turn automatic sync off (config stays)
func cmdSync(args []string) {
	if len(args) == 0 {
		printSyncUsage()
		os.Exit(1)
	}
	sub := args[0]
	rest := args[1:]
	if hasHelpFlag(rest) {
		printSyncUsage()
		return
	}
	var err error
	switch sub {
	case "init":
		err = runSyncInit(os.Stdout, rest)
	case "status":
		err = runSyncStatus(os.Stdout, rest)
	case "refresh", "pull":
		err = runSyncRefresh(os.Stdout, rest)
	case "publish", "push":
		err = runSyncPublish(os.Stdout, rest)
	case "enable":
		err = runSyncEnable(os.Stdout, rest, true)
	case "disable":
		err = runSyncEnable(os.Stdout, rest, false)
	default:
		fmt.Fprintf(os.Stderr, "Unknown sync command '%s'\n\n", sub)
		printSyncUsage()
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func printSyncUsage() {
	fmt.Printf(`Optional Git synchronization for an OKF bundle — no backend, just Git.

Sync is opt-in. Without a config file the bundle is plain local memory and
nothing changes: no Git repository is required. With sync enabled, every
write is validated, committed and pushed automatically, and the bundle is
refreshed from the remote at session start.

Usage:
  okf sync <command> [bundle] [flags]

Commands:
  init [bundle]        Enable sync: ensure a Git repo, write %s
  status [bundle]       Show sync state (enabled, branch, dirty files, ahead/behind)
  refresh [bundle]     Pull the configured branch (fetch + fast-forward/rebase)
  publish [bundle]     Validate, commit and push bundle changes now
  enable [bundle]      Re-enable sync after 'disable'
  disable [bundle]     Turn automatic sync off (config file stays)

Init flags:
  --remote <name>      Remote name to publish to (default: origin; add the remote with plain Git)
  --branch <name>      Published branch (default: current branch or main)
  --agent <id>         Commit identity for this machine (default: agent/<hostname>)
  --no-auto-pull       Do not refresh from the remote at session start
  --no-auto-push        Do not commit/push automatically after writes (manual 'okf sync publish' only)

Other flags:
  --message <text>     Commit summary for publish (default: "update knowledge")
  --json               Machine-readable output

Examples:
  okf sync init knowledge
  okf sync init knowledge --agent agent/codex --no-auto-pull
  okf sync status knowledge --json
  okf sync refresh knowledge
  okf sync publish knowledge --message "add auth decision"

Multi-agent safety:
  - Pushes rejected by the remote trigger fetch + rebase with bounded retries.
  - log.md conflicts merge automatically (append-only semantic union).
  - Concept conflicts abort the rebase and are reported for manual resolution.
  - The published branch is never force-pushed.

Environment:
  %s=1        Kill switch: disable all automatic sync behavior
  %s=<id>     Override the commit identity without editing the config
`, gitsync.ConfigFileName, gitsync.EnvDisable, gitsync.EnvAgentID)
}

// afterWriteSync is the CLI automation hook: after every successful write it
// publishes automatically when sync is enabled, and reports honestly what
// happened (never pretending a local save is a published one). When sync is
// not enabled it is a silent no-op — which is exactly what keeps plain,
// Git-less directories working unchanged.
func afterWriteSync(bundleDir, summary string) {
	res, err := gitsync.AutoPublish(bundleDir, summary)
	if res == nil {
		return // sync off: nothing to say, nothing to do
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: sync publish failed: %v (your write is safe locally)\n", err)
		return
	}
	switch res.State {
	case "pushed":
		fmt.Fprintf(os.Stderr, "sync: pushed %s to %s\n", res.Commit, res.Branch)
	case "local_committed":
		fmt.Fprintf(os.Stderr, "sync: %s\n", res.Message)
	case "conflict":
		fmt.Fprintf(os.Stderr, "sync: CONFLICT with remote — %s\n", res.Message)
		for _, c := range res.Conflicts {
			fmt.Fprintf(os.Stderr, "sync:   %s\n", c)
		}
	case "validate_failed":
		fmt.Fprintf(os.Stderr, "sync: validation failed, nothing was pushed:\n")
		for _, v := range res.Validation {
			fmt.Fprintf(os.Stderr, "sync:   %s\n", v)
		}
	case "wrong_branch":
		fmt.Fprintf(os.Stderr, "sync: %s\n", res.Message)
	}
}

func syncParseBundle(rest []string) (string, []string) {
	return defaultBundle(rest)
}

func runSyncInit(w io.Writer, rest []string) error {
	bundleDir, flagArgs := syncParseBundle(rest)
	fs := flag.NewFlagSet("sync init", flag.ExitOnError)
	remote := fs.String("remote", "origin", "Remote name to publish to")
	branch := fs.String("branch", "", "Published branch (default: current or main)")
	agent := fs.String("agent", "", "Commit identity for this machine")
	noAutoPull := fs.Bool("no-auto-pull", false, "Disable automatic refresh at session start")
	noAutoPush := fs.Bool("no-auto-push", false, "Disable automatic publish after writes")
	_ = fs.Parse(flagArgs)

	// Preserve previously chosen automation flags when not explicitly changed.
	autoPull, autoPush := true, true
	if cfg, found, _ := gitsync.LoadConfig(bundleDir); found && cfg != nil {
		autoPull, autoPush = cfg.AutoPull, cfg.AutoPush
	}
	if *noAutoPull {
		autoPull = false
	}
	if *noAutoPush {
		autoPush = false
	}

	res, err := gitsync.Init(bundleDir, *remote, *branch, *agent, autoPull, autoPush)
	if err != nil {
		return err
	}
	data, _ := json.MarshalIndent(res, "", "  ")
	fmt.Fprintf(w, "Sync enabled for '%s'.\n%s\n", bundleDir, string(data))
	return nil
}

func runSyncStatus(w io.Writer, rest []string) error {
	bundleDir, flagArgs := syncParseBundle(rest)
	fs := flag.NewFlagSet("sync status", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "Machine-readable output")
	_ = fs.Parse(flagArgs)

	res, err := gitsync.Status(bundleDir)
	if err != nil {
		return err
	}
	if *jsonOut {
		data, _ := json.MarshalIndent(res, "", "  ")
		fmt.Fprintln(w, string(data))
		return nil
	}

	fmt.Fprintf(w, "Sync status for '%s':\n", bundleDir)
	if res.SyncEnabled {
		fmt.Fprintf(w, "  enabled:     true (branch %s, remote %s)\n", res.Branch, res.Remote)
	} else {
		fmt.Fprintf(w, "  enabled:     false (%s)\n", res.Reason)
	}
	if res.RepoRoot != "" {
		fmt.Fprintf(w, "  repo:        %s\n", res.RepoRoot)
		fmt.Fprintf(w, "  bundle rel:  %s\n", res.BundleRel)
	}
	if res.OnBranch != "" {
		fmt.Fprintf(w, "  on branch:   %s\n", res.OnBranch)
	}
	if res.Remote != "" {
		fmt.Fprintf(w, "  remote set:  %t\n", res.RemoteSet)
	}
	if len(res.Dirty) > 0 {
		fmt.Fprintf(w, "  dirty files: %d\n", len(res.Dirty))
		for _, d := range res.Dirty {
			fmt.Fprintf(w, "    - %s\n", d)
		}
	} else {
		fmt.Fprintf(w, "  dirty files: 0\n")
	}
	if res.SyncEnabled && res.RemoteSet {
		fmt.Fprintf(w, "  ahead/behind: %d / %d\n", res.Ahead, res.Behind)
	}
	return nil
}

func runSyncRefresh(w io.Writer, rest []string) error {
	bundleDir, flagArgs := syncParseBundle(rest)
	fs := flag.NewFlagSet("sync refresh", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "Machine-readable output")
	_ = fs.Parse(flagArgs)

	res, err := gitsync.Refresh(bundleDir)
	if err != nil {
		return err
	}
	if *jsonOut {
		data, _ := json.MarshalIndent(res, "", "  ")
		fmt.Fprintln(w, string(data))
		return nil
	}
	switch res.State {
	case "refreshed":
		fmt.Fprintf(w, "Refreshed '%s' from remote.\n", bundleDir)
	case "nothing_new":
		fmt.Fprintf(w, "Remote has nothing new for '%s'.\n", bundleDir)
	case "no_remote":
		fmt.Fprintf(w, "No remote configured: '%s' stays local. %s\n", bundleDir, res.Message)
	default:
		fmt.Fprintf(w, "Refresh skipped: %s\n", res.Message)
	}
	return nil
}

func runSyncPublish(w io.Writer, rest []string) error {
	bundleDir, flagArgs := syncParseBundle(rest)
	fs := flag.NewFlagSet("sync publish", flag.ExitOnError)
	message := fs.String("message", "update knowledge", "Commit summary")
	jsonOut := fs.Bool("json", false, "Machine-readable output")
	_ = fs.Parse(flagArgs)

	res, err := gitsync.Publish(bundleDir, *message)
	if err != nil {
		return err
	}
	if *jsonOut {
		data, _ := json.MarshalIndent(res, "", "  ")
		fmt.Fprintln(w, string(data))
	} else {
		writePublishHuman(w, res)
	}
	if res.State == "conflict" || res.State == "validate_failed" || res.State == "wrong_branch" {
		return fmt.Errorf("publish ended in state %q", res.State)
	}
	return nil
}

func writePublishHuman(w io.Writer, res *gitsync.PublishResult) {
	switch res.State {
	case "pushed":
		fmt.Fprintf(w, "Pushed %s to %s.\n", res.Commit, res.Branch)
	case "local_committed":
		fmt.Fprintf(w, "Committed %s locally (%s).\n", res.Commit, res.Message)
	case "local_only":
		fmt.Fprintf(w, "Sync not active: %s\n", res.Message)
	case "nothing_to_do":
		fmt.Fprintf(w, "Nothing to publish: no bundle changes.\n")
	case "conflict":
		fmt.Fprintf(w, "CONFLICT — remote changed the same file(s):\n")
		for _, c := range res.Conflicts {
			fmt.Fprintf(w, "  - %s\n", c)
		}
		fmt.Fprintf(w, "%s\n", res.Message)
	case "validate_failed":
		fmt.Fprintf(w, "Validation failed; nothing was committed or pushed:\n")
		for _, v := range res.Validation {
			fmt.Fprintf(w, "  - %s\n", v)
		}
	case "wrong_branch":
		fmt.Fprintf(w, "%s\n", res.Message)
	default:
		fmt.Fprintf(w, "Publish state: %s\n%s\n", res.State, res.Message)
	}
}

func runSyncEnable(w io.Writer, rest []string, enable bool) error {
	bundleDir, flagArgs := syncParseBundle(rest)
	_ = flagArgs
	if err := gitsync.SetEnabled(bundleDir, enable); err != nil {
		return err
	}
	if enable {
		fmt.Fprintf(w, "Sync enabled for '%s'.\n", bundleDir)
	} else {
		fmt.Fprintf(w, "Sync disabled for '%s'. Writes stay local until you publish manually.\n", bundleDir)
	}
	return nil
}
