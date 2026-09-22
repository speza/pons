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
	"github.com/samperrin/pons/plugins/bash"
	"github.com/samperrin/pons/plugins/brain/llm"
	"github.com/samperrin/pons/plugins/edit"
	"github.com/samperrin/pons/plugins/external"
	"github.com/samperrin/pons/plugins/fs"
	"github.com/samperrin/pons/protocol"
	ponsruntime "github.com/samperrin/pons/runtime"
	"github.com/samperrin/pons/runtime/checkpoint"
	"github.com/samperrin/pons/runtime/httptransport"
	runtimesqlite "github.com/samperrin/pons/runtime/sqlite"
)

type serverOptions struct {
	Address                 string
	StateDir                string
	Workspace               string
	MaxConcurrent           int
	MaxTurns                int
	Brain                   llm.Config
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
	logger.Debug("e2b lifecycle", append(fields, "event", event)...)
}

func runServer(ctx context.Context, logger *slog.Logger, opts serverOptions) error {
	return runServerReady(ctx, logger, opts, nil)
}

func runServerReady(ctx context.Context, logger *slog.Logger, opts serverOptions, started chan<- string) error {
	if err := validateLoopbackAddress(opts.Address); err != nil {
		return err
	}
	if _, ok := opts.Environment.(environment.DurableProvider); ok {
		if err := validateRemoteStateDirectory(opts.Workspace, opts.StateDir); err != nil {
			return err
		}
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
	runner := &agentRunner{opts: opts, logger: logger}
	backgroundErrors := make(chan error, 1)
	manager, err := ponsruntime.New(ponsruntime.Config{
		Store: store, Workspace: opts.Workspace,
		IndependentWorkspaces: opts.Sandbox == "e2b",
		Runner:                runner, MaxConcurrent: opts.MaxConcurrent,
		ValidateConversation: func(selection ponsruntime.ConversationOptions) error {
			if (selection.GitRepository == "") != (selection.GitRevision == "") {
				return errors.New("git repository and revision must be set together")
			}
			if selection.GitRepository != "" {
				if opts.Sandbox != "e2b" {
					return errors.New("git workspace requires E2B sandbox")
				}
				if err := gitworkspace.ValidatePlan(environment.WorkspacePlan{
					Strategy:     environment.WorkspaceStrategyGit,
					SourceRef:    selection.GitRepository,
					BaseRevision: selection.GitRevision,
				}); err != nil {
					return err
				}
			}
			if selection.GitAllRepositories {
				provider, ok := opts.Environment.(*e2b.Provider)
				if !ok || provider.GitCredentials == nil {
					return errors.New("installation-wide Git access requires E2B with GitHub App authentication")
				}
			}
			return nil
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
		Addr:              opts.Address,
		Handler:           httptransport.Handler(manager),
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

func logDebugConfiguration(logger *slog.Logger, opts serverOptions) {
	if !opts.Debug {
		return
	}
	model := opts.Brain.Model
	if model == "" {
		model = "provider-default"
	}
	logger.Debug("runtime configured", "provider", opts.Brain.Provider, "model", model,
		"workspace", opts.Workspace, "max_turns", opts.MaxTurns, "max_concurrent", opts.MaxConcurrent)

	sandbox, network, hands := opts.Sandbox, "host", "in-process"
	if sandbox == "" {
		if opts.Environment == nil {
			sandbox = "none"
		} else {
			sandbox = "custom"
		}
	}
	if opts.Environment != nil {
		network = string(opts.EnvironmentSpec.Network)
		if network == "" {
			network = "unspecified"
		}
		if len(opts.EnvironmentSpec.Command) > 0 {
			hands = opts.EnvironmentSpec.Command[0]
		}
	}
	logger.Debug("hands configured", "sandbox", sandbox, "network", network, "hands", hands,
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
}

func (r *agentRunner) Run(ctx context.Context, request ponsruntime.RunRequest) (result ponsruntime.RunResult, err error) {
	brainConfig := r.opts.Brain
	brainConfig.Logger = nil
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
	core.Workspace, core.MaxTurns = request.Workspace, r.opts.MaxTurns
	runLog := r.logger.With("conversation_id", request.ConversationID, "run_id", request.RunID,
		"workspace_id", request.ConversationID)
	runLog.Info("run started")
	defer func() {
		if err != nil {
			runLog.Error("run failed", "error", err)
		} else {
			runLog.Info("run completed")
		}
	}()

	externals := make([]*external.Plugin, 0, len(r.opts.PluginPaths))
	defer func() {
		for _, plugin := range slices.Backward(externals) {
			err = errors.Join(err, plugin.Close())
		}
	}()
	plugins := make([]pons.Plugin, 0, 4+len(r.opts.PluginPaths))
	effectiveSandbox, effectiveNetwork, effectiveEnvironmentID := "none", "host", ""
	if r.opts.Environment != nil {
		spec := r.opts.EnvironmentSpec
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
		if request.GitRepository != "" || request.GitAllRepositories {
			spec.Network = environment.NetworkEnabled
		}
		if emitErr := request.Emit(ponsruntime.RunEvent{Type: ponsruntime.EventRunProgress, Stage: "Preparing sandbox…"}); emitErr != nil {
			return result, emitErr
		}
		runLog.Debug("environment starting", "sandbox", r.opts.Sandbox)
		session, startErr := r.opts.Environment.Start(ctx, spec)
		if startErr != nil {
			return result, fmt.Errorf("start execution environment: %w", startErr)
		}
		defer func() {
			stage := "Closing sandbox…"
			if r.opts.Sandbox == "e2b" {
				stage = "Saving sandbox state…"
			}
			if emitErr := request.Emit(ponsruntime.RunEvent{Type: ponsruntime.EventRunProgress, Stage: stage}); emitErr != nil {
				err = errors.Join(err, emitErr)
			}
			closeErr := session.Close()
			err = errors.Join(err, closeErr)
			if closeErr == nil {
				runLog.Debug("environment closed")
			}
		}()
		metadata := session.Metadata()
		core.Workspace, core.Platform = metadata.WorkspacePath, metadata.Platform
		effectiveSandbox = metadata.Provider
		effectiveNetwork = string(metadata.Network)
		effectiveEnvironmentID = metadata.EnvironmentID
		if effectiveSandbox == "" {
			effectiveSandbox = r.opts.Sandbox
		}
		if effectiveNetwork == "" {
			effectiveNetwork = string(spec.Network)
		}
		plugins = append(plugins, environment.Proxy(session))
	} else {
		fsTools, fsErr := fs.New(fs.Config{Root: request.Workspace, MaxReadBytes: r.opts.FSReadBytes})
		if fsErr != nil {
			return result, fsErr
		}
		editTool, editErr := edit.New(edit.Config{Root: request.Workspace})
		if editErr != nil {
			return result, editErr
		}
		plugins = append(plugins, fsTools, editTool,
			bash.New(bash.Config{Root: request.Workspace, Timeout: r.opts.BashTimeout, MaxLines: r.opts.BashMaxLines, MaxBytes: r.opts.BashMaxBytes}))
		for _, manifestPath := range r.opts.PluginPaths {
			plugin, pluginErr := external.NewHands(manifestPath, external.HostConfig{
				Workspace: request.Workspace, Path: r.opts.PluginPath, CallTimeout: 60 * time.Second,
				Limits: external.Limits{MaxResultBytes: r.opts.PluginMaxResultBytes},
			})
			if pluginErr != nil {
				return result, pluginErr
			}
			externals = append(externals, plugin)
			plugins = append(plugins, plugin)
		}
	}
	brain.Seed(turns, request.Text, core.Workspace, core.Platform)
	plugins = append(plugins, brain)
	if err := core.Use(plugins...); err != nil {
		return result, err
	}
	runLog.Debug("hands ready", "sandbox", effectiveSandbox, "sandbox_id", effectiveEnvironmentID,
		"network", effectiveNetwork, "workspace", request.Workspace, "tools", len(core.ToolSpecs()))
	if r.opts.Environment != nil {
		if emitErr := request.Emit(ponsruntime.RunEvent{Type: ponsruntime.EventRunProgress, Stage: "Sandbox ready"}); emitErr != nil {
			return result, emitErr
		}
	}
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
	if conversationID != "" && (options.GitRepository != "" || options.GitRevision != "" || options.GitAllRepositories) {
		return errors.New("runtime client: Git repository and access options apply only when creating a conversation")
	}
	client := httptransport.Client{BaseURL: serverURL}
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
		result, err := client.SendWithOptions(ctx, conversationID, key, text, options, onSnapshot, func(event ponsruntime.Event) {
			switch event.Type {
			case ponsruntime.EventRunProgress:
				if event.RunProgress != nil {
					fmt.Printf("  %s\n", event.RunProgress.Stage)
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
