package e2b

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/samperrin/pons/environment"
	"github.com/samperrin/pons/environment/gitworkspace"
	"github.com/samperrin/pons/plugins/external"
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
	OnDebug           func(string)
	GitCredentials    gitworkspace.CredentialSource

	lifecycleMu     sync.Mutex
	stateStore      environment.StateStore
	checkpointStore environment.CheckpointStore
	janitorMu       sync.Mutex
	stopJanitor     context.CancelFunc
	janitorDone     chan struct{}
	active          map[string]string
	workspaceLocks  [64]sync.Mutex
}

func (p *Provider) Start(ctx context.Context, spec environment.Spec) (handsSession environment.HandsSession, startErr error) {
	progress := spec.ReportProgress
	if progress == nil {
		progress = func(string, string) error { return nil }
	}
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
	workspaceID := spec.WorkspaceID
	workspaceLock := p.workspaceLock(workspaceID)
	workspaceLock.Lock()
	defer workspaceLock.Unlock()
	p.lifecycleMu.Lock()
	store, checkpoints := p.stateStore, p.checkpointStore
	if store == nil || checkpoints == nil {
		p.lifecycleMu.Unlock()
		return nil, errors.New("environment: E2B durable workspace stores are required")
	}
	runID := spec.RunID
	if p.active == nil {
		p.active = make(map[string]string)
	}
	if activeRun := p.active[workspaceID]; activeRun != "" {
		p.lifecycleMu.Unlock()
		return nil, fmt.Errorf("environment: workspace %q already has active E2B run %q", workspaceID, activeRun)
	}
	p.lifecycleMu.Unlock()

	if runID == "" {
		return nil, errors.New("environment: durable E2B session requires a run ID")
	}
	_, existingWorkspaceErr := store.WorkspaceState(ctx, workspaceID)
	if existingWorkspaceErr != nil && !errors.Is(existingWorkspaceErr, environment.ErrStateNotFound) {
		return nil, existingWorkspaceErr
	}
	initialSetup := errors.Is(existingWorkspaceErr, environment.ErrStateNotFound)
	if initialSetup {
		if err := progress("workspace.prepare", "Preparing workspace source…"); err != nil {
			return nil, err
		}
	}
	workspaceState, err := loadOrCreateWorkspace(
		ctx,
		store,
		checkpoints,
		workspaceID,
		workspace,
		spec.WorkspacePlan,
		cfg.maxWorkspaceBytes,
		cfg.onError,
	)
	if err != nil {
		return nil, err
	}

	p.debugf("workspace=%q strategy=%s checkpoint=%t", workspaceID, workspaceState.Strategy, workspaceState.CheckpointRef != "")
	var credentials gitworkspace.Credentials
	defer func() { startErr = credentials.RedactError(startErr) }()
	if spec.GitAllRepositories && p.GitCredentials == nil {
		return nil, errors.New("environment: installation-wide Git access requires GitHub App authentication")
	}
	if p.GitCredentials != nil && (spec.WorkspacePlan.Strategy == environment.WorkspaceStrategyGit || spec.GitAllRepositories) {
		if initialSetup {
			if err := progress("git.credentials", "Requesting Git access…"); err != nil {
				return nil, err
			}
		}
		p.debugf("workspace=%q requesting GitHub credentials scope=%s", workspaceID, gitScope(spec.GitAllRepositories))
		credentials, err = p.GitCredentials.Credentials(ctx, spec.WorkspacePlan.SourceRef, spec.GitAllRepositories)
		if err != nil {
			return nil, err
		}
		maps.Copy(env, credentials.Environment())
	}

	if initialSetup {
		if err := progress("sandbox.start", "Starting E2B sandbox…"); err != nil {
			return nil, err
		}
	}
	sandbox, resumed, err := p.acquireSandbox(ctx, client, cfg, workspaceID, runID, network)
	if err != nil {
		return nil, err
	}
	showSetup := initialSetup || !resumed
	cleanup := func() error {
		p.debugf("workspace=%q sandbox=%q deleting after startup failure", workspaceID, sandbox.ID)
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := client.killSandbox(cleanupCtx, sandbox.ID); err != nil {
			return err
		}
		if err := store.DeleteEnvironmentState(cleanupCtx, workspaceID, sandbox.ID); err != nil {
			return err
		}
		p.debugf("workspace=%q sandbox=%q deleted", workspaceID, sandbox.ID)
		return nil
	}

	if !resumed {
		if !initialSetup {
			if err := progress("sandbox.restore", "Restoring workspace in a new E2B sandbox…"); err != nil {
				return nil, errors.Join(err, cleanup())
			}
		}
		initialCheckout := workspaceState.CheckpointRef == ""
		if initialCheckout {
			p.debugf("workspace=%q sandbox=%q provisioning Git checkout repository=%q revision=%s", workspaceID, sandbox.ID, workspaceState.SourceRef, workspaceState.BaseRevision)
		} else {
			p.debugf("workspace=%q sandbox=%q restoring checkpoint=%s", workspaceID, sandbox.ID, workspaceState.CheckpointRef)
		}
		workspaceState, err = placeWorkspace(
			ctx, client, sandbox, store, checkpoints, workspaceState, env, credentials, cfg.maxWorkspaceBytes, cfg.onError, p.OnDebug, progress,
		)
		if err != nil {
			return nil, errors.Join(err, cleanup())
		}
		if !initialCheckout {
			p.debugf("workspace=%q sandbox=%q checkpoint=%s restored", workspaceID, sandbox.ID, workspaceState.CheckpointRef)
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
		p.debugf("workspace=%q sandbox=%q state=active", workspaceID, sandbox.ID)
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
	if showSetup {
		if err := progress("tools.start", "Starting agent tools…"); err != nil {
			return nil, errors.Join(err, host.Close(), cleanup())
		}
	}
	if err := host.Start(ctx); err != nil {
		return nil, errors.Join(err, host.Close(), cleanup())
	}

	if !stopStartupCancel() || ctx.Err() != nil {
		return nil, errors.Join(ctx.Err(), host.Close(), cleanup())
	}
	if showSetup {
		if err := progress("sandbox.ready", "Sandbox ready"); err != nil {
			return nil, errors.Join(err, host.Close(), cleanup())
		}
	}
	started = true
	p.lifecycleMu.Lock()
	p.active[workspaceID] = runID
	p.lifecycleMu.Unlock()

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
		onDebug:           p.OnDebug,
		credentials:       credentials,
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

func (p *Provider) debugf(format string, args ...any) {
	debugE2B(p.OnDebug, format, args...)
}

func (p *Provider) workspaceLock(id string) *sync.Mutex {
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(id))
	return &p.workspaceLocks[hash.Sum32()%uint32(len(p.workspaceLocks))]
}

func debugE2B(debug func(string), format string, args ...any) {
	if debug != nil {
		debug(fmt.Sprintf(format, args...))
	}
}

func gitScope(all bool) string {
	if all {
		return "installation"
	}
	return "repository"
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
			p.debugf("workspace=%q sandbox=%q state=%s blocked_until=%s", key, state.EnvironmentID, state.Status, state.ExpiresAt.UTC().Format(time.RFC3339))
			// A failed metadata write may leave an active record instead of a
			// recovery record. Neither may be replaced before its recorded expiry.
			return e2bSandbox{}, false, fmt.Errorf("environment: sandbox %q is %s; recover workspace %q manually before %s; new runs are blocked until then",
				state.EnvironmentID, state.Status, key, state.ExpiresAt.UTC().Format(time.RFC3339))
		case err == nil && reusableState(state, cfg, network):
			p.debugf("workspace=%q sandbox=%q reconnecting state=idle", key, state.EnvironmentID)
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
				p.debugf("workspace=%q sandbox=%q reconnected state=active", key, sandbox.ID)
				return sandbox, true, nil
			}
			if !hasHTTPStatus(connectErr, http.StatusNotFound) {
				return e2bSandbox{}, false, fmt.Errorf("environment: reconnect E2B sandbox %q: %w", state.EnvironmentID, connectErr)
			}
			if deleteErr := store.DeleteEnvironmentState(ctx, key, state.EnvironmentID); deleteErr != nil {
				return e2bSandbox{}, false, deleteErr
			}
			p.debugf("workspace=%q sandbox=%q missing at provider; creating replacement", key, state.EnvironmentID)
		case err == nil:
			p.debugf("workspace=%q sandbox=%q removing prior state=%s idle_until=%s", key, state.EnvironmentID, state.Status, state.IdleUntil.UTC().Format(time.RFC3339))
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
			p.debugf("workspace=%q sandbox=%q removed", key, state.EnvironmentID)
		case !errors.Is(err, environment.ErrStateNotFound):
			return e2bSandbox{}, false, err
		}
	}
	// Explicit hands variables belong to the short-lived pons-hands process,
	// not the retained sandbox's ambient environment.
	p.debugf("workspace=%q creating sandbox template=%q network=%s", key, cfg.template, network)
	sandbox, err := client.createSandbox(ctx, cfg.template, cfg.timeout, network, nil)
	if err != nil {
		return e2bSandbox{}, false, err
	}
	p.debugf("workspace=%q sandbox=%q created", key, sandbox.ID)
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
	p.lifecycleMu.Lock()
	store := p.stateStore
	p.lifecycleMu.Unlock()
	if store == nil {
		return
	}
	states, err := store.ExpiredEnvironmentStates(ctx, "e2b", time.Now().UTC(), 100)
	if err != nil {
		p.report(cfg, err)
		return
	}
	client := &e2bClient{apiKey: cfg.apiKey, apiURL: cfg.apiURL, envdURL: cfg.envdURL, http: cfg.http}
	for _, state := range states {
		p.cleanExpiredState(ctx, cfg, client, store, state)
	}
}

