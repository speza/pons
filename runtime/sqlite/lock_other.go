//go:build !unix

package sqlite

import (
	"errors"
	"os"
)

func lockStateFile(*os.File) error {
	return errors.New("exclusive runtime state locking is unavailable on this platform")
}
