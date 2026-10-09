package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/samperrin/pons/environment"
	ponsruntime "github.com/samperrin/pons/runtime"
)

// History lists and restores workspace checkpoints recorded in the state
// store. Restorer is the provider's, and nil when it cannot restore.
type History struct {
	State    environment.StateStore
	Archives environment.CheckpointStore
	Restorer environment.WorkspaceRestorer
	OnError  func(error)
	// Now defaults to time.Now.
	Now func() time.Time
}

var (
	_ ponsruntime.WorkspaceHistory  = (*History)(nil)
	_ ponsruntime.WorkspaceRestorer = (*History)(nil)
)

func (h *History) WorkspaceCheckpoints(ctx context.Context, workspaceID string) ([]ponsruntime.WorkspaceCheckpoint, error) {
	history, err := h.history(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	checkpoints := make([]ponsruntime.WorkspaceCheckpoint, 0, len(history))
	for _, checkpoint := range slices.Backward(history) {
		checkpoints = append(checkpoints, publicCheckpoint(checkpoint, len(checkpoints) == 0))
	}
	return checkpoints, nil
}

// RestoreWorkspace validates checkpoint seq, has the provider apply it, and
// appends a restore entry pointing at its archive. The caller holds the
// workspace's reservation. A failure before the append leaves the history
// and the current checkpoint unchanged.
func (h *History) RestoreWorkspace(ctx context.Context, workspaceID string, seq int64) (ponsruntime.WorkspaceCheckpoint, error) {
	if h.Restorer == nil {
		return ponsruntime.WorkspaceCheckpoint{}, ponsruntime.ErrRestoreUnsupported
	}
	history, err := h.history(ctx, workspaceID)
	if err != nil {
		return ponsruntime.WorkspaceCheckpoint{}, err
	}
	index := slices.IndexFunc(history, func(checkpoint environment.WorkspaceCheckpoint) bool {
		return checkpoint.Seq == seq
	})
	if index < 0 {
		return ponsruntime.WorkspaceCheckpoint{}, fmt.Errorf("%w: %s has no checkpoint %d", ponsruntime.ErrCheckpointNotFound, workspaceID, seq)
	}
	target := history[index]

	archive, err := h.Archives.WorkspaceCheckpoint(ctx, workspaceID, target.Ref, target.SizeBytes)
	if err != nil {
		return ponsruntime.WorkspaceCheckpoint{}, fmt.Errorf("runtime: checkpoint %d is unavailable: %w", seq, err)
	}
	if err := archive.Close(); err != nil {
		return ponsruntime.WorkspaceCheckpoint{}, err
	}
	// A recovery sandbox holds edits that were never checkpointed.
	placement, err := h.State.EnvironmentState(ctx, workspaceID)
	if err != nil && !errors.Is(err, environment.ErrStateNotFound) {
		return ponsruntime.WorkspaceCheckpoint{}, err
	}
	if err == nil && placement.Status != environment.StateIdle && h.now().Before(placement.ExpiresAt) {
		return ponsruntime.WorkspaceCheckpoint{}, fmt.Errorf("%w: workspace %q has a %s environment until %s",
			ponsruntime.ErrWorkspaceBusy, workspaceID, placement.Status, placement.ExpiresAt.UTC().Format(time.RFC3339))
	}

	if err := h.Restorer.RestoreWorkspace(ctx, workspaceID, target); err != nil {
		if errors.Is(err, environment.ErrWorkspaceInUse) {
			err = fmt.Errorf("%w: %w", ponsruntime.ErrWorkspaceBusy, err)
		}
		return ponsruntime.WorkspaceCheckpoint{}, err
	}
	// Once the provider has applied it, the restore is recorded even if the
	// request is canceled, so it is never left half done.
	restored, err := environment.RecordWorkspaceCheckpoint(context.WithoutCancel(ctx), h.State, h.Archives, environment.WorkspaceCheckpoint{
		WorkspaceID:  workspaceID,
		Ref:          target.Ref,
		Kind:         environment.CheckpointRestore,
		RestoredFrom: target.Seq,
		SizeBytes:    target.SizeBytes,
		CreatedAt:    h.now().UTC(),
	}, h.OnError)
	if err != nil {
		return ponsruntime.WorkspaceCheckpoint{}, err
	}
	return publicCheckpoint(restored, true), nil
}

func (h *History) history(ctx context.Context, workspaceID string) ([]environment.WorkspaceCheckpoint, error) {
	if _, err := h.State.WorkspaceState(ctx, workspaceID); errors.Is(err, environment.ErrStateNotFound) {
		return nil, fmt.Errorf("%w: %s", ponsruntime.ErrWorkspaceNotFound, workspaceID)
	} else if err != nil {
		return nil, err
	}
	return h.State.WorkspaceCheckpoints(ctx, workspaceID)
}

func (h *History) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

func publicCheckpoint(checkpoint environment.WorkspaceCheckpoint, current bool) ponsruntime.WorkspaceCheckpoint {
	return ponsruntime.WorkspaceCheckpoint{
		Seq:          checkpoint.Seq,
		Kind:         string(checkpoint.Kind),
		RunID:        checkpoint.RunID,
		RestoredFrom: checkpoint.RestoredFrom,
		SizeBytes:    checkpoint.SizeBytes,
		CreatedAt:    checkpoint.CreatedAt,
		Current:      current,
	}
}
