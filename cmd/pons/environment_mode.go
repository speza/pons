package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/samperrin/pons/environment"
	"github.com/samperrin/pons/environment/e2b"
	"github.com/samperrin/pons/environment/githubapp"
	"github.com/samperrin/pons/environment/seatbelt"
	"github.com/samperrin/pons/plugins/external"
)

func executionEnvironment(backend, handsCommand string, allowNetwork bool, opts serverOptions) (environment.Provider, environment.Spec, error) {
	if backend == "" {
		if allowNetwork || handsCommand != "" || opts.GitRepository != "" || opts.GitRevision != "" ||
			opts.GitHubAppID != 0 || opts.GitHubAppPrivateKey != "" {
			return nil, environment.Spec{}, errors.New("sandbox, Git workspace, and GitHub App options require --sandbox")
		}
		return nil, environment.Spec{}, nil
	}
	if backend != "seatbelt" && backend != "e2b" {
		return nil, environment.Spec{}, fmt.Errorf("unknown backend %q", backend)
	}
	if backend == "e2b" {
		if handsCommand != "" {
			return nil, environment.Spec{}, errors.New("--hands-command is local-only; use --e2b-hands-path")
		}
		if len(opts.PluginPaths) != 0 || opts.PluginPath != "" {
			return nil, environment.Spec{}, errors.New("E2B does not yet support external plugins")
		}
		handsPath := opts.E2BHandsPath
		if handsPath == "" {
			handsPath = "/usr/local/bin/pons-hands"
		}
		command := handsCommandArgs(handsPath, opts)
		network := environment.NetworkDisabled
		if allowNetwork || opts.GitRepository != "" {
			network = environment.NetworkEnabled
		}
		if (opts.GitRepository == "") != (opts.GitRevision == "") {
			return nil, environment.Spec{}, errors.New("--git-repository and --git-revision must be set together")
		}
		var workspacePlan environment.WorkspacePlan
		if opts.GitRepository != "" {
			workspacePlan = environment.WorkspacePlan{
				Strategy:     environment.WorkspaceStrategyGit,
				SourceRef:    opts.GitRepository,
				BaseRevision: opts.GitRevision,
			}
		}
		if opts.SandboxIdleTimeout < 0 {
			return nil, environment.Spec{}, errors.New("--sandbox-idle-timeout must not be negative")
		}
		provider := &e2b.Provider{
			Template:    opts.E2BTemplate,
			HandsPath:   handsPath,
			IdleTimeout: opts.SandboxIdleTimeout,
			OnError:     opts.EnvironmentError,
		}
		if opts.GitHubAppID != 0 || opts.GitHubAppPrivateKey != "" {
			if opts.GitRepository == "" {
				return nil, environment.Spec{}, errors.New("GitHub App authentication requires --git-repository and --git-revision")
			}
			app, appErr := githubapp.New(githubapp.Config{
				AppID: opts.GitHubAppID, PrivateKeyPath: opts.GitHubAppPrivateKey,
			})
			if appErr != nil {
				return nil, environment.Spec{}, appErr
			}
			provider.GitCredentials = app
		}
		return provider, environment.Spec{
			Command: command, Network: network, WorkspacePlan: workspacePlan,
			Limits: environment.ResourceLimits{
				CallTimeout:    60 * time.Second,
				MaxResultBytes: opts.PluginMaxResultBytes,
			},
		}, nil
	}
	if opts.GitRepository != "" || opts.GitRevision != "" || opts.GitHubAppID != 0 || opts.GitHubAppPrivateKey != "" {
		return nil, environment.Spec{}, errors.New("git workspace provisioning and GitHub App authentication require --sandbox e2b")
	}
	commandPath := handsCommand
	var err error
	if commandPath == "" {
		commandPath, err = exec.LookPath("pons-hands")
		if err != nil {
			return nil, environment.Spec{}, fmt.Errorf("find pons-hands: %w (use --hands-command)", err)
		}
	}
	commandPath, err = filepath.Abs(commandPath)
	if err != nil {
		return nil, environment.Spec{}, fmt.Errorf("hands command: %w", err)
	}
	command := handsCommandArgs(commandPath, opts)
	var readOnly []string
	if opts.PluginPath != "" {
		command = append(command, "--plugin-path", opts.PluginPath)
		for _, path := range filepath.SplitList(opts.PluginPath) {
			absolute, absErr := filepath.Abs(path)
			if absErr == nil {
				if info, statErr := os.Stat(absolute); statErr == nil && info.IsDir() {
					readOnly = append(readOnly, absolute)
				}
			}
		}
	}
	for _, manifest := range opts.PluginPaths {
		loaded, loadErr := external.LoadManifest(manifest)
		if loadErr != nil {
			return nil, environment.Spec{}, loadErr
		}
		command = append(command, "--plugin", loaded.Path())
		readOnly = append(readOnly, loaded.Path(), loaded.ResolvedEntrypoint())
		for _, arg := range loaded.Args {
			candidate := arg
			if !filepath.IsAbs(candidate) {
				candidate = filepath.Join(loaded.Dir(), candidate)
			}
			if _, statErr := os.Stat(candidate); statErr == nil {
				readOnly = append(readOnly, candidate)
			}
		}
	}
	network := environment.NetworkDisabled
	if allowNetwork {
		network = environment.NetworkEnabled
	}
	return seatbelt.Provider{}, environment.Spec{
		Command: command, ReadOnly: readOnly,
		Network: network,
		Limits: environment.ResourceLimits{
			CallTimeout:    60 * time.Second,
			MaxResultBytes: opts.PluginMaxResultBytes,
		},
	}, nil
}

func handsCommandArgs(commandPath string, opts serverOptions) []string {
	return []string{
		commandPath,
		"--fs-read-bytes", fmt.Sprint(opts.FSReadBytes),
		"--bash-timeout", fmt.Sprint(opts.BashTimeout),
		"--bash-max-lines", fmt.Sprint(opts.BashMaxLines),
		"--bash-max-bytes", fmt.Sprint(opts.BashMaxBytes),
		"--plugin-max-result-bytes", fmt.Sprint(opts.PluginMaxResultBytes),
	}
}
