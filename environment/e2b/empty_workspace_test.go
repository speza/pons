package e2b

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samperrin/pons/environment"
	checkpointstore "github.com/samperrin/pons/runtime/checkpoint"
	runtimesqlite "github.com/samperrin/pons/runtime/sqlite"
)

// fakeSandboxes is a fake E2B API and envd whose sandboxes each keep their
// workspace in a separate host directory.
type fakeSandboxes struct {
	t       *testing.T
	mu      sync.Mutex
	created int
	dirs    map[string]string
	input   atomic.Pointer[io.PipeWriter]
}

func (f *fakeSandboxes) dir(id string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	dir, ok := f.dirs[id]
	return dir, ok
}

// expire removes a sandbox, as E2B does when its timeout passes.
func (f *fakeSandboxes) expire(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.dirs, id)
}

func (f *fakeSandboxes) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id := r.Header.Get("E2b-Sandbox-Id")
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/sandboxes":
		f.mu.Lock()
		f.created++
		id = fmt.Sprintf("sandbox-%d", f.created)
		f.dirs[id] = f.t.TempDir()
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"sandboxID":%q,"envdAccessToken":"token"}`, id)
	case strings.HasSuffix(r.URL.Path, "/connect"):
		id = strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/sandboxes/"), "/connect")
		if _, ok := f.dir(id); !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = fmt.Fprintf(w, `{"sandboxID":%q,"envdAccessToken":"token"}`, id)
	case strings.HasSuffix(r.URL.Path, "/timeout") || r.Method == http.MethodDelete:
		w.WriteHeader(http.StatusNoContent)
	case r.URL.Path == "/files":
		dir, ok := f.dir(id)
		if !ok {
			http.NotFound(w, r)
			return
		}
		f.files(w, r, dir)
	case r.URL.Path == "/process.Process/SendInput":
		if err := forwardInput(r, &f.input); err != nil {
			f.t.Error(err)
			w.WriteHeader(http.StatusInternalServerError)
		}
	case r.URL.Path == "/process.Process/Start":
		body, _ := io.ReadAll(r.Body)
		request, ok := decodeStart(body)
		if !ok {
			f.t.Error("invalid Start frame")
			return
		}
		if request.Process.Cmd == defaultE2BHandsPath {
			serveHands(w, r, &f.input)
			return
		}
		// The upload already unpacked the workspace and the download packs
		// it, so the prepare and checkpoint scripts have nothing left to do.
		w.Header().Set("Content-Type", "application/connect+json")
		writeEnd(w)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeSandboxes) files(w http.ResponseWriter, r *http.Request, dir string) {
	switch path := r.URL.Query().Get("path"); {
	case r.Method == http.MethodPost && path == workspaceUploadPath:
		body, err := io.ReadAll(r.Body)
		if err == nil {
			err = restoreWorkspace(dir, body, 1<<20)
		}
		if err != nil {
			f.t.Error(err)
			w.WriteHeader(http.StatusInternalServerError)
		}
	case r.Method == http.MethodGet && path == workspaceCheckpointPath:
		archive, err := archiveWorkspace(dir, 1<<20)
		if err != nil {
			f.t.Error(err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(archive)
	default:
		f.t.Errorf("unexpected file %s %s", r.Method, path)
		w.WriteHeader(http.StatusInternalServerError)
	}
}

func TestEmptyWorkspaceSurvivesReplacementSandbox(t *testing.T) {
	ctx := context.Background()
	stateDir := t.TempDir()
	store, err := runtimesqlite.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	fake := &fakeSandboxes{t: t, dirs: make(map[string]string)}
	server := httptest.NewServer(fake)
	defer server.Close()
	provider := &Provider{APIKey: "key", APIURL: server.URL, EnvdURL: server.URL, HTTPClient: server.Client(), CleanupInterval: time.Hour}
	checkpoints := checkpointstore.New(filepath.Join(stateDir, "workspaces"))
	if err := provider.SetStores(store, checkpoints); err != nil {
		t.Fatal(err)
	}
	defer provider.Close()

	// Each run stands in for a conversation of the agent; all share its
	// workspace and none names a host source.
	run := func(runID string) (environment.HandsSession, string) {
		t.Helper()
		session, err := provider.Start(ctx, environment.Spec{
			WorkspaceID: "agent-default", RunID: runID, Command: []string{defaultE2BHandsPath},
			WorkspacePlan: environment.WorkspacePlan{Strategy: environment.WorkspaceStrategyEmpty},
		})
		if err != nil {
			t.Fatal(err)
		}
		dir, ok := fake.dir(session.Metadata().EnvironmentID)
		if !ok {
			t.Fatalf("session placed on unknown sandbox %q", session.Metadata().EnvironmentID)
		}
		return session, dir
	}

	first, firstDir := run("run-1")
	seeded, err := store.WorkspaceState(ctx, "agent-default")
	if err != nil {
		t.Fatal(err)
	}
	if seeded.Strategy != environment.WorkspaceStrategyEmpty || seeded.SourceRef != "" ||
		seeded.CheckpointRef == "" || seeded.BaseRevision != seeded.CheckpointRef {
		t.Fatalf("seeded workspace = %+v", seeded)
	}
	if entries, err := os.ReadDir(firstDir); err != nil || len(entries) != 0 {
		t.Fatalf("first placement = %v, %v; want an empty workspace", entries, err)
	}
	if err := os.WriteFile(filepath.Join(firstDir, "notes.txt"), []byte("kept"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	fake.expire(first.Metadata().EnvironmentID)
	second, secondDir := run("run-2")
	defer second.Close()
	if second.Metadata().EnvironmentID == first.Metadata().EnvironmentID {
		t.Fatal("second run reused the expired sandbox")
	}
	if data, err := os.ReadFile(filepath.Join(secondDir, "notes.txt")); err != nil || string(data) != "kept" {
		t.Fatalf("replacement sandbox notes = %q, %v", data, err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	persisted, err := store.WorkspaceState(ctx, "agent-default")
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Strategy != environment.WorkspaceStrategyEmpty || persisted.BaseRevision != seeded.BaseRevision {
		t.Fatalf("persisted workspace = %+v, seeded %+v", persisted, seeded)
	}
	base, err := checkpoints.WorkspaceCheckpoint(ctx, "agent-default", seeded.BaseRevision, 1<<20)
	if err != nil {
		t.Fatalf("empty base checkpoint pruned: %v", err)
	}
	if err := base.Close(); err != nil {
		t.Fatal(err)
	}
}
