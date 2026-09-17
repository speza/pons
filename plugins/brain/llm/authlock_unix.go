//go:build unix

package llm

import (
	"os"
	"syscall"
)

// lockFileExclusive takes an advisory exclusive lock on f, returning the
// release function. Locking is best-effort: on platforms or filesystems
// without flock support the caller proceeds unlocked.
func lockFileExclusive(f *os.File) func() {
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return func() {}
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }
}
