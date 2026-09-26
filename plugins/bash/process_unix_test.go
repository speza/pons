//go:build unix

package bash

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSuccessfulCommandStopsBackgroundChildren(t *testing.T) {
	root := t.TempDir()
	release := func() { _ = os.WriteFile(filepath.Join(root, "release"), nil, 0o600) }
	t.Cleanup(release)
	res, _ := New(Config{Root: root}).run(t.Context(), Run(
		`sh -c 'while [ ! -f release ]; do sleep 0.01; done; printf escaped > marker' >/dev/null 2>&1 &`,
		5,
	))
	if !res.OK {
		t.Fatalf("background command: %+v", res)
	}
	release()
	time.Sleep(time.Second)
	if _, err := os.Stat(filepath.Join(root, "marker")); !os.IsNotExist(err) {
		t.Fatalf("child wrote after its command completed: %v", err)
	}
}
