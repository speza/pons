package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/samperrin/pons"
	"github.com/samperrin/pons/environment"
	"github.com/samperrin/pons/environment/e2b"
	"github.com/samperrin/pons/environment/gitworkspace"
	"github.com/samperrin/pons/plugins/brain/llm"
	"github.com/samperrin/pons/protocol"
	ponsruntime "github.com/samperrin/pons/runtime"
	"github.com/samperrin/pons/runtime/agentdir"
	"github.com/samperrin/pons/runtime/checkpoint"
	"github.com/samperrin/pons/runtime/httptransport"
	runtimesqlite "github.com/samperrin/pons/runtime/sqlite"
)

type serverOptions struct {
	Address                 string
	StateDir                string
	WorkspaceRoot           string
	ClientWorkspace         string
	MaxConcurrent           int
	MaxTurns                int
	Brain                   llm.Config
	ProviderSlot            string
	FSReadBytes             int
	BashTimeout             int
	BashMaxLines            int
	BashMaxBytes            int
	PluginPaths             []string
	PluginPath              string
	PluginMaxResultBytes    int
	Debug                   bool
	Sandbox                 string
	E2BTemplate             string
	E2BAPIKey               string
	E2BHandsPath            string
	GitRepository           string
	GitRevision             string
	GitAllRepositories      bool
	GitHubAppID             int64
	GitHubAppInstallationID int64
	GitHubAppPrivateKey     string
	SandboxIdleTimeout      time.Duration
	EnvironmentError        func(error)
	EnvironmentDebug        func(string)
	Environment             environment.Provider
	EnvironmentSpec         environment.Spec
}

func newServerLogger(output io.Writer, debug bool) *slog.Logger {
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewJSONHandler(output, &slog.HandlerOptions{Level: level}))
}

func logE2BDebug(logger *slog.Logger, event string) {
	var fields []any
	for _, name := range []string{"workspace", "sandbox"} {
		prefix := name + "="
		if !strings.HasPrefix(event, prefix) {
			break
		}
		value, remainder, ok := strings.Cut(event[len(prefix):], " ")
		if !ok {
			value, remainder = event[len(prefix):], ""
		}
		decoded, err := strconv.Unquote(value)
		if err != nil {
			break
		}
		fields = append(fields, name+"_id", decoded)
		event = remainder
	}
	for token := range strings.FieldsSeq(event) {
		name, value, ok := strings.Cut(token, "=")
		if !ok {
			continue
		}
		switch name {
		case "state", "strategy", "checkpoint", "scope", "network", "revision", "blocked_until", "idle_until", "until":
			fields = append(fields, name, value)
		}
	}
	logger.Debug("e2b lifecycle", append(fields, "event", event)...)
}

func runServer(ctx context.Context, logger *slog.Logger, opts serverOptions) error {
	return runServerReady(ctx, logger, opts, nil)
}

