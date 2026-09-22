package e2b

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
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
	"github.com/samperrin/pons/environment/gitworkspace"
	"github.com/samperrin/pons/plugins/external"
	checkpointstore "github.com/samperrin/pons/runtime/checkpoint"
)

type gitCredentialSourceFunc func(context.Context, string, bool) (gitworkspace.Credentials, error)

func (f gitCredentialSourceFunc) Credentials(ctx context.Context, repository string, all bool) (gitworkspace.Credentials, error) {
	return f(ctx, repository, all)
}

func TestE2BConfigUsesRestrictiveDefaults(t *testing.T) {
	t.Setenv("E2B_API_KEY", "test-key")
	cfg, err := (&Provider{}).config()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.template != "pons-hands" || cfg.handsPath != "/usr/local/bin/pons-hands" {
		t.Fatalf("defaults = template %q hands %q", cfg.template, cfg.handsPath)
	}
	if cfg.timeout != 15*time.Minute || cfg.maxWorkspaceBytes != 256<<20 {
		t.Fatalf("limits = timeout %v workspace %d", cfg.timeout, cfg.maxWorkspaceBytes)
	}
}

func TestValidateE2BSpecDoesNotRequireLocalRemoteCommand(t *testing.T) {
	workspace := t.TempDir()
	gotWorkspace, args, network, env, err := validateE2BSpec(environment.Spec{
		WorkspaceID:   "workspace",
		WorkspacePath: workspace,
		Command:       []string{"/usr/local/bin/pons-hands", "--bash-timeout", "7"},
		Environment: []string{
			"EXPLICIT=value",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	wantWorkspace, err := filepath.Abs(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if gotWorkspace != wantWorkspace || strings.Join(args, " ") != "--bash-timeout 7" {
		t.Fatalf("workspace %q args %v", gotWorkspace, args)
	}
	if network != environment.NetworkDisabled || env["EXPLICIT"] != "value" {
		t.Fatalf("network %q env %v", network, env)
	}
}

func TestInvalidEnvironmentDoesNotExposeValue(t *testing.T) {
	_, _, _, _, err := validateE2BSpec(environment.Spec{
		WorkspaceID: "workspace", WorkspacePath: t.TempDir(), Command: []string{defaultE2BHandsPath},
		Environment: []string{"TOKEN=secret-value\x00"},
	})
	if err == nil || strings.Contains(err.Error(), "secret-value") {
		t.Fatalf("invalid environment error = %v", err)
	}
}

func TestValidateE2BGitWorkspaceRequiresNetwork(t *testing.T) {
	spec := environment.Spec{
		WorkspaceID: "workspace", WorkspacePath: t.TempDir(), Command: []string{defaultE2BHandsPath},
		Network: environment.NetworkDisabled,
		WorkspacePlan: environment.WorkspacePlan{
			Strategy:     environment.WorkspaceStrategyGit,
			SourceRef:    "https://github.com/example/project.git",
			BaseRevision: strings.Repeat("a", 40),
		},
	}
	if _, _, _, _, err := validateE2BSpec(spec); err == nil || !strings.Contains(err.Error(), "requires network access") {
		t.Fatalf("network-disabled Git plan error = %v", err)
	}
}

func TestE2BWorkspaceGetsCredentialsBeforeCreatingSandbox(t *testing.T) {
	wantErr := errors.New("token unavailable")
	var called bool
	store := &recordingStateStore{}
	provider := &Provider{
		APIKey: "key", CleanupInterval: time.Hour,
		GitCredentials: gitCredentialSourceFunc(func(_ context.Context, _ string, all bool) (gitworkspace.Credentials, error) {
			called = true
			if !all {
				t.Error("archive workspace did not request installation access")
			}
			return gitworkspace.Credentials{}, wantErr
		}),
	}
	if err := provider.SetStores(store, store); err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	_, err := provider.Start(context.Background(), environment.Spec{
		WorkspaceID: "workspace", WorkspacePath: t.TempDir(), RunID: "run",
		Command: []string{defaultE2BHandsPath}, Network: environment.NetworkEnabled,
		GitAllRepositories: true,
	})
	if !errors.Is(err, wantErr) || !called {
		t.Fatalf("Start error = %v, credential source called = %v", err, called)
	}
	if store.environmentSaves.Load() != 0 {
		t.Fatal("sandbox state was saved before GitHub authentication succeeded")
	}
}

func TestConnectSandboxAcceptsBoundedSuccessResponses(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusCreated} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			for _, size := range []int{70 << 10, maxE2BJSONResponseBytes} {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(status)
					_ = json.NewEncoder(w).Encode(map[string]string{
						"sandboxID": "sandbox", "envdAccessToken": "access", "extra": strings.Repeat("x", size),
					})
				}))
				client := &e2bClient{apiURL: server.URL, http: server.Client()}
				sandbox, err := client.connectSandbox(context.Background(), "sandbox", time.Minute)
				server.Close()
				if size == maxE2BJSONResponseBytes {
					if err == nil || !strings.Contains(err.Error(), "JSON response exceeds") {
						t.Fatalf("oversized response = %v", err)
					}
				} else if err != nil || sandbox.ID != "sandbox" || sandbox.AccessToken != "access" {
					t.Fatalf("connect failed: %v", err)
				}
			}
		})
	}
}

