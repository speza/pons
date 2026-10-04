package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/samperrin/pons/environment"
)

// checkpointEnvironment is a durable provider that can restore and never
// runs hands.
type checkpointEnvironment struct {
	store       environment.StateStore
	checkpoints environment.CheckpointStore
	restored    []int64
}

func (p *checkpointEnvironment) Start(context.Context, environment.Spec) (environment.HandsSession, error) {
	return nil, errors.New("unexpected environment start")
}

func (p *checkpointEnvironment) SetStores(store environment.StateStore, checkpoints environment.CheckpointStore) error {
	p.store, p.checkpoints = store, checkpoints
	return nil
}

func (p *checkpointEnvironment) RestoreWorkspace(_ context.Context, _ string, checkpoint environment.WorkspaceCheckpoint) error {
	p.restored = append(p.restored, checkpoint.Seq)
	return nil
}

func startWorkspaceServer(t *testing.T, provider environment.Provider) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan string, 1)
	done := make(chan error, 1)
	stateDir, root := t.TempDir(), t.TempDir()
	go func() {
		done <- runServerReady(ctx, newServerLogger(io.Discard, false), serverOptions{
			Address: "127.0.0.1:0", StateDir: stateDir, WorkspaceRoot: root,
			MaxConcurrent: 1, Sandbox: "test", Environment: provider,
		}, started)
	}()
	var serverURL string
	select {
	case serverURL = <-started:
	case err := <-done:
		t.Fatalf("server stopped: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	return serverURL
}

func TestWorkspaceCommandsListAndRestoreAgentWorkspace(t *testing.T) {
	ctx := context.Background()
	provider := &checkpointEnvironment{}
	serverURL := startWorkspaceServer(t, provider)
	now := time.Now().UTC()
	if err := provider.store.SaveWorkspaceState(ctx, environment.WorkspaceState{
		ID: "agent-default", Strategy: environment.WorkspaceStrategyEmpty, SetupGeneration: 1, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	for _, checkpoint := range []environment.WorkspaceCheckpoint{
		{Kind: environment.CheckpointBase},
		{Kind: environment.CheckpointRun, RunID: "run-1"},
	} {
		ref, size, err := provider.checkpoints.PutWorkspaceCheckpoint(ctx, "agent-default", strings.NewReader(string(checkpoint.Kind)), 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		checkpoint.WorkspaceID, checkpoint.Ref, checkpoint.SizeBytes, checkpoint.CreatedAt = "agent-default", ref, size, now
		if _, err := environment.RecordWorkspaceCheckpoint(ctx, provider.store, provider.checkpoints, checkpoint, nil); err != nil {
			t.Fatal(err)
		}
	}
	workspace := func(args ...string) (string, error) {
		var stdout bytes.Buffer
		err := runWorkspace(ctx, append(args, "-server", serverURL), &stdout, io.Discard)
		return stdout.String(), err
	}

	listed, err := workspace("checkpoints")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(listed), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[1], "2 ") || !strings.Contains(lines[1], "run run-1, current") ||
		!strings.HasPrefix(lines[2], "1 ") || !strings.Contains(lines[2], "base") {
		t.Fatalf("checkpoints:\n%s", listed)
	}

	// Flags precede the SEQ argument.
	var stdout bytes.Buffer
	if err := runWorkspace(ctx, []string{"restore", "-server", serverURL, "99"}, &stdout, io.Discard); err == nil ||
		!strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("restore of an unknown checkpoint = %v", err)
	}
	if err := runWorkspace(ctx, []string{"restore", "-server", serverURL, "-workspace-id", "agent-default", "1"}, &stdout, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := stdout.String(); got != "restored checkpoint 1 of agent-default as checkpoint 3\n" || len(provider.restored) != 1 {
		t.Fatalf("restore output = %q, provider restored %v", got, provider.restored)
	}
	if listed, err := workspace("checkpoints"); err != nil || !strings.Contains(listed, "restored from 1, current") {
		t.Fatalf("checkpoints after restore:\n%s%v", listed, err)
	}
	if _, err := workspace("checkpoints", "-workspace-id", "missing"); err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("checkpoints of a missing workspace = %v", err)
	}
}

func TestWorkspaceRestoreIsUnsupportedWithoutProviderRestore(t *testing.T) {
	serverURL := startWorkspaceServer(t, pipeHands{})
	err := runWorkspace(context.Background(), []string{"restore", "-server", serverURL, "1"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "HTTP 501") {
		t.Fatalf("restore = %v", err)
	}
}

func TestWorkspaceCommandRequiresSubcommandAndSeq(t *testing.T) {
	for _, args := range [][]string{nil, {"list"}, {"restore"}, {"restore", "x"}, {"checkpoints", "extra"}} {
		if err := runWorkspace(context.Background(), args, io.Discard, io.Discard); err == nil {
			t.Fatalf("runWorkspace(%q) succeeded", args)
		}
	}
}
