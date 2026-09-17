// Package filelock serializes read-modify-write style file operations
// across tool plugins within one process. Keys are canonical jail-resolved
// paths, so the fs and edit plugins coordinate on the same underlying file
// even when the action spelled the path differently.
//
// This is in-process coordination only: it cannot see another process (e.g.
// a bash child) touching the same path.
package filelock

import "sync"

var (
	mu    sync.Mutex
	locks = map[string]*sync.Mutex{}
)

// Lock acquires the per-path lock and returns its release function.
func Lock(path string) func() {
	mu.Lock()
	l := locks[path]
	if l == nil {
		l = &sync.Mutex{}
		locks[path] = l
	}
	mu.Unlock()
	l.Lock()
	return l.Unlock
}
