package e2b

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type archiveEntry struct {
	header tar.Header
	body   string
}

func workspaceArchive(t *testing.T, entries ...archiveEntry) []byte {
	t.Helper()
	var out bytes.Buffer
	w := tar.NewWriter(&out)
	for _, entry := range entries {
		if err := w.WriteHeader(&entry.header); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(entry.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestRestoreWorkspaceRejectsSymlinkLeafReplacementEscapes(t *testing.T) {
	tests := []struct {
		name        string
		final       tar.Header
		body        string
		wantMode    os.FileMode
		wantBody    string
		sentinelDir bool
	}{
		{
			name:     "write",
			final:    tar.Header{Name: "b", Typeflag: tar.TypeReg, Mode: 0o600, Size: 6},
			body:     "attack",
			wantMode: 0o640,
			wantBody: "safe",
		},
		{
			name:        "chmod",
			final:       tar.Header{Name: "b", Typeflag: tar.TypeDir, Mode: 0o777},
			wantMode:    0o700,
			sentinelDir: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parent := t.TempDir()
			workspace := filepath.Join(parent, "workspace")
			if err := os.Mkdir(workspace, 0o755); err != nil {
				t.Fatal(err)
			}
			sentinel := filepath.Join(parent, "sentinel")
			if test.sentinelDir {
				if err := os.Mkdir(sentinel, test.wantMode); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(sentinel, []byte(test.wantBody), test.wantMode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(sentinel, test.wantMode); err != nil {
				t.Fatal(err)
			}
			archive := workspaceArchive(t,
				archiveEntry{header: tar.Header{Name: "a", Typeflag: tar.TypeSymlink, Linkname: "."}},
				archiveEntry{header: tar.Header{Name: "b", Typeflag: tar.TypeSymlink, Linkname: "a/../sentinel"}},
				archiveEntry{header: test.final, body: test.body},
			)
			if err := restoreWorkspace(workspace, archive, 1<<20); err == nil || !strings.Contains(err.Error(), "duplicate") {
				t.Fatalf("error = %v, want duplicate-path rejection", err)
			}
			info, err := os.Stat(sentinel)
			if err != nil {
				t.Fatal(err)
			}
			if got := info.Mode().Perm(); got != test.wantMode {
				t.Fatalf("sentinel mode = %v, want %v", got, test.wantMode)
			}
			if !test.sentinelDir {
				body, err := os.ReadFile(sentinel)
				if err != nil || string(body) != test.wantBody {
					t.Fatalf("sentinel = %q, %v", body, err)
				}
			}
		})
	}
}

func TestRestoreWorkspaceRejectsDuplicateNormalizedPaths(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	if err := os.Mkdir(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	archive := workspaceArchive(t,
		archiveEntry{header: tar.Header{Name: "dir/../file", Typeflag: tar.TypeReg, Mode: 0o600, Size: 1}, body: "a"},
		archiveEntry{header: tar.Header{Name: "file", Typeflag: tar.TypeReg, Mode: 0o600, Size: 1}, body: "b"},
	)
	if err := restoreWorkspace(workspace, archive, 1<<20); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("error = %v, want duplicate-path rejection", err)
	}
}

func TestArchiveWorkspaceUsesCheckpointSymlinkPolicy(t *testing.T) {
	for _, target := range []string{"/absolute", "../escape"} {
		t.Run(target, func(t *testing.T) {
			workspace := t.TempDir()
			if err := os.Symlink(target, filepath.Join(workspace, "link")); err != nil {
				t.Fatal(err)
			}
			if _, err := archiveWorkspace(workspace, 1<<20); err == nil || !strings.Contains(err.Error(), "unsafe symlink") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestArchiveWorkspaceAllowsConfinedSymlink(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "target"), []byte("safe"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(workspace, "dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../target", filepath.Join(workspace, "dir", "link")); err != nil {
		t.Fatal(err)
	}
	archive, err := archiveWorkspace(workspace, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(t.TempDir(), "restored")
	if err := os.Mkdir(restored, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := restoreWorkspace(restored, archive, 1<<20); err != nil {
		t.Fatal(err)
	}
	if target, err := os.Readlink(filepath.Join(restored, "dir", "link")); err != nil || target != "../target" {
		t.Fatalf("link = %q, %v", target, err)
	}
}
