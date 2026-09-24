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

// ResolvePath makes path absolute relative to root when needed, resolves
// existing symlinks, and verifies containment. The returned path is the
// canonical path the caller should use for the operation; checking one path
// and opening a different spelling would reintroduce symlink surprises.
func ResolvePath(root, path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("jail: empty path")
	}

	abs := path
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(root, abs)
	}
	abs, err := filepath.Abs(abs)
	if err != nil {
		return "", err
	}

	real, err := resolveExisting(abs)
	if err != nil {
		return "", fmt.Errorf("jail: cannot resolve path: %w", err)
	}

	rel, err := filepath.Rel(root, real)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("jail: path %q escapes root %q", path, root)
	}
	return real, nil
}

// ResolvePathUnconfined resolves path like ResolvePath but accepts an
// absolute path anywhere, for tools inside an execution environment that
// alone decides what is reachable. Relative paths still stay under root.
func ResolvePathUnconfined(root, path string) (string, error) {
	if !filepath.IsAbs(path) {
		return ResolvePath(root, path)
	}
	real, err := resolveExisting(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("jail: cannot resolve path: %w", err)
	}
	return real, nil
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
