//go:build integration

package e2b_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samperrin/pons/environment"
	"github.com/samperrin/pons/environment/e2b"
	"github.com/samperrin/pons/plugins/bash"
	"github.com/samperrin/pons/protocol"
	runtimesqlite "github.com/samperrin/pons/runtime/sqlite"
)

func TestE2BWorkspaceCheckpointRecovery(t *testing.T) {
	if os.Getenv("PONS_E2B_TEST") != "1" {
		t.Skip("set PONS_E2B_TEST=1 to run the live E2B test")
	}
	workspace := filepath.Join(t.TempDir(), "workspace")
	if err := os.Mkdir(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "seed.txt"), []byte("seed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	store, err := runtimesqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	provider := &e2b.Provider{IdleTimeout: time.Second, CleanupInterval: 100 * time.Millisecond}
	if err := provider.SetStores(store, store); err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	spec := environment.Spec{
		WorkspaceID:   "live-workspace",
		WorkspacePath: workspace,
		RunID:         "first-run",
		Command:       []string{"/usr/local/bin/pons-hands", "--bash-timeout", "30"},
		Network:       environment.NetworkDisabled,
		Limits: environment.ResourceLimits{
			CallTimeout: 45 * time.Second,
		},
	}
	session, err := provider.Start(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("E2B sandbox ID: %s", session.Metadata().EnvironmentID)
	args, err := json.Marshal(map[string]string{"command": `cat seed.txt && printf 'Hello from E2B!\n' > hello.txt`})
	if err != nil {
		t.Fatal(err)
	}
	result, err := session.Execute(ctx, protocol.Action{ID: "live", Kind: bash.KindBash, Args: args})
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK {
		t.Fatalf("tool result = %+v", result)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	firstID := session.Metadata().EnvironmentID
	stateKey := session.Metadata().WorkspaceID
	if err := provider.Close(); err != nil {
		t.Fatal(err)
	}
	// A new provider instance simulates a server restart: only SQLite and the
	// host-side API key carry over.
	provider = &e2b.Provider{IdleTimeout: time.Second, CleanupInterval: 100 * time.Millisecond}
	if err := provider.SetStores(store, store); err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	spec.RunID = "second-run"
	second, err := provider.Start(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if second.Metadata().EnvironmentID != firstID {
		t.Fatalf("sandbox was not reused: first %q, second %q", firstID, second.Metadata().EnvironmentID)
	}
	t.Logf("reused E2B sandbox ID: %s", firstID)
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "hello.txt")); !os.IsNotExist(err) {
		t.Fatalf("remote result was written back to source workspace: %v", err)
	}
	waitForEnvironmentDeletion(t, ctx, store, stateKey)
	workspaceState, err := store.WorkspaceState(ctx, stateKey)
	if err != nil || workspaceState.CheckpointRef == "" {
		t.Fatalf("durable workspace = %+v, %v", workspaceState, err)
	}
	third, err := provider.Start(ctx, environment.Spec{
		WorkspaceID:   spec.WorkspaceID,
		WorkspacePath: spec.WorkspacePath,
		RunID:         "third-run",
		Command:       spec.Command,
		Network:       spec.Network,
		Limits:        spec.Limits,
	})
	if err != nil {
		t.Fatal(err)
	}
	if third.Metadata().EnvironmentID == firstID {
		t.Fatal("deleted sandbox was unexpectedly reused")
	}
	args, err = json.Marshal(map[string]string{"command": "cat hello.txt"})
	if err != nil {
		t.Fatal(err)
	}
	result, err = third.Execute(ctx, protocol.Action{ID: "restored", Kind: bash.KindBash, Args: args})
	if err != nil || !result.OK || !strings.Contains(result.Output, "Hello from E2B!") {
		t.Fatalf("restored result = %+v, %v", result, err)
	}
	if err := third.Close(); err != nil {
		t.Fatal(err)
	}
	waitForEnvironmentDeletion(t, ctx, store, stateKey)
	t.Log(strings.TrimSpace(result.Output))
}

func waitForEnvironmentDeletion(t *testing.T, ctx context.Context, store *runtimesqlite.Store, workspaceID string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		_, err := store.EnvironmentState(ctx, workspaceID)
		if errors.Is(err, environment.ErrStateNotFound) {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("idle sandbox was not cleaned up")
}
