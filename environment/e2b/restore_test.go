package e2b

import (
	"context"
	"errors"
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
