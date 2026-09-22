package e2b

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samperrin/pons/environment"
	"github.com/samperrin/pons/plugins/external/sdk"
)

func TestCanceledRunStillCheckpointsSession(t *testing.T) {
	testSessionCheckpoint(t, "")
}

func TestHeartbeatFailureStillCheckpointsSession(t *testing.T) {
	testSessionCheckpoint(t, "heartbeat")
}

func TestCheckpointFailuresRetainSandboxForRecovery(t *testing.T) {
	for _, fault := range []string{"checkpoint", "metadata", "download", "idle", "reservation", "timeout"} {
		t.Run(fault, func(t *testing.T) { testSessionCheckpoint(t, fault) })
	}
}

type checkpointFailureStore struct {
	recordingStateStore
	fault string
}

func (s *checkpointFailureStore) PutWorkspaceCheckpoint(ctx context.Context, id string, body []byte) (string, error) {
	seed := s.workspaceLast.Load() == nil
	if !seed && s.fault == "checkpoint" {
		return "", errors.New("checkpoint disk full")
	}
	_, err := s.recordingStateStore.PutWorkspaceCheckpoint(ctx, id, body)
	if seed {
		return "seed", err
	}
	return "updated", err
}

func (s *checkpointFailureStore) SaveWorkspaceState(ctx context.Context, state environment.WorkspaceState) error {
	if s.workspaceLast.Load() != nil && s.fault == "metadata" {
		return errors.New("workspace metadata unavailable")
	}
	return s.recordingStateStore.SaveWorkspaceState(ctx, state)
}

func (s *checkpointFailureStore) SaveEnvironmentState(ctx context.Context, state environment.State) error {
	if (s.fault == "idle" && state.Status == environment.StateIdle) || (s.fault == "reservation" && state.Status == environment.StateRecovery) {
		return errors.New("placement metadata unavailable")
	}
	return s.recordingStateStore.SaveEnvironmentState(ctx, state)
}

