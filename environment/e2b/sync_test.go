package e2b

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samperrin/pons/environment"
	"github.com/samperrin/pons/plugins/external/sdk"
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

var (
	extractCommand = regexp.MustCompile(`tar -xf (\S+) -C (\S+)`)
	packCommand    = regexp.MustCompile(`find (\S+) -type l -delete && tar --hard-dereference -cf (\S+) -C \S+ \.`)
)

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
		if match := packCommand.FindStringSubmatch(args[1]); match != nil {
			dir := f.local(match[1])
			if err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
				if err == nil && entry.Type()&fs.ModeSymlink != 0 {
					return os.Remove(path)
				}
				return err
			}); err != nil {
				return err
			}
			archive, err := archiveWorkspace(dir, 1<<20)
			f.files[match[2]] = archive
			return err
		}
		match := extractCommand.FindStringSubmatch(args[1])
		dir := f.local(match[2])
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		return restoreWorkspace(dir, f.files[match[1]], 1<<20)
	case "/bin/tar":
		archive, err := archiveWorkspace(f.local(flagValue(args, "-C")), 1<<20)
		f.files[flagValue(args, "-cf")] = archive
		return err
	}
	return nil
}

func flagValue(args []string, name string) string {
	for i := range len(args) - 1 {
		if args[i] == name {
			return args[i+1]
		}
	}
	return ""
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

	// The agent edits its copy, including links that are never synced, while
	// another run changes tea.md on the host.
	write(filepath.Join(remote, "MEMORY.md"), "- [Tea](tea.md)\n- [Coffee](coffee.md)\n")
	if err := os.Symlink("/etc/passwd", filepath.Join(remote, "passwd.md")); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(remote, "coffee.md"), "flat white\n")
	write(filepath.Join(memory, "tea.md"), "oolong\n")

	var reported []error
	session := &e2bSession{
		client: client, sandbox: sandbox, synced: synced, maxWorkspaceBytes: 1 << 20,
		onError: func(err error) { reported = append(reported, err) },
	}
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
	if _, err := os.Lstat(filepath.Join(memory, "passwd.md")); !os.IsNotExist(err) {
		t.Fatalf("a sandbox link was synced back: %v", err)
	}
}

// startWithMemory runs Provider.Start against the fake envd with one synced
// memory directory, serving hands in-process.
func startWithMemory(t *testing.T, failSyncUpload bool) (*fakeSyncEnvd, string, *atomic.Pointer[[]string], *atomic.Int32, environment.HandsSession, error) {
	t.Helper()
	envd := &fakeSyncEnvd{t: t, root: t.TempDir(), files: map[string][]byte{}}
	var handsArgs atomic.Pointer[[]string]
	var deletes atomic.Int32
	var input atomic.Pointer[io.PipeWriter]
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/sandboxes":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"sandboxID":"sandbox","envdAccessToken":"token"}`)
		case r.Method == http.MethodDelete:
			deletes.Add(1)
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/sandboxes/sandbox/timeout":
			w.WriteHeader(http.StatusNoContent)
		case failSyncUpload && r.URL.Path == "/files" && r.URL.Query().Get("path") == "/tmp/pons-sync-0.tar":
			http.Error(w, "upload unavailable", http.StatusServiceUnavailable)
		case r.URL.Path == "/process.Process/SendInput":
			var payload struct{ Input struct{ Stdin string } }
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
				return
			}
			body, err := base64.StdEncoding.DecodeString(payload.Input.Stdin)
			if err == nil {
				_, err = input.Load().Write(body)
			}
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
			}
		case r.URL.Path == "/process.Process/Start":
			body, _ := io.ReadAll(r.Body)
			var request struct {
				Process struct {
					Cmd  string
					Args []string
				}
			}
			if len(body) < 5 || json.Unmarshal(body[5:], &request) != nil {
				t.Error("invalid Start frame")
				return
			}
			if request.Process.Cmd != defaultE2BHandsPath {
				r.Body = io.NopCloser(bytes.NewReader(body))
				envd.ServeHTTP(w, r)
				return
			}
			handsArgs.Store(&request.Process.Args)
			w.Header().Set("Content-Type", "application/connect+json")
			inR, inW := io.Pipe()
			outR, outW := io.Pipe()
			input.Store(inW)
			defer inW.Close()
			defer outR.Close()
			go func() {
				serveErr := (sdk.Server{Name: "pons.hands", Version: "test"}).Serve(r.Context(), inR, outW)
				_ = outW.CloseWithError(serveErr)
			}()
			_ = writeConnectFrame(w, map[string]any{"event": map[string]any{"start": map[string]int{"pid": 42}}})
			w.(http.Flusher).Flush()
			buffer := make([]byte, 4096)
			for {
				n, err := outR.Read(buffer)
				if n > 0 {
					_ = writeConnectFrame(w, map[string]any{"event": map[string]any{"data": map[string]string{"stdout": base64.StdEncoding.EncodeToString(buffer[:n])}}})
					w.(http.Flusher).Flush()
				}
				if err != nil {
					if errors.Is(err, io.EOF) {
						_ = writeConnectFrame(w, map[string]any{"event": map[string]any{"end": map[string]string{"status": "exit status 0"}}})
					}
					return
				}
			}
		default:
			envd.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(server.Close)

	provider := &Provider{APIKey: "key", APIURL: server.URL, EnvdURL: server.URL, HTTPClient: server.Client(), CleanupInterval: time.Hour}
	store := &recordingStateStore{}
	if err := provider.SetStores(store, store); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = provider.Close() })
	memory := filepath.Join(t.TempDir(), "memory")
	if err := os.MkdirAll(memory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(memory, "MEMORY.md"), []byte("- tea\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	session, err := provider.Start(context.Background(), environment.Spec{
		WorkspaceID: "workspace", WorkspacePath: t.TempDir(), RunID: "run",
		Command: []string{defaultE2BHandsPath}, ReadWrite: []string{memory},
	})
	return envd, memory, &handsArgs, &deletes, session, err
}

func TestStartSyncsMemoryAndGrantsItToHands(t *testing.T) {
	envd, memory, handsArgs, _, session, err := startWithMemory(t, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := session.Metadata().ReadWrite; len(got) != 1 || got[0] != "/home/user/.pons/memory" {
		t.Fatalf("metadata read-write = %v", got)
	}
	if args := handsArgs.Load(); args == nil || flagValue(*args, "--read-write") != "/home/user/.pons/memory" {
		t.Fatalf("hands args = %v", args)
	}
	remote := envd.local("/home/user/.pons/memory")
	if data, err := os.ReadFile(filepath.Join(remote, "MEMORY.md")); err != nil || string(data) != "- tea\n" {
		t.Fatalf("remote memory = %q, %v", data, err)
	}
	if err := os.WriteFile(filepath.Join(remote, "MEMORY.md"), []byte("- coffee\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(memory, "MEMORY.md")); err != nil || string(data) != "- coffee\n" {
		t.Fatalf("host memory after close = %q, %v", data, err)
	}
}

func TestStartDeletesSandboxWhenMemorySyncFails(t *testing.T) {
	_, _, _, deletes, session, err := startWithMemory(t, true)
	if err == nil {
		_ = session.Close()
		t.Fatal("start succeeded without syncing memory")
	}
	if deletes.Load() != 1 {
		t.Fatalf("sandbox deletes = %d, want 1", deletes.Load())
	}
}
