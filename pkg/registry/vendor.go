package registry

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// A bundle id names the directory under .okf/vendor that `okf pull` and
// `okf restore` write into, and that rollback and `okf vendor remove` delete
// recursively. Ids arrive from a registry manifest or from a repository's
// okf.lock, so they are untrusted input: before any of those operations touch
// the filesystem, the id must be a canonical relative path and its directory
// must resolve (through symlinks) to a location inside the vendor root.

// ValidateBundleID accepts any canonical relative slash-separated path, such as
// "nextjs-15", "peter/django-5-rules", "@peter/django-5-rules" or deeper nested
// ids, and rejects only what could leave the vendor root: empty ids, absolute
// or volume-qualified paths, "", "." or ".." segments, backslashes and NUL bytes.
func ValidateBundleID(id string) error {
	switch {
	case id == "":
		return fmt.Errorf("unsafe bundle id %q: empty id", id)
	case strings.ContainsRune(id, 0):
		return fmt.Errorf("unsafe bundle id %q: contains NUL byte", id)
	case strings.ContainsRune(id, '\\'):
		return fmt.Errorf("unsafe bundle id %q: contains backslash", id)
	case strings.HasPrefix(id, "/") || filepath.IsAbs(filepath.FromSlash(id)) || filepath.VolumeName(filepath.FromSlash(id)) != "":
		return fmt.Errorf("unsafe bundle id %q: absolute path", id)
	}
	for _, seg := range strings.Split(id, "/") {
		switch seg {
		case "..":
			return fmt.Errorf("unsafe bundle id %q: parent directory traversal", id)
		case "", ".":
			return fmt.Errorf("unsafe bundle id %q: non-canonical path", id)
		}
	}
	return nil
}

// VendorRoot returns the vendor directory of a workspace.
func VendorRoot(workspace string) string {
	return filepath.Join(workspace, ".okf", "vendor")
}

// ResolveVendorDir validates id and returns the symlink-resolved directory it
// installs into. The vendor root must resolve inside the workspace, and the
// bundle directory strictly inside the vendor root. Neither has to exist yet.
func ResolveVendorDir(workspace, id string) (string, error) {
	if err := ValidateBundleID(id); err != nil {
		return "", err
	}
	root, err := resolvedVendorRoot(workspace)
	if err != nil {
		return "", err
	}
	return containedPath(root, filepath.Join(VendorRoot(workspace), filepath.FromSlash(id)), true, fmt.Sprintf("bundle id %q", id))
}

// RemoveVendorDir removes an installed (or partially installed) bundle and,
// for nested ids, its parent directory if that is left empty. Both paths are
// re-resolved immediately before removal and must lie strictly inside the
// vendor root, so a cleanup can never delete outside it. The bundle directory
// itself is not followed if it is a symlink: the link is removed, as
// os.RemoveAll would.
func RemoveVendorDir(workspace, id string) error {
	if err := ValidateBundleID(id); err != nil {
		return err
	}
	root, err := resolvedVendorRoot(workspace)
	if err != nil {
		return err
	}
	kind := fmt.Sprintf("bundle id %q", id)
	dir, err := containedPath(root, filepath.Join(VendorRoot(workspace), filepath.FromSlash(id)), false, kind)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if !strings.Contains(id, "/") {
		return nil
	}
	// Best effort: the parent is only removed if it is empty and strictly
	// inside the vendor root (never the vendor root itself).
	if parent, err := containedPath(root, filepath.Dir(dir), false, kind); err == nil {
		if entries, err := os.ReadDir(parent); err == nil && len(entries) == 0 {
			_ = os.Remove(parent)
		}
	}
	return nil
}

// Staging is a fresh directory that one install extracts and validates a
// bundle in before it is moved into .okf/vendor. A failed install discards
// only this directory, so a rollback never deletes anything this invocation
// did not create (in particular not a previous installation or a directory a
// hostile id or symlink points at).
type Staging struct {
	Dir  string
	info os.FileInfo
}

