// Package jail implements the shared path-containment check used by
// filesystem-touching tool plugins (fs, edit).
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

// Check verifies that path stays within the (already resolved) root.
// Both sides are resolved through symlinks (e.g. /var → /private/var on
// macOS), and paths may reference files that do not exist yet (writes).
func Check(root, path string) error {
	if path == "" {
		return fmt.Errorf("jail: empty path")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	real, err := resolveExisting(abs)
	if err != nil {
		return fmt.Errorf("jail: cannot resolve path: %w", err)
	}
	if !strings.HasPrefix(real, root+string(os.PathSeparator)) && real != root {
		return fmt.Errorf("jail: path %q escapes root %q", path, root)
	}
	return nil
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
