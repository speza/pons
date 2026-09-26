//go:build unix

package dirsync

import (
	"os"
	"path/filepath"
	"testing"
)

func TestApplyReplacesDirectoryWithFile(t *testing.T) {
	checkApplyReplacement(t,
		Tree{"topic/notes/old.md": []byte("old")},
		Tree{"topic": []byte("new")},
	)
}

// A directory found by the cleanup walk can become a file or link before the
// removal reaches it. The deletion primitive must check the type atomically.
func TestRemoveDirectoryPreservesOtherEntries(t *testing.T) {
	for _, kind := range []string{"file", "symlink", "nonempty directory", "empty directory"} {
		t.Run(kind, func(t *testing.T) {
			host := t.TempDir()
			entry := filepath.Join(host, "entry")
			switch kind {
			case "file":
				writeTestFile(t, entry, "keep")
			case "symlink":
				writeTestFile(t, filepath.Join(host, "target", "note"), "keep")
				if err := os.Symlink("target", entry); err != nil {
					t.Fatal(err)
				}
			case "nonempty directory":
				writeTestFile(t, filepath.Join(entry, "note"), "keep")
			case "empty directory":
				if err := os.Mkdir(entry, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			root, err := os.OpenRoot(host)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			err = removeDirectory(root, "entry")
			if kind == "empty directory" {
				if err != nil {
					t.Fatal(err)
				}
				if _, err := os.Lstat(entry); !os.IsNotExist(err) {
					t.Fatalf("empty directory survived: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("directory-only removal accepted another entry type or a nonempty directory")
			}
			preserved := entry
			if kind != "file" {
				preserved = filepath.Join(entry, "note")
			}
			if data, err := os.ReadFile(preserved); err != nil || string(data) != "keep" {
				t.Fatalf("entry was changed: %q, %v", data, err)
			}
			if kind == "symlink" {
				if target, err := os.Readlink(entry); err != nil || target != "target" {
					t.Fatalf("symlink was changed: %q, %v", target, err)
				}
			}
		})
	}
}