func TestArchiveWorkspaceUsesSourceOnlyForInitialSeed(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "seed"), []byte("seed"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := &recordingStateStore{}
	first, err := loadOrCreateWorkspace(
		context.Background(), store, store, "workspace", source, environment.WorkspacePlan{}, 1<<20, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(source); err != nil {
		t.Fatal(err)
	}
	second, err := loadOrCreateWorkspace(
		context.Background(), store, store, "workspace", source, environment.WorkspacePlan{}, 1<<20, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if second.CheckpointRef != first.CheckpointRef || second.SourceRef != first.SourceRef {
		t.Fatalf("workspace changed after source removal: first=%+v second=%+v", first, second)
	}
}

func TestProvisionGitWorkspaceUsesImmutableRevisionAndPersistsCheckpoint(t *testing.T) {
	remote := t.TempDir()
	if err := os.Mkdir(filepath.Join(remote, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(remote, ".git", "HEAD"), []byte("ref: refs/heads/pons/work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	archive, err := archiveWorkspace(remote, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	var commands []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/process.Process/Start":
			body, _ := io.ReadAll(r.Body)
			var request struct {
				Process struct {
					Cmd  string
					Args []string
				}
			}
			if len(body) < 5 || json.Unmarshal(body[5:], &request) != nil {
				t.Error("invalid process frame")
				return
			}
			commands = append(commands, strings.Join(append([]string{request.Process.Cmd}, request.Process.Args...), " "))
			w.Header().Set("Content-Type", "application/connect+json")
			_ = writeConnectFrame(w, map[string]any{"event": map[string]any{"end": map[string]string{"status": "exit status 0"}}})
		case r.URL.Path == "/files" && r.Method == http.MethodGet:
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	store := &recordingStateStore{}
	revision := strings.Repeat("b", 40)
	var debugEvents []string
	state, err := provisionGitWorkspace(
		context.Background(),
		&e2bClient{envdURL: server.URL, http: server.Client()},
		e2bSandbox{ID: "sandbox", AccessToken: "access"},
		store,
		store,
		environment.WorkspaceState{
			ID: "workspace", Strategy: environment.WorkspaceStrategyGit,
			SourceRef: "https://github.com/example/project.git", BaseRevision: revision,
		},
		map[string]string{"GIT_CONFIG_VALUE_0": "secret-token"},
		1<<20,
		nil,
		func(event string) { debugEvents = append(debugEvents, event) },
	)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(commands, "\n")
	if !strings.Contains(joined, "git -C "+defaultE2BWorkspace+" fetch --depth=1 --no-tags origin "+revision) ||
		!strings.Contains(joined, "git -C "+defaultE2BWorkspace+" checkout -b "+gitworkspace.BranchName("workspace")+" FETCH_HEAD") {
		t.Fatalf("Git commands:\n%s", joined)
	}
	if state.CheckpointRef != "checkpoint" || store.workspaceLast.Load().CheckpointRef != "checkpoint" {
		t.Fatalf("workspace state = %+v, stored = %+v", state, store.workspaceLast.Load())
	}
	debug := strings.Join(debugEvents, "\n")
	for _, step := range []string{"git init", "git remote add", "git fetch", "git checkout", "initial checkpoint=checkpoint saved"} {
		if !strings.Contains(debug, step) {
			t.Fatalf("missing debug step %q: %s", step, debug)
		}
	}
	if strings.Contains(debug, "secret-token") {
		t.Fatalf("Git credential leaked in debug events: %s", debug)
	}
}

func TestE2BDoesNotPersistSandboxBeforeWorkspaceSetup(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/sandboxes":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"sandboxID":"sandbox","templateID":"template","envdAccessToken":"access"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/files":
			http.Error(w, "upload failed", http.StatusInternalServerError)
		case r.Method == http.MethodDelete && r.URL.Path == "/sandboxes/sandbox":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	store := &recordingStateStore{}
	provider := &Provider{
		APIKey: "key", Template: "template", APIURL: server.URL, EnvdURL: server.URL,
		HTTPClient: server.Client(), CleanupInterval: time.Hour,
	}
	if err := provider.SetStores(store, store); err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	_, err := provider.Start(context.Background(), environment.Spec{
		WorkspaceID: "workspace", WorkspacePath: t.TempDir(), RunID: "run",
		Command: []string{"/usr/local/bin/pons-hands"},
	})
	if err == nil || !strings.Contains(err.Error(), "upload failed") {
		t.Fatalf("error = %v", err)
	}
	if store.environmentSaves.Load() != 0 {
		t.Fatalf("saved %d environment states before setup completed", store.environmentSaves.Load())
	}
}

func TestE2BCreateSandboxKeepsCredentialOnPlatformRequest(t *testing.T) {
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sandboxes" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("X-API-Key") != "secret" || r.Header.Get("X-Access-Token") != "" {
			t.Errorf("headers = %v", r.Header)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"sandboxID":"sandbox","envdAccessToken":"access"}`)
	}))
	defer server.Close()
	client := &e2bClient{apiKey: "secret", apiURL: server.URL, envdURL: server.URL, http: server.Client()}
	sandbox, err := client.createSandbox(context.Background(), "template", time.Minute, environment.NetworkDisabled, map[string]string{"SAFE": "yes"})
	if err != nil {
		t.Fatal(err)
	}
	if sandbox.ID != "sandbox" || got["allow_internet_access"] != false || got["secure"] != true {
		t.Fatalf("sandbox = %+v payload = %#v", sandbox, got)
	}
}

func TestE2BProcessOutputIsBounded(t *testing.T) {
	buffer := boundedProcessBuffer{remaining: maxE2BProcessOutputBytes}
	if _, err := buffer.Write(make([]byte, maxE2BProcessOutputBytes)); err != nil {
		t.Fatal(err)
	}
	if _, err := buffer.Write([]byte("overflow")); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("error = %v", err)
	}
}

func TestE2BProcessEndReportsErrorWithoutStatus(t *testing.T) {
	message := "process could not start"
	err := e2bProcessEndError(&e2bProcessEnd{Error: &message}, "")
	if err == nil || !strings.Contains(err.Error(), message) {
		t.Fatalf("error = %v", err)
	}
}

func TestProcessStreamDecodesConnectFrames(t *testing.T) {
	body, err := json.Marshal(map[string]any{"event": map[string]any{"start": map[string]int{"pid": 42}}})
	if err != nil {
		t.Fatal(err)
	}
	var framed bytes.Buffer
	framed.WriteByte(0)
	if err := binary.Write(&framed, binary.BigEndian, uint32(len(body))); err != nil {
		t.Fatal(err)
	}
	framed.Write(body)
	stream := &e2bProcessStream{body: io.NopCloser(&framed)}
	response, err := stream.next()
	if err != nil {
		t.Fatal(err)
	}
	if response.Event.Start == nil || response.Event.Start.PID != 42 {
		t.Fatalf("response = %+v", response)
	}
}

func TestE2BConnectionKillUnblocksWait(t *testing.T) {
	release := make(chan struct{})
	var signals atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/process.Process/Start":
			w.Header().Set("Content-Type", "application/connect+json")
			if err := writeConnectFrame(w, map[string]any{
				"event": map[string]any{"start": map[string]int{"pid": 42}},
			}); err != nil {
				t.Error(err)
				return
			}
			w.(http.Flusher).Flush()
			if err := writeConnectFrame(w, map[string]any{
				"event": map[string]any{"data": map[string]string{
					"stdout": base64.StdEncoding.EncodeToString([]byte("blocked output")),
				}},
			}); err != nil {
				t.Error(err)
				return
			}
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
			case <-release:
			}
		case "/process.Process/SendSignal":
			signals.Add(1)
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(func() {
		close(release)
		server.Close()
	})

	client := &e2bClient{envdURL: server.URL, http: server.Client()}
	connection, err := client.startConnection(
		context.Background(),
		e2bSandbox{ID: "sandbox", AccessToken: "access"},
		"/usr/local/bin/pons-hands",
		nil,
		"/workspace",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Kill(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan struct{})
	go func() {
		_ = connection.Wait()
		close(waited)
	}()
	select {
	case <-waited:
	case <-time.After(time.Second):
		t.Fatal("connection Wait did not unblock after Kill")
	}
	if err := connection.Kill(); err != nil {
		t.Fatal(err)
	}
	if got := signals.Load(); got != 1 {
		t.Fatalf("signals = %d, want 1", got)
	}
}

func writeConnectFrame(w io.Writer, value any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	var header [5]byte
	binary.BigEndian.PutUint32(header[1:], uint32(len(body)))
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	_, err = w.Write(body)
	return err
}

func TestSessionRefreshesSandboxAndDurableExpiry(t *testing.T) {
	var gotTimeout int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sandboxes/sandbox/timeout" {
			http.NotFound(w, r)
			return
		}
		var payload struct {
			Timeout int `json:"timeout"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
			return
		}
		gotTimeout = payload.Timeout
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	store := &recordingStateStore{}
	session := &e2bSession{
		client:    &e2bClient{apiKey: "secret", apiURL: server.URL, http: server.Client()},
		sandbox:   e2bSandbox{ID: "sandbox"},
		workspace: environment.WorkspaceState{ID: "workspace"},
		store:     store,
		runID:     "run",
		template:  "template",
		timeout:   5 * time.Minute,
		metadata:  environment.Metadata{Network: environment.NetworkDisabled},
	}
	before := time.Now().UTC()
	if err := session.refreshTimeout(context.Background()); err != nil {
		t.Fatal(err)
	}
	state := store.environmentLast.Load()
	if gotTimeout != 300 || state == nil {
		t.Fatalf("timeout = %d, state = %+v", gotTimeout, state)
	}
	if state.Status != environment.StateActive || state.RunID != "run" ||
		state.EnvironmentID != "sandbox" || state.ExpiresAt.Before(before.Add(session.timeout)) {
		t.Fatalf("state = %+v", state)
	}
}

func TestSessionCheckpointDoesNotReplaceSourceWorkspace(t *testing.T) {
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "source.txt"), []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	remote := t.TempDir()
	if err := os.WriteFile(filepath.Join(remote, "result.txt"), []byte("result"), 0o600); err != nil {
		t.Fatal(err)
	}
	archive, err := archiveWorkspace(remote, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	store := &recordingStateStore{}
	session := &e2bSession{
		store:       store,
		checkpoints: store,
		workspace: environment.WorkspaceState{
			ID: "workspace", Strategy: environment.WorkspaceStrategyArchive, SourceRef: source,
			BaseRevision: "base", CheckpointRef: "old-checkpoint",
		},
		maxWorkspaceBytes: 1 << 20,
	}
	if err := session.persistCheckpoint(context.Background(), bytes.NewReader(archive)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(source, "result.txt")); !os.IsNotExist(err) {
		t.Fatalf("remote result was written to source workspace: %v", err)
	}
	if body, err := os.ReadFile(filepath.Join(source, "source.txt")); err != nil || string(body) != "source" {
		t.Fatalf("source workspace = %q, %v", body, err)
	}
	if state := store.workspaceLast.Load(); state == nil || state.CheckpointRef != "checkpoint" {
		t.Fatalf("workspace state = %+v", state)
	}
	if pruned := store.prunedCheckpointRefs.Load(); pruned == nil || *pruned != "base,checkpoint" {
		t.Fatalf("retained checkpoint refs = %v", pruned)
	}
}

func TestGitCheckpointPruningKeepsOnlyLatestArchive(t *testing.T) {
	ctx := context.Background()
	checkpoints := checkpointstore.New(t.TempDir())
	metadata := &recordingStateStore{}
	workspaceID := "git-workspace"
	putArchive := func(contents string) string {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "contents.txt"), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		archive, err := archiveWorkspace(dir, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		ref, err := checkpoints.PutWorkspaceCheckpoint(ctx, workspaceID, bytes.NewReader(archive), 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		return ref
	}
	initialRef := putArchive("initial")
	session := &e2bSession{
		store: metadata, checkpoints: checkpoints,
		workspace: environment.WorkspaceState{
			ID: workspaceID, Strategy: environment.WorkspaceStrategyGit,
			BaseRevision: strings.Repeat("a", 40), CheckpointRef: initialRef,
		},
		maxWorkspaceBytes: 1 << 20,
		onError: func(err error) {
			t.Errorf("checkpoint pruning: %v", err)
		},
	}
	var refs []string
	for _, contents := range []string{"first run", "second run"} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "contents.txt"), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		archive, err := archiveWorkspace(dir, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		if err := session.persistCheckpoint(ctx, bytes.NewReader(archive)); err != nil {
			t.Fatal(err)
		}
		refs = append(refs, session.workspace.CheckpointRef)
	}
	for _, ref := range []string{initialRef, refs[0]} {
		reader, err := checkpoints.WorkspaceCheckpoint(ctx, workspaceID, ref, 1<<20)
		if reader != nil {
			_ = reader.Close()
		}
		if !errors.Is(err, environment.ErrStateNotFound) {
			t.Fatalf("superseded checkpoint %q remains: %v", ref, err)
		}
	}
	reader, err := checkpoints.WorkspaceCheckpoint(ctx, workspaceID, refs[1], 1<<20)
	if err != nil {
		t.Fatalf("latest checkpoint: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSessionCloseSkipsCheckpointAfterHandsFailure(t *testing.T) {
	var processStarts, sandboxDeletes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete && r.URL.Path == "/sandboxes/sandbox":
			sandboxDeletes.Add(1)
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/process.Process/Start":
			processStarts.Add(1)
			http.Error(w, "checkpoint unexpectedly started", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	host, err := external.NewConnectionHost(external.Manifest{
		ManifestVersion: external.ManifestVersion,
		Name:            "pons.hands",
		Entrypoint:      "/usr/local/bin/pons-hands",
		RuntimeProtocol: external.RuntimeProtocol,
	}, external.HostConfig{Workspace: defaultE2BWorkspace}, func(context.Context) (external.Connection, error) {
		return external.Connection{}, errors.New("hands unavailable")
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Start(context.Background()); err == nil {
		t.Fatal("host startup unexpectedly succeeded")
	}
	session := &e2bSession{
		host: host,
		client: &e2bClient{
			apiKey: "secret", apiURL: server.URL, envdURL: server.URL, http: server.Client(),
		},
		sandbox: e2bSandbox{ID: "sandbox"},
	}
	if err := session.Close(); err == nil || !strings.Contains(err.Error(), "hands unavailable") {
		t.Fatalf("error = %v", err)
	}
	if processStarts.Load() != 0 || sandboxDeletes.Load() != 1 {
		t.Fatalf("process starts = %d, sandbox deletes = %d", processStarts.Load(), sandboxDeletes.Load())
	}
}

func TestWorkspaceArchiveRestoreReconcilesDeletions(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	if err := os.Mkdir(workspace, 0o750); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = removeWorkspaceTree(workspace) })
	keep := filepath.Join(workspace, "keep")
	if err := os.WriteFile(keep, []byte("remote"), 0o760); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(keep, 0o760); err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(workspace, "locked")
	if err := os.Mkdir(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locked, "nested"), []byte("inside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("keep", filepath.Join(workspace, "link")); err != nil {
		t.Fatal(err)
	}
	archive, err := archiveWorkspace(workspace, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "local-only"), []byte("remove"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "keep"), []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := restoreWorkspace(workspace, archive, 1<<20); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(workspace, "keep"))
	if err != nil || string(body) != "remote" {
		t.Fatalf("keep = %q, %v", body, err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "local-only")); !os.IsNotExist(err) {
		t.Fatalf("local-only remains: %v", err)
	}
	target, err := os.Readlink(filepath.Join(workspace, "link"))
	if err != nil || target != "keep" {
		t.Fatalf("link = %q, %v", target, err)
	}
	for path, want := range map[string]os.FileMode{
		workspace:                          0o750,
		filepath.Join(workspace, "keep"):   0o760,
		filepath.Join(workspace, "locked"): 0o500,
	} {
		info, statErr := os.Stat(path)
		if statErr != nil {
			t.Fatal(statErr)
		}
		if got := info.Mode().Perm(); got != want {
			t.Fatalf("mode %s = %v, want %v", path, got, want)
		}
	}
	if body, err := os.ReadFile(filepath.Join(workspace, "locked", "nested")); err != nil || string(body) != "inside" {
		t.Fatalf("nested = %q, %v", body, err)
	}
}

func TestArchiveWorkspaceEnforcesLimit(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "large"), bytes.Repeat([]byte("x"), 1024), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := archiveWorkspace(workspace, 512); err == nil || !strings.Contains(err.Error(), "exceeds configured limit") {
		t.Fatalf("error = %v", err)
	}
}

func TestArchiveWorkspaceRejectsSpecialEntries(t *testing.T) {
	workspace := t.TempDir()
	listener, err := net.Listen("unix", filepath.Join(workspace, "socket"))
	if err != nil {
		t.Skipf("unix sockets unavailable: %v", err)
	}
	defer listener.Close()
	if _, err := archiveWorkspace(workspace, 1<<20); err == nil || !strings.Contains(err.Error(), "unsupported workspace entry") {
		t.Fatalf("error = %v", err)
	}
}

type recordingStateStore struct {
	environmentSaves     atomic.Int32
	environmentDeletes   atomic.Int32
	environmentLast      atomic.Pointer[environment.State]
	workspaceLast        atomic.Pointer[environment.WorkspaceState]
	prunedCheckpointRefs atomic.Pointer[string]
	checkpointMu         sync.Mutex
	checkpoint           []byte
}

func (s *recordingStateStore) WorkspaceState(context.Context, string) (environment.WorkspaceState, error) {
	state := s.workspaceLast.Load()
	if state == nil {
		return environment.WorkspaceState{}, environment.ErrStateNotFound
	}
	return *state, nil
}
func (s *recordingStateStore) SaveWorkspaceState(_ context.Context, state environment.WorkspaceState) error {
	s.workspaceLast.Store(&state)
	return nil
}
func (s *recordingStateStore) PutWorkspaceCheckpoint(_ context.Context, _ string, archive io.Reader, limit int64) (string, error) {
	s.checkpointMu.Lock()
	defer s.checkpointMu.Unlock()
	body, err := io.ReadAll(io.LimitReader(archive, limit+1))
	if err != nil {
		return "", err
	}
	if int64(len(body)) > limit {
		return "", errors.New("checkpoint exceeds limit")
	}
	s.checkpoint = body
	return "checkpoint", nil
}
func (s *recordingStateStore) WorkspaceCheckpoint(context.Context, string, string, int64) (io.ReadCloser, error) {
	s.checkpointMu.Lock()
	defer s.checkpointMu.Unlock()
	if s.checkpoint == nil {
		return nil, environment.ErrStateNotFound
	}
	return io.NopCloser(bytes.NewReader(s.checkpoint)), nil
}
func (s *recordingStateStore) PruneWorkspaceCheckpoints(_ context.Context, _ string, refs []string) error {
	retained := strings.Join(refs, ",")
	s.prunedCheckpointRefs.Store(&retained)
	return nil
}
func (*recordingStateStore) EnvironmentState(context.Context, string) (environment.State, error) {
	return environment.State{}, environment.ErrStateNotFound
}
func (s *recordingStateStore) SaveEnvironmentState(_ context.Context, state environment.State) error {
	s.environmentSaves.Add(1)
	s.environmentLast.Store(&state)
	return nil
}
func (s *recordingStateStore) DeleteEnvironmentState(context.Context, string, string) error {
	s.environmentDeletes.Add(1)
	return nil
}
func (*recordingStateStore) ExpiredEnvironmentStates(context.Context, string, time.Time, int) ([]environment.State, error) {
	return nil, nil
}

func TestRestoreWorkspaceRejectsUnsafeAndOversizedCheckpoints(t *testing.T) {
	tests := []struct {
		name   string
		header tar.Header
		body   string
		limit  int64
	}{
		{name: "parent traversal", header: tar.Header{Name: "../escape", Typeflag: tar.TypeReg, Mode: 0o600, Size: 1}, body: "x", limit: 1 << 20},
		{name: "absolute path", header: tar.Header{Name: "/escape", Typeflag: tar.TypeReg, Mode: 0o600, Size: 1}, body: "x", limit: 1 << 20},
		{name: "oversized contents", header: tar.Header{Name: "large", Typeflag: tar.TypeReg, Mode: 0o600, Size: 6}, body: "123456", limit: 5},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			workspace := filepath.Join(t.TempDir(), "workspace")
			if err := os.Mkdir(workspace, 0o755); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(workspace, "original")
			if err := os.WriteFile(marker, []byte("preserved"), 0o600); err != nil {
				t.Fatal(err)
			}
			archive := checkpointArchive(t, test.header, test.body)
			if err := restoreWorkspace(workspace, archive, test.limit); err == nil {
				t.Fatal("checkpoint unexpectedly restored")
			}
			if body, err := os.ReadFile(marker); err != nil || string(body) != "preserved" {
				t.Fatalf("original workspace = %q, %v", body, err)
			}
		})
	}
}

func checkpointArchive(t *testing.T, header tar.Header, body string) []byte {
	t.Helper()
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	if err := writer.WriteHeader(&header); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(writer, body); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}

func TestRestoreWorkspaceRejectsSymlinkTraversal(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	if err := os.Mkdir(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	if err := writer.WriteHeader(&tar.Header{Name: "escape", Typeflag: tar.TypeSymlink, Linkname: "../outside"}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := restoreWorkspace(workspace, archive.Bytes(), 1<<20); err == nil || !strings.Contains(err.Error(), "unsafe symlink") {
		t.Fatalf("error = %v", err)
	}
}
