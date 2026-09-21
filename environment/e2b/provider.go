package e2b

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/samperrin/pons/environment"
	"github.com/samperrin/pons/plugins/external"
	"github.com/samperrin/pons/protocol"
)

const (
	defaultE2BAPIURL        = "https://api.e2b.app"
	defaultE2BEnvdURL       = "https://sandbox.e2b.app"
	defaultE2BTemplate      = "pons-hands"
	defaultE2BHandsPath     = "/usr/local/bin/pons-hands"
	defaultE2BWorkspace     = "/home/user/pons-workspace"
	workspaceUploadPath     = "/tmp/pons-workspace.tar"
	workspaceCheckpointPath = "/tmp/pons-workspace-out.tar"
	prepareWorkspaceScript  = "rm -rf " + defaultE2BWorkspace +
		" && mkdir -p " + defaultE2BWorkspace +
		" && tar -xf " + workspaceUploadPath + " -C " + defaultE2BWorkspace
	defaultE2BTimeout        = 15 * time.Minute
	defaultE2BWorkspaceBytes = 256 << 20
	e2bSetupGeneration       = 1
)

// Provider provisions or reconnects a workspace-affine E2B sandbox. The
// selected template must contain a Linux pons-hands executable at HandsPath.
// Workspace contents are uploaded on creation and checkpointed after every run.
type Provider struct {
	APIKey            string
	Template          string
	HandsPath         string
	Timeout           time.Duration
	IdleTimeout       time.Duration
	CleanupInterval   time.Duration
	MaxWorkspaceBytes int64
	HTTPClient        *http.Client
	APIURL            string
	EnvdURL           string
	OnError           func(error)

	lifecycleMu sync.Mutex
	stateStore  environment.StateStore
	stopJanitor context.CancelFunc
	janitorDone chan struct{}
	active      map[string]string
}

func (p *Provider) Start(ctx context.Context, spec environment.Spec) (environment.HandsSession, error) {
	cfg, err := p.config()
	if err != nil {
		return nil, err
	}
	workspace, args, network, env, err := validateE2BSpec(spec)
	if err != nil {
		return nil, err
	}
	if spec.Command[0] != cfg.handsPath {
		return nil, fmt.Errorf("environment: E2B spec command %q does not match configured hands path %q", spec.Command[0], cfg.handsPath)
	}
	client := &e2bClient{apiKey: cfg.apiKey, apiURL: cfg.apiURL, envdURL: cfg.envdURL, http: cfg.http}
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	store := p.stateStore
	leaseID := spec.LeaseID
	if p.active == nil {
		p.active = make(map[string]string)
	}
	if activeLease := p.active[workspace]; activeLease != "" {
		return nil, fmt.Errorf("environment: workspace %q already has active E2B lease %q", workspace, activeLease)
	}
	if store != nil && leaseID == "" {
		return nil, errors.New("environment: durable E2B session requires a lease ID")
	}
	sandbox, resumed, err := p.acquireSandbox(ctx, client, cfg, workspace, leaseID, network)
	if err != nil {
		return nil, err
	}
	cleanup := func() error {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		var stateErr error
		if store != nil {
			stateErr = store.DeleteEnvironmentState(cleanupCtx, workspace, sandbox.ID)
		}
		return errors.Join(client.killSandbox(cleanupCtx, sandbox.ID), stateErr)
	}
	if !resumed {
		archive, archiveErr := archiveWorkspace(workspace, cfg.maxWorkspaceBytes)
		if archiveErr != nil {
			return nil, errors.Join(archiveErr, cleanup())
		}
		if err := client.upload(ctx, sandbox, workspaceUploadPath, bytes.NewReader(archive)); err != nil {
			return nil, errors.Join(err, cleanup())
		}
		if _, _, err := client.run(ctx, sandbox, "/bin/sh", []string{
			"-c", prepareWorkspaceScript,
		}, "/home/user", nil); err != nil {
			return nil, errors.Join(fmt.Errorf("environment: prepare E2B workspace: %w", err), cleanup())
		}
		if store != nil {
			now := time.Now().UTC()
			if err := store.SaveEnvironmentState(ctx, environment.State{
				Key:               workspace,
				Provider:          "e2b",
				EnvironmentID:     sandbox.ID,
				Template:          cfg.template,
				Network:           network,
				WorkspaceStrategy: environment.WorkspaceStrategyArchive,
				SetupGeneration:   e2bSetupGeneration,
				Status:            environment.StateActive,
				LeaseID:           leaseID,
				ExpiresAt:         now.Add(cfg.timeout),
				UpdatedAt:         now,
			}); err != nil {
				return nil, errors.Join(err, cleanup())
			}
		}
	} else {
		// A host crash may leave the prior protocol process alive. Runs are
		// recovered as interrupted before they can reacquire this workspace, so
		// it is safe to replace that idle/stale endpoint without retrying a call.
		_, _, _ = client.run(ctx, sandbox, "/usr/bin/pkill", []string{
			"-f", "^" + cfg.handsPath + "( |$)",
		}, "/home/user", nil)
	}
	remoteArgs := append([]string(nil), args...)
	remoteArgs = append(remoteArgs, "--workspace", defaultE2BWorkspace)
	manifest := external.Manifest{
		ManifestVersion: external.ManifestVersion,
		Name:            "pons.hands",
		Entrypoint:      cfg.handsPath,
		Args:            remoteArgs,
		RuntimeProtocol: external.RuntimeProtocol,
	}
	host, err := external.NewConnectionHost(manifest, external.HostConfig{
		Workspace:   defaultE2BWorkspace,
		CallTimeout: spec.Limits.CallTimeout,
		Limits: external.Limits{
			MaxFrameBytes:      spec.Limits.MaxFrameBytes,
			MaxStderrBytes:     spec.Limits.MaxStderrBytes,
			MaxResultBytes:     spec.Limits.MaxResultBytes,
			MaxTools:           spec.Limits.MaxTools,
			MaxPendingRequests: spec.Limits.MaxPendingRequests,
			MaxConcurrency:     spec.Limits.MaxConcurrency,
		},
	}, func(connectCtx context.Context) (external.Connection, error) {
		return client.startConnection(connectCtx, sandbox, cfg.handsPath, remoteArgs, defaultE2BWorkspace, env)
	})
	if err != nil {
		return nil, errors.Join(err, cleanup())
	}
	if err := host.Start(ctx); err != nil {
		return nil, errors.Join(err, host.Close(), cleanup())
	}
	if leaseID == "" {
		leaseID = sandbox.ID
	}
	p.active[workspace] = leaseID
	return &e2bSession{
		host:              host,
		client:            client,
		sandbox:           sandbox,
		workspace:         workspace,
		owner:             p,
		store:             store,
		leaseID:           leaseID,
		template:          cfg.template,
		idleTimeout:       cfg.idleTimeout,
		maxWorkspaceBytes: cfg.maxWorkspaceBytes,
		metadata: environment.Metadata{
			Provider:      "e2b",
			EnvironmentID: sandbox.ID,
			Workspace:     workspace,
			Network:       network,
		},
	}, nil
}

