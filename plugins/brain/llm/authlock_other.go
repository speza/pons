//go:build !unix

package llm

import "os"

func lockFileExclusive(f *os.File) func() { return func() {} }
