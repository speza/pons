// Package dirsync copies a small host directory into a remote environment and
// applies the remote copy's changes back. Only regular files are synced.
// Applying writes just the files a run added, changed, or deleted relative to
// the snapshot it started from, so concurrent runs only collide on the same
// file.
package dirsync

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"slices"
	"strings"
	"time"
)

const (
	// MaxFiles and MaxBytes bound a synced directory.
	MaxFiles = 2000
	MaxBytes = 16 << 20
	// MaxArchiveBytes bounds an archive of a directory within the limits.
	MaxArchiveBytes = MaxBytes + MaxFiles*2048 + 4096
)

// Tree maps slash-separated relative paths to regular file contents.
type Tree map[string][]byte

// Read snapshots dir's regular files. Symlinks and special files are
// skipped: they are never synced, so a remote copy cannot smuggle one back.
func Read(dir string) (Tree, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("dirsync: %w", err)
	}
	defer root.Close()

	tree, total := Tree{}, 0
	err = fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		if len(tree) == MaxFiles {
			return fmt.Errorf("more than %d files", MaxFiles)
		}
		data, err := root.ReadFile(name)
		if err != nil {
			return err
		}
		if total += len(data); total > MaxBytes {
			return fmt.Errorf("more than %d bytes", MaxBytes)
		}
		tree[name] = data
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("dirsync: read %s: %w", dir, err)
	}
	return tree, nil
}

// Archive returns a tar archive of the tree.
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

// Unarchive reads a tar archive produced by a remote environment. Regular
// files are kept, directories are implied, and any other entry, unsafe path,
// or overrun of the limits rejects the whole archive.
func Unarchive(archive io.Reader) (Tree, error) {
	reader := tar.NewReader(archive)
	tree, total := Tree{}, 0
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return tree, nil
		}
		if err != nil {
			return nil, fmt.Errorf("dirsync: unarchive: %w", err)
		}
		name := strings.TrimPrefix(header.Name, "./")
		switch header.Typeflag {
		case tar.TypeDir:
			continue
		case tar.TypeReg:
		default:
			return nil, fmt.Errorf("dirsync: unsupported entry %q", header.Name)
		}
		if !safeName(name) {
			return nil, fmt.Errorf("dirsync: unsafe path %q", header.Name)
		}
		if len(tree) == MaxFiles || header.Size < 0 || int64(total)+header.Size > MaxBytes {
			return nil, errors.New("dirsync: archive exceeds its limits")
		}
		data, err := io.ReadAll(io.LimitReader(reader, header.Size))
		if err != nil {
			return nil, fmt.Errorf("dirsync: unarchive %s: %w", name, err)
		}
		total += len(data)
		tree[name] = data
	}
}

// Apply writes result's changes relative to base into dir: files added or
// changed are written and files removed are deleted. Files the run did not
// touch are left as they are now, even if they changed since base. It returns
// the changed paths.
func Apply(dir string, base, result Tree) ([]string, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("dirsync: %w", err)
	}
	defer root.Close()

	var changed []string
	for _, name := range slices.Sorted(maps.Keys(result)) {
		data := result[name]
		if previous, ok := base[name]; ok && bytes.Equal(previous, data) {
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
