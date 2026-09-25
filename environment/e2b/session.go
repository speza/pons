package e2b

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/samperrin/pons/environment"
	"github.com/samperrin/pons/environment/gitworkspace"
	"github.com/samperrin/pons/plugins/external"
	"github.com/samperrin/pons/protocol"
)

type e2bSession struct {
	host              *external.Host
	cancelTransport   context.CancelFunc
	client            *e2bClient
	sandbox           e2bSandbox
	workspace         environment.WorkspaceState
	owner             *Provider
	store             environment.StateStore
	checkpoints       environment.CheckpointStore
	runID             string
	template          string
	idleTimeout       time.Duration
	timeout           time.Duration
	maxWorkspaceBytes int64
	onError           func(error)
	onDebug           func(string)
	credentials       gitworkspace.Credentials
	synced            []syncedDirectory
	metadata          environment.Metadata
	keepaliveCancel   context.CancelFunc
	keepaliveDone     chan struct{}
	closeOnce         sync.Once
	closeErr          error
}

func (s *e2bSession) Catalog() []external.ToolDescription { return s.host.Tools() }
func (s *e2bSession) Metadata() environment.Metadata      { return s.metadata }
func (s *e2bSession) Execute(ctx context.Context, action protocol.Action) (protocol.ToolResult, error) {
	result, err := s.host.Execute(ctx, action)
	result = s.credentials.RedactResult(result)
	if err != nil {
		redacted := s.credentials.Redact(err.Error())
		if redacted != err.Error() {
			err = errors.New(redacted)
		}
	}
	return result, err
}

func (s *e2bSession) startKeepalive() {
	ctx, cancel := context.WithCancel(context.Background())
	s.keepaliveCancel = cancel
	s.keepaliveDone = make(chan struct{})
	go s.keepalive(ctx)
}

func (s *e2bSession) keepalive(ctx context.Context) {
	defer close(s.keepaliveDone)
	interval := min(s.timeout/3, time.Minute)
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			refreshCtx, cancel := context.WithTimeout(ctx, min(s.timeout/3, 15*time.Second))
			err := s.refreshTimeout(refreshCtx)
			cancel()
			if ctx.Err() != nil {
				return
			}
			if err != nil && s.onError != nil {
				s.onError(err)
			}
		}
	}
}

func (s *e2bSession) refreshTimeout(ctx context.Context) error {
	if err := s.client.setSandboxTimeout(ctx, s.sandbox.ID, s.timeout); err != nil {
		return err
	}
	if s.store == nil {
		return nil
	}
	now := time.Now().UTC()
	if err := s.store.SaveEnvironmentState(ctx, environment.State{
		WorkspaceID:   s.workspace.ID,
		Provider:      "e2b",
		EnvironmentID: s.sandbox.ID,
		Template:      s.template,
		Network:       s.metadata.Network,
		Status:        environment.StateActive,
		RunID:         s.runID,
		ExpiresAt:     now.Add(s.timeout),
		UpdatedAt:     now,
	}); err != nil {
		return fmt.Errorf("environment: refresh E2B state: %w", err)
	}
	return nil
}

func (s *e2bSession) debugf(format string, args ...any) {
	if s.onDebug == nil {
		return
	}
	prefix := fmt.Sprintf("workspace=%q sandbox=%q ", s.workspace.ID, s.sandbox.ID)
	s.onDebug(prefix + fmt.Sprintf(format, args...))
}

func (s *e2bSession) persistCheckpoint(ctx context.Context, archive io.ReadSeeker) error {
	if err := validateWorkspaceArchive(&contextReader{ctx: ctx, reader: archive}, s.maxWorkspaceBytes); err != nil {
		return err
	}

	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return err
	}
	checkpointRef, err := s.checkpoints.PutWorkspaceCheckpoint(ctx, s.workspace.ID, archive, s.maxWorkspaceBytes)
	if err != nil {
		return err
	}
	workspace := s.workspace
	workspace.CheckpointRef = checkpointRef
	workspace.UpdatedAt = time.Now().UTC()
	if err := s.store.SaveWorkspaceState(ctx, workspace); err != nil {
		return err
	}

	s.workspace = workspace
	s.debugf("checkpoint=%s saved", checkpointRef)
	// The new reference is durable before pruning. archive/v1's BaseRevision is
	// a checkpoint reference; git/v1's is a Git object ID and must not be passed
	// to the checkpoint store.
	keep := []string{s.workspace.CheckpointRef}
	if s.workspace.Strategy == environment.WorkspaceStrategyArchive {
		keep = append([]string{s.workspace.BaseRevision}, keep...)
	}
	if err := s.checkpoints.PruneWorkspaceCheckpoints(ctx, s.workspace.ID, keep); err != nil && s.onError != nil {
		s.onError(fmt.Errorf("environment: prune superseded workspace checkpoints: %w", err))
	}
	return nil
}

