// Package dirsync snapshots a host directory for a remote copy and applies
// the copy's changes back. Only regular files are synced. Applying writes
// just the files a run added, changed, or deleted relative to the snapshot it
// started from, so concurrent runs only collide on the same file. Moving the
// copy in and out, and validating what comes back, belongs to the caller.
package dirsync

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"slices"
	"strings"
	"time"
)

// Tree maps slash-separated relative paths to regular file contents.
type Tree map[string][]byte

// Digests maps each path of a snapshot to its content's SHA-256. It is what
// Apply compares against, so callers need not keep the snapshot itself.
type Digests map[string][sha256.Size]byte

// Digests summarizes the tree for a later Apply.
func (t Tree) Digests() Digests {
	digests := make(Digests, len(t))
	for name, data := range t {
		digests[name] = sha256.Sum256(data)
	}
	return digests
}

// Read snapshots dir's regular files. Symlinks and special files are
// skipped: they are never synced, so a remote copy cannot smuggle one back.
func Read(dir string) (Tree, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("dirsync: %w", err)
	}
	defer root.Close()

	tree := Tree{}
	err = fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		data, err := root.ReadFile(name)
		if err != nil {
			return err
		}
		tree[name] = data
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("dirsync: read %s: %w", dir, err)
	}
	return tree, nil
}

// Archive returns a tar archive of exactly the snapshot, so what a remote
// copy starts from always matches the base its changes are compared with.
func Archive(tree Tree) ([]byte, error) {
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	for _, name := range slices.Sorted(maps.Keys(tree)) {
		data := tree[name]
		if err := writer.WriteHeader(&tar.Header{
			Typeflag: tar.TypeReg, Name: name, Mode: 0o600, Size: int64(len(data)),
			ModTime: time.Unix(0, 0), Format: tar.FormatPAX,
		}); err != nil {
			return nil, fmt.Errorf("dirsync: archive %s: %w", name, err)
		}
		if _, err := writer.Write(data); err != nil {
			return nil, fmt.Errorf("dirsync: archive %s: %w", name, err)
		}
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("dirsync: archive: %w", err)
	}
	return buffer.Bytes(), nil
}

// Apply writes result's changes relative to base into dir: files added or
// changed are written and files removed are deleted. Files the run did not
// touch are left as they are now, even if they changed since base. It returns
// the changed paths.
func Apply(dir string, base Digests, result Tree) ([]string, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("dirsync: %w", err)
	}
	defer root.Close()

	var changed []string
	for _, name := range slices.Sorted(maps.Keys(result)) {
		data := result[name]
		if previous, ok := base[name]; ok && previous == sha256.Sum256(data) {
			continue
		}
		if !safeName(name) {
			return changed, fmt.Errorf("dirsync: unsafe path %q", name)
		}
		if err := writeFile(root, name, data); err != nil {
			return changed, err
		}
		changed = append(changed, name)
	}
	for _, name := range slices.Sorted(maps.Keys(base)) {
		if _, ok := result[name]; ok || !safeName(name) {
			continue
		}
		if err := root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return changed, fmt.Errorf("dirsync: remove %s: %w", name, err)
		}
		changed = append(changed, name)
	}
	return changed, nil
}

// writeFile replaces one file through the root, so neither the path nor a
// symlink inside dir can place it outside dir.
func writeFile(root *os.Root, name string, data []byte) error {
	if parent := path.Dir(name); parent != "." {
		if err := root.MkdirAll(parent, 0o700); err != nil {
			return fmt.Errorf("dirsync: create %s: %w", parent, err)
		}
	}
	temporary := path.Join(path.Dir(name), ".dirsync-"+path.Base(name))
	if err := root.WriteFile(temporary, data, 0o600); err != nil {
		return fmt.Errorf("dirsync: write %s: %w", name, err)
	}
	if err := root.Rename(temporary, name); err != nil {
		_ = root.Remove(temporary)
		return fmt.Errorf("dirsync: install %s: %w", name, err)
	}
	return nil
}

func safeName(name string) bool {
	return name != "" && name != "." && !path.IsAbs(name) && path.Clean(name) == name &&
		name != ".." && !strings.HasPrefix(name, "../") && !strings.ContainsRune(name, 0)
}