func runServerReady(ctx context.Context, logger *slog.Logger, opts serverOptions, started chan<- string) error {
	if err := validateLoopbackAddress(opts.Address); err != nil {
		return err
	}
	if opts.Environment == nil || opts.Sandbox == "" {
		return errors.New("runtime server: hands run only in a sandbox; use -sandbox seatbelt (macOS) or -sandbox e2b")
	}

	logDebugConfiguration(logger, opts)
	store, err := runtimesqlite.Open(opts.StateDir)
	if err != nil {
		return err
	}
	defer store.Close()
	if durable, ok := opts.Environment.(environment.DurableProvider); ok {
		checkpoints := checkpoint.New(filepath.Join(opts.StateDir, "workspaces"))
		if err := durable.SetStores(store, checkpoints); err != nil {
			return fmt.Errorf("configure environment state: %w", err)
		}
	}
	if closer, ok := opts.Environment.(interface{ Close() error }); ok {
		defer func() {
			if err := closer.Close(); err != nil {
				logger.Error("environment shutdown failed", "error", err)
			}
		}()
	}

	agents, err := agentdir.Open(opts.StateDir)
	if err != nil {
		return err
	}
	agent, err := defaultAgent(opts, agents)
	if err != nil {
		return err
	}
	logger.Info("agent ready", "agent_id", agent.ID, "agent_name", agent.Name,
		"agent_revision", agent.Revision(), "agent_dir", agents.Dir(agent.ID))

	environmentOptions := []string{opts.Sandbox}
	network := opts.EnvironmentSpec.Network
	logger.Info("hands sandbox ready", "sandbox", opts.Sandbox, "network", network)
	if network != environment.NetworkEnabled {
		logger.Info("hands have no network access; use -sandbox-network to allow it")
	}

	runner := &agentRunner{opts: opts, logger: logger, agents: agents}
	backgroundErrors := make(chan error, 1)
	manager, err := ponsruntime.New(ponsruntime.Config{
		Store:              store,
		Runner:             runner,
		Agent:              agent,
		AgentRevisions:     agents,
		MaxConcurrent:      opts.MaxConcurrent,
		EnvironmentOptions: environmentOptions,
		DefaultEnvironment: opts.Sandbox,
		PrepareConversation: func(selection ponsruntime.ConversationOptions) (ponsruntime.ConversationOptions, error) {
			if (selection.GitRepository == "") != (selection.GitRevision == "") {
				return selection, errors.New("git repository and revision must be set together")
			}
			if selection.GitRepository != "" {
				if opts.Sandbox != "e2b" || selection.Environment != "e2b" {
					return selection, errors.New("git workspace requires E2B sandbox")
				}
				if selection.Workspace != "" {
					return selection, errors.New("git conversation cannot also select a host workspace")
				}
				if err := gitworkspace.ValidatePlan(environment.WorkspacePlan{
					Strategy:     environment.WorkspaceStrategyGit,
					SourceRef:    selection.GitRepository,
					BaseRevision: selection.GitRevision,
				}); err != nil {
					return selection, err
				}
			}
			if selection.GitRepository == "" {
				var err error
				selection.Workspace, err = validateConversationWorkspace(selection.Workspace, opts.WorkspaceRoot, opts.StateDir)
				if err != nil {
					return selection, err
				}
			}
			if selection.GitAllRepositories {
				if selection.Environment != "e2b" {
					return selection, errors.New("installation-wide Git access requires E2B sandbox")
				}
				provider, ok := opts.Environment.(*e2b.Provider)
				if !ok || provider.GitCredentials == nil {
					return selection, errors.New("installation-wide Git access requires E2B with GitHub App authentication")
				}
			}
			return selection, nil
		},

		OnError: func(err error) {
			logger.Error("runtime background failure", "error", err)
			select {
			case backgroundErrors <- err:
			default:
			}
		},
	})
	if err != nil {
		return err
	}

	defer manager.Close()
	server := &http.Server{
		Addr: opts.Address,
		Handler: loopbackRequestOnly(httptransport.HandlerWithOptions(manager, httptransport.HandlerOptions{
			Agent:              manager.Agent(),
			Environments:       environmentOptions,
			DefaultEnvironment: opts.Sandbox,
		})),

		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	listener, err := net.Listen("tcp", opts.Address)
	if err != nil {
		return fmt.Errorf("runtime server: listen: %w", err)
	}
	logger.Info("runtime listening", "address", listener.Addr().String())
	if started != nil {
		select {
		case started <- "http://" + listener.Addr().String():
		case <-ctx.Done():
			_ = listener.Close()
			return ctx.Err()
		}
	}

	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case backgroundErr := <-backgroundErrors:
		closeErr := server.Close()
		serveErr := <-done
		if errors.Is(serveErr, http.ErrServerClosed) {
			serveErr = nil
		}
		return errors.Join(backgroundErr, closeErr, serveErr)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		shutdownDone := make(chan error, 1)
		go func() { shutdownDone <- server.Shutdown(shutdownCtx) }()
		// Shutdown stops accepting connections but waits for active handlers.
		// Close the runtime concurrently so SSE subscriptions terminate instead
		// of holding graceful shutdown open until its deadline.
		managerErr := manager.Close()
		shutdownErr := <-shutdownDone
		serveErr := <-done
		if errors.Is(serveErr, http.ErrServerClosed) {
			serveErr = nil
		}
		return errors.Join(managerErr, shutdownErr, serveErr)
	}
}

