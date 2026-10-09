package environment

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"
)

// CheckpointKind records why a workspace checkpoint was taken.
type CheckpointKind string

const (
	// CheckpointBase is the first archive of a workspace: the seed of an
	// archive/v1 or empty workspace, or a git/v1 workspace's initial checkout.
	CheckpointBase CheckpointKind = "base"
	// CheckpointRun is the workspace after a completed run that changed it.
	CheckpointRun CheckpointKind = "run"
	// CheckpointRestore makes an earlier checkpoint's archive current again.
	CheckpointRestore CheckpointKind = "restore"
)

// Fixed checkpoint retention. The current checkpoint and the base are always
// kept as well. The archives a workspace keeps are also bounded to
// RetainCheckpointBytes, or RetainCheckpointCopies times the current
// checkpoint's size if that is larger.
const (
	RetainRecentCheckpoints = 10
	RetainDailyCheckpoints  = 14
	RetainCheckpointBytes   = 1 << 30
	RetainCheckpointCopies  = 4
)

// WorkspaceCheckpoint is one entry in a workspace's checkpoint history. Seq
// increases by one per workspace; the highest Seq is the current checkpoint.
// Ref is a CheckpointStore reference, and several entries may share one.
// RestoredFrom is the Seq a restore entry restored, and zero otherwise.
type WorkspaceCheckpoint struct {
	WorkspaceID  string
	Seq          int64
	Ref          string
	Kind         CheckpointKind
	RunID        string
	RestoredFrom int64
	SizeBytes    int64
	CreatedAt    time.Time
}

// ErrWorkspaceInUse reports a restore refused because a run or a retained
// active or recovery environment holds the workspace.
var ErrWorkspaceInUse = errors.New("environment: workspace is in use")

// WorkspaceRestorer is implemented by providers that can make an earlier
// checkpoint the content of a workspace's next placement. The caller holds
// the workspace exclusively and records the restore after it returns. A
// workspace a provider cannot release fails with ErrWorkspaceInUse.
type WorkspaceRestorer interface {
	RestoreWorkspace(ctx context.Context, workspaceID string, checkpoint WorkspaceCheckpoint) error
}

// ExpiredCheckpoints returns the entries of an ascending history that fixed
// retention no longer keeps: everything except the current checkpoint, the
// RetainRecentCheckpoints newest, the newest of each UTC day for the last
// RetainDailyCheckpoints days counting today, and the base. If the archives
// those entries keep exceed the byte budget, the oldest entries other than the
// current checkpoint and the base go first until they fit. The source of a
// kept restore entry is kept too, so its RestoredFrom always resolves; it
// shares the restore's archive, so it costs no bytes.
func ExpiredCheckpoints(history []WorkspaceCheckpoint, now time.Time) []WorkspaceCheckpoint {
	keep := make(map[int64]bool, len(history))
	for i := max(0, len(history)-RetainRecentCheckpoints); i < len(history); i++ {
		keep[history[i].Seq] = true
	}
	today := now.UTC().Truncate(24 * time.Hour)
	oldestDay := today.AddDate(0, 0, -(RetainDailyCheckpoints - 1))
	newestOfDay := make(map[time.Time]WorkspaceCheckpoint)
	for _, checkpoint := range history {
		if checkpoint.Kind == CheckpointBase {
			keep[checkpoint.Seq] = true
		}
		day := checkpoint.CreatedAt.UTC().Truncate(24 * time.Hour)
		if day.Before(oldestDay) || day.After(today) {
			continue
		}
		if newest, ok := newestOfDay[day]; !ok || checkpoint.Seq > newest.Seq {
			newestOfDay[day] = checkpoint
		}
	}
	for _, checkpoint := range newestOfDay {
		keep[checkpoint.Seq] = true
	}
	applyByteBudget(history, keep)
	// A source precedes its restore, so walking newest first also keeps the
	// sources of restores kept only as sources.
	for _, checkpoint := range slices.Backward(history) {
		if keep[checkpoint.Seq] && checkpoint.RestoredFrom != 0 {
			keep[checkpoint.RestoredFrom] = true
		}
	}

	var expired []WorkspaceCheckpoint
	for _, checkpoint := range history {
		if !keep[checkpoint.Seq] {
			expired = append(expired, checkpoint)
		}
	}
	return expired
}