// Reserve recovery before attempting a checkpoint, so a crash or storage
// failure cannot make the next run discard the only remaining copy of edits.
func (s *e2bSession) reserveRecovery(ctx context.Context, until time.Time) error {
	stateErr := s.store.SaveEnvironmentState(ctx, environment.State{
		WorkspaceID: s.workspace.ID, Provider: "e2b", EnvironmentID: s.sandbox.ID,
		Template: s.template, Network: s.metadata.Network, Status: environment.StateRecovery,
		ExpiresAt: until, UpdatedAt: time.Now().UTC(),
	})
	timeoutErr := s.client.setSandboxTimeout(ctx, s.sandbox.ID, time.Until(until)+time.Minute)
	return errors.Join(stateErr, timeoutErr)
}

func (s *e2bSession) stopKeepalive() {
	if s.keepaliveCancel == nil {
		return
	}
	s.keepaliveCancel()
	<-s.keepaliveDone
}

func (s *e2bSession) Close() error {
	s.closeOnce.Do(func() {
		if s.cancelTransport != nil {
			defer s.cancelTransport()
		}
		s.closeErr = s.host.Close()
		retain := s.store != nil
		s.syncDirectoriesOut()
		// Stop active-state writes before publishing the recovery reservation.
		// A past heartbeat failure does not prevent a fresh checkpoint attempt.
		s.stopKeepalive()

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		recoveryUntil := time.Now().UTC().Add(checkpointRecoveryTimeout)
		var recoveryErr error
		if retain {
			recoveryErr = s.reserveRecovery(ctx, recoveryUntil)
			s.closeErr = errors.Join(s.closeErr, recoveryErr)
		}

		if s.closeErr == nil && retain {
			s.debugf("checkpoint started")
			if _, _, err := s.client.run(ctx, s.sandbox, "/bin/tar", []string{
				"--hard-dereference", "-cf", workspaceCheckpointPath, "-C", defaultE2BWorkspace, ".",
			}, "/home/user", nil); err != nil {
				s.closeErr = fmt.Errorf("environment: checkpoint E2B workspace: %w", err)
			} else if body, err := stageWorkspaceArchive(func(out io.Writer) error {
				return s.client.download(ctx, s.sandbox, workspaceCheckpointPath, out, s.maxWorkspaceBytes)
			}); err != nil {
				s.closeErr = err
			} else {
				defer os.Remove(body.Name())
				defer body.Close()
				s.closeErr = s.persistCheckpoint(ctx, body)
			}
		}

		if s.closeErr == nil && s.store != nil {
			now := time.Now().UTC()
			idleUntil := now.Add(s.idleTimeout)
			if err := s.client.setSandboxTimeout(ctx, s.sandbox.ID, s.idleTimeout+time.Minute); err != nil {
				s.closeErr = err
			} else {
				s.closeErr = s.store.SaveEnvironmentState(ctx, environment.State{
					WorkspaceID:   s.workspace.ID,
					Provider:      "e2b",
					EnvironmentID: s.sandbox.ID,
					Template:      s.template,
					Network:       s.metadata.Network,
					Status:        environment.StateIdle,
					IdleUntil:     idleUntil,
					ExpiresAt:     idleUntil.Add(time.Minute),
					UpdatedAt:     now,
				})
				if s.closeErr == nil {
					s.debugf("state=idle until=%s", idleUntil.Format(time.RFC3339))
				}
			}
		}

		if retain && s.closeErr != nil {
			// Idle transition may have shortened the provider TTL before its
			// metadata write failed. Restore the original, non-renewing window.
			recoveryCtx, recoveryCancel := context.WithTimeout(context.Background(), 15*time.Second)
			recoveryErr = s.reserveRecovery(recoveryCtx, recoveryUntil)
			recoveryCancel()
			s.closeErr = errors.Join(s.closeErr, recoveryErr)
			if recoveryErr != nil {
				s.debugf("state=recovery retention incomplete")
				s.closeErr = fmt.Errorf("environment: sandbox %q was not deleted, but recovery retention could not be fully recorded or extended; recover files immediately (provider TTL may expire sooner): %w", s.sandbox.ID, s.closeErr)
			} else {
				s.debugf("state=recovery retained until=%s", recoveryUntil.Format(time.RFC3339))
				s.closeErr = fmt.Errorf("environment: sandbox %q retained for manual recovery until %s; new runs are blocked until then: %w",
					s.sandbox.ID, recoveryUntil.Format(time.RFC3339), s.closeErr)
			}
		} else if s.store == nil || s.closeErr != nil {
			s.debugf("deleting after session close failure")
			killCtx, killCancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer killCancel()
			killErr := s.client.killSandbox(killCtx, s.sandbox.ID)
			s.closeErr = errors.Join(s.closeErr, killErr)
			if killErr == nil && s.store != nil {
				deleteErr := s.store.DeleteEnvironmentState(killCtx, s.workspace.ID, s.sandbox.ID)
				s.closeErr = errors.Join(s.closeErr, deleteErr)
				if deleteErr == nil {
					s.debugf("deleted")
				}
			}
		}
		if s.owner != nil {
			s.owner.lifecycleMu.Lock()
			if s.owner.active[s.workspace.ID] == s.runID {
				delete(s.owner.active, s.workspace.ID)
			}
			s.owner.lifecycleMu.Unlock()
		}
	})
	return s.closeErr
}