func testSessionCheckpoint(t *testing.T, fault string) {
	t.Helper()
	source, remote := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "seed"), []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	var input atomic.Pointer[io.PipeWriter]
	var checkpoint atomic.Pointer[[]byte]
	var deletes atomic.Int32
	var timeoutSeconds atomic.Int32
	heartbeatErrors := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/sandboxes":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"sandboxID":"sandbox","envdAccessToken":"token"}`)
		case r.Method == http.MethodDelete:
			deletes.Add(1)
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/sandboxes/sandbox/timeout":
			var payload struct{ Timeout int32 }
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			timeoutSeconds.Store(payload.Timeout)
			if fault == "timeout" || (fault == "heartbeat" && payload.Timeout == 1) {
				http.Error(w, "timeout update unavailable", http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/files" && r.Method == http.MethodPost:
			body, err := io.ReadAll(r.Body)
			if err == nil {
				err = restoreWorkspace(remote, body, 1<<20)
			}
			if err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusInternalServerError)
			}
		case r.URL.Path == "/files":
			if fault == "download" {
				http.Error(w, "download unavailable", http.StatusServiceUnavailable)
				return
			}
			body := checkpoint.Load()
			if body == nil {
				t.Error("download before checkpoint")
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, _ = w.Write(*body)
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
				t.Error(err)
				w.WriteHeader(http.StatusInternalServerError)
			}
		case r.URL.Path == "/process.Process/Start":
			body, _ := io.ReadAll(r.Body)
			var request struct{ Process struct{ Cmd string } }
			if len(body) < 5 || json.Unmarshal(body[5:], &request) != nil {
				t.Error("invalid Start frame")
				return
			}
			w.Header().Set("Content-Type", "application/connect+json")
			if request.Process.Cmd != defaultE2BHandsPath {
				if request.Process.Cmd == "/bin/tar" {
					archive, err := archiveWorkspace(remote, 1<<20)
					if err != nil {
						t.Error(err)
						return
					}
					checkpoint.Store(&archive)
				}
				_ = writeConnectFrame(w, map[string]any{"event": map[string]any{"end": map[string]string{"status": "exit status 0"}}})
				return
			}
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
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	store := &checkpointFailureStore{fault: fault}
	provider := &Provider{APIKey: "key", APIURL: server.URL, EnvdURL: server.URL, HTTPClient: server.Client(), CleanupInterval: time.Hour}
	if fault == "heartbeat" {
		provider.Timeout = time.Second
		provider.OnError = func(err error) {
			select {
			case heartbeatErrors <- err:
			default:
			}
		}
	}
	if err := provider.SetStores(store, store); err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	session, err := provider.Start(ctx, environment.Spec{
		WorkspaceID: "workspace", WorkspacePath: source, RunID: "run", Command: []string{defaultE2BHandsPath},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if metadata := session.Metadata(); metadata.WorkspacePath != defaultE2BWorkspace || metadata.Platform != "linux/amd64" {
		t.Fatalf("metadata = %+v", metadata)
	}
	if err := os.WriteFile(filepath.Join(remote, "result"), []byte("completed edit"), 0o600); err != nil {
		t.Fatal(err)
	}
	if fault == "heartbeat" {
		select {
		case err := <-heartbeatErrors:
			if !strings.Contains(err.Error(), "timeout update unavailable") {
				t.Fatalf("heartbeat = %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("heartbeat failure was not reported")
		}
	}
	cancel()
	closeErr := session.Close()
	wantFailure := fault != "" && fault != "heartbeat"
	wantRetained := "seed"
	if !wantFailure || fault == "idle" {
		wantRetained = "seed,updated"
	}
	if retained := store.prunedCheckpointRefs.Load(); retained == nil || *retained != wantRetained {
		t.Fatalf("pruning advanced without a durable checkpoint: retained=%v, want %q", retained, wantRetained)
	}
	if wantFailure {
		if closeErr == nil || !strings.Contains(closeErr.Error(), `sandbox "sandbox"`) {
			t.Fatalf("Close = %v", closeErr)
		}
		state := store.environmentLast.Load()
		wantStatus := environment.StateRecovery
		if fault == "reservation" {
			wantStatus = environment.StateActive
		}
		if deletes.Load() != 0 || state.Status != wantStatus {
			t.Fatalf("deletes=%d state=%+v", deletes.Load(), state)
		}
		if fault == "timeout" || fault == "reservation" {
			if !strings.Contains(closeErr.Error(), "recover files immediately") {
				t.Fatalf("missing incomplete-retention warning: %v", closeErr)
			}
		} else if !strings.Contains(closeErr.Error(), "retained for manual recovery until "+state.ExpiresAt.Format(time.RFC3339)) {
			t.Fatalf("missing recovery deadline: %v", closeErr)
		}
		if err := session.Close(); err != closeErr || !store.environmentLast.Load().ExpiresAt.Equal(state.ExpiresAt) {
			t.Fatal("repeated Close changed the recovery window")
		}
		if seconds := timeoutSeconds.Load(); seconds < 3600 || seconds > 3660 {
			t.Fatalf("recovery TTL = %d", seconds)
		}
		if fault != "idle" && store.workspaceLast.Load().CheckpointRef != "seed" {
			t.Fatal("failed checkpoint advanced durable reference")
		}
		if fault == "idle" && store.workspaceLast.Load().CheckpointRef != "updated" {
			t.Fatal("idle failure lost the completed checkpoint reference")
		}
		// A fresh provider must block rather than reconnect OR delete the reserved sandbox.
		restarted := &Provider{stateStore: &placementStore{state: *state}}
		_, _, err := restarted.acquireSandbox(context.Background(), &e2bClient{apiURL: server.URL, http: server.Client()}, e2bConfig{}, "workspace", "next-run", environment.NetworkDisabled)
		if err == nil || !strings.Contains(err.Error(), "new runs are blocked") || deletes.Load() != 0 {
			t.Fatalf("restarted acquisition = %v", err)
		}
		if body, err := os.ReadFile(filepath.Join(remote, "result")); err != nil || string(body) != "completed edit" {
			t.Fatalf("recovery data = %q, %v", body, err)
		}
		return
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	if deletes.Load() != 0 || store.environmentLast.Load().Status != environment.StateIdle {
		t.Fatal("canceled run discarded its sandbox instead of checkpointing and marking idle")
	}
	archive, err := store.WorkspaceCheckpoint(context.Background(), "workspace", "checkpoint", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	restored := t.TempDir()
	if err := restoreWorkspace(restored, archive, 1<<20); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(filepath.Join(restored, "result")); err != nil || string(body) != "completed edit" {
		t.Fatalf("restored result = %q, %v", body, err)
	}
	if _, err := os.Stat(filepath.Join(source, "result")); !os.IsNotExist(err) {
		t.Fatalf("source changed: %v", err)
	}
}

type expiredStateStore struct{ recordingStateStore }

func (*expiredStateStore) ExpiredEnvironmentStates(context.Context, string, time.Time, int) ([]environment.State, error) {
	return []environment.State{{WorkspaceID: "workspace", EnvironmentID: "sandbox"}}, nil
}

func TestProviderCloseCancelsBlockedJanitor(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-r.Context().Done():
		case <-release:
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	defer close(release)
	store := &expiredStateStore{}
	p := &Provider{APIKey: "key", APIURL: server.URL, HTTPClient: server.Client(), CleanupInterval: 10 * time.Millisecond}
	if err := p.SetStores(store, store); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("janitor did not start cleanup")
	}
	done := make(chan error, 1)
	go func() { done <- p.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close could not cancel blocked DELETE")
	}
}

func TestJanitorSkipsBusyLifecycleAndRetriesNextSweep(t *testing.T) {
	var deletes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		deletes.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	store := &expiredStateStore{}
	p := &Provider{stateStore: store}
	cfg := e2bConfig{apiURL: server.URL, http: server.Client()}
	func() {
		p.lifecycleMu.Lock()
		defer p.lifecycleMu.Unlock()
		done := make(chan struct{})
		go func() {
			p.cleanExpired(context.Background(), cfg)
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("maintenance blocked behind startup")
		}
	}()
	if deletes.Load() != 0 {
		t.Fatal("maintenance ran during a lifecycle transition")
	}
	p.cleanExpired(context.Background(), cfg)
	if deletes.Load() != 1 || store.environmentDeletes.Load() != 1 {
		t.Fatal("next sweep did not clean up the expired placement")
	}
}

func TestProcessWriterCloseCancelsBlockedWrite(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(entered)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	w := &e2bProcessWriter{client: &e2bClient{http: server.Client(), envdURL: server.URL}, pid: 42}
	written := make(chan error, 1)
	go func() { _, err := w.Write([]byte("frame")); written <- err }()
	<-entered
	closed := make(chan error, 1)
	go func() { closed <- w.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close blocked on SendInput")
	}
	select {
	case err := <-written:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Write = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Write did not cancel")
	}
	if _, err := w.Write([]byte("again")); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("closed Write = %v", err)
	}
}

func TestControlProcessOutputIsBoundedAcrossFrames(t *testing.T) {
	for _, streamName := range []string{"stdout", "stderr"} {
		t.Run(streamName, func(t *testing.T) {
			var frames bytes.Buffer
			for range 3 {
				if err := writeConnectFrame(&frames, map[string]any{"event": map[string]any{"data": map[string]string{
					streamName: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("x"), 512<<10)),
				}}}); err != nil {
					t.Fatal(err)
				}
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(frames.Bytes()) }))
			defer server.Close()
			client := &e2bClient{envdURL: server.URL, http: server.Client()}
			_, _, err := client.run(context.Background(), e2bSandbox{}, "tar", nil, "/workspace", nil)
			if err == nil || !strings.Contains(err.Error(), "control process output") {
				t.Fatalf("run = %v", err)
			}
		})
	}
}

