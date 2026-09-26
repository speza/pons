//go:build unix

package dirsync

import (
	"os"
	"path"

	"golang.org/x/sys/unix"
)

// Open the parent through the root, then atomically require a directory at
// the leaf. Unlike os.Root.Remove, AT_REMOVEDIR cannot unlink a file or link
// that another run installed after the cleanup walk inspected this path.
func removeDirectory(root *os.Root, name string) error {
	parent, err := root.Open(path.Dir(name))
	if err != nil {
		return err
	}
	defer parent.Close()
	if err := unix.Unlinkat(int(parent.Fd()), path.Base(name), unix.AT_REMOVEDIR); err != nil {
		return &os.PathError{Op: "removedir", Path: name, Err: err}
	}
	return nil
}
