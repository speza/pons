package e2b

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type archiveEntry struct {
	header tar.Header
	body   string
}

// Small fixtures may buffer archives; production paths always stream.
func archiveWorkspace(workspace string, limit int64) ([]byte, error) {
	var out bytes.Buffer
	err := writeWorkspaceArchive(context.Background(), &out, workspace, limit)
	return out.Bytes(), err
}

func restoreWorkspace(workspace string, archive []byte, limit int64) error {
	return restoreWorkspaceArchive(workspace, bytes.NewReader(archive), limit)
}

func TestStagedArchiveUploadAndDownload(t *testing.T) {
	want := "checkpoint content"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			body, err := io.ReadAll(r.Body)
			if err != nil || string(body) != want {
				t.Errorf("uploaded body = %q, %v", body, err)
			}
			return
		}
		_, _ = io.WriteString(w, want)
	}))
	defer server.Close()
	client := &e2bClient{envdURL: server.URL, http: server.Client()}
	ctx := context.Background()
	file, err := stageWorkspaceArchive(func(out io.Writer) error {
		return client.download(ctx, e2bSandbox{}, "/checkpoint", out, int64(len(want)))
	})
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if err := client.upload(ctx, e2bSandbox{}, "/checkpoint", file); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("upload closed caller's archive: %v", err)
	}
	if err := client.download(ctx, e2bSandbox{}, "/checkpoint", io.Discard, int64(len(want)-1)); err == nil {
		t.Fatal("accepted download over limit")
	}
}

func TestStagedArchiveFailureCleanup(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	want := errors.New("disk full")
	file, err := stageWorkspaceArchive(func(out io.Writer) error {
		_, _ = io.WriteString(out, "partial")
		return want
	})
	if file != nil || !errors.Is(err, want) {
		t.Fatalf("staging = %v, %v", file, err)
	}
	entries, err := os.ReadDir(os.TempDir())
	if err != nil || len(entries) != 0 {
		t.Fatalf("staging left files: %v, %v", entries, err)
	}
}

func TestArchiveStreamingAndCancellation(t *testing.T) {
	workspace := t.TempDir()
	file, err := os.Create(filepath.Join(workspace, "large"))
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(8 << 20); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	out := &boundedChunkWriter{}
	if err := writeWorkspaceArchive(context.Background(), out, workspace, 9<<20); err != nil {
		t.Fatal(err)
	}
	if out.total < 8<<20 {
		t.Fatalf("wrote only %d bytes", out.total)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := writeWorkspaceArchive(ctx, io.Discard, workspace, 9<<20); !errors.Is(err, context.Canceled) {
		t.Fatalf("archive after cancellation = %v", err)
	}
}

type boundedChunkWriter struct{ total int64 }

func (w *boundedChunkWriter) Write(data []byte) (int, error) {
	if len(data) > 32<<10 {
		return 0, errors.New("archive write was not streamed in bounded chunks")
	}
	w.total += int64(len(data))
	return len(data), nil
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