// defaultAgent loads the default agent's directory, resolves its settings
// against the server's provider chain and limits, and records the resulting
// revision. Empty settings use the server's defaults.
func defaultAgent(opts serverOptions, agents *agentdir.Store) (ponsruntime.AgentDefinition, error) {
	settings, persona, err := agents.Load(ponsruntime.DefaultAgentID)
	if err != nil {
		return ponsruntime.AgentDefinition{}, err
	}

	slot := opts.ProviderSlot
	model := opts.Brain.Model
	if settings.Provider != "" && settings.Provider != slot {
		index := fallbackIndex(opts.Brain.Fallbacks, settings.Provider)
		if index < 0 {
			return ponsruntime.AgentDefinition{}, fmt.Errorf("agent %s: provider slot %q is not configured",
				agents.Dir(ponsruntime.DefaultAgentID), settings.Provider)
		}
		slot, model = settings.Provider, opts.Brain.Fallbacks[index].Model
	}
	if settings.Model != "" {
		model = settings.Model
	}
	maxTurns := opts.MaxTurns
	if settings.MaxTurns > 0 {
		maxTurns = settings.MaxTurns
	}

	agent := ponsruntime.AgentDefinition{
		ID:           ponsruntime.DefaultAgentID,
		Name:         settings.Name,
		Persona:      persona,
		ProviderSlot: slot,
		Model:        model,
		MaxTurns:     maxTurns,
		PluginPaths:  slices.Clone(opts.PluginPaths),
	}
	if err := agents.Record(agent); err != nil {
		return ponsruntime.AgentDefinition{}, err
	}
	return agent, nil
}

func fallbackIndex(fallbacks []llm.Fallback, id string) int {
	return slices.IndexFunc(fallbacks, func(fallback llm.Fallback) bool { return fallback.ID == id })
}

// brainConfig selects the agent revision's provider slot as the primary,
// keeping the rest of the configured chain as fallbacks in order.
func (r *agentRunner) brainConfig(agent ponsruntime.AgentDefinition) (llm.Config, error) {
	config := r.opts.Brain
	config.Logger = nil
	if agent.ProviderSlot != r.opts.ProviderSlot {
		index := fallbackIndex(config.Fallbacks, agent.ProviderSlot)
		if index < 0 {
			return llm.Config{}, fmt.Errorf("agent revision uses provider slot %q, which is not configured", agent.ProviderSlot)
		}

		selected := config.Fallbacks[index]
		primary := llm.Fallback{
			ID: r.opts.ProviderSlot, Provider: config.Provider, Model: config.Model,
			BaseURL: config.BaseURL, APIKey: config.APIKey,
		}
		config.Fallbacks = slices.Concat([]llm.Fallback{primary}, config.Fallbacks[:index], config.Fallbacks[index+1:])
		config.ID, config.Provider = selected.ID, selected.Provider
		config.BaseURL, config.APIKey = selected.BaseURL, selected.APIKey
	}
	config.Model = agent.Model
	config.Persona = personaPrompt(agent)
	return config, nil
}

// personaPrompt renders the agent's name and persona instructions as the
// brain's identity section.
// An unnamed agent must not borrow a name from its model's training.
const unnamedIdentity = "Your owner has not named you yet. If asked your name, say so " +
	"rather than using another assistant's name."

func personaPrompt(agent ponsruntime.AgentDefinition) string {
	identity := unnamedIdentity
	introduce := "introduce yourself"
	if agent.Name != "" {
		identity = "Your name is " + agent.Name + "."
		introduce = "introduce yourself as " + agent.Name
	}
	if agent.Persona == "" {
		return identity + "\n\n" + fmt.Sprintf(onboarding, introduce)
	}
	return identity + "\n\n" + agent.Persona
}

// onboarding stands in for an empty PERSONA.md. The owner shapes a new agent
// by talking to it; what the agent learns goes to memory, while PERSONA.md
// stays owner-written.
const onboarding = "You are new: your owner has not written standing instructions for you yet. " +
	"At the start of a conversation, briefly %s, say that you are new, and ask what they would like " +
	"help with; then help with whatever they ask. Save what you learn about how they want you to " +
	"work in your memory."

func loopbackRequestOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		ip := net.ParseIP(host)
		origin := r.Header.Get("Origin")
		if err != nil || (host != "localhost" && (ip == nil || !ip.IsLoopback())) ||
			(origin != "" && origin != "http://"+r.Host) {
			http.Error(w, "request must originate from a loopback client", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func logDebugConfiguration(logger *slog.Logger, opts serverOptions) {

	if !opts.Debug {
		return
	}
	model := opts.Brain.Model
	if model == "" {
		model = "provider-default"
	}
	logger.Debug("runtime configured", "provider", opts.Brain.Provider, "model", model,
		"workspace_root", opts.WorkspaceRoot, "max_turns", opts.MaxTurns, "max_concurrent", opts.MaxConcurrent)

	hands := ""
	if len(opts.EnvironmentSpec.Command) > 0 {
		hands = opts.EnvironmentSpec.Command[0]
	}
	logger.Debug("hands configured", "sandbox", opts.Sandbox, "network", opts.EnvironmentSpec.Network, "hands", hands,
		"idle_timeout", opts.SandboxIdleTimeout, "external_plugins", len(opts.PluginPaths), "read_only_paths", len(opts.EnvironmentSpec.ReadOnly))
}

func runBundled(ctx context.Context, logger *slog.Logger, opts serverOptions, conversationID, idempotencyKey, message string, interactive bool) error {
	serverCtx, stopServer := context.WithCancel(ctx)
	defer stopServer()
	opts.Address = "127.0.0.1:0"
	started := make(chan string)
	done := make(chan error, 1)
	go func() { done <- runServerReady(serverCtx, logger, opts, started) }()
	var serverURL string
	select {
	case serverURL = <-started:
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
	clientErr := runClient(ctx, serverURL, conversationID, idempotencyKey, message, interactive, opts.Debug,
		ponsruntime.ConversationOptions{
			Workspace:     opts.ClientWorkspace,
			GitRepository: opts.GitRepository, GitRevision: opts.GitRevision,
			GitAllRepositories: opts.GitAllRepositories,
		})
	stopServer()
	serverErr := <-done
	if errors.Is(serverErr, context.Canceled) {
		serverErr = nil
	}
	return errors.Join(clientErr, serverErr)
}

func validateLoopbackAddress(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("runtime server: address: %w", err)
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("runtime server: v1 must bind to a loopback address")
	}
	return nil
}

func validateRemoteStateDirectory(workspace, stateDir string) error {
	workspacePath, err := resolvePathWithMissingLeaf(workspace)
	if err != nil {
		return fmt.Errorf("runtime server: resolve workspace: %w", err)
	}
	statePath, err := resolvePathWithMissingLeaf(stateDir)
	if err != nil {
		return fmt.Errorf("runtime server: resolve state directory: %w", err)
	}
	contained, err := pathContains(workspacePath, statePath)
	if err != nil {
		return fmt.Errorf("runtime server: compare workspace and state directory: %w", err)
	}
	if contained {
		return errors.New("runtime server: remote workspace state directory must be outside the source workspace")
	}
	return nil
}

func validateConversationWorkspace(workspace, root, stateDir string) (string, error) {
	if workspace == "" || !filepath.IsAbs(workspace) {
		return "", errors.New("host workspace must be an absolute path")
	}
	resolved, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return "", fmt.Errorf("resolve host workspace: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("stat host workspace: %w", err)
	}
	if !info.IsDir() {
		return "", errors.New("host workspace must be a directory")
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve workspace root: %w", err)
	}
	allowed, err := pathContains(resolvedRoot, resolved)
	if err != nil {
		return "", err
	}
	if !allowed {
		return "", fmt.Errorf("host workspace %q is outside allowed root %q", workspace, root)
	}
	if err := validateRemoteStateDirectory(resolved, stateDir); err != nil {
		return "", err
	}
	return resolved, nil
}

func pathContains(parent, child string) (bool, error) {
	relative, err := filepath.Rel(parent, child)
	if err != nil {
		return false, err
	}
	if relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))) {
		return true, nil
	}
	parentInfo, err := os.Stat(parent)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for current := child; ; current = filepath.Dir(current) {
		info, statErr := os.Stat(current)
		if statErr == nil && os.SameFile(parentInfo, info) {
			return true, nil
		}
		if statErr != nil && !os.IsNotExist(statErr) {
			return false, statErr
		}
		if next := filepath.Dir(current); next == current {
			return false, nil
		}
	}
}