func (p *Provider) cleanExpiredState(ctx context.Context, cfg e2bConfig, client *e2bClient, store environment.StateStore, state environment.State) {
	workspaceLock := p.workspaceLock(state.WorkspaceID)
	// Maintenance can wait for the next sweep if this workspace is busy.
	if !workspaceLock.TryLock() {
		return
	}
	defer workspaceLock.Unlock()
	p.lifecycleMu.Lock()
	active := p.active[state.WorkspaceID] != ""
	p.lifecycleMu.Unlock()
	if active {
		return
	}
	p.debugf("workspace=%q sandbox=%q cleanup expired state=%s", state.WorkspaceID, state.EnvironmentID, state.Status)
	if err := client.killSandbox(ctx, state.EnvironmentID); err != nil {
		p.report(cfg, err)
		return
	}
	if err := store.DeleteEnvironmentState(ctx, state.WorkspaceID, state.EnvironmentID); err != nil {
		p.report(cfg, err)
	} else {
		p.debugf("workspace=%q sandbox=%q deleted", state.WorkspaceID, state.EnvironmentID)
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
	if spec.WorkspacePath == "" && spec.WorkspacePlan.Strategy != environment.WorkspaceStrategyGit {
		return "", nil, "", nil, errors.New("environment: workspace is required")
	}
	workspace := spec.WorkspacePath
	if workspace != "" {
		var err error
		workspace, err = filepath.Abs(workspace)
		if err != nil {
			return "", nil, "", nil, fmt.Errorf("environment: workspace: %w", err)
		}
	}
	if len(spec.Command) == 0 {
		return "", nil, "", nil, errors.New("environment: hands command is required")
	}
	if len(spec.ReadOnly) != 0 {
		return "", nil, "", nil, errors.New("environment: E2B does not yet support external plugin paths")
	}
	if len(spec.ReadWrite) != 0 {
		return "", nil, "", nil, errors.New("environment: E2B does not yet support read-write host grants")
	}
	network := spec.Network
	if network == "" {
		network = environment.NetworkDisabled
	}
	if network != environment.NetworkDisabled && network != environment.NetworkEnabled {
		return "", nil, "", nil, fmt.Errorf("environment: invalid network policy %q", network)
	}
	if err := gitworkspace.ValidatePlan(spec.WorkspacePlan); err != nil {
		return "", nil, "", nil, err
	}
	if (spec.WorkspacePlan.Strategy == environment.WorkspaceStrategyGit || spec.GitAllRepositories) && network != environment.NetworkEnabled {
		return "", nil, "", nil, errors.New("environment: Git workspace access requires network access")
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