type e2bConfig struct {
	apiKey, template, handsPath, apiURL, envdURL string
	timeout, idleTimeout, cleanupInterval        time.Duration
	maxWorkspaceBytes                            int64
	http                                         *http.Client
	onError                                      func(error)
}

func (p *Provider) config() (e2bConfig, error) {
	cfg := e2bConfig{
		apiKey:            p.APIKey,
		template:          p.Template,
		handsPath:         p.HandsPath,
		timeout:           p.Timeout,
		idleTimeout:       p.IdleTimeout,
		cleanupInterval:   p.CleanupInterval,
		maxWorkspaceBytes: p.MaxWorkspaceBytes,
		apiURL:            strings.TrimRight(p.APIURL, "/"),
		envdURL:           strings.TrimRight(p.EnvdURL, "/"),
		http:              p.HTTPClient,
		onError:           p.OnError,
	}
	if cfg.apiKey == "" {
		cfg.apiKey = os.Getenv("E2B_API_KEY")
	}
	if cfg.apiKey == "" {
		return cfg, errors.New("environment: E2B_API_KEY is required")
	}
	if cfg.template == "" {
		cfg.template = defaultE2BTemplate
	}
	if cfg.handsPath == "" {
		cfg.handsPath = defaultE2BHandsPath
	}
	if !strings.HasPrefix(cfg.handsPath, "/") {
		return cfg, errors.New("environment: E2B hands path must be absolute")
	}
	if cfg.timeout <= 0 {
		cfg.timeout = defaultE2BTimeout
	}
	if cfg.idleTimeout <= 0 {
		cfg.idleTimeout = 10 * time.Minute
	}
	if cfg.cleanupInterval <= 0 {
		cfg.cleanupInterval = time.Minute
	}
	if cfg.maxWorkspaceBytes <= 0 {
		cfg.maxWorkspaceBytes = defaultE2BWorkspaceBytes
	}
	if cfg.apiURL == "" {
		cfg.apiURL = defaultE2BAPIURL
	}
	if cfg.envdURL == "" {
		cfg.envdURL = defaultE2BEnvdURL
	}
	if cfg.http == nil {
		cfg.http = &http.Client{
			Transport: http.DefaultTransport,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	return cfg, nil
}

// SetStateStore enables durable workspace affinity and idle cleanup.
func (p *Provider) SetStateStore(store environment.StateStore) error {
	if store == nil {
		return errors.New("environment: E2B state store is nil")
	}
	cfg, err := p.config()
	if err != nil {
		return err
	}
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	if p.stateStore != nil {
		return errors.New("environment: E2B state store is already configured")
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.stateStore = store
	p.stopJanitor = cancel
	p.janitorDone = make(chan struct{})
	go p.janitor(ctx, cfg)
	return nil
}

// Close stops process-local maintenance. Durable idle sandboxes remain owned
// by the database and can be reconnected after a server restart.
func (p *Provider) Close() error {
	p.lifecycleMu.Lock()
	cancel, done := p.stopJanitor, p.janitorDone
	p.lifecycleMu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	<-done
	p.lifecycleMu.Lock()
	p.stopJanitor, p.janitorDone = nil, nil
	p.lifecycleMu.Unlock()
	return nil
}

func (p *Provider) acquireSandbox(
	ctx context.Context,
	client *e2bClient,
	cfg e2bConfig,
	key, leaseID string,
	network environment.NetworkPolicy,
) (e2bSandbox, bool, error) {
	store := p.stateStore
	if store != nil {
		state, err := store.EnvironmentState(ctx, key)
		switch {
		case err == nil && reusableState(state, cfg, network):
			sandbox, connectErr := client.connectSandbox(ctx, state.EnvironmentID, cfg.timeout)
			if connectErr == nil {
				state.Status = environment.StateActive
				state.LeaseID = leaseID
				state.IdleUntil = time.Time{}
				state.ExpiresAt = time.Now().UTC().Add(cfg.timeout)
				state.UpdatedAt = time.Now().UTC()
				if saveErr := store.SaveEnvironmentState(ctx, state); saveErr != nil {
					return e2bSandbox{}, false, saveErr
				}
				if sandbox.Template == "" {
					sandbox.Template = cfg.template
				}
				return sandbox, true, nil
			}
			if !hasHTTPStatus(connectErr, http.StatusNotFound) {
				return e2bSandbox{}, false, fmt.Errorf("environment: reconnect E2B sandbox %q: %w", state.EnvironmentID, connectErr)
			}
			if deleteErr := store.DeleteEnvironmentState(ctx, key, state.EnvironmentID); deleteErr != nil {
				return e2bSandbox{}, false, deleteErr
			}
		case err == nil:
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			killErr := client.killSandbox(cleanupCtx, state.EnvironmentID)
			deleteErr := store.DeleteEnvironmentState(cleanupCtx, key, state.EnvironmentID)
			cancel()
			if err := errors.Join(killErr, deleteErr); err != nil {
				return e2bSandbox{}, false, err
			}
		case !errors.Is(err, environment.ErrStateNotFound):
			return e2bSandbox{}, false, err
		}
	}
	// Explicit hands variables belong to the short-lived pons-hands process,
	// not the retained sandbox's ambient environment.
	sandbox, err := client.createSandbox(ctx, cfg.template, cfg.timeout, network, nil)
	if err != nil {
		return e2bSandbox{}, false, err
	}
	return sandbox, false, nil
}

func reusableState(state environment.State, cfg e2bConfig, network environment.NetworkPolicy) bool {
	return state.Provider == "e2b" &&
		state.Template == cfg.template &&
		state.Network == network &&
		state.WorkspaceStrategy == environment.WorkspaceStrategyArchive &&
		state.SetupGeneration == e2bSetupGeneration
}

func (p *Provider) janitor(ctx context.Context, cfg e2bConfig) {
	defer close(p.janitorDone)
	ticker := time.NewTicker(cfg.cleanupInterval)
	defer ticker.Stop()
	p.cleanExpired(ctx, cfg)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.cleanExpired(ctx, cfg)
		}
	}
}

func (p *Provider) cleanExpired(ctx context.Context, cfg e2bConfig) {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	if p.stateStore == nil {
		return
	}
	states, err := p.stateStore.ExpiredEnvironmentStates(ctx, "e2b", time.Now().UTC(), 100)
	if err != nil {
		p.report(cfg, err)
		return
	}
	client := &e2bClient{apiKey: cfg.apiKey, apiURL: cfg.apiURL, envdURL: cfg.envdURL, http: cfg.http}
	for _, state := range states {
		if p.active[state.Key] != "" {
			continue
		}
		if err := client.killSandbox(ctx, state.EnvironmentID); err != nil {
			p.report(cfg, err)
			continue
		}
		if err := p.stateStore.DeleteEnvironmentState(ctx, state.Key, state.EnvironmentID); err != nil {
			p.report(cfg, err)
		}
	}
}

func (p *Provider) report(cfg e2bConfig, err error) {
	if err != nil && cfg.onError != nil {
		cfg.onError(err)
	}
}

func validateE2BSpec(spec environment.Spec) (string, []string, environment.NetworkPolicy, map[string]string, error) {
	if spec.Workspace == "" {
		return "", nil, "", nil, errors.New("environment: workspace is required")
	}
	workspace, err := filepath.Abs(spec.Workspace)
	if err != nil {
		return "", nil, "", nil, fmt.Errorf("environment: workspace: %w", err)
	}
	workspace, err = filepath.EvalSymlinks(workspace)
	if err != nil {
		return "", nil, "", nil, fmt.Errorf("environment: workspace: %w", err)
	}
	info, err := os.Stat(workspace)
	if err != nil || !info.IsDir() {
		if err != nil {
			return "", nil, "", nil, fmt.Errorf("environment: workspace: %w", err)
		}
		return "", nil, "", nil, errors.New("environment: workspace must be a directory")
	}
	if len(spec.Command) == 0 {
		return "", nil, "", nil, errors.New("environment: hands command is required")
	}
	if len(spec.ReadOnly) != 0 {
		return "", nil, "", nil, errors.New("environment: E2B does not yet support external plugin paths")
	}
	network := spec.Network
	if network == "" {
		network = environment.NetworkDisabled
	}
	if network != environment.NetworkDisabled && network != environment.NetworkEnabled {
		return "", nil, "", nil, fmt.Errorf("environment: invalid network policy %q", network)
	}
	env := make(map[string]string, len(spec.Environment))
	for _, entry := range spec.Environment {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" || strings.ContainsAny(key, "\x00\n=") || strings.ContainsRune(value, '\x00') {
			return "", nil, "", nil, fmt.Errorf("environment: invalid environment entry %q", entry)
		}
		env[key] = value
	}
	return workspace, append([]string(nil), spec.Command[1:]...), network, env, nil
}

