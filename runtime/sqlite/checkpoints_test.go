package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/samperrin/pons/environment"
	ponsruntime "github.com/samperrin/pons/runtime"
)

func TestWorkspaceCheckpointHistory(t *testing.T) {
	ctx := context.Background()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := store.AppendWorkspaceCheckpoint(ctx, environment.WorkspaceCheckpoint{
		WorkspaceID: "workspace", Ref: "sha256:base", Kind: environment.CheckpointBase, CreatedAt: now,
	}); err == nil {
		t.Fatal("checkpoint accepted for an unrecorded workspace")
	}
	if err := store.SaveWorkspaceState(ctx, environment.WorkspaceState{
		ID: "workspace", Strategy: environment.WorkspaceStrategyEmpty, SetupGeneration: 1, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CurrentWorkspaceCheckpoint(ctx, "workspace"); !errors.Is(err, environment.ErrStateNotFound) {
		t.Fatalf("current before first checkpoint = %v", err)
	}

	appended := []environment.WorkspaceCheckpoint{
		{WorkspaceID: "workspace", Ref: "sha256:base", Kind: environment.CheckpointBase, SizeBytes: 1024, CreatedAt: now},
		{WorkspaceID: "workspace", Ref: "sha256:one", Kind: environment.CheckpointRun, RunID: "run-1", SizeBytes: 2048, CreatedAt: now},
		{WorkspaceID: "workspace", Ref: "sha256:base", Kind: environment.CheckpointRestore, RestoredFrom: 1, SizeBytes: 1024, CreatedAt: now},
	}
	for i, checkpoint := range appended {
		recorded, err := store.AppendWorkspaceCheckpoint(ctx, checkpoint)
		if err != nil {
			t.Fatal(err)
		}
		if recorded.Seq != int64(i+1) {
			t.Fatalf("seq = %d, want %d", recorded.Seq, i+1)
		}
		appended[i] = recorded
	}
	if _, err := store.AppendWorkspaceCheckpoint(ctx, environment.WorkspaceCheckpoint{
		WorkspaceID: "workspace", Ref: "sha256:x", Kind: "other", CreatedAt: now,
	}); err == nil {
		t.Fatal("unknown checkpoint kind accepted")
	}

	history, err := store.WorkspaceCheckpoints(ctx, "workspace")
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 3 {
		t.Fatalf("history = %+v", history)
	}
	for i, checkpoint := range history {
		if checkpoint != appended[i] {
			t.Fatalf("history[%d] = %+v, want %+v", i, checkpoint, appended[i])
		}
	}
	current, err := store.CurrentWorkspaceCheckpoint(ctx, "workspace")
	if err != nil || current != appended[2] {
		t.Fatalf("current = %+v, %v", current, err)
	}

	// The current checkpoint survives a request to delete it.
	if err := store.DeleteWorkspaceCheckpoints(ctx, "workspace", []int64{2, 3}); err != nil {
		t.Fatal(err)
	}
	history, err = store.WorkspaceCheckpoints(ctx, "workspace")
	if err != nil || len(history) != 2 || history[0].Seq != 1 || history[1].Seq != 3 {
		t.Fatalf("history after delete = %+v, %v", history, err)
	}
	next, err := store.AppendWorkspaceCheckpoint(ctx, environment.WorkspaceCheckpoint{
		WorkspaceID: "workspace", Ref: "sha256:two", Kind: environment.CheckpointRun, RunID: "run-2", CreatedAt: now,
	})
	if err != nil || next.Seq != 4 {
		t.Fatalf("seq after delete = %+v, %v", next, err)
	}
}

func TestWorkspaceReservationExcludesClaimsUntilReleasedOrRestarted(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	conversation := ponsruntime.Conversation{
		ID: ponsruntime.NewID(), AgentID: testAgent.ID, WorkspaceID: "agent-default", AgentWorkspace: true,
		WorkspaceLock: "agent-default", Environment: "e2b", CreatedAt: time.Now().UTC(),
	}
	if err := store.CreateConversation(ctx, conversation); err != nil {
		t.Fatal(err)
	}
	if _, _, err := acceptInput(store, ctx, conversation.ID, "first", []ponsruntime.TextPart{{Type: "text", Text: "go"}}); err != nil {
		t.Fatal(err)
	}

	if err := store.ReserveWorkspace(ctx, "agent-default"); err != nil {
		t.Fatal(err)
	}
	if err := store.ReserveWorkspace(ctx, "agent-default"); !errors.Is(err, ponsruntime.ErrWorkspaceBusy) {
		t.Fatalf("second reservation = %v", err)
	}
	if claim, err := store.ClaimRunnable(ctx); err != nil || claim != nil {
		t.Fatalf("claim during reservation = %+v, %v", claim, err)
	}
	if err := store.ReleaseWorkspace(ctx, "agent-default"); err != nil {
		t.Fatal(err)
	}
	claim, err := store.ClaimRunnable(ctx)
	if err != nil || claim == nil {
		t.Fatalf("claim after release = %+v, %v", claim, err)
	}
	if err := store.ReserveWorkspace(ctx, "agent-default"); !errors.Is(err, ponsruntime.ErrWorkspaceBusy) {
		t.Fatalf("reservation during a run = %v", err)
	}
	if _, err := store.FinishRun(ctx, claim.Run, "done"); err != nil {
		t.Fatal(err)
	}

	// A reservation left by a crash is cleared when the next Manager starts.
	if err := store.ReserveWorkspace(ctx, "agent-default"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, _, err := acceptInput(store, ctx, conversation.ID, "second", []ponsruntime.TextPart{{Type: "text", Text: "again"}}); err != nil {
		t.Fatal(err)
	}
	if claim, err := store.ClaimRunnable(ctx); err != nil || claim != nil {
		t.Fatalf("claim before recovery = %+v, %v", claim, err)
	}
	if err := store.RecoverRunning(ctx); err != nil {
		t.Fatal(err)
	}
	if claim, err := store.ClaimRunnable(ctx); err != nil || claim == nil {
		t.Fatalf("claim after recovery = %+v, %v", claim, err)
	}
}
