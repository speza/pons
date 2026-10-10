package environment_test

import (
	"context"
	"errors"
	"fmt"
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

func TestExpiredCheckpointsFitsTheByteBudget(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	const mib = 1 << 20
	history := []environment.WorkspaceCheckpoint{
		{Seq: 1, Ref: "base", Kind: environment.CheckpointBase, SizeBytes: 300 * mib, CreatedAt: now},
	}
	// Ten distinct 200 MiB runs today; the newest is current.
	for seq := int64(2); seq <= 11; seq++ {
		history = append(history, environment.WorkspaceCheckpoint{
			Seq: seq, Ref: fmt.Sprintf("run-%d", seq), Kind: environment.CheckpointRun, SizeBytes: 200 * mib, CreatedAt: now,
		})
	}
	// A restore of seq 9 shares its archive.
	history = append(history, environment.WorkspaceCheckpoint{
		Seq: 12, Ref: "run-9", Kind: environment.CheckpointRestore, RestoredFrom: 9, SizeBytes: 200 * mib, CreatedAt: now,
	})

	var expired []int64
	for _, checkpoint := range environment.ExpiredCheckpoints(history, now) {
		expired = append(expired, checkpoint.Seq)
	}
	// The 1 GiB budget (over 4 x 200 MiB) holds the base and three distinct
	// run archives: run-9 (current, through the restore), run-10 and run-11.
	// Seq 9 survives as the restore's source at no extra cost.
	if want := []int64{2, 3, 4, 5, 6, 7, 8}; !slices.Equal(expired, want) {
		t.Fatalf("expired = %v, want %v", expired, want)
	}

	// The current checkpoint and the base are kept even over budget.
	huge := []environment.WorkspaceCheckpoint{
		{Seq: 1, Ref: "base", Kind: environment.CheckpointBase, SizeBytes: 2 << 30, CreatedAt: now},
		{Seq: 2, Ref: "old", Kind: environment.CheckpointRun, SizeBytes: 3 << 30, CreatedAt: now},
		{Seq: 3, Ref: "current", Kind: environment.CheckpointRun, SizeBytes: 3 << 30, CreatedAt: now},
	}
	// The budget scales to four copies of the 3 GiB current workspace.
	if expired := environment.ExpiredCheckpoints(huge, now); len(expired) != 0 {
		t.Fatalf("expired within the scaled budget = %+v", expired)
	}
	huge[0].SizeBytes = 7 << 30
	if expired := environment.ExpiredCheckpoints(huge, now); len(expired) != 1 || expired[0].Seq != 2 {
		t.Fatalf("expired over the scaled budget = %+v", expired)
	}
}

func TestByteBudgetDropsOnlyEntriesThatFreeBytes(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	const mib = 1 << 20
	history := []environment.WorkspaceCheckpoint{
		{Seq: 1, Ref: "base", Kind: environment.CheckpointBase, SizeBytes: 100 * mib, CreatedAt: now},
		{Seq: 2, Ref: "x", Kind: environment.CheckpointRun, SizeBytes: 300 * mib, CreatedAt: now},
		{Seq: 3, Ref: "y", Kind: environment.CheckpointRun, SizeBytes: 500 * mib, CreatedAt: now},
		// Reverts to seq 2's content, so x was used more recently than y.
		{Seq: 4, Ref: "x", Kind: environment.CheckpointRun, SizeBytes: 300 * mib, CreatedAt: now},
		{Seq: 5, Ref: "z", Kind: environment.CheckpointRun, SizeBytes: 200 * mib, CreatedAt: now},
	}
	var expired []int64
	for _, checkpoint := range environment.ExpiredCheckpoints(history, now) {
		expired = append(expired, checkpoint.Seq)
	}
	// 1100 MiB is over 1 GiB; dropping y alone fits, and seq 2 frees nothing.
	if !slices.Equal(expired, []int64{3}) {
		t.Fatalf("expired = %v, want [3]", expired)
	}
}

func TestExpiredCheckpointsKeepsSourcesOfKeptRestores(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	old := now.AddDate(0, 0, -30)
	history := []environment.WorkspaceCheckpoint{
		{Seq: 1, Kind: environment.CheckpointBase, CreatedAt: old},
		{Seq: 2, Kind: environment.CheckpointRun, CreatedAt: old},
		{Seq: 3, Kind: environment.CheckpointRun, CreatedAt: old},
		// Seq 4 restores 2 and is itself restored by the kept seq 5.
		{Seq: 4, Kind: environment.CheckpointRestore, RestoredFrom: 2, CreatedAt: old},
		{Seq: 5, Kind: environment.CheckpointRestore, RestoredFrom: 4, CreatedAt: now},
	}
	for seq := int64(6); seq < 6+environment.RetainRecentCheckpoints-1; seq++ {
		history = append(history, environment.WorkspaceCheckpoint{Seq: seq, Kind: environment.CheckpointRun, CreatedAt: now})
	}

	var expired []int64
	for _, checkpoint := range environment.ExpiredCheckpoints(history, now) {
		expired = append(expired, checkpoint.Seq)
	}
	if !slices.Equal(expired, []int64{3}) {
		t.Fatalf("expired = %v, want [3]", expired)
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
	if state, err := store.WorkspaceState(ctx, "workspace"); err != nil || !state.UpdatedAt.Equal(current.CreatedAt) {
		t.Fatalf("workspace updated at %v, want the latest checkpoint's %v (%v)", state.UpdatedAt, current.CreatedAt, err)
	}
}

func TestRecordWorkspaceCheckpointSkipsUnchangedRun(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := runtimesqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	archives := checkpoint.New(dir + "/workspaces")
	now := time.Now().UTC()
	if err := store.SaveWorkspaceState(ctx, environment.WorkspaceState{
		ID: "workspace", Strategy: environment.WorkspaceStrategyEmpty, SetupGeneration: 1, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	ref, size, err := archives.PutWorkspaceCheckpoint(ctx, "workspace", strings.NewReader("same"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	record := func(kind environment.CheckpointKind, runID string) environment.WorkspaceCheckpoint {
		t.Helper()
		recorded, err := environment.RecordWorkspaceCheckpoint(ctx, store, archives, environment.WorkspaceCheckpoint{
			WorkspaceID: "workspace", Ref: ref, Kind: kind, RunID: runID, SizeBytes: size, CreatedAt: now,
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return recorded
	}
	base := record(environment.CheckpointBase, "")
	// Runs that leave the workspace as it was add no entry.
	for range environment.RetainRecentCheckpoints + 1 {
		if got := record(environment.CheckpointRun, "no-op"); got != base {
			t.Fatalf("unchanged run recorded %+v, want current %+v", got, base)
		}
	}
	if restored := record(environment.CheckpointRestore, ""); restored.Seq != 2 {
		t.Fatalf("restore of the current archive = %+v; restores are always recorded", restored)
	}
	history, err := store.WorkspaceCheckpoints(ctx, "workspace")
	if err != nil || len(history) != 2 {
		t.Fatalf("history = %+v, %v", history, err)
	}
}
