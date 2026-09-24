// Package jail implements the shared path-containment check used by
// filesystem-touching tool plugins (fs, edit).
//
// Coordination covers fs/edit only (via internal/filelock): bash children
// and external plugins bypass it. Resolve-then-open is check-then-use, not
// an OS security boundary (see ADR-0004) — deployment isolation applies
// where the hands must not reach outside the workspace.
package jail

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Resolve returns the symlink-resolved absolute root.
func Resolve(root string) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil { // root must exist
		return "", fmt.Errorf("jail: root: %w", err)
	}
	return resolved, nil
}

// ResolveRoots resolves a primary root and any additional roots.
func ResolveRoots(root string, extra []string) ([]string, error) {
	roots := make([]string, 0, 1+len(extra))
	for _, value := range append([]string{root}, extra...) {
		resolved, err := Resolve(value)
		if err != nil {
			return nil, err
		}
		roots = append(roots, resolved)
	}
	return roots, nil
}

// ResolvePath makes path absolute relative to root when needed, resolves
// existing symlinks, and verifies containment. The returned path is the
// canonical path the caller should use for the operation; checking one path
// and opening a different spelling would reintroduce symlink surprises.
func ResolvePath(root, path string) (string, error) {
	return ResolvePathWithin([]string{root}, path)
}

// ResolvePathWithin resolves path like ResolvePath, relative to the first
// root, and accepts it when it stays within any of the (already resolved)
// roots. Additional roots let a host grant a directory outside the
// workspace, such as an agent's memory, without widening the workspace.
func ResolvePathWithin(roots []string, path string) (string, error) {
	if len(roots) == 0 {
		return "", fmt.Errorf("jail: no root")
	}
	if path == "" {
		return "", fmt.Errorf("jail: empty path")
	}

	abs := path
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(roots[0], abs)
	}
	abs, err := filepath.Abs(abs)
	if err != nil {
		return "", err
	}

	real, err := resolveExisting(abs)
	if err != nil {
		return "", fmt.Errorf("jail: cannot resolve path: %w", err)
	}

	for _, root := range roots {
		rel, err := filepath.Rel(root, real)
		if err != nil {
			continue
		}
		if rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) && !filepath.IsAbs(rel) {
			return real, nil
		}
	}
	return "", fmt.Errorf("jail: path %q escapes root %q", path, roots[0])
}

// Check verifies that path stays within the (already resolved) root.
// Both sides are resolved through symlinks (e.g. /var → /private/var on
// macOS), and paths may reference files that do not exist yet (writes).
func Check(root, path string) error {
	_, err := ResolvePath(root, path)
	return err
}

// resolveExisting resolves a path through symlinks even when the target or
// intermediate dirs don't exist yet: it resolves the deepest existing
// ancestor and rejoins the unresolved remainder.
func resolveExisting(abs string) (string, error) {
	suffix := ""
	cur := abs
	for {
		resolved, err := filepath.EvalSymlinks(cur)
		if err == nil {
			return filepath.Join(resolved, suffix), nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", err
		}
		suffix = filepath.Join(filepath.Base(cur), suffix)
		cur = parent
	}
}