type placementStore struct {
	recordingStateStore
	state environment.State
}

func (s *placementStore) EnvironmentState(context.Context, string) (environment.State, error) {
	return s.state, nil
}

func TestFailedSandboxDeletionKeepsPlacement(t *testing.T) {
	for _, stage := range []string{"replace", "startup"} {
		t.Run(stage, func(t *testing.T) {
			var deletes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodDelete:
					deletes.Add(1)
					http.Error(w, "delete unavailable", http.StatusServiceUnavailable)
				case r.URL.Path == "/sandboxes/old/connect":
					_, _ = io.WriteString(w, `{"sandboxID":"old","envdAccessToken":"token"}`)
				case r.URL.Path == "/process.Process/Start":
					http.Error(w, "hands unavailable", http.StatusServiceUnavailable)
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			store := &placementStore{state: environment.State{
				Provider: "e2b", EnvironmentID: "old", Template: "template",
				Network: environment.NetworkDisabled, Status: environment.StateActive,
				ExpiresAt: time.Now().Add(-time.Second),
			}}
			p := &Provider{
				APIKey: "key", Template: "template", APIURL: server.URL, EnvdURL: server.URL,
				HTTPClient: server.Client(), stateStore: store, checkpointStore: store,
			}
			var err error
			if stage == "startup" {
				store.state.Status = environment.StateIdle
				_, err = p.Start(context.Background(), environment.Spec{
					WorkspaceID: "workspace", WorkspacePath: t.TempDir(), RunID: "run",
					Command: []string{defaultE2BHandsPath},
				})
			} else {
				_, _, err = p.acquireSandbox(context.Background(), &e2bClient{apiURL: server.URL, http: server.Client()},
					e2bConfig{template: "template"}, "workspace", "run", environment.NetworkDisabled)
			}
			if err == nil || !strings.Contains(err.Error(), "delete unavailable") {
				t.Fatalf("error = %v", err)
			}
			if deletes.Load() != 1 || store.environmentDeletes.Load() != 0 {
				t.Fatalf("sandbox deletes=%d metadata deletes=%d", deletes.Load(), store.environmentDeletes.Load())
			}
		})
	}
}

