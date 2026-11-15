package sync

import (
	"path/filepath"
	"testing"
)

func TestValidateTreePath(t *testing.T) {
	ok := []string{"index.md", "notes/a.md", "a/b/c/d.md", "weird..name.md", "spaced name/ü.md", "log.md", "x.conflict-local.md"}
	for _, p := range ok {
		if err := validateTreePath(p); err != nil {
			t.Errorf("%q refused: %v", p, err)
		}
	}
	bad := []string{"", "..", "../x", "a/../../x", "a/..", "/etc/passwd", "./a", "a//b", "a/./b", "a/", ".okf-vault.json", ".okf/sync-state.json", ".git/hooks/pre-commit", "notes/.hidden", "a\x00b"}
	if filepath.Separator == '\\' {
		bad = append(bad, `..\x`, `C:\x`, `a\b`)
	}
	for _, p := range bad {
		if err := validateTreePath(p); err == nil {
			t.Errorf("%q accepted", p)
		}
	}
}
