package e2b

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/samperrin/pons/environment"
	"github.com/samperrin/pons/environment/gitworkspace"
)

func provisionGitWorkspace(
	ctx context.Context,
	client *e2bClient,
	sandbox e2bSandbox,
	store environment.StateStore,
	checkpoints environment.CheckpointStore,
	state environment.WorkspaceState,
	env map[string]string,
	limit int64,
	onError func(error),
) (environment.WorkspaceState, error) {
	commands := []struct {
		command       string
		args          []string
		cwd           string
		authenticated bool
	}{
		{command: "/bin/rm", args: []string{"-rf", defaultE2BWorkspace}, cwd: "/home/user"},
		{command: "/usr/bin/git", args: []string{"init", defaultE2BWorkspace}, cwd: "/home/user"},
		{command: "/usr/bin/git", args: []string{"-C", defaultE2BWorkspace, "remote", "add", "origin", state.SourceRef}, cwd: "/home/user"},
		{command: "/usr/bin/git", args: []string{"-C", defaultE2BWorkspace, "fetch", "--depth=1", "--no-tags", "origin", state.BaseRevision}, cwd: "/home/user", authenticated: true},
		{command: "/usr/bin/git", args: []string{"-C", defaultE2BWorkspace, "checkout", "-b", gitworkspace.BranchName(state.ID), "FETCH_HEAD"}, cwd: "/home/user"},
		{command: "/bin/tar", args: []string{"--hard-dereference", "-cf", workspaceCheckpointPath, "-C", defaultE2BWorkspace, "."}, cwd: "/home/user"},
	}
	for _, command := range commands {
		var commandEnv map[string]string
		if command.authenticated {
			commandEnv = env
		}
		if _, _, err := client.run(ctx, sandbox, command.command, command.args, command.cwd, commandEnv); err != nil {
			return environment.WorkspaceState{}, fmt.Errorf("environment: provision E2B Git workspace: %w", err)
		}
	}
	body, err := stageWorkspaceArchive(func(out io.Writer) error {
		return client.download(ctx, sandbox, workspaceCheckpointPath, out, limit)
	})
	if err != nil {
		return environment.WorkspaceState{}, err
	}
	defer os.Remove(body.Name())
	defer body.Close()
	if err := validateWorkspaceArchive(&contextReader{ctx: ctx, reader: body}, limit); err != nil {
		return environment.WorkspaceState{}, err
	}
	if _, err := body.Seek(0, io.SeekStart); err != nil {
		return environment.WorkspaceState{}, err
	}
	checkpointRef, err := checkpoints.PutWorkspaceCheckpoint(ctx, state.ID, body, limit)
	if err != nil {
		return environment.WorkspaceState{}, err
	}
	state.CheckpointRef = checkpointRef
	state.UpdatedAt = time.Now().UTC()
	if err := store.SaveWorkspaceState(ctx, state); err != nil {
		return environment.WorkspaceState{}, err
	}
	if err := checkpoints.PruneWorkspaceCheckpoints(ctx, state.ID, []string{checkpointRef}); err != nil && onError != nil {
		onError(fmt.Errorf("environment: prune initial Git workspace checkpoints: %w", err))
	}
	return state, nil
}
