package environment_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/samperrin/pons/environment"
	"github.com/samperrin/pons/runtime/checkpoint"
	runtimesqlite "github.com/samperrin/pons/runtime/sqlite"
)

func TestExpiredCheckpointsKeepsCurrentRecentDailyAndBase(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	day := func(daysAgo, hour int) time.Time {
		return time.Date(2026, 10, 4-daysAgo, hour, 0, 0, 0, time.UTC)
	}
	var history []environment.WorkspaceCheckpoint
	add := func(kind environment.CheckpointKind, at time.Time) int64 {
		seq := int64(len(history) + 1)
		history = append(history, environment.WorkspaceCheckpoint{Seq: seq, Kind: kind, CreatedAt: at})
		return seq
	}
	base := add(environment.CheckpointBase, day(30, 9))
	add(environment.CheckpointRun, day(20, 9)) // older than 14 days
	add(environment.CheckpointRun, day(13, 9))
	oldestDaily := add(environment.CheckpointRun, day(13, 18)) // newest of the 14th day back
	add(environment.CheckpointRun, day(5, 9))
	fifthDay := add(environment.CheckpointRestore, day(5, 10))
	var today []int64
	for hour := range 12 {
		today = append(today, add(environment.CheckpointRun, day(0, hour)))
	}

	var expired []int64
	for _, checkpoint := range environment.ExpiredCheckpoints(history, now) {
		expired = append(expired, checkpoint.Seq)
	}
	// Today's two oldest fall outside the 10 newest and are not their day's
	// newest; the 20-day-old run is outside every rule.
	want := []int64{2, 3, 5, today[0], today[1]}
	if !slices.Equal(expired, want) {
		t.Fatalf("expired = %v, want %v", expired, want)
	}
	for _, kept := range []int64{base, oldestDaily, fifthDay, today[len(today)-1]} {
		if slices.Contains(expired, kept) {
			t.Fatalf("checkpoint %d expired", kept)
		}
	}
	if expired := environment.ExpiredCheckpoints(history[:1], now); len(expired) != 0 {
		t.Fatalf("a lone base expired: %+v", expired)
	}
}

func TestRecordWorkspaceCheckpointPrunesRowsAndUnreferencedArchives(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := runtimesqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	archives := checkpoint.New(dir + "/workspaces")
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	if err := store.SaveWorkspaceState(ctx, environment.WorkspaceState{
		ID: "workspace", Strategy: environment.WorkspaceStrategyEmpty, SetupGeneration: 1, CreatedAt: start, UpdatedAt: start,
	}); err != nil {
		t.Fatal(err)
	}
	put := func(body string) (string, int64) {
		t.Helper()
		ref, size, err := archives.PutWorkspaceCheckpoint(ctx, "workspace", strings.NewReader(body), 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		return ref, size
	}
	record := func(ref string, size int64, kind environment.CheckpointKind, at time.Time) environment.WorkspaceCheckpoint {
		t.Helper()
		recorded, err := environment.RecordWorkspaceCheckpoint(ctx, store, archives, environment.WorkspaceCheckpoint{
			WorkspaceID: "workspace", Ref: ref, Kind: kind, SizeBytes: size, CreatedAt: at,
		}, func(err error) { t.Errorf("prune: %v", err) })
		if err != nil {
			t.Fatal(err)
		}
		return recorded
	}

	baseRef, baseSize := put("base")
	record(baseRef, baseSize, environment.CheckpointBase, start)
	// Two runs on an old day; a restore of the first shares its archive.
	sharedRef, sharedSize := put("shared")
	expiring := record(sharedRef, sharedSize, environment.CheckpointRun, start.Add(time.Hour))
	orphanRef, orphanSize := put("orphan")
	record(orphanRef, orphanSize, environment.CheckpointRun, start.Add(2*time.Hour))
	later := start.AddDate(0, 0, 30)
	record(sharedRef, sharedSize, environment.CheckpointRestore, later)
	var current environment.WorkspaceCheckpoint
	for i := range environment.RetainRecentCheckpoints - 1 {
		ref, size := put("run " + string(rune('a'+i)))
		current = record(ref, size, environment.CheckpointRun, later.Add(time.Duration(i+1)*time.Minute))
	}

	history, err := store.WorkspaceCheckpoints(ctx, "workspace")
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != environment.RetainRecentCheckpoints+1 || history[0].Kind != environment.CheckpointBase ||
		history[len(history)-1] != current || slices.ContainsFunc(history, func(c environment.WorkspaceCheckpoint) bool {
		return c.Seq == expiring.Seq
	}) {
		t.Fatalf("history = %+v", history)
	}
	for _, ref := range []string{baseRef, sharedRef} {
		reader, err := archives.WorkspaceCheckpoint(ctx, "workspace", ref, 1<<20)
		if err != nil {
			t.Fatalf("retained archive %s: %v", ref, err)
		}
		_ = reader.Close()
	}
	if reader, err := archives.WorkspaceCheckpoint(ctx, "workspace", orphanRef, 1<<20); !errors.Is(err, environment.ErrStateNotFound) {
		if reader != nil {
			_ = reader.Close()
		}
		t.Fatalf("unreferenced archive read = %v", err)
	}
}