// applyByteBudget drops the oldest kept entries, never the current checkpoint
// or the base, until the distinct archives still kept fit the budget.
// Entries sharing an archive count it once.
func applyByteBudget(history []WorkspaceCheckpoint, keep map[int64]bool) {
	if len(history) == 0 {
		return
	}
	current := history[len(history)-1]
	budget := max(int64(RetainCheckpointBytes), RetainCheckpointCopies*current.SizeBytes)
	users := make(map[string]int)
	var total int64
	for _, checkpoint := range history {
		if !keep[checkpoint.Seq] {
			continue
		}
		if users[checkpoint.Ref] == 0 {
			total += checkpoint.SizeBytes
		}
		users[checkpoint.Ref]++
	}
	for _, checkpoint := range history {
		if total <= budget {
			return
		}
		if !keep[checkpoint.Seq] || checkpoint.Seq == current.Seq || checkpoint.Kind == CheckpointBase {
			continue
		}
		keep[checkpoint.Seq] = false
		if users[checkpoint.Ref]--; users[checkpoint.Ref] == 0 {
			total -= checkpoint.SizeBytes
		}
	}
}

// RecordWorkspaceCheckpoint appends a checkpoint whose archive is already
// durable, then applies retention. A run checkpoint whose archive is already
// current is not appended, so runs that change no files cannot push earlier
// checkpoints out of retention; the current entry is returned instead. Once
// the append succeeds the checkpoint is recorded: retention failures go to
// onError, and an orphaned archive is removed by a later prune. Callers must
// not record checkpoints of one workspace concurrently: pruning removes any
// archive not yet appended.
func RecordWorkspaceCheckpoint(
	ctx context.Context,
	store StateStore,
	archives CheckpointStore,
	checkpoint WorkspaceCheckpoint,
	onError func(error),
) (WorkspaceCheckpoint, error) {
	if checkpoint.Kind == CheckpointRun {
		current, err := store.CurrentWorkspaceCheckpoint(ctx, checkpoint.WorkspaceID)
		if err == nil && current.Ref == checkpoint.Ref {
			return current, nil
		}
		if err != nil && !errors.Is(err, ErrStateNotFound) {
			return WorkspaceCheckpoint{}, err
		}
	}
	recorded, err := store.AppendWorkspaceCheckpoint(ctx, checkpoint)
	if err != nil {
		return WorkspaceCheckpoint{}, err
	}
	if err := pruneWorkspaceCheckpoints(ctx, store, archives, recorded.WorkspaceID, recorded.CreatedAt); err != nil && onError != nil {
		onError(fmt.Errorf("environment: prune workspace checkpoints: %w", err))
	}
	return recorded, nil
}

func pruneWorkspaceCheckpoints(
	ctx context.Context,
	store StateStore,
	archives CheckpointStore,
	workspaceID string,
	now time.Time,
) error {
	history, err := store.WorkspaceCheckpoints(ctx, workspaceID)
	if err != nil {
		return err
	}
	if len(history) == 0 {
		return errors.New("environment: workspace has no checkpoints")
	}
	expired := make(map[int64]bool)
	for _, checkpoint := range ExpiredCheckpoints(history, now) {
		expired[checkpoint.Seq] = true
	}
	if len(expired) != 0 {
		seqs := slices.Sorted(maps.Keys(expired))
		if err := store.DeleteWorkspaceCheckpoints(ctx, workspaceID, seqs); err != nil {
			return err
		}
	}

	// An archive is kept while any retained entry still points at it.
	var refs []string
	for _, checkpoint := range history {
		if !expired[checkpoint.Seq] && !slices.Contains(refs, checkpoint.Ref) {
			refs = append(refs, checkpoint.Ref)
		}
	}
	return archives.PruneWorkspaceCheckpoints(ctx, workspaceID, refs)
}