// NewStaging creates a staging directory for workspace. It lives in the
// resolved .okf directory (outside the vendor root, so vendor discovery never
// sees a half-installed bundle), or in the vendor root itself if .okf does not
// resolve inside the workspace.
func NewStaging(workspace string) (*Staging, error) {
	if err := os.MkdirAll(VendorRoot(workspace), 0o755); err != nil {
		return nil, fmt.Errorf("cannot create vendor root: %w", err)
	}
	root, err := resolvedVendorRoot(workspace)
	if err != nil {
		return nil, err
	}
	parent := root
	ws, err := resolvePath(workspace, true)
	if err != nil {
		return nil, err
	}
	if okfDir, err := resolvePath(filepath.Join(workspace, ".okf"), true); err == nil && within(ws, okfDir) && okfDir != ws {
		parent = okfDir
	}
	dir, err := os.MkdirTemp(parent, ".vendor-staging-")
	if err != nil {
		return nil, fmt.Errorf("cannot create staging dir: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	return &Staging{Dir: dir, info: info}, nil
}

// Discard removes the staging directory, and only if it is still the
// directory NewStaging created (never a symlink swapped in for it).
func (s *Staging) Discard() error {
	if err := s.verify(); err != nil {
		return err
	}
	return os.RemoveAll(s.Dir)
}

// Commit moves the staged bundle into place as the vendor directory of id.
// The destination is re-resolved and must lie strictly inside the vendor
// root. An existing installation (--force) is moved aside and removed only
// after the new one is in place; if it is a symlink, the link itself is
// replaced, never followed.
func (s *Staging) Commit(workspace, id string) error {
	if err := ValidateBundleID(id); err != nil {
		return err
	}
	if err := s.verify(); err != nil {
		return err
	}
	root, err := resolvedVendorRoot(workspace)
	if err != nil {
		return err
	}
	kind := fmt.Sprintf("bundle id %q", id)
	dest, err := containedPath(root, filepath.Join(VendorRoot(workspace), filepath.FromSlash(id)), false, kind)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("cannot create vendor dir: %w", err)
	}
	if _, err := containedPath(root, dest, false, kind); err != nil {
		return err
	}
	if _, err := os.Lstat(dest); os.IsNotExist(err) {
		return os.Rename(s.Dir, dest)
	}
	aside, err := os.MkdirTemp(filepath.Dir(s.Dir), ".vendor-replaced-")
	if err != nil {
		return fmt.Errorf("cannot create staging dir: %w", err)
	}
	old := filepath.Join(aside, "previous")
	if err := os.Rename(dest, old); err != nil {
		_ = os.Remove(aside)
		return fmt.Errorf("cannot replace existing installation: %w", err)
	}
	if err := os.Rename(s.Dir, dest); err != nil {
		_ = os.Rename(old, dest)
		_ = os.Remove(aside)
		return fmt.Errorf("cannot move staged bundle into place: %w", err)
	}
	return os.RemoveAll(aside)
}

func (s *Staging) verify() error {
	info, err := os.Lstat(s.Dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || !os.SameFile(info, s.info) {
		return fmt.Errorf("refusing staging dir %q: no longer the directory this install created", s.Dir)
	}
	return nil
}

// resolvedVendorRoot resolves the workspace and its .okf/vendor root through
// symlinks and refuses a vendor root that lies outside the workspace (for
// example a repository that ships `.okf -> /elsewhere`).
func resolvedVendorRoot(workspace string) (string, error) {
	ws, err := resolvePath(workspace, true)
	if err != nil {
		return "", fmt.Errorf("failed to resolve workspace %q: %w", workspace, err)
	}
	root, err := resolvePath(VendorRoot(workspace), true)
	if err != nil {
		return "", fmt.Errorf("failed to resolve vendor root: %w", err)
	}
	if !within(ws, root) {
		return "", fmt.Errorf("refusing vendor root %q: resolves to %q outside workspace %q", VendorRoot(workspace), root, ws)
	}
	return root, nil
}

// containedPath resolves target (see resolvePath) and returns it only if it
// lies strictly inside the already-resolved root.
func containedPath(root, target string, followLeaf bool, kind string) (string, error) {
	resolved, err := resolvePath(target, followLeaf)
	if err != nil {
		return "", fmt.Errorf("failed to resolve %s path %q: %w", kind, target, err)
	}
	if !within(root, resolved) || resolved == root {
		return "", fmt.Errorf("refusing %s: %q resolves to %q outside %q", kind, target, resolved, root)
	}
	return resolved, nil
}

// resolvePath returns the absolute path of target with symlinks resolved in its
// closest existing ancestor, so paths not yet created resolve too. With
// followLeaf false the final element is not resolved, so a symlink at target
// is treated as the link itself.
func resolvePath(target string, followLeaf bool) (string, error) {
	abs, err := filepath.Abs(target)
	if err != nil {
		return "", err
	}
	existing, missing, leaf := abs, "", ""
	if !followLeaf {
		existing, leaf = filepath.Dir(abs), filepath.Base(abs)
	}
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			break
		}
		missing = filepath.Join(filepath.Base(existing), missing)
		existing = parent
	}
	resolved, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolved, missing, leaf), nil
}

// within reports whether path equals root or lies below it (filepath.Rel, not a
// string prefix).
func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