func resolvePathWithMissingLeaf(path string) (string, error) {
	current, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	var missing []string
	for {
		resolved, resolveErr := filepath.EvalSymlinks(current)
		if resolveErr == nil {
			for _, part := range slices.Backward(missing) {
				resolved = filepath.Join(resolved, part)
			}
			return filepath.Clean(resolved), nil
		}
		if !os.IsNotExist(resolveErr) {
			return "", resolveErr
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", resolveErr
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

type agentRunner struct {
	opts   serverOptions
	logger *slog.Logger
	// agents supplies each run's memory directory; nil runs without memory.
	agents *agentdir.Store
}

func (r *agentRunner) Run(ctx context.Context, request ponsruntime.RunRequest) (result ponsruntime.RunResult, err error) {
	agent := request.Agent
	if err := agent.Validate(); err != nil {
		return result, err
	}
	if request.GitRepository == "" {
		if _, err := validateConversationWorkspace(request.Workspace, r.opts.WorkspaceRoot, r.opts.StateDir); err != nil {
			return result, fmt.Errorf("run host workspace: %w", err)
		}
	}

	brainConfig, err := r.brainConfig(agent)
	if err != nil {
		return result, err
	}
	provider, spec, err := r.executionEnvironment(request.Environment)
	if err != nil {
		return result, err
	}
	// Memory is the one part of the agent directory hands may use. Seatbelt
	// grants it in place; E2B copies it in and applies the run's changes back
	// when the session closes.
	var memoryGrant []string
	if r.agents != nil {
		memory, err := r.agents.Memory(agent.ID, agentdir.DefaultMemoryBudget)
		if err != nil {
			return result, err
		}
		brainConfig.Memory = &llm.Memory{Index: memory.Index, Truncated: memory.Truncated}
		memoryGrant = []string{memory.Path}
	}
	// Build the brain before provisioning hands so configuration errors fail
	// fast; what the hands see arrives with Seed.
	brain, err := llm.New(brainConfig)
	if err != nil {
		return result, fmt.Errorf("brain: %w", err)
	}
	defer func() { err = errors.Join(err, brain.Close(context.Background())) }()

	turns, turnsErr := runtimeTurns(request.Messages)
	if turnsErr != nil {
		return result, turnsErr
	}

	core := pons.New()
	core.Workspace, core.MaxTurns = request.Workspace, agent.MaxTurns
	runLog := r.logger.With("conversation_id", request.ConversationID, "run_id", request.RunID,
		"workspace_id", request.ConversationID, "agent_id", agent.ID, "agent_revision", agent.Revision())
	runLog.Info("run started")
	defer func() {
		if err != nil {
			runLog.Error("run failed", "error", err)
		} else {
			runLog.Info("run completed")
		}
	}()

	// Hands always run in an execution environment; there is no in-process
	// fallback. Only the memory directory is granted beyond the workspace;
	// the rest of the agent directory stays outside every grant. Concat
	// copies so concurrent runs never share a slice.
	spec.ReadWrite = slices.Concat(spec.ReadWrite, memoryGrant)
	spec.WorkspaceID = request.ConversationID
	spec.WorkspacePath = request.Workspace
	spec.RunID = request.RunID
	spec.WorkspacePlan = environment.WorkspacePlan{}
	if request.GitRepository != "" {
		spec.WorkspacePlan = environment.WorkspacePlan{
			Strategy:     environment.WorkspaceStrategyGit,
			SourceRef:    request.GitRepository,
			BaseRevision: request.GitRevision,
		}
	}
	spec.GitAllRepositories = request.GitAllRepositories
	spec.ReportProgress = func(step, message string) error {
		return request.Emit(ponsruntime.RunEvent{Type: ponsruntime.EventEnvironmentProgress, Step: step, Message: message})
	}
	if request.GitRepository != "" || request.GitAllRepositories {
		spec.Network = environment.NetworkEnabled
	}
	runLog.Debug("environment starting", "sandbox", r.opts.Sandbox)
	session, startErr := provider.Start(ctx, spec)
	if startErr != nil {
		return result, fmt.Errorf("start execution environment: %w", startErr)
	}
	defer func() {
		closeErr := session.Close()
		err = errors.Join(err, closeErr)
		if closeErr == nil {
			runLog.Debug("environment closed")
		}
	}()

	metadata := session.Metadata()
	if len(metadata.ReadWrite) != len(spec.ReadWrite) {
		return result, fmt.Errorf("execution environment %q did not grant the agent's memory", r.opts.Sandbox)
	}
	hands := llm.Hands{Workspace: metadata.WorkspacePath, Platform: metadata.Platform, Network: string(metadata.Network)}
	if len(memoryGrant) != 0 {
		// The prompt names memory where the hands see it; memory is the only
		// read-write grant.
		hands.MemoryPath = metadata.ReadWrite[0]
	}
	core.Workspace, core.Platform = metadata.WorkspacePath, metadata.Platform

	brain.Seed(turns, request.Text, hands)
	if err := core.Use(environment.Proxy(session), brain); err != nil {
		return result, err
	}

	runLog.Debug("hands ready", "sandbox", metadata.Provider, "sandbox_id", metadata.EnvironmentID,
		"network", metadata.Network, "workspace", request.Workspace, "tools", len(core.ToolSpecs()))
	core.OnEventError(func(event pons.Event) error {
		switch event.Type {
		case pons.EventAssistantResponse:
			runLog.Debug("assistant turn", "turn", event.Turn, "tool_calls", len(event.Actions))
			parts := make([]ponsruntime.MessagePart, 0, len(event.Parts))
			for _, part := range event.Parts {
				switch part.Type {
				case pons.AssistantPartText:
					parts = append(parts, ponsruntime.MessagePart{Type: "text", Text: part.Text})
				case pons.AssistantPartToolCall:
					parts = append(parts, ponsruntime.MessagePart{
						Type: "tool_call", ToolCallID: part.Action.ID, ToolKind: string(part.Action.Kind),
						Arguments: append(json.RawMessage(nil), part.Action.Args...),
					})
				}
			}
			return request.Emit(ponsruntime.RunEvent{
				Type: ponsruntime.RunEventAssistantTurn, Parts: parts,
			})
		case pons.EventActionEnd:
			runLog.Debug("tool completed", "turn", event.Turn, "tool", event.Action.Kind,
				"ok", event.Result.OK)
			return request.Emit(ponsruntime.RunEvent{
				Type: ponsruntime.RunEventToolCompleted, ToolCallID: event.Action.ID, Result: event.Result,
			})
		}
		return nil
	})

	runResult, err := core.Run(ctx, request.Text)
	if err != nil {
		return result, err
	}

	if runResult.Exhausted {
		return result, fmt.Errorf("agent exhausted %d turns", runResult.Turns)
	}
	runLog.Debug("answer ready", "turns", runResult.Turns, "answer_chars", len(runResult.Answer))
	return ponsruntime.RunResult{Answer: runResult.Answer}, nil
}

// executionEnvironment resolves a conversation's environment. The server has
// exactly one; a conversation recorded under another fails rather than
// running elsewhere.
func (r *agentRunner) executionEnvironment(requested string) (environment.Provider, environment.Spec, error) {
	if r.opts.Environment == nil || (requested != "" && requested != r.opts.Sandbox) {
		return nil, environment.Spec{}, fmt.Errorf("execution environment %q is not configured", requested)
	}
	return r.opts.Environment, r.opts.EnvironmentSpec, nil
}

func runtimeTurns(messages []ponsruntime.Message) ([]llm.Turn, error) {
	turns := make([]llm.Turn, 0, len(messages))
	for _, message := range messages {
		if message.Role == "assistant" && !message.Complete {
			continue
		}
		var blocks []llm.Block
		for _, part := range message.Parts {
			switch part.Type {
			case "text":
				blocks = append(blocks, llm.Text{Value: part.Text})
			case "tool_call":
				input := make(map[string]any)
				arguments := bytes.TrimSpace(part.Arguments)
				if len(arguments) == 0 || bytes.Equal(arguments, []byte("null")) {
					arguments = []byte("{}")
				}
				decoder := json.NewDecoder(bytes.NewReader(arguments))
				decoder.UseNumber()
				if err := decoder.Decode(&input); err != nil {
					return nil, fmt.Errorf("hydrate tool call %q arguments: %w", part.ToolCallID, err)
				}
				var extra any
				if err := decoder.Decode(&extra); err != io.EOF {
					return nil, fmt.Errorf("hydrate tool call %q arguments: trailing JSON", part.ToolCallID)
				}
				blocks = append(blocks, llm.ToolUse{ID: part.ToolCallID, Name: part.ToolKind, Input: input})
			case "tool_result":
				if part.Result != nil {
					blocks = append(blocks, llm.Result{ToolUseID: part.ToolCallID, Content: part.Result.Observation(), IsError: !part.Result.OK})
				}
			}
		}
		if len(blocks) == 0 {
			continue
		}
		role := message.Role
		if role == "tool" {
			role = "user"
		}
		turns = append(turns, llm.Turn{Role: role, Blocks: blocks})
	}
	return turns, nil
}

func runClient(ctx context.Context, serverURL, conversationID, idempotencyKey, message string, interactive, debug bool, options ponsruntime.ConversationOptions) error {
	if conversationID != "" && (options.Workspace != "" || options.GitRepository != "" || options.GitRevision != "" || options.GitAllRepositories) {
		return errors.New("runtime client: workspace and Git options apply only when creating a conversation")
	}
	client := httptransport.Client{BaseURL: serverURL}
	runtimeOptions, err := client.RuntimeOptions(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "agent: %s\n", agentLabel(runtimeOptions.Agent))
	sendCount := 0
	send := func(text string) error {
		key := idempotencyKey
		if sendCount > 0 && key != "" {
			key = fmt.Sprintf("%s-%d", key, sendCount+1)
		}
		wasNew := conversationID == ""
		var onSnapshot func(ponsruntime.ConversationView)
		if sendCount == 0 && conversationID != "" {
			onSnapshot = func(view ponsruntime.ConversationView) {
				renderConversationSnapshot(view, debug)
			}
		}
		result, err := client.Send(ctx, conversationID, key, text, options, onSnapshot, func(event ponsruntime.Event) {
			switch event.Type {
			case ponsruntime.EventEnvironmentProgress:
				if event.EnvironmentProgress != nil {
					fmt.Printf("  %s\n", event.EnvironmentProgress.Message)
				}
			case ponsruntime.EventToolCallUpdated:
				if event.ToolCall != nil && event.ToolCall.Status == ponsruntime.ToolRequested {
					action := &protocol.Action{ID: event.ToolCall.ID, Kind: protocol.ActionKind(event.ToolCall.Kind), Args: event.ToolCall.Arguments}
					fmt.Printf("▸ %s: %s\n", action.Kind, primaryArg(action))
				}
				if event.ToolCall != nil && event.ToolCall.Result != nil {
					showResult(event.ToolCall.Result.Observation(), debug)
				}
			}
		})
		if err != nil {
			return err
		}
		sendCount++
		conversationID = result.ConversationID
		if wasNew {
			fmt.Fprintf(os.Stderr, "conversation: %s\n", conversationID)
		}
		fmt.Printf("\n%s\n", result.Answer)
		return nil
	}
	if message != "" {
		if err := send(message); err != nil {
			return err
		}
	}
	if !interactive {
		if message == "" {
			return errors.New("runtime client: --message is required unless -i is set")
		}
		return nil
	}
	scanner := bufio.NewScanner(os.Stdin)
	for ctx.Err() == nil {
		if isTTY(os.Stdin) {
			fmt.Print("> ")
		}
		if !scanner.Scan() {
			return scanner.Err()
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if line == "exit" || line == "quit" {
			return nil
		}
		if err := send(line); err != nil {
			return err
		}
	}
	return ctx.Err()
}

// agentLabel names the agent for the CLI, falling back to its ID.
func agentLabel(agent ponsruntime.AgentSummary) string {
	if agent.Name == "" {
		return agent.ID
	}
	return fmt.Sprintf("%s (%s)", agent.Name, agent.ID)
}

func renderConversationSnapshot(view ponsruntime.ConversationView, debug bool) {
	for _, message := range view.Messages {
		switch message.Role {
		case "user":
			for _, part := range message.Parts {
				if part.Type == "text" {
					fmt.Printf("> %s\n", part.Text)
				}
			}
		case "assistant":
			for _, part := range message.Parts {
				switch part.Type {
				case "text":
					fmt.Printf("%s\n", part.Text)
				case "tool_call":
					action := &protocol.Action{ID: part.ToolCallID, Kind: protocol.ActionKind(part.ToolKind), Args: part.Arguments}
					fmt.Printf("▸ %s: %s\n", action.Kind, primaryArg(action))
				}
			}
		case "tool":
			for _, part := range message.Parts {
				if part.Type == "tool_result" && part.Result != nil {
					showResult(part.Result.Observation(), debug)
				}
			}
		}
	}
}

var _ ponsruntime.Runner = (*agentRunner)(nil)
