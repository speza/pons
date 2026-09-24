package seatbelt

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"

	"github.com/samperrin/pons/environment"
	"github.com/samperrin/pons/plugins/external"
	"github.com/samperrin/pons/protocol"
)

// ErrSeatbeltUnavailable reports that the local Seatbelt backend cannot run
// on this host.
var ErrSeatbeltUnavailable = errors.New("environment: macOS Seatbelt is unavailable")

// Provider launches hands under macOS Seatbelt. SandboxExec may be overridden
// by tests or deployments; the default is /usr/bin/sandbox-exec.
type Provider struct {
	SandboxExec string
}

// Start launches a complete pons-hands tool host under a generated Seatbelt
// profile and connects it to the existing tool_provider/v1 host adapter.
func (p Provider) Start(ctx context.Context, spec environment.Spec) (environment.HandsSession, error) {
	if runtime.GOOS != "darwin" {
		return nil, ErrSeatbeltUnavailable
	}
	workspace, command, readOnly, readWrite, network, err := validateSpec(spec)
	if err != nil {
		return nil, err
	}

	scratch, err := os.MkdirTemp("", "pons-hands-")
	if err != nil {
		return nil, fmt.Errorf("environment: create scratch: %w", err)
	}
	if canonical, canonicalErr := filepath.EvalSymlinks(scratch); canonicalErr == nil {
		scratch = canonical
	}
	cleanup := func() { _ = os.RemoveAll(scratch) }

	profile, err := seatbeltProfile(workspace, scratch, command[0], readOnly, readWrite, network)
	if err != nil {
		cleanup()
		return nil, err
	}
	sandboxExec := p.SandboxExec
	if sandboxExec == "" {
		sandboxExec = "/usr/bin/sandbox-exec"
	}
	if !filepath.IsAbs(sandboxExec) {
		cleanup()
		return nil, errors.New("environment: sandbox-exec path must be absolute")
	}
	if info, err := os.Stat(sandboxExec); err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
		cleanup()
		if err != nil {
			return nil, fmt.Errorf("environment: sandbox-exec: %w", err)
		}
		return nil, errors.New("environment: sandbox-exec is not executable")
	}

	command = append(command, "--workspace", workspace)
	for _, path := range readWrite {
		command = append(command, "--read-write", path)
	}
	args := []string{"-p", profile, "--"}
	args = append(args, command...)
	manifest := external.Manifest{
		ManifestVersion: external.ManifestVersion,
		Name:            "pons.hands",
		Entrypoint:      sandboxExec,
		Args:            args,
		RuntimeProtocol: external.RuntimeProtocol,
	}
	env, err := cleanEnvironment(spec.Environment, scratch)
	if err != nil {
		cleanup()
		return nil, err
	}

	host, err := external.NewHost(manifest, external.HostConfig{
		Workspace:        workspace,
		WorkingDirectory: workspace,
		CallTimeout:      spec.Limits.CallTimeout,
		Env:              env,
		Limits: external.Limits{
			MaxFrameBytes:      spec.Limits.MaxFrameBytes,
			MaxStderrBytes:     spec.Limits.MaxStderrBytes,
			MaxResultBytes:     spec.Limits.MaxResultBytes,
			MaxTools:           spec.Limits.MaxTools,
			MaxPendingRequests: spec.Limits.MaxPendingRequests,
			MaxConcurrency:     spec.Limits.MaxConcurrency,
		},
	})
	if err != nil {
		cleanup()
		return nil, err
	}
	if err := host.Start(ctx); err != nil {
		_ = host.Close()
		stderr := strings.TrimSpace(host.Stderr())
		cleanup()
		if stderr != "" {
			return nil, fmt.Errorf("%w: %s", err, stderr)
		}
		return nil, err
	}

	return &seatbeltSession{
		host:    host,
		scratch: scratch,
		metadata: environment.Metadata{
			Provider:      "seatbelt",
			WorkspaceID:   spec.WorkspaceID,
			WorkspacePath: workspace,
			ReadWrite:     readWrite,
			Platform:      runtime.GOOS + "/" + runtime.GOARCH,
			Network:       network,
		},
	}, nil
}

