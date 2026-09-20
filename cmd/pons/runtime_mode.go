package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/samperrin/pons"
	"github.com/samperrin/pons/environment"
	"github.com/samperrin/pons/plugins/bash"
	"github.com/samperrin/pons/plugins/brain/llm"
	"github.com/samperrin/pons/plugins/edit"
	"github.com/samperrin/pons/plugins/external"
	"github.com/samperrin/pons/plugins/fs"
	"github.com/samperrin/pons/protocol"
	ponsruntime "github.com/samperrin/pons/runtime"
	"github.com/samperrin/pons/runtime/httptransport"
	runtimesqlite "github.com/samperrin/pons/runtime/sqlite"
)

type serverOptions struct {
	Address              string
	StateDir             string
	Workspace            string
	MaxConcurrent        int
	MaxTurns             int
	Brain                llm.Config
	FSReadBytes          int
	BashTimeout          int
	BashMaxLines         int
	BashMaxBytes         int
	PluginPaths          []string
	PluginPath           string
	PluginMaxResultBytes int
	Debug                bool
	Environment          environment.Provider
	EnvironmentSpec      environment.Spec
}

func runServer(ctx context.Context, logger *log.Logger, opts serverOptions) error {
	return runServerReady(ctx, logger, opts, nil)
}

func runServerReady(ctx context.Context, logger *log.Logger, opts serverOptions, started chan<- string) error {
	if err := validateLoopbackAddress(opts.Address); err != nil {
		return err
	}
	store, err := runtimesqlite.Open(opts.StateDir)
	if err != nil {
		return err
	}
	defer store.Close()
	runner := &agentRunner{opts: opts, logger: logger}
	backgroundErrors := make(chan error, 1)
	manager, err := ponsruntime.New(ponsruntime.Config{
		Store: store, Workspace: opts.Workspace,
		Runner: runner, MaxConcurrent: opts.MaxConcurrent,
		OnError: func(err error) {
			logger.Printf("runtime background: %v", err)
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
	logger.Printf("runtime: http://%s", listener.Addr())
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
		if err := server.Shutdown(shutdownCtx); err != nil {
			return err
		}
		<-done
		return nil
	}
}

func runBundled(ctx context.Context, logger *log.Logger, opts serverOptions, conversationID, idempotencyKey, message string, interactive bool) error {
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
	clientErr := runClient(ctx, serverURL, conversationID, idempotencyKey, message, interactive)
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

func executionEnvironment(backend, handsCommand string, allowNetwork bool, opts serverOptions) (environment.Provider, environment.Spec, error) {
	if backend == "" {
		if allowNetwork || handsCommand != "" {
			return nil, environment.Spec{}, errors.New("--sandbox-network and --hands-command require --sandbox")
		}
		return nil, environment.Spec{}, nil
	}
	if backend != "seatbelt" {
		return nil, environment.Spec{}, fmt.Errorf("unknown backend %q", backend)
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
	command := []string{
		commandPath,
		"--fs-read-bytes", fmt.Sprint(opts.FSReadBytes),
		"--bash-timeout", fmt.Sprint(opts.BashTimeout),
		"--bash-max-lines", fmt.Sprint(opts.BashMaxLines),
		"--bash-max-bytes", fmt.Sprint(opts.BashMaxBytes),
		"--plugin-max-result-bytes", fmt.Sprint(opts.PluginMaxResultBytes),
	}
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
	return environment.Seatbelt{}, environment.Spec{
		Command: command, ReadOnly: readOnly,
		Network: network,
		Limits: environment.ResourceLimits{
			CallTimeout:    60 * time.Second,
			MaxResultBytes: opts.PluginMaxResultBytes,
		},
	}, nil
}

type agentRunner struct {
	opts   serverOptions
	logger *log.Logger
}

func (r *agentRunner) Run(ctx context.Context, request ponsruntime.RunRequest) (result ponsruntime.RunResult, err error) {
	brainConfig := r.opts.Brain
	if !r.opts.Debug {
		brainConfig.Logger = nil
	}
	brain, err := llm.New(brainConfig)
	if err != nil {
		return result, fmt.Errorf("brain: %w", err)
	}
	defer func() { err = errors.Join(err, brain.Close(context.Background())) }()
	turns, turnsErr := runtimeTurns(request.Messages)
	if turnsErr != nil {
		return result, turnsErr
	}
	brain.Seed(turns, request.Text, request.Workspace)

	core := pons.New()
	core.Workspace, core.MaxTurns = request.Workspace, r.opts.MaxTurns
	if r.opts.Debug {
		core.Log = r.logger
	}

	externals := make([]*external.Plugin, 0, len(r.opts.PluginPaths))
	defer func() {
		for _, plugin := range slices.Backward(externals) {
			err = errors.Join(err, plugin.Close())
		}
	}()
	plugins := make([]pons.Plugin, 0, 4+len(r.opts.PluginPaths))
	if r.opts.Environment != nil {
		spec := r.opts.EnvironmentSpec
		spec.Workspace = request.Workspace
		session, startErr := r.opts.Environment.Start(ctx, spec)
		if startErr != nil {
			return result, fmt.Errorf("start execution environment: %w", startErr)
		}
		defer func() { err = errors.Join(err, session.Close()) }()
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
	plugins = append(plugins, brain)
	if err := core.Use(plugins...); err != nil {
		return result, err
	}
	core.OnEventError(func(event pons.Event) error {
		switch event.Type {
		case pons.EventAssistantResponse:
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

func runClient(ctx context.Context, serverURL, conversationID, idempotencyKey, message string, interactive bool) error {
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
			onSnapshot = renderConversationSnapshot
		}
		result, err := client.Send(ctx, conversationID, key, text, onSnapshot, func(event ponsruntime.Event) {
			switch event.Type {
			case ponsruntime.EventToolCallUpdated:
				if event.ToolCall != nil && event.ToolCall.Status == ponsruntime.ToolRequested {
					action := &protocol.Action{ID: event.ToolCall.ID, Kind: protocol.ActionKind(event.ToolCall.Kind), Args: event.ToolCall.Arguments}
					fmt.Printf("▸ %s: %s\n", action.Kind, primaryArg(action))
				}
				if event.ToolCall != nil && event.ToolCall.Result != nil {
					showResult(event.ToolCall.Result.Observation())
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

func renderConversationSnapshot(view ponsruntime.ConversationView) {
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
					showResult(part.Result.Observation())
				}
			}
		}
	}
}

var _ ponsruntime.Runner = (*agentRunner)(nil)
