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
	runID string,
	env map[string]string,
	credentials gitworkspace.Credentials,
	limit int64,
	onError func(error),
	onDebug func(string),
	progress func(string, string) error,
) (environment.WorkspaceCheckpoint, error) {
	if progress == nil {
		progress = func(string, string) error { return nil }
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
		var step, message string
		switch command.step {
		case "prepare":
			step, message = "git.prepare", "Preparing Git workspace…"
		case "git fetch":
			step, message = "git.fetch", "Fetching repository…"
		case "git checkout":
			step, message = "git.checkout", "Creating work branch…"
		case "archive initial checkout":
			step, message = "checkpoint.save", "Saving initial checkout…"
		}
		if step != "" {
			if err := progress(step, message); err != nil {
				return environment.WorkspaceCheckpoint{}, err
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
			return environment.WorkspaceCheckpoint{}, fmt.Errorf("environment: provision E2B Git workspace step %q: %w", command.step, credentials.RedactError(err))
		}
		if command.command == "/usr/bin/git" {
			debugE2B(onDebug, "workspace=%q sandbox=%q %s completed", state.ID, sandbox.ID, command.step)
		}
	}

	if err := progress("checkpoint.download", "Downloading initial checkout…"); err != nil {
		return environment.WorkspaceCheckpoint{}, err
	}
	body, err := stageWorkspaceArchive(func(out io.Writer) error {
		return client.download(ctx, sandbox, workspaceCheckpointPath, out, limit)
	})
	if err != nil {
		return environment.WorkspaceCheckpoint{}, err
	}
	defer os.Remove(body.Name())
	defer body.Close()
	if err := validateWorkspaceArchive(&contextReader{ctx: ctx, reader: body}, limit); err != nil {
		return environment.WorkspaceCheckpoint{}, err
	}

	if _, err := body.Seek(0, io.SeekStart); err != nil {
		return environment.WorkspaceCheckpoint{}, err
	}
	ref, size, err := checkpoints.PutWorkspaceCheckpoint(ctx, state.ID, body, limit)
	if err != nil {
		return environment.WorkspaceCheckpoint{}, err
	}
	now := time.Now().UTC()
	state.UpdatedAt = now
	if err := store.SaveWorkspaceState(ctx, state); err != nil {
		return environment.WorkspaceCheckpoint{}, err
	}
	// A git/v1 workspace has no base entry: its first checkpoint is this
	// run's initial checkout.
	checkpoint, err := environment.RecordWorkspaceCheckpoint(ctx, store, checkpoints, environment.WorkspaceCheckpoint{
		WorkspaceID: state.ID,
		Ref:         ref,
		Kind:        environment.CheckpointRun,
		RunID:       runID,
		SizeBytes:   size,
		CreatedAt:   now,
	}, onError)
	if err != nil {
		return environment.WorkspaceCheckpoint{}, err
	}

	debugE2B(onDebug, "workspace=%q sandbox=%q initial checkpoint=%s saved", state.ID, sandbox.ID, ref)
	return checkpoint, nil
}