type seatbeltSession struct {
	host      *external.Host
	scratch   string
	metadata  environment.Metadata
	closeOnce sync.Once
	closeErr  error
}

func (s *seatbeltSession) Catalog() []external.ToolDescription { return s.host.Tools() }
func (s *seatbeltSession) Metadata() environment.Metadata      { return s.metadata }
func (s *seatbeltSession) Execute(ctx context.Context, action protocol.Action) (protocol.ToolResult, error) {
	return s.host.Execute(ctx, action)
}
func (s *seatbeltSession) Close() error {
	s.closeOnce.Do(func() {
		s.closeErr = s.host.Close()
		if err := os.RemoveAll(s.scratch); err != nil {
			s.closeErr = errors.Join(s.closeErr, fmt.Errorf("environment: remove scratch: %w", err))
		}
	})
	return s.closeErr
}

func validateSpec(spec environment.Spec) (string, []string, []string, []string, environment.NetworkPolicy, error) {
	if spec.WorkspacePath == "" {
		return "", nil, nil, nil, "", errors.New("environment: workspace is required")
	}
	workspace, err := filepath.Abs(spec.WorkspacePath)
	if err != nil {
		return "", nil, nil, nil, "", fmt.Errorf("environment: workspace: %w", err)
	}
	workspace, err = filepath.EvalSymlinks(workspace)
	if err != nil {
		return "", nil, nil, nil, "", fmt.Errorf("environment: workspace: %w", err)
	}
	info, err := os.Stat(workspace)
	if err != nil || !info.IsDir() {
		if err != nil {
			return "", nil, nil, nil, "", fmt.Errorf("environment: workspace: %w", err)
		}
		return "", nil, nil, nil, "", errors.New("environment: workspace must be a directory")
	}

	if len(spec.Command) == 0 || spec.Command[0] == "" {
		return "", nil, nil, nil, "", errors.New("environment: hands command is required")
	}
	command := append([]string(nil), spec.Command...)
	if !filepath.IsAbs(command[0]) {
		return "", nil, nil, nil, "", errors.New("environment: hands command must be absolute")
	}
	command[0], err = filepath.EvalSymlinks(command[0])
	if err != nil {
		return "", nil, nil, nil, "", fmt.Errorf("environment: hands command: %w", err)
	}
	info, err = os.Stat(command[0])
	if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
		if err != nil {
			return "", nil, nil, nil, "", fmt.Errorf("environment: hands command: %w", err)
		}
		return "", nil, nil, nil, "", errors.New("environment: hands command is not executable")
	}

	readOnly := make([]string, 0, len(spec.ReadOnly))
	seen := make(map[string]bool, len(spec.ReadOnly))
	for _, path := range spec.ReadOnly {
		if !filepath.IsAbs(path) {
			return "", nil, nil, nil, "", errors.New("environment: read-only path must be absolute")
		}
		path, err = filepath.EvalSymlinks(path)
		if err != nil {
			return "", nil, nil, nil, "", fmt.Errorf("environment: read-only path: %w", err)
		}
		if _, err := os.Stat(path); err != nil {
			return "", nil, nil, nil, "", fmt.Errorf("environment: read-only path: %w", err)
		}
		if !seen[path] {
			seen[path] = true
			readOnly = append(readOnly, path)
		}
	}

	readWrite := make([]string, 0, len(spec.ReadWrite))
	for _, path := range spec.ReadWrite {
		if !filepath.IsAbs(path) {
			return "", nil, nil, nil, "", errors.New("environment: read-write path must be absolute")
		}
		path, err = filepath.EvalSymlinks(path)
		if err != nil {
			return "", nil, nil, nil, "", fmt.Errorf("environment: read-write path: %w", err)
		}
		if info, err := os.Stat(path); err != nil || !info.IsDir() {
			return "", nil, nil, nil, "", fmt.Errorf("environment: read-write path %q must be a directory", path)
		}
		if !seen[path] {
			seen[path] = true
			readWrite = append(readWrite, path)
		}
	}

	network := spec.Network
	if network == "" {
		network = environment.NetworkDisabled
	}
	if network != environment.NetworkDisabled && network != environment.NetworkEnabled {
		return "", nil, nil, nil, "", fmt.Errorf("environment: invalid network policy %q", network)
	}
	return workspace, command, readOnly, readWrite, network, nil
}

