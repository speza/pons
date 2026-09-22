package e2b

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	defaultE2BTimeout         = 15 * time.Minute
	defaultE2BWorkspaceBytes  = 256 << 20
	checkpointRecoveryTimeout = time.Hour
	e2bSetupGeneration        = 1
)

// Provider provisions or reconnects a workspace-affine E2B sandbox. The
// selected template must contain a Linux pons-hands executable at HandsPath.
// A local source seeds each logical workspace once; replacement sandboxes load
// its latest durable checkpoint. Completed runs produce a new checkpoint.
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

	lifecycleMu     sync.Mutex
	stateStore      environment.StateStore
	checkpointStore environment.CheckpointStore
	janitorMu       sync.Mutex
	stopJanitor     context.CancelFunc
	janitorDone     chan struct{}
	active          map[string]string
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
	store, checkpoints := p.stateStore, p.checkpointStore
	if store == nil || checkpoints == nil {
		return nil, errors.New("environment: E2B durable workspace stores are required")
	}
	workspaceID := spec.WorkspaceID
	runID := spec.RunID
	if p.active == nil {
		p.active = make(map[string]string)
	}
	if activeRun := p.active[workspaceID]; activeRun != "" {
		return nil, fmt.Errorf("environment: workspace %q already has active E2B run %q", workspaceID, activeRun)
	}
	if runID == "" {
		return nil, errors.New("environment: durable E2B session requires a run ID")
	}
	workspaceState, err := loadOrCreateWorkspace(
		ctx,
		store,
		checkpoints,
		workspaceID,
		workspace,
		cfg.maxWorkspaceBytes,
		cfg.onError,
	)
	if err != nil {
		return nil, err
	}
	sandbox, resumed, err := p.acquireSandbox(ctx, client, cfg, workspaceID, runID, network)
	if err != nil {
		return nil, err
	}
	cleanup := func() error {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := client.killSandbox(cleanupCtx, sandbox.ID); err != nil {
			return err
		}
		return store.DeleteEnvironmentState(cleanupCtx, workspaceID, sandbox.ID)
	}
	if !resumed {
		archive, archiveErr := checkpoints.WorkspaceCheckpoint(
			ctx,
			workspaceID,
			workspaceState.CheckpointRef,
			cfg.maxWorkspaceBytes,
		)
		if archiveErr != nil {
			return nil, errors.Join(archiveErr, cleanup())
		}
		uploadErr := client.upload(ctx, sandbox, workspaceUploadPath, archive)
		if err := errors.Join(uploadErr, archive.Close()); err != nil {
			return nil, errors.Join(err, cleanup())
		}
		if _, _, err := client.run(ctx, sandbox, "/bin/sh", []string{
			"-c", prepareWorkspaceScript,
		}, "/home/user", nil); err != nil {
			return nil, errors.Join(fmt.Errorf("environment: prepare E2B workspace: %w", err), cleanup())
		}
		now := time.Now().UTC()
		if err := store.SaveEnvironmentState(ctx, environment.State{
			WorkspaceID:   workspaceID,
			Provider:      "e2b",
			EnvironmentID: sandbox.ID,
			Template:      cfg.template,
			Network:       network,
			Status:        environment.StateActive,
			RunID:         runID,
			ExpiresAt:     now.Add(cfg.timeout),
			UpdatedAt:     now,
		}); err != nil {
			return nil, errors.Join(err, cleanup())
		}
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
	// Cancellation owns startup and tool calls, but a successfully started
	// transport must stay alive for the bounded shutdown/checkpoint sequence.
	sessionCtx, cancelSession := context.WithCancel(context.WithoutCancel(ctx))
	stopStartupCancel := context.AfterFunc(ctx, cancelSession)
	started := false
	defer func() {
		stopStartupCancel()
		if !started {
			cancelSession()
		}
	}()
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
	}, func(context.Context) (external.Connection, error) {
		return client.startConnection(sessionCtx, sandbox, cfg.handsPath, remoteArgs, defaultE2BWorkspace, env)
	})
	if err != nil {
		return nil, errors.Join(err, cleanup())
	}
	if err := host.Start(ctx); err != nil {
		return nil, errors.Join(err, host.Close(), cleanup())
	}
	if !stopStartupCancel() || ctx.Err() != nil {
		return nil, errors.Join(ctx.Err(), host.Close(), cleanup())
	}
	started = true
	p.active[workspaceID] = runID
	session := &e2bSession{
		host:              host,
		cancelTransport:   cancelSession,
		client:            client,
		sandbox:           sandbox,
		workspace:         workspaceState,
		owner:             p,
		store:             store,
		checkpoints:       checkpoints,
		runID:             runID,
		template:          cfg.template,
		idleTimeout:       cfg.idleTimeout,
		timeout:           cfg.timeout,
		maxWorkspaceBytes: cfg.maxWorkspaceBytes,
		onError:           cfg.onError,
		metadata: environment.Metadata{
			Provider:      "e2b",
			EnvironmentID: sandbox.ID,
			WorkspaceID:   workspaceID,
			WorkspacePath: defaultE2BWorkspace,
			Platform:      "linux/amd64",
			Network:       network,
		},
	}
	session.startKeepalive()
	return session, nil
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
	} else if cfg.timeout < time.Second {
		cfg.timeout = time.Second
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

// SetStores enables durable workspace checkpoints, affinity, and idle cleanup.
func (p *Provider) SetStores(store environment.StateStore, checkpoints environment.CheckpointStore) error {
	if store == nil || checkpoints == nil {
		return errors.New("environment: E2B durable workspace stores are nil")
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
	p.checkpointStore = checkpoints
	p.janitorMu.Lock()
	defer p.janitorMu.Unlock()
	p.stopJanitor = cancel
	p.janitorDone = make(chan struct{})
	go p.janitor(ctx, cfg)
	return nil
}

// Close stops process-local maintenance. Durable idle sandboxes remain owned
// by the database and can be reconnected after a server restart.
func (p *Provider) Close() error {
	// Never acquire the lock held by maintenance I/O before canceling it.
	p.janitorMu.Lock()
	defer p.janitorMu.Unlock()
	cancel, done := p.stopJanitor, p.janitorDone
	if cancel == nil {
		return nil
	}
	cancel()
	<-done
	p.stopJanitor, p.janitorDone = nil, nil
	return nil
}

func loadOrCreateWorkspace(
	ctx context.Context,
	store environment.StateStore,
	checkpoints environment.CheckpointStore,
	workspaceID string,
	sourcePath string,
	limit int64,
	onError func(error),
) (environment.WorkspaceState, error) {
	state, err := store.WorkspaceState(ctx, workspaceID)
	if err == nil {
		if state.Strategy != environment.WorkspaceStrategyArchive ||
			state.SetupGeneration != e2bSetupGeneration ||
			state.CheckpointRef == "" {
			return environment.WorkspaceState{}, fmt.Errorf("environment: workspace %q is incompatible with the requested archive source", workspaceID)
		}
		return state, nil
	}
	if !errors.Is(err, environment.ErrStateNotFound) {
		return environment.WorkspaceState{}, err
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

func (p *Provider) acquireSandbox(
	ctx context.Context,
	client *e2bClient,
	cfg e2bConfig,
	key, runID string,
	network environment.NetworkPolicy,
) (e2bSandbox, bool, error) {
	store := p.stateStore
	if store != nil {
		state, err := store.EnvironmentState(ctx, key)
		switch {
		case err == nil && state.Status != environment.StateIdle && time.Now().Before(state.ExpiresAt):
			// A failed metadata write may leave an active record instead of a
			// recovery record. Neither may be replaced before its recorded expiry.
			return e2bSandbox{}, false, fmt.Errorf("environment: sandbox %q is %s; recover workspace %q manually before %s; new runs are blocked until then",
				state.EnvironmentID, state.Status, key, state.ExpiresAt.UTC().Format(time.RFC3339))
		case err == nil && reusableState(state, cfg, network):
			sandbox, connectErr := client.connectSandbox(ctx, state.EnvironmentID, cfg.timeout)
			if connectErr == nil {
				state.Status = environment.StateActive
				state.RunID = runID
				state.IdleUntil = time.Time{}
				state.ExpiresAt = time.Now().UTC().Add(cfg.timeout)
				state.UpdatedAt = time.Now().UTC()
				if saveErr := store.SaveEnvironmentState(ctx, state); saveErr != nil {
					return e2bSandbox{}, false, saveErr
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
			if err := client.killSandbox(cleanupCtx, state.EnvironmentID); err != nil {
				cancel()
				return e2bSandbox{}, false, err
			}
			deleteErr := store.DeleteEnvironmentState(cleanupCtx, key, state.EnvironmentID)
			cancel()
			if deleteErr != nil {
				return e2bSandbox{}, false, deleteErr
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
		state.Status == environment.StateIdle &&
		state.Template == cfg.template &&
		state.Network == network
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
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	// Maintenance can wait for the next sweep. Never block shutdown behind a
	// startup holding the lifecycle lock while doing network I/O.
	if !p.lifecycleMu.TryLock() {
		return
	}
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
		if p.active[state.WorkspaceID] != "" {
			continue
		}
		if err := client.killSandbox(ctx, state.EnvironmentID); err != nil {
			p.report(cfg, err)
			continue
		}
		if err := p.stateStore.DeleteEnvironmentState(ctx, state.WorkspaceID, state.EnvironmentID); err != nil {
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
	if strings.TrimSpace(spec.WorkspaceID) == "" || strings.ContainsRune(spec.WorkspaceID, '\x00') {
		return "", nil, "", nil, errors.New("environment: workspace ID is required")
	}
	if spec.WorkspacePath == "" {
		return "", nil, "", nil, errors.New("environment: workspace is required")
	}
	workspace, err := filepath.Abs(spec.WorkspacePath)
	if err != nil {
		return "", nil, "", nil, fmt.Errorf("environment: workspace: %w", err)
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
			return "", nil, "", nil, errors.New("environment: invalid E2B environment entry")
		}
		env[key] = value
	}
	return workspace, append([]string(nil), spec.Command[1:]...), network, env, nil
}

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
	metadata          environment.Metadata
	keepaliveCancel   context.CancelFunc
	keepaliveDone     chan struct{}
	closeOnce         sync.Once
	closeErr          error
}

func (s *e2bSession) Catalog() []external.ToolDescription { return s.host.Tools() }
func (s *e2bSession) Metadata() environment.Metadata      { return s.metadata }
func (s *e2bSession) Execute(ctx context.Context, action protocol.Action) (protocol.ToolResult, error) {
	return s.host.Execute(ctx, action)
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
	// The new reference is durable before pruning. Retain the immutable base
	// checkpoint and report best-effort cleanup without invalidating the run.
	keep := []string{s.workspace.BaseRevision, s.workspace.CheckpointRef}
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
		retain := s.closeErr == nil && s.store != nil
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
		if s.closeErr == nil {
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
				s.closeErr = fmt.Errorf("environment: sandbox %q was not deleted, but recovery retention could not be fully recorded or extended; recover files immediately (provider TTL may expire sooner): %w", s.sandbox.ID, s.closeErr)
			} else {
				s.closeErr = fmt.Errorf("environment: sandbox %q retained for manual recovery until %s; new runs are blocked until then: %w",
					s.sandbox.ID, recoveryUntil.Format(time.RFC3339), s.closeErr)
			}
		} else if s.store == nil || s.closeErr != nil {
			killCtx, killCancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer killCancel()
			killErr := s.client.killSandbox(killCtx, s.sandbox.ID)
			s.closeErr = errors.Join(s.closeErr, killErr)
			if killErr == nil && s.store != nil {
				s.closeErr = errors.Join(s.closeErr, s.store.DeleteEnvironmentState(killCtx, s.workspace.ID, s.sandbox.ID))
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
