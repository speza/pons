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

func TestE2BHandsRoundTrip(t *testing.T) {
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
	if err := provider.SetStateStore(store); err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	spec := environment.Spec{
		Workspace: workspace,
		LeaseID:   "first-run",
		Command:   []string{"/usr/local/bin/pons-hands", "--bash-timeout", "30"},
		Network:   environment.NetworkDisabled,
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
	stateKey := session.Metadata().Workspace
	if err := provider.Close(); err != nil {
		t.Fatal(err)
	}
	// A new provider instance simulates a server restart: only SQLite and the
	// host-side API key carry over.
	provider = &e2b.Provider{IdleTimeout: time.Second, CleanupInterval: 100 * time.Millisecond}
	if err := provider.SetStateStore(store); err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	spec.LeaseID = "second-run"
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
	body, err := os.ReadFile(filepath.Join(workspace, "hello.txt"))
	if err != nil || string(body) != "Hello from E2B!\n" {
		t.Fatalf("checkpointed file = %q, %v", body, err)
	}
	t.Log(strings.TrimSpace(string(body)))
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		_, stateErr := store.EnvironmentState(ctx, stateKey)
		if errors.Is(stateErr, environment.ErrStateNotFound) {
			return
		}
		if stateErr != nil {
			t.Fatal(stateErr)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("idle sandbox was not cleaned up")
}
