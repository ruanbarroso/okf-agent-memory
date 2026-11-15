package sync

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/okf-memory/okf-agent-memory/pkg/vault"
)

// Tree manifests arrive from the hub, authored by whichever vault peer holds
// the key. Their entry paths are untrusted input: before Pull or Sync writes
// or deletes anything, every path must be one that Engine.ScanBundle could have
// produced, and must resolve (through symlinks) to a location inside the
// bundle root.

// validateTreePath rejects tree paths that no bundle scan can produce: empty,
// absolute, non-canonical ("", ".", ".." segments), hidden segments (ScanBundle
// never syncs dot files or dot directories, so a hub path such as
// ".okf-vault.json" or ".git/hooks/x" can only be an injection), NUL bytes, and
// backslashes where they are a path separator.
func validateTreePath(p string) error {
	switch {
	case p == "":
		return fmt.Errorf("sync: refusing hub path %q: empty path", p)
	case strings.ContainsRune(p, 0):
		return fmt.Errorf("sync: refusing hub path %q: contains NUL byte", p)
	case filepath.Separator == '\\' && strings.ContainsRune(p, '\\'):
		return fmt.Errorf("sync: refusing hub path %q: contains backslash separator", p)
	case strings.HasPrefix(p, "/") || filepath.IsAbs(filepath.FromSlash(p)) || filepath.VolumeName(filepath.FromSlash(p)) != "":
		return fmt.Errorf("sync: refusing hub path %q: absolute path", p)
	}
	for _, seg := range strings.Split(p, "/") {
		switch {
		case seg == "..":
			return fmt.Errorf("sync: refusing hub path %q: parent directory traversal", p)
		case seg == "" || seg == ".":
			return fmt.Errorf("sync: refusing hub path %q: non-canonical path", p)
		case strings.HasPrefix(seg, "."):
			return fmt.Errorf("sync: refusing hub path %q: hidden path is never synced", p)
		}
	}
	return nil
}

// validateTreePaths checks every entry of a tree manifest (in sorted order, so
// the reported path is deterministic).
func validateTreePaths(t *vault.Tree) error {
	if t == nil {
		return nil
	}
	paths := make([]string, 0, len(t.Entries))
	for p := range t.Entries {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		if err := validateTreePath(p); err != nil {
			return err
		}
	}
	return nil
}

// hubTargets resolves the filesystem targets for tree paths that are about to
// be written and deleted, before anything is touched, so a single escaping entry
// refuses the whole operation instead of leaving it half-applied. Writes follow
// a final symlink (WriteFile would); deletes do not (Remove unlinks the link).
func (e *Engine) hubTargets(writes, deletes []string) (map[string]string, map[string]string, error) {
	resolve := func(paths []string, followLeaf bool) (map[string]string, error) {
		out := make(map[string]string, len(paths))
		for _, p := range paths {
			if err := validateTreePath(p); err != nil {
				return nil, err
			}
			target, err := e.containedPath("hub", filepath.Join(e.BundlePath, filepath.FromSlash(p)), followLeaf)
			if err != nil {
				return nil, err
			}
			out[p] = target
		}
		return out, nil
	}
	w, err := resolve(writes, true)
	if err != nil {
		return nil, nil, err
	}
	d, err := resolve(deletes, false)
	if err != nil {
		return nil, nil, err
	}
	return w, d, nil
}
