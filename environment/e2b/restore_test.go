package e2b

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samperrin/pons/environment"
	checkpointstore "github.com/samperrin/pons/runtime/checkpoint"
	runtimesqlite "github.com/samperrin/pons/runtime/sqlite"
)

func TestRestoredCheckpointSeedsNextSandbox(t *testing.T) {
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
	history := &checkpointstore.History{State: store, Archives: checkpoints, Restorer: provider}

	start := func(runID string) (environment.HandsSession, string) {
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
	for _, run := range []struct{ id, notes string }{{"run-1", "good"}, {"run-2", "mangled"}} {
		session, dir := start(run.id)
		if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte(run.notes), 0o600); err != nil {
			t.Fatal(err)
		}
		if run.id == "run-2" {
			// A live session holds the workspace.
			if err := provider.RestoreWorkspace(ctx, "agent-default", environment.WorkspaceCheckpoint{}); !errors.Is(err, environment.ErrWorkspaceInUse) {
				t.Fatalf("restore during a session = %v", err)
			}
		}
		if err := session.Close(); err != nil {
			t.Fatal(err)
		}
	}

	recorded, err := store.WorkspaceCheckpoints(ctx, "agent-default")
	if err != nil {
		t.Fatal(err)
	}
	if len(recorded) != 3 || recorded[0].Kind != environment.CheckpointBase ||
		recorded[1].Kind != environment.CheckpointRun || recorded[1].RunID != "run-1" ||
		recorded[2].RunID != "run-2" || recorded[1].SizeBytes == 0 || recorded[1].Ref == recorded[2].Ref {
		t.Fatalf("history = %+v", recorded)
	}
	idle, err := store.EnvironmentState(ctx, "agent-default")
	if err != nil || idle.Status != environment.StateIdle {
		t.Fatalf("environment before restore = %+v, %v", idle, err)
	}

	oversized := recorded[1]
	oversized.SizeBytes = defaultE2BWorkspaceBytes + 1
	if err := provider.RestoreWorkspace(ctx, "agent-default", oversized); err == nil ||
		!strings.Contains(err.Error(), "workspace limit") {
		t.Fatalf("restore of an oversized checkpoint = %v", err)
	}

	restored, err := history.RestoreWorkspace(ctx, "agent-default", 2)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Seq != 4 || restored.RestoredFrom != 2 {
		t.Fatalf("restored = %+v", restored)
	}
	if _, err := store.EnvironmentState(ctx, "agent-default"); !errors.Is(err, environment.ErrStateNotFound) {
		t.Fatalf("idle sandbox survived restore: %v", err)
	}

	session, dir := start("run-3")
	defer session.Close()
	if session.Metadata().EnvironmentID == idle.EnvironmentID {
		t.Fatal("run after restore reused the idle sandbox")
	}
	if data, err := os.ReadFile(filepath.Join(dir, "notes.txt")); err != nil || string(data) != "good" {
		t.Fatalf("restored notes = %q, %v", data, err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
}

// restoreFixture opens durable stores and an E2B provider whose API refuses
// every call, then records a base checkpoint for "workspace".
func restoreFixture(t *testing.T) (*Provider, *runtimesqlite.Store, string, environment.WorkspaceCheckpoint) {
	t.Helper()
	ctx := context.Background()
	stateDir := t.TempDir()
	store, err := runtimesqlite.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected E2B request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	provider := &Provider{APIKey: "key", APIURL: server.URL, EnvdURL: server.URL, HTTPClient: server.Client(), CleanupInterval: time.Hour}
	archiveDir := filepath.Join(stateDir, "workspaces")
	checkpoints := checkpointstore.New(archiveDir)
	if err := provider.SetStores(store, checkpoints); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = provider.Close() })
	_, base, err := loadOrCreateWorkspace(ctx, store, checkpoints, "workspace", "",
		environment.WorkspacePlan{Strategy: environment.WorkspaceStrategyEmpty}, 1<<20, nil)
	if err != nil {
		t.Fatal(err)
	}
	return provider, store, archiveDir, base
}

func TestRestoreRefusesRecoverySandboxBeforeReadingArchive(t *testing.T) {
	ctx := context.Background()
	provider, store, _, base := restoreFixture(t)
	now := time.Now().UTC()
	if err := store.SaveEnvironmentState(ctx, environment.State{
		WorkspaceID: "workspace", Provider: "e2b", EnvironmentID: "sandbox", Template: defaultE2BTemplate,
		Network: environment.NetworkDisabled, Status: environment.StateRecovery,
		ExpiresAt: now.Add(time.Hour), UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	missing := base
	missing.Ref = "sha256:" + strings.Repeat("0", 64)
	if err := provider.RestoreWorkspace(ctx, "workspace", missing); !errors.Is(err, environment.ErrWorkspaceInUse) ||
		!strings.Contains(err.Error(), "recovery") {
		t.Fatalf("restore with a recovery sandbox = %v", err)
	}
}

func TestRestoreOfCorruptArchiveKeepsSandboxAndHistory(t *testing.T) {
	ctx := context.Background()
	provider, store, archiveDir, base := restoreFixture(t)
	now := time.Now().UTC()
	idle := environment.State{
		WorkspaceID: "workspace", Provider: "e2b", EnvironmentID: "sandbox", Template: defaultE2BTemplate,
		Network: environment.NetworkDisabled, Status: environment.StateIdle,
		IdleUntil: now.Add(time.Hour), ExpiresAt: now.Add(time.Hour), UpdatedAt: now,
	}
	if err := store.SaveEnvironmentState(ctx, idle); err != nil {
		t.Fatal(err)
	}
	workspaceDir := sha256.Sum256([]byte("workspace"))
	path := filepath.Join(archiveDir, hex.EncodeToString(workspaceDir[:]), strings.TrimPrefix(base.Ref, "sha256:")+".tar")
	// An empty tar is all zero bytes, so corrupt it with other bytes.
	if err := os.WriteFile(path, bytes.Repeat([]byte{0xff}, int(base.SizeBytes)), 0o600); err != nil {
		t.Fatal(err)
	}

	history := &checkpointstore.History{State: store, Archives: provider.checkpointStore, Restorer: provider}
	if _, err := history.RestoreWorkspace(ctx, "workspace", base.Seq); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("restore of a corrupt archive = %v", err)
	}
	if current, err := store.CurrentWorkspaceCheckpoint(ctx, "workspace"); err != nil || current != base {
		t.Fatalf("current = %+v, %v", current, err)
	}
	if state, err := store.EnvironmentState(ctx, "workspace"); err != nil || state.EnvironmentID != "sandbox" {
		t.Fatalf("idle sandbox = %+v, %v; want it kept", state, err)
	}
}
