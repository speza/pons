//go:build !unix

package advisorylock

import "os"

// Exclusive is a no-op where advisory file locks are unavailable.
func Exclusive(*os.File) func() { return func() {} }
