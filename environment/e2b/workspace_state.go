package e2b

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/samperrin/pons/environment"
	"github.com/samperrin/pons/environment/gitworkspace"
)

// loadOrCreateWorkspace returns the logical workspace and its current
// checkpoint. A git/v1 workspace that has not been checked out yet has no
// checkpoint, and its state is not yet saved.
func loadOrCreateWorkspace(
	ctx context.Context,
	store environment.StateStore,
	checkpoints environment.CheckpointStore,
	workspaceID string,
	sourcePath string,
	plan environment.WorkspacePlan,
	limit int64,
	onError func(error),
) (environment.WorkspaceState, environment.WorkspaceCheckpoint, error) {
	state, err := store.WorkspaceState(ctx, workspaceID)
	if err == nil {
		strategy := plan.Strategy
		if strategy == "" {
			strategy = environment.WorkspaceStrategyArchive
		}
		compatible := state.Strategy == strategy && state.SetupGeneration == e2bSetupGeneration
		if strategy == environment.WorkspaceStrategyGit {
			compatible = compatible && state.SourceRef == plan.SourceRef && state.BaseRevision == plan.BaseRevision
		}
		if !compatible {
			return environment.WorkspaceState{}, environment.WorkspaceCheckpoint{},
				fmt.Errorf("environment: workspace %q is incompatible with the requested workspace plan", workspaceID)
		}
		current, err := store.CurrentWorkspaceCheckpoint(ctx, workspaceID)
		switch {
		case err == nil:
			return state, current, nil
		case !errors.Is(err, environment.ErrStateNotFound):
			return environment.WorkspaceState{}, environment.WorkspaceCheckpoint{}, err
		case strategy == environment.WorkspaceStrategyGit:
			return state, environment.WorkspaceCheckpoint{}, nil
		}
		// An interrupted seed saved the workspace but not its base; seed again.
	} else if !errors.Is(err, environment.ErrStateNotFound) {
		return environment.WorkspaceState{}, environment.WorkspaceCheckpoint{}, err
	}

	if plan.Strategy == environment.WorkspaceStrategyGit {
		now := time.Now().UTC()
		return environment.WorkspaceState{
			ID:              workspaceID,
			Strategy:        environment.WorkspaceStrategyGit,
			SourceRef:       plan.SourceRef,
			BaseRevision:    plan.BaseRevision,
			SetupGeneration: e2bSetupGeneration,
			CreatedAt:       now,
			UpdatedAt:       now,
		}, environment.WorkspaceCheckpoint{}, nil
	}

	if plan.Strategy == environment.WorkspaceStrategyEmpty {
		var archive bytes.Buffer
		if err := tar.NewWriter(&archive).Close(); err != nil {
			return environment.WorkspaceState{}, environment.WorkspaceCheckpoint{}, err
		}
		return seedWorkspace(
			ctx, store, checkpoints, workspaceID, environment.WorkspaceStrategyEmpty, "", &archive, limit, onError,
		)
	}

	canonicalSource, err := filepath.EvalSymlinks(sourcePath)
	if err != nil {
		return environment.WorkspaceState{}, environment.WorkspaceCheckpoint{}, fmt.Errorf("environment: workspace source: %w", err)
	}
	info, err := os.Stat(canonicalSource)
	if err != nil || !info.IsDir() {
		if err != nil {
			return environment.WorkspaceState{}, environment.WorkspaceCheckpoint{}, fmt.Errorf("environment: workspace source: %w", err)
		}
		return environment.WorkspaceState{}, environment.WorkspaceCheckpoint{}, errors.New("environment: workspace source must be a directory")
	}
	archive, err := stageWorkspaceArchive(func(out io.Writer) error {
		return writeWorkspaceArchive(ctx, out, canonicalSource, limit)
	})
	if err != nil {
		return environment.WorkspaceState{}, environment.WorkspaceCheckpoint{}, err
	}

	defer os.Remove(archive.Name())
	defer archive.Close()
	return seedWorkspace(
		ctx, store, checkpoints, workspaceID, environment.WorkspaceStrategyArchive, canonicalSource, archive, limit, onError,
	)
}

// seedWorkspace records the logical workspace and stores its first archive
// as the base checkpoint.
func seedWorkspace(
	ctx context.Context,
	store environment.StateStore,
	checkpoints environment.CheckpointStore,
	workspaceID string,
	strategy environment.WorkspaceStrategy,
	sourceRef string,
	archive io.Reader,
	limit int64,
	onError func(error),
) (environment.WorkspaceState, environment.WorkspaceCheckpoint, error) {
	ref, size, err := checkpoints.PutWorkspaceCheckpoint(ctx, workspaceID, archive, limit)
	if err != nil {
		return environment.WorkspaceState{}, environment.WorkspaceCheckpoint{}, err
	}
	now := time.Now().UTC()
	state := environment.WorkspaceState{
		ID:              workspaceID,
		Strategy:        strategy,
		SourceRef:       sourceRef,
		BaseRevision:    ref,
		SetupGeneration: e2bSetupGeneration,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := store.SaveWorkspaceState(ctx, state); err != nil {
		return environment.WorkspaceState{}, environment.WorkspaceCheckpoint{}, err
	}

	base, err := environment.RecordWorkspaceCheckpoint(ctx, store, checkpoints, environment.WorkspaceCheckpoint{
		WorkspaceID: workspaceID,
		Ref:         ref,
		Kind:        environment.CheckpointBase,
		SizeBytes:   size,
		CreatedAt:   now,
	}, onError)
	if err != nil {
		return environment.WorkspaceState{}, environment.WorkspaceCheckpoint{}, err
	}
	return state, base, nil
}

func placeWorkspace(
	ctx context.Context,
	client *e2bClient,
	sandbox e2bSandbox,
	store environment.StateStore,
	checkpoints environment.CheckpointStore,
	state environment.WorkspaceState,
	current environment.WorkspaceCheckpoint,
	runID string,
	env map[string]string,
	credentials gitworkspace.Credentials,
	limit int64,
	onError func(error),
	onDebug func(string),
	progress func(string, string) error,
) (environment.WorkspaceCheckpoint, error) {
	if current.Ref == "" {
		return provisionGitWorkspace(
			ctx, client, sandbox, store, checkpoints, state, runID, env, credentials, limit, onError, onDebug, progress,
		)
	}
	if err := progress("checkpoint.restore", "Restoring workspace checkpoint…"); err != nil {
		return environment.WorkspaceCheckpoint{}, err
	}

	archive, err := checkpoints.WorkspaceCheckpoint(ctx, state.ID, current.Ref, limit)
	if err != nil {
		return environment.WorkspaceCheckpoint{}, err
	}
	uploadErr := client.upload(ctx, sandbox, workspaceUploadPath, archive)
	if err := errors.Join(uploadErr, archive.Close()); err != nil {
		return environment.WorkspaceCheckpoint{}, err
	}

	if _, _, err := client.run(ctx, sandbox, "/bin/sh", []string{
		"-c", prepareWorkspaceScript,
	}, "/home/user", nil); err != nil {
		return environment.WorkspaceCheckpoint{}, fmt.Errorf("environment: prepare E2B workspace: %w", err)
	}
	return current, nil
}
