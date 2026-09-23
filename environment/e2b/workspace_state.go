package e2b

import (
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

func loadOrCreateWorkspace(
	ctx context.Context,
	store environment.StateStore,
	checkpoints environment.CheckpointStore,
	workspaceID string,
	sourcePath string,
	plan environment.WorkspacePlan,
	limit int64,
	onError func(error),
) (environment.WorkspaceState, error) {
	state, err := store.WorkspaceState(ctx, workspaceID)
	if err == nil {
		strategy := plan.Strategy
		if strategy == "" {
			strategy = environment.WorkspaceStrategyArchive
		}
		compatible := state.Strategy == strategy &&
			state.SetupGeneration == e2bSetupGeneration &&
			state.CheckpointRef != ""
		if strategy == environment.WorkspaceStrategyGit {
			compatible = compatible && state.SourceRef == plan.SourceRef && state.BaseRevision == plan.BaseRevision
		}
		if !compatible {
			return environment.WorkspaceState{}, fmt.Errorf("environment: workspace %q is incompatible with the requested workspace plan", workspaceID)
		}
		return state, nil
	}

	if !errors.Is(err, environment.ErrStateNotFound) {
		return environment.WorkspaceState{}, err
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
		}, nil
	}

	canonicalSource, err := filepath.EvalSymlinks(sourcePath)
	if err != nil {
		return environment.WorkspaceState{}, fmt.Errorf("environment: workspace source: %w", err)
	}
	info, err := os.Stat(canonicalSource)
	if err != nil || !info.IsDir() {
		if err != nil {
			return environment.WorkspaceState{}, fmt.Errorf("environment: workspace source: %w", err)
		}
		return environment.WorkspaceState{}, errors.New("environment: workspace source must be a directory")
	}
	archive, err := stageWorkspaceArchive(func(out io.Writer) error {
		return writeWorkspaceArchive(ctx, out, canonicalSource, limit)
	})
	if err != nil {
		return environment.WorkspaceState{}, err
	}

	defer os.Remove(archive.Name())
	defer archive.Close()
	checkpointRef, err := checkpoints.PutWorkspaceCheckpoint(ctx, workspaceID, archive, limit)
	if err != nil {
		return environment.WorkspaceState{}, err
	}
	now := time.Now().UTC()
	state = environment.WorkspaceState{
		ID:              workspaceID,
		Strategy:        environment.WorkspaceStrategyArchive,
		SourceRef:       canonicalSource,
		BaseRevision:    checkpointRef,
		CheckpointRef:   checkpointRef,
		SetupGeneration: e2bSetupGeneration,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := store.SaveWorkspaceState(ctx, state); err != nil {
		return environment.WorkspaceState{}, err
	}

	if err := checkpoints.PruneWorkspaceCheckpoints(ctx, workspaceID, []string{checkpointRef}); err != nil && onError != nil {
		onError(fmt.Errorf("environment: prune initial workspace checkpoints: %w", err))
	}
	return state, nil
}

func placeWorkspace(
	ctx context.Context,
	client *e2bClient,
	sandbox e2bSandbox,
	store environment.StateStore,
	checkpoints environment.CheckpointStore,
	state environment.WorkspaceState,
	env map[string]string,
	credentials gitworkspace.Credentials,
	limit int64,
	onError func(error),
	onDebug func(string),
	progress func(string, string) error,
) (environment.WorkspaceState, error) {
	if state.CheckpointRef == "" {
		return provisionGitWorkspace(ctx, client, sandbox, store, checkpoints, state, env, credentials, limit, onError, onDebug, progress)
	}
	if err := progress("checkpoint.restore", "Restoring workspace checkpoint…"); err != nil {
		return environment.WorkspaceState{}, err
	}

	archive, err := checkpoints.WorkspaceCheckpoint(ctx, state.ID, state.CheckpointRef, limit)
	if err != nil {
		return environment.WorkspaceState{}, err
	}
	uploadErr := client.upload(ctx, sandbox, workspaceUploadPath, archive)
	if err := errors.Join(uploadErr, archive.Close()); err != nil {
		return environment.WorkspaceState{}, err
	}

	if _, _, err := client.run(ctx, sandbox, "/bin/sh", []string{
		"-c", prepareWorkspaceScript,
	}, "/home/user", nil); err != nil {
		return environment.WorkspaceState{}, fmt.Errorf("environment: prepare E2B workspace: %w", err)
	}
	return state, nil
}
