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
	credentials gitworkspace.Credentials,
	limit int64,
	onError func(error),
	onDebug func(string),
	progress func(string) error,
) (environment.WorkspaceState, error) {
	if progress == nil {
		progress = func(string) error { return nil }
	}
	commands := []struct {
		step          string
		command       string
		args          []string
		cwd           string
		authenticated bool
	}{
		{step: "prepare", command: "/bin/rm", args: []string{"-rf", defaultE2BWorkspace}, cwd: "/home/user"},
		{step: "git init", command: "/usr/bin/git", args: []string{"init", defaultE2BWorkspace}, cwd: "/home/user"},
		{step: "git remote add", command: "/usr/bin/git", args: []string{"-C", defaultE2BWorkspace, "remote", "add", "origin", state.SourceRef}, cwd: "/home/user"},
		{step: "git fetch", command: "/usr/bin/git", args: []string{"-C", defaultE2BWorkspace, "fetch", "--no-tags", "origin", state.BaseRevision}, cwd: "/home/user", authenticated: true},
		{step: "git checkout", command: "/usr/bin/git", args: []string{"-C", defaultE2BWorkspace, "checkout", "-b", gitworkspace.BranchName(state.ID), "FETCH_HEAD"}, cwd: "/home/user"},
		{step: "archive initial checkout", command: "/bin/tar", args: []string{"--hard-dereference", "-cf", workspaceCheckpointPath, "-C", defaultE2BWorkspace, "."}, cwd: "/home/user"},
	}

	for _, command := range commands {
		var stage string
		switch command.step {
		case "prepare":
			stage = "Preparing Git workspace…"
		case "git fetch":
			stage = "Fetching repository…"
		case "git checkout":
			stage = "Creating work branch…"
		case "archive initial checkout":
			stage = "Saving initial checkout…"
		}
		if stage != "" {
			if err := progress(stage); err != nil {
				return environment.WorkspaceState{}, err
			}
		}
		if command.step == "git fetch" {
			debugE2B(onDebug, "workspace=%q sandbox=%q git fetch started", state.ID, sandbox.ID)
		}
		var commandEnv map[string]string
		if command.authenticated {
			commandEnv = env
		}
		if _, _, err := client.run(ctx, sandbox, command.command, command.args, command.cwd, commandEnv); err != nil {
			return environment.WorkspaceState{}, fmt.Errorf("environment: provision E2B Git workspace step %q: %w", command.step, credentials.RedactError(err))
		}
		if command.command == "/usr/bin/git" {
			debugE2B(onDebug, "workspace=%q sandbox=%q %s completed", state.ID, sandbox.ID, command.step)
		}
	}

	if err := progress("Downloading initial checkout…"); err != nil {
		return environment.WorkspaceState{}, err
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

	debugE2B(onDebug, "workspace=%q sandbox=%q initial checkpoint=%s saved", state.ID, sandbox.ID, checkpointRef)
	if err := checkpoints.PruneWorkspaceCheckpoints(ctx, state.ID, []string{checkpointRef}); err != nil && onError != nil {
		onError(fmt.Errorf("environment: prune initial Git workspace checkpoints: %w", err))
	}
	return state, nil
}
