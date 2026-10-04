package runtime_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samperrin/pons/environment"
	ponsruntime "github.com/samperrin/pons/runtime"
	"github.com/samperrin/pons/runtime/checkpoint"
	runtimesqlite "github.com/samperrin/pons/runtime/sqlite"
)

// gatedRestorer reports each restore and holds it until released.
type gatedRestorer struct {
	entered chan struct{}
	release chan struct{}
}

func (r gatedRestorer) RestoreWorkspace(context.Context, string, environment.WorkspaceCheckpoint) error {
	r.entered <- struct{}{}
	<-r.release
	return nil
}

func TestRestoreExcludesRunsOfTheWorkspace(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := runtimesqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	archives := checkpoint.New(filepath.Join(dir, "workspaces"))
	agent := testAgent
	agent.WorkspacePolicy = ponsruntime.WorkspaceAgent
	workspaceID := ponsruntime.AgentWorkspaceID(agent.ID)
	now := time.Now().UTC()
	if err := store.SaveWorkspaceState(ctx, environment.WorkspaceState{
		ID: workspaceID, Strategy: environment.WorkspaceStrategyEmpty, SetupGeneration: 1, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	var refs []string
	for _, body := range []string{"base", "latest"} {
		ref, size, err := archives.PutWorkspaceCheckpoint(ctx, workspaceID, strings.NewReader(body), 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		kind := environment.CheckpointRun
		if body == "base" {
			kind = environment.CheckpointBase
		}
		if _, err := environment.RecordWorkspaceCheckpoint(ctx, store, archives, environment.WorkspaceCheckpoint{
			WorkspaceID: workspaceID, Ref: ref, Kind: kind, SizeBytes: size, CreatedAt: now,
		}, nil); err != nil {
			t.Fatal(err)
		}
		refs = append(refs, ref)
	}

	restorer := gatedRestorer{entered: make(chan struct{}, 1), release: make(chan struct{})}
	history := &checkpoint.History{State: store, Archives: archives, Restorer: restorer}
	// Each run reports the checkpoint its workspace would be placed from.
	placed := make(chan string, 2)
	finish := make(chan struct{}, 2)
	manager, err := New(Config{
		Agent: agent, AgentRevisions: agentRevisions{agent.Revision(): agent}, Store: store, MaxConcurrent: 2,
		WorkspaceHistory: history, WorkspaceRestorer: history,
		Runner: RunnerFunc(func(ctx context.Context, request RunRequest) (RunResult, error) {
			current, err := store.CurrentWorkspaceCheckpoint(ctx, request.WorkspaceID)
			if err != nil {
				return RunResult{}, err
			}
			placed <- current.Ref
			<-finish
			return RunResult{Answer: "done"}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	conversation, err := manager.CreateConversation(ctx, ponsruntime.ConversationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Submit(ctx, conversation.ID, "first", []TextPart{{Type: "text", Text: "go"}}); err != nil {
		t.Fatal(err)
	}
	if ref := receive(t, placed, "first run"); ref != refs[1] {
		t.Fatalf("first run placed from %q, want the latest checkpoint", ref)
	}

	if _, err := manager.RestoreWorkspace(ctx, workspaceID, 1); !errors.Is(err, ponsruntime.ErrWorkspaceBusy) {
		t.Fatalf("restore during a run = %v", err)
	}
	finish <- struct{}{}

	type result struct {
		checkpoint ponsruntime.WorkspaceCheckpoint
		err        error
	}
	restored := make(chan result, 1)
	go func() {
		// The first run's terminal state lands just after the runner returns.
		for {
			checkpoint, err := manager.RestoreWorkspace(ctx, workspaceID, 1)
			if !errors.Is(err, ponsruntime.ErrWorkspaceBusy) {
				restored <- result{checkpoint, err}
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	receive(t, restorer.entered, "restore")
	if _, err := manager.Submit(ctx, conversation.ID, "second", []TextPart{{Type: "text", Text: "again"}}); err != nil {
		t.Fatal(err)
	}
	select {
	case ref := <-placed:
		t.Fatalf("run placed from %q during the restore", ref)
	case <-time.After(100 * time.Millisecond):
	}
	close(restorer.release)
	outcome := receive(t, restored, "restore result")
	if outcome.err != nil || outcome.checkpoint.Seq != 3 || outcome.checkpoint.RestoredFrom != 1 {
		t.Fatalf("restore = %+v, %v", outcome.checkpoint, outcome.err)
	}
	if ref := receive(t, placed, "queued run"); ref != refs[0] {
		t.Fatalf("queued run placed from %q, want the restored base", ref)
	}
	finish <- struct{}{}

	listed, err := manager.WorkspaceCheckpoints(ctx, workspaceID)
	if err != nil || len(listed.Checkpoints) != 3 || listed.Checkpoints[0].Kind != "restore" {
		t.Fatalf("listed = %+v, %v", listed, err)
	}
}

func TestRestoreWithoutProviderSupportIsUnsupported(t *testing.T) {
	manager := testManager(t, RunnerFunc(func(context.Context, RunRequest) (RunResult, error) {
		return RunResult{}, nil
	}))
	if _, err := manager.RestoreWorkspace(context.Background(), "agent-default", 1); !errors.Is(err, ponsruntime.ErrRestoreUnsupported) {
		t.Fatalf("restore = %v", err)
	}
}

func receive[T any](t *testing.T, values <-chan T, what string) T {
	t.Helper()
	select {
	case value := <-values:
		return value
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		panic("unreachable")
	}
}