func TestAcquireReusesIdleButReplacesStaleActivePlacement(t *testing.T) {
	for _, status := range []environment.LifecycleStatus{environment.StateIdle, environment.StateActive, environment.StateRecovery} {
		t.Run(string(status), func(t *testing.T) {
			requests := make(chan string, 3)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests <- r.Method + " " + r.URL.Path
				switch r.URL.Path {
				case "/sandboxes/old/connect":
					_, _ = io.WriteString(w, `{"sandboxID":"old","envdAccessToken":"token"}`)
				case "/sandboxes/old":
					w.WriteHeader(http.StatusNoContent)
				case "/sandboxes":
					w.WriteHeader(http.StatusCreated)
					_, _ = io.WriteString(w, `{"sandboxID":"new","envdAccessToken":"token"}`)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			store := &placementStore{state: environment.State{
				Provider: "e2b", EnvironmentID: "old", Template: "template", Network: environment.NetworkDisabled, Status: status,
				ExpiresAt: time.Now().Add(-time.Second),
			}}
			provider := &Provider{stateStore: store}
			client := &e2bClient{apiURL: server.URL, http: server.Client()}
			sandbox, resumed, err := provider.acquireSandbox(context.Background(), client, e2bConfig{template: "template"}, "workspace", "run", environment.NetworkDisabled)
			if err != nil {
				t.Fatal(err)
			}
			if status == environment.StateIdle {
				if !resumed || sandbox.ID != "old" || len(requests) != 1 || <-requests != "POST /sandboxes/old/connect" {
					t.Fatalf("idle acquisition: sandbox=%+v resumed=%v", sandbox, resumed)
				}
			} else if resumed || sandbox.ID != "new" || len(requests) != 2 || <-requests != "DELETE /sandboxes/old" || <-requests != "POST /sandboxes" {
				t.Fatalf("active placement was not destroyed before replacement: sandbox=%+v resumed=%v", sandbox, resumed)
			}
		})
	}
}

func TestCanceledStartupStopsTransport(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var deletes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete:
			deletes.Add(1)
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/sandboxes":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"sandboxID":"sandbox","envdAccessToken":"token"}`)
		case r.URL.Path == "/process.Process/Start":
			body, _ := io.ReadAll(r.Body)
			if bytes.Contains(body, []byte(defaultE2BHandsPath)) {
				close(started)
				select {
				case <-r.Context().Done():
				case <-release:
				}
				return
			}
			_ = writeConnectFrame(w, map[string]any{"event": map[string]any{"end": map[string]string{"status": "exit status 0"}}})
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()
	defer close(release)
	store := &recordingStateStore{}
	p := &Provider{APIKey: "key", APIURL: server.URL, EnvdURL: server.URL, HTTPClient: server.Client(), CleanupInterval: time.Hour}
	if err := p.SetStores(store, store); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	source := t.TempDir()
	go func() {
		_, err := p.Start(ctx, environment.Spec{WorkspaceID: "workspace", WorkspacePath: source, RunID: "run", Command: []string{defaultE2BHandsPath}})
		done <- err
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Start = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("startup transport ignored cancellation")
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if deletes.Load() != 1 {
		t.Fatalf("sandbox deletes = %d", deletes.Load())
	}
}
