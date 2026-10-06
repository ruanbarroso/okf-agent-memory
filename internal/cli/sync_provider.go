package cli

import (
	"io"
	"time"

	"github.com/okf-memory/okf-agent-memory/pkg/gitsync"
	"github.com/okf-memory/okf-agent-memory/pkg/okf"
)

// gitSyncProvider adapts pkg/gitsync to okf.SyncProvider so the MCP server
// (in pkg/okf) can drive Git sync without importing pkg/gitsync, which
// itself depends on pkg/okf.
type gitSyncProvider struct{}

func (gitSyncProvider) Status(bundleDir string) (any, error) {
	return gitsync.Status(bundleDir)
}

func (gitSyncProvider) Refresh(bundleDir string) (string, any, error) {
	res, err := gitsync.Refresh(bundleDir)
	if err != nil || res == nil {
		return "", res, err
	}
	return res.State, res, nil
}

func (gitSyncProvider) Publish(bundleDir, summary string) (string, any, error) {
	res, err := gitsync.Publish(bundleDir, summary)
	if err != nil || res == nil {
		return "", res, err
	}
	return res.State, res, nil
}

func (gitSyncProvider) Auto(bundleDir string) (bool, bool, bool, time.Duration, error) {
	cfg, _, err := gitsync.ActiveConfig(bundleDir)
	if err != nil || cfg == nil {
		return false, false, false, 0, err
	}
	return true, cfg.AutoPull, cfg.AutoPush, time.Duration(cfg.DebounceMS) * time.Millisecond, nil
}

func (gitSyncProvider) LogPublish(w io.Writer, result any) {
	res, ok := result.(*gitsync.PublishResult)
	if !ok || res == nil {
		return
	}
	logSyncResult(w, res, "okf sync")
}

func init() {
	okf.SetSyncProvider(gitSyncProvider{})
}