func cleanEnvironment(explicit []string, scratch string) ([]string, error) {
	values := map[string]string{
		"PATH":   "/usr/bin:/bin",
		"TMPDIR": scratch,
	}
	order := []string{"PATH", "TMPDIR"}
	for _, entry := range explicit {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" || strings.ContainsAny(key, "\x00\n=") || strings.ContainsRune(value, '\x00') {
			return nil, fmt.Errorf("environment: invalid environment entry %q", entry)
		}
		if key == "TMPDIR" {
			return nil, errors.New("environment: TMPDIR is owned by the provider")
		}
		if _, exists := values[key]; !exists {
			order = append(order, key)
		}
		values[key] = value
	}
	out := make([]string, 0, len(order))
	for _, key := range order {
		out = append(out, key+"="+values[key])
	}
	return out, nil
}

func seatbeltProfile(workspace, scratch, handsCommand string, readOnly, readWrite []string, network environment.NetworkPolicy) (string, error) {
	quoted := make([]string, 3)
	for i, value := range []string{workspace, scratch, handsCommand} {
		var err error
		quoted[i], err = seatbeltString(value)
		if err != nil {
			return "", err
		}
	}
	lines := []string{
		"(version 1)",
		"(deny default)",
		"(allow process-fork process-exec)",
		"(allow signal (target self))",
		"(allow sysctl-read)",
		// tool_provider/v1 uses inherited anonymous pipes. Limit the pathless
		// data permission to non-regular files; regular files still require an
		// explicit workspace, scratch, system, or command path below.
		"(allow file-read-data file-write-data",
		"  (require-not (vnode-type REGULAR-FILE)))",
		"(allow file-read-metadata file-test-existence",
	}
	ancestorPaths := slices.Concat([]string{workspace, scratch, handsCommand}, readOnly, readWrite)
	for _, ancestor := range pathAncestors(ancestorPaths...) {
		literal, err := seatbeltString(ancestor)
		if err != nil {
			return "", err
		}
		lines = append(lines, "  (literal "+literal+")")
	}
	lines = append(lines,
		")",
		"(allow file-read*",
		"  (literal \"/dev/null\")",
		"  (literal \"/dev/urandom\")",
		"  (subpath \"/bin\")",
		"  (subpath \"/usr/bin\")",
		"  (subpath \"/usr/lib\")",
		"  (subpath \"/System/Library\")",
		"  (subpath \"/private/var/db/dyld\")",
		"  (literal "+quoted[2]+"))",
		"(allow file-read* file-write*",
		"  (subpath "+quoted[0]+")",
		"  (subpath "+quoted[1]+"))",
	)
	for _, path := range readWrite {
		literal, err := seatbeltString(path)
		if err != nil {
			return "", err
		}
		lines = append(lines, "(allow file-read* file-write* (subpath "+literal+"))")
	}
	for _, path := range readOnly {
		literal, err := seatbeltString(path)
		if err != nil {
			return "", err
		}
		info, err := os.Stat(path)
		if err != nil {
			return "", err
		}
		selector := "literal"
		if info.IsDir() {
			selector = "subpath"
		}
		lines = append(lines, "(allow file-read* ("+selector+" "+literal+"))")
	}
	if network == environment.NetworkEnabled {
		lines = append(lines, "(allow network*)")
	}
	return strings.Join(lines, "\n") + "\n", nil
}

func pathAncestors(paths ...string) []string {
	seen := make(map[string]bool)
	var ancestors []string
	for _, path := range paths {
		dir := filepath.Dir(filepath.Clean(path))
		for {
			if !seen[dir] {
				seen[dir] = true
				ancestors = append(ancestors, dir)
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	return ancestors
}

func seatbeltString(value string) (string, error) {
	if value == "" || strings.ContainsAny(value, "\x00\r\n") {
		return "", errors.New("environment: invalid path in Seatbelt policy")
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value) + `"`, nil
}
