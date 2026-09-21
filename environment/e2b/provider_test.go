package e2b

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
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
)

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
		WorkspacePath: workspace,
		Command:       []string{"/usr/local/bin/pons-hands", "--bash-timeout", "7"},
		Environment: []string{
			"EXPLICIT=value",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	wantWorkspace, err := filepath.EvalSymlinks(workspace)
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
	if err := provider.SetStateStore(store); err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	_, err := provider.Start(context.Background(), environment.Spec{
		WorkspacePath: t.TempDir(), RunID: "run", Command: []string{"/usr/local/bin/pons-hands"},
	})
	if err == nil || !strings.Contains(err.Error(), "upload failed") {
		t.Fatalf("error = %v", err)
	}
	if store.saves.Load() != 0 {
		t.Fatalf("saved %d environment states before setup completed", store.saves.Load())
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

func TestWorkspaceArchiveRestoreReconcilesDeletions(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	if err := os.Mkdir(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "keep"), []byte("remote"), 0o755); err != nil {
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
}

type recordingStateStore struct{ saves atomic.Int32 }

func (*recordingStateStore) EnvironmentState(context.Context, string) (environment.State, error) {
	return environment.State{}, environment.ErrStateNotFound
}
func (s *recordingStateStore) SaveEnvironmentState(context.Context, environment.State) error {
	s.saves.Add(1)
	return nil
}
func (*recordingStateStore) DeleteEnvironmentState(context.Context, string, string) error {
	return nil
}
func (*recordingStateStore) ExpiredEnvironmentStates(context.Context, string, time.Time, int) ([]environment.State, error) {
	return nil, nil
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