type e2bSession struct {
	host              *external.Host
	client            *e2bClient
	sandbox           e2bSandbox
	workspace         string
	owner             *Provider
	store             environment.StateStore
	leaseID           string
	template          string
	idleTimeout       time.Duration
	maxWorkspaceBytes int64
	metadata          environment.Metadata
	closeOnce         sync.Once
	closeErr          error
}

func (s *e2bSession) Catalog() []external.ToolDescription { return s.host.Tools() }
func (s *e2bSession) Metadata() environment.Metadata      { return s.metadata }
func (s *e2bSession) Execute(ctx context.Context, action protocol.Action) (protocol.ToolResult, error) {
	return s.host.Execute(ctx, action)
}
func (s *e2bSession) Close() error {
	s.closeOnce.Do(func() {
		s.closeErr = s.host.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if _, _, err := s.client.run(ctx, s.sandbox, "/bin/tar", []string{
			"--hard-dereference", "-cf", workspaceCheckpointPath, "-C", defaultE2BWorkspace, ".",
		}, "/home/user", nil); err != nil {
			s.closeErr = errors.Join(s.closeErr, fmt.Errorf("environment: checkpoint E2B workspace: %w", err))
		} else if body, err := s.client.download(ctx, s.sandbox, workspaceCheckpointPath, s.maxWorkspaceBytes); err != nil {
			s.closeErr = errors.Join(s.closeErr, err)
		} else if err := restoreWorkspace(s.workspace, body, s.maxWorkspaceBytes); err != nil {
			s.closeErr = errors.Join(s.closeErr, err)
		}
		if s.closeErr == nil && s.store != nil {
			now := time.Now().UTC()
			idleUntil := now.Add(s.idleTimeout)
			if err := s.client.setSandboxTimeout(ctx, s.sandbox.ID, s.idleTimeout+time.Minute); err != nil {
				s.closeErr = err
			} else {
				s.closeErr = s.store.SaveEnvironmentState(ctx, environment.State{
					Key:               s.workspace,
					Provider:          "e2b",
					EnvironmentID:     s.sandbox.ID,
					Template:          s.template,
					Network:           s.metadata.Network,
					WorkspaceStrategy: environment.WorkspaceStrategyArchive,
					SetupGeneration:   e2bSetupGeneration,
					Status:            environment.StateIdle,
					IdleUntil:         idleUntil,
					ExpiresAt:         idleUntil.Add(time.Minute),
					UpdatedAt:         now,
				})
			}
		}
		if s.store == nil || s.closeErr != nil {
			killCtx, killCancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer killCancel()
			s.closeErr = errors.Join(s.closeErr, s.client.killSandbox(killCtx, s.sandbox.ID))
			if s.store != nil {
				s.closeErr = errors.Join(s.closeErr, s.store.DeleteEnvironmentState(killCtx, s.workspace, s.sandbox.ID))
			}
		}
		if s.owner != nil {
			s.owner.lifecycleMu.Lock()
			if s.owner.active[s.workspace] == s.leaseID {
				delete(s.owner.active, s.workspace)
			}
			s.owner.lifecycleMu.Unlock()
		}
	})
	return s.closeErr
}
