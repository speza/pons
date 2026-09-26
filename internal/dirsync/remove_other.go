//go:build !unix

package dirsync

import (
	"errors"
	"os"
)

// Other platforms must provide directory-only removal before enabling this
// replacement. Generic os.Root.Remove can delete a concurrent file or link.
func removeDirectory(_ *os.Root, _ string) error {
	return errors.New("dirsync: directory replacement is unsupported on this platform")
}
