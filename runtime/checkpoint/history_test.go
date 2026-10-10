package checkpoint

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samperrin/pons/environment"
	ponsruntime "github.com/samperrin/pons/runtime"
	runtimesqlite "github.com/samperrin/pons/runtime/sqlite"
)

type recordingRestorer struct {
	restored []environment.WorkspaceCheckpoint
}

func (r *recordingRestorer) RestoreWorkspace(_ context.Context, _ string, checkpoint environment.WorkspaceCheckpoint) error {
	r.restored = append(r.restored, checkpoint)
	return nil
}

// historyFixture records a base and two runs for workspace "workspace".
func historyFixture(t *testing.T) (*History, *runtimesqlite.Store, *recordingRestorer, []environment.WorkspaceCheckpoint) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	store, err := runtimesqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	if err := store.SaveWorkspaceState(ctx, environment.WorkspaceState{
		ID: "workspace", Strategy: environment.WorkspaceStrategyEmpty, SetupGeneration: 1, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	archives := New(filepath.Join(dir, "workspaces"))
	restorer := &recordingRestorer{}
	history := &History{
		State: store, Archives: archives, Restorer: restorer,
		OnError: func(err error) { t.Errorf("history: %v", err) },
		Now:     func() time.Time { return now },
	}
	var recorded []environment.WorkspaceCheckpoint
	for i, kind := range []environment.CheckpointKind{environment.CheckpointBase, environment.CheckpointRun, environment.CheckpointRun} {
		ref, size, err := archives.PutWorkspaceCheckpoint(ctx, "workspace", strings.NewReader(strings.Repeat("x", i+1)), 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		checkpoint := environment.WorkspaceCheckpoint{
			WorkspaceID: "workspace", Ref: ref, Kind: kind, SizeBytes: size, CreatedAt: now.Add(time.Duration(i) * time.Minute),
		}
		if kind == environment.CheckpointRun {
			checkpoint.RunID = "run-" + string(rune('0'+i))
		}
		checkpoint, err = environment.RecordWorkspaceCheckpoint(ctx, store, archives, checkpoint, nil)
		if err != nil {
			t.Fatal(err)
		}
		recorded = append(recorded, checkpoint)
	}
	return history, store, restorer, recorded
}

func TestHistoryListsNewestFirstAndRestoresByAppending(t *testing.T) {
	ctx := context.Background()
	history, store, restorer, recorded := historyFixture(t)

	listed, err := history.WorkspaceCheckpoints(ctx, "workspace")
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 3 || listed[0].Seq != 3 || !listed[0].Current || listed[0].RunID != "run-2" ||
		listed[1].Current || listed[2].Kind != "base" || listed[2].SizeBytes != 1 {
		t.Fatalf("listed = %+v", listed)
	}
	if _, err := history.WorkspaceCheckpoints(ctx, "missing"); !errors.Is(err, ponsruntime.ErrWorkspaceNotFound) {
		t.Fatalf("missing workspace = %v", err)
	}

	restored, err := history.RestoreWorkspace(ctx, "workspace", 2)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Seq != 4 || restored.Kind != "restore" || restored.RestoredFrom != 2 || !restored.Current ||
		restored.SizeBytes != recorded[1].SizeBytes {
		t.Fatalf("restored = %+v", restored)
	}
	if len(restorer.restored) != 1 || restorer.restored[0] != recorded[1] {
		t.Fatalf("provider restored %+v, want %+v", restorer.restored, recorded[1])
	}
	after, err := store.WorkspaceCheckpoints(ctx, "workspace")
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 4 || after[3].Ref != recorded[1].Ref {
		t.Fatalf("history after restore = %+v", after)
	}
	for i := range recorded {
		if after[i] != recorded[i] {
			t.Fatalf("restore rewrote checkpoint %d: %+v, want %+v", i+1, after[i], recorded[i])
		}
	}

	if _, err := history.RestoreWorkspace(ctx, "workspace", 99); !errors.Is(err, ponsruntime.ErrCheckpointNotFound) {
		t.Fatalf("unknown seq = %v", err)
	}
}

// cancelingRestorer cancels the request once the restore is applied, as a
// client disconnecting at that moment would.
type cancelingRestorer struct{ cancel context.CancelFunc }

func (r cancelingRestorer) RestoreWorkspace(context.Context, string, environment.WorkspaceCheckpoint) error {
	r.cancel()
	return nil
}

func TestHistoryRecordsAppliedRestoreAfterCancellation(t *testing.T) {
	history, store, _, _ := historyFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	history.Restorer = cancelingRestorer{cancel: cancel}
	restored, err := history.RestoreWorkspace(ctx, "workspace", 1)
	if err != nil || restored.Seq != 4 {
		t.Fatalf("restore = %+v, %v", restored, err)
	}
	current, err := store.CurrentWorkspaceCheckpoint(context.Background(), "workspace")
	if err != nil || current.Kind != environment.CheckpointRestore || current.RestoredFrom != 1 {
		t.Fatalf("current = %+v, %v", current, err)
	}
}
