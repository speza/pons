package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"
)

var (
	ErrWorkspaceNotFound  = errors.New("runtime: workspace not found")
	ErrCheckpointNotFound = errors.New("runtime: workspace checkpoint not found")
	// ErrWorkspaceBusy reports a restore refused because a run, another
	// restore, or a retained environment holds the workspace.
	ErrWorkspaceBusy = errors.New("runtime: workspace is busy")
	// ErrRestoreUnsupported reports a server whose provider cannot restore.
	ErrRestoreUnsupported = errors.New("runtime: this server's execution environment cannot restore workspaces")
)

// WorkspaceCheckpoint is one entry of a workspace's checkpoint history as the
// owner sees it. Seq is the handle; the archive reference is not exposed.
type WorkspaceCheckpoint struct {
	Seq          int64     `json:"seq"`
	Kind         string    `json:"kind"`
	RunID        string    `json:"run_id,omitempty"`
	RestoredFrom int64     `json:"restored_from,omitempty"`
	SizeBytes    int64     `json:"size_bytes"`
	CreatedAt    time.Time `json:"created_at"`
	Current      bool      `json:"current,omitempty"`
}

// WorkspaceCheckpoints is a workspace's history, newest first.
type WorkspaceCheckpoints struct {
	WorkspaceID string                `json:"workspace_id"`
	Checkpoints []WorkspaceCheckpoint `json:"checkpoints"`
}

// WorkspaceHistory lists a workspace's checkpoints, newest first. An unknown
// workspace is ErrWorkspaceNotFound.
type WorkspaceHistory interface {
	WorkspaceCheckpoints(ctx context.Context, workspaceID string) ([]WorkspaceCheckpoint, error)
}

// WorkspaceRestorer makes checkpoint seq a workspace's newest checkpoint and
// returns the entry it recorded. Manager calls it only while it holds the
// workspace's reservation.
type WorkspaceRestorer interface {
	RestoreWorkspace(ctx context.Context, workspaceID string, seq int64) (WorkspaceCheckpoint, error)
}

// WorkspaceCheckpoints returns the workspace's checkpoint history.
func (m *Manager) WorkspaceCheckpoints(ctx context.Context, workspaceID string) (WorkspaceCheckpoints, error) {
	if err := m.checkOpen(); err != nil {
		return WorkspaceCheckpoints{}, err
	}
	if m.cfg.WorkspaceHistory == nil {
		return WorkspaceCheckpoints{}, ErrWorkspaceNotFound
	}
	checkpoints, err := m.cfg.WorkspaceHistory.WorkspaceCheckpoints(ctx, workspaceID)
	if err != nil {
		return WorkspaceCheckpoints{}, err
	}
	return WorkspaceCheckpoints{WorkspaceID: workspaceID, Checkpoints: checkpoints}, nil
}

// RestoreWorkspace reserves the workspace so no run is claimed for it,
// restores checkpoint seq, and releases the reservation. Submissions queued
// meanwhile run afterwards against the restored workspace. Close waits for a
// restore in progress, so the store outlives it.
func (m *Manager) RestoreWorkspace(ctx context.Context, workspaceID string, seq int64) (WorkspaceCheckpoint, error) {
	if m.cfg.WorkspaceRestorer == nil {
		if err := m.checkOpen(); err != nil {
			return WorkspaceCheckpoint{}, err
		}
		return WorkspaceCheckpoint{}, ErrRestoreUnsupported
	}
	// Adding under mu, before Close sets closed, orders this Add before
	// Close's Wait.
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return WorkspaceCheckpoint{}, ErrClosed
	}
	m.wg.Add(1)
	m.mu.Unlock()
	defer m.wg.Done()

	if err := m.store.ReserveWorkspace(ctx, workspaceID); err != nil {
		return WorkspaceCheckpoint{}, err
	}
	defer func() {
		// The release outlives a canceled request. A failed release does not
		// change the restore's outcome; it is reported, retried once, and
		// otherwise cleared at the next startup.
		releaseCtx := context.WithoutCancel(ctx)
		if err := m.store.ReleaseWorkspace(releaseCtx, workspaceID); err != nil {
			if retryErr := m.store.ReleaseWorkspace(releaseCtx, workspaceID); retryErr != nil {
				m.report(fmt.Errorf("runtime: release workspace %q; its runs wait until restart: %w",
					workspaceID, errors.Join(err, retryErr)))
			}
		}
		m.notify()
	}()
	return m.cfg.WorkspaceRestorer.RestoreWorkspace(ctx, workspaceID, seq)
}
