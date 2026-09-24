package e2b

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"testing"

	"github.com/samperrin/pons/internal/dirsync"
)

// fakeSyncEnvd emulates the envd file and process calls sync uses, keeping
// sandbox paths under a local directory.
type fakeSyncEnvd struct {
	t      *testing.T
	root   string
	mu     sync.Mutex
	files  map[string][]byte
	events []string
}

var extractCommand = regexp.MustCompile(`tar -xf (\S+) -C (\S+)`)

func (f *fakeSyncEnvd) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.URL.Path == "/files" && r.Method == http.MethodPost:
		body, _ := io.ReadAll(r.Body)
		f.files[r.URL.Query().Get("path")] = body
	case r.URL.Path == "/files":
		_, _ = w.Write(f.files[r.URL.Query().Get("path")])
	case r.URL.Path == "/process.Process/Start":
		body, _ := io.ReadAll(r.Body)
		var request struct {
			Process struct {
				Cmd  string
				Args []string
			}
		}
		if len(body) < 5 || json.Unmarshal(body[5:], &request) != nil {
			f.t.Error("invalid Start frame")
			return
		}
		if err := f.run(request.Process.Cmd, request.Process.Args); err != nil {
			f.t.Error(err)
		}
		w.Header().Set("Content-Type", "application/connect+json")
		_ = writeConnectFrame(w, map[string]any{"event": map[string]any{"end": map[string]string{"status": "exit status 0"}}})
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeSyncEnvd) run(command string, args []string) error {
	f.events = append(f.events, command)
	switch command {
	case "/bin/sh":
		match := extractCommand.FindStringSubmatch(args[1])
		tree, err := dirsync.Unarchive(bytes.NewReader(f.files[match[1]]))
		if err != nil {
			return err
		}
		dir := f.local(match[2])
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		_, err = dirsync.Apply(dir, nil, tree)
		return err
	case "/bin/tar":
		tree, err := dirsync.Read(f.local(args[3]))
		if err != nil {
			return err
		}
		f.files[args[1]], err = dirsync.Archive(tree)
		return err
	}
	return nil
}

func (f *fakeSyncEnvd) local(remote string) string {
	return filepath.Join(f.root, filepath.FromSlash(remote))
}

func TestSyncedDirectoryRoundTripsOnlyTheRunsChanges(t *testing.T) {
	envd := &fakeSyncEnvd{t: t, root: t.TempDir(), files: map[string][]byte{}}
	server := httptest.NewServer(envd)
	defer server.Close()
	client := &e2bClient{envdURL: server.URL, http: server.Client()}
	sandbox := e2bSandbox{ID: "sandbox", AccessToken: "token"}

	memory := filepath.Join(t.TempDir(), "memory")
	if err := os.MkdirAll(memory, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(memory, "MEMORY.md"), "- [Tea](tea.md)\n")
	write(filepath.Join(memory, "tea.md"), "green\n")

	synced, err := syncDirectoriesIn(context.Background(), client, sandbox, []string{memory})
	if err != nil {
		t.Fatal(err)
	}
	if paths := syncedRemotePaths(synced); len(paths) != 1 || paths[0] != "/home/user/.pons/memory" {
		t.Fatalf("remote paths = %v", paths)
	}
	remote := envd.local(synced[0].remote)
	if data, err := os.ReadFile(filepath.Join(remote, "tea.md")); err != nil || string(data) != "green\n" {
		t.Fatalf("remote copy = %q, %v", data, err)
	}

	// The agent edits its copy while another run changes tea.md on the host.
	write(filepath.Join(remote, "MEMORY.md"), "- [Tea](tea.md)\n- [Coffee](coffee.md)\n")
	write(filepath.Join(remote, "coffee.md"), "flat white\n")
	write(filepath.Join(memory, "tea.md"), "oolong\n")

	var reported []error
	session := &e2bSession{client: client, sandbox: sandbox, synced: synced, onError: func(err error) { reported = append(reported, err) }}
	session.syncDirectoriesOut()
	if len(reported) != 0 {
		t.Fatalf("sync errors = %v", reported)
	}
	for name, want := range map[string]string{
		"MEMORY.md": "- [Tea](tea.md)\n- [Coffee](coffee.md)\n",
		"coffee.md": "flat white\n",
		"tea.md":    "oolong\n",
	} {
		if data, err := os.ReadFile(filepath.Join(memory, name)); err != nil || string(data) != want {
			t.Fatalf("%s = %q, %v; want %q", name, data, err, want)
		}
	}
}
