//go:build unix

package sqlite

import (
	"strings"
	"testing"
)

func TestStateDirectoryHasOneOwner(t *testing.T) {
	dir := t.TempDir()
	first, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := Open(dir); err == nil {
		_ = second.Close()
		t.Fatal("second runtime opened an active state directory")
	} else if !strings.Contains(err.Error(), "already in use") {
		t.Fatalf("second open: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := Open(dir)
	if err != nil {
		t.Fatalf("state directory remained locked after close: %v", err)
	}
	defer second.Close()
}
