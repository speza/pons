package external

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/samperrin/pons/protocol"
)

// Limits are host-side resource limits.  A zero value gets conservative
// defaults; MaxConcurrency is an additional host cap on a provider's
// advertised concurrency (zero means use the provider's value).
type Limits struct {
	MaxFrameBytes      int
	MaxStderrBytes     int
	MaxResultBytes     int
	MaxTools           int
	MaxPendingRequests int
	MaxConcurrency     int
}

func (l Limits) withDefaults() Limits {
	if l.MaxFrameBytes <= 0 {
		l.MaxFrameBytes = 1 << 20
	}
	if l.MaxStderrBytes <= 0 {
		l.MaxStderrBytes = 1 << 20
	}
	if l.MaxResultBytes <= 0 {
		l.MaxResultBytes = 1 << 20
	}
	if l.MaxTools <= 0 {
		l.MaxTools = 256
	}
	if l.MaxPendingRequests <= 0 {
		l.MaxPendingRequests = 128
	}
	return l
}

// HostConfig controls the child process and its placement.  The default
// placement is hands; there is no generic "run anywhere" mode for v1.
type HostConfig struct {
	Placement    Placement
	HostName     string
	HostVersion  string
	Workspace    string
	PluginConfig json.RawMessage
	// WorkingDirectory overrides the manifest directory for the child. It is
	// used by environment providers whose generated launcher has no manifest
	// file but must start inside an explicitly selected workspace.
	WorkingDirectory string

	StartupTimeout  time.Duration
	ShutdownTimeout time.Duration
	// CallTimeout is the host-owned default deadline for each tool call;
	// zero selects the conservative 60-second default.
	CallTimeout time.Duration
	CancelGrace time.Duration
	Limits      Limits

	// Env is either appended to the inherited environment (when
	// InheritEnv=true) or is the complete child environment. Inherited
	// variables are opt-in so host credentials do not enter hands by default.
	Env        []string
	InheritEnv bool
	// Path supplies only the child's PATH while keeping the rest of the
	// environment clean. This is the practical setting for interpreters such
	// as /usr/bin/env node; it does not import credentials or other variables.
	Path string
}

func (c HostConfig) withDefaults() HostConfig {
	if c.Placement == "" {
		c.Placement = PlacementHands
	}
	if c.HostName == "" {
		c.HostName = "pons"
	}
	if c.HostVersion == "" {
		c.HostVersion = "0.1.0"
	}
	if c.StartupTimeout <= 0 {
		c.StartupTimeout = 10 * time.Second
	}
	if c.ShutdownTimeout <= 0 {
		c.ShutdownTimeout = 2 * time.Second
	}
	if c.CancelGrace <= 0 {
		c.CancelGrace = 250 * time.Millisecond
	}
	if c.CallTimeout <= 0 {
		c.CallTimeout = 60 * time.Second
	}
	c.Limits = c.Limits.withDefaults()
	return c
}

func (c HostConfig) environment() []string {
	var env []string
	if c.InheritEnv {
		env = append(os.Environ(), c.Env...)
	} else {
		// A non-nil empty slice is important: nil means exec.Cmd inherits
		// the parent environment, which would defeat the hands default.
		env = append([]string{}, c.Env...)
	}
	if c.Path != "" {
		env = replaceEnv(env, "PATH", c.Path)
	}
	return env
}

func replaceEnv(env []string, key, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if !strings.HasPrefix(entry, prefix) {
			out = append(out, entry)
		}
	}
	return append(out, prefix+value)
}

type pendingCall struct {
	response chan rpcResponse
}

type rpcResponse struct {
	response RPCResponse
	err      error
}

// Connection is a running tool-provider byte stream. Environment providers
// use it to attach the existing protocol host to endpoints which are not local
// child processes. Wait must unblock after Kill.
type Connection struct {
	Stdin  io.WriteCloser
	Stdout io.ReadCloser
	Stderr io.ReadCloser
	Wait   func() error
	Kill   func() error
}

// Connector establishes one tool-provider connection.
type Connector func(context.Context) (Connection, error)

// Host is one persistent plugin child or remote connection and its JSON-RPC
// protocol state. It is safe for multiple tool calls to Execute concurrently.
type Host struct {
	manifest  Manifest
	cfg       HostConfig
	connector Connector

	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser
	wait   func() error
	kill   func() error

	writeMu sync.Mutex
	mu      sync.Mutex
	pending map[string]*pendingCall
	nextID  atomic.Uint64
	failure error
	failed  chan struct{}
	started bool
	ready   bool
	closing bool

	processDone     chan struct{}
	processDoneOnce sync.Once
	waitErr         error
	readerDone      chan struct{}
	stderrDone      chan struct{}
	startMu         sync.Mutex
	closeOnce       sync.Once
	closeErr        error

	tools     map[string]ToolDescription
	hooks     map[string]bool
	plugin    PluginInfo
	toolSem   chan struct{}
	stderrMu  sync.Mutex
	stderrBuf bytes.Buffer
}

// NewHost constructs a host. It does not execute the manifest until Start or
// the first Setup call through Plugin.
func NewHost(manifest Manifest, cfg HostConfig) (*Host, error) {
	return newHost(manifest, cfg, nil)
}

// NewConnectionHost constructs a host whose transport is supplied by an
// environment provider rather than an exec.Cmd. The protocol, validation,
// limits, cancellation, and result normalization remain identical.
func NewConnectionHost(manifest Manifest, cfg HostConfig, connector Connector) (*Host, error) {
	if connector == nil {
		return nil, errors.New("external: nil connector")
	}
	return newHost(manifest, cfg, connector)
}

func newHost(manifest Manifest, cfg HostConfig, connector Connector) (*Host, error) {
	if err := manifest.Validate(); err != nil {
		return nil, fmt.Errorf("external: %w", err)
	}
	if cfg.Placement == "" {
		cfg.Placement = PlacementHands
	}
	if cfg.Placement != PlacementHands && cfg.Placement != PlacementHost {
		return nil, fmt.Errorf("external: unsupported placement %q", cfg.Placement)
	}
	if manifest.Placement != "" && manifest.Placement != cfg.Placement {
		return nil, fmt.Errorf("external: manifest placement %q does not match host placement %q", manifest.Placement, cfg.Placement)
	}
	if cfg.Limits.MaxConcurrency < 0 {
		return nil, errors.New("external: host MaxConcurrency must be non-negative")
	}
	if manifest.ResolvedEntrypoint() == "" {
		return nil, errors.New("external: empty resolved entrypoint")
	}
	cfg = cfg.withDefaults()
	return &Host{
		manifest:    manifest,
		cfg:         cfg,
		connector:   connector,
		pending:     make(map[string]*pendingCall),
		failed:      make(chan struct{}),
		processDone: make(chan struct{}),
		readerDone:  make(chan struct{}),
		stderrDone:  make(chan struct{}),
		tools:       make(map[string]ToolDescription),
		hooks:       make(map[string]bool),
	}, nil
}

// Manifest returns the installation manifest used by this host.
func (h *Host) Manifest() Manifest { return h.manifest }

// PluginInfo returns the identity advertised by the authoritative initialize
// handshake. Before Start it is the zero value.
func (h *Host) PluginInfo() PluginInfo {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.plugin
}

// Start launches the child and completes the one initialize exchange.
func (h *Host) Start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	h.startMu.Lock()
	defer h.startMu.Unlock()

	h.mu.Lock()
	if h.closing {
		h.mu.Unlock()
		return errors.New("external: plugin is closed")
	}
	if h.ready {
		h.mu.Unlock()
		return nil
	}
	if h.started {
		err := h.failure
		h.mu.Unlock()
		if err == nil {
			return errors.New("external: plugin is starting")
		}
		return err
	}
	h.started = true
	h.mu.Unlock()

	connection, err := h.connect(ctx)
	if err != nil {
		return h.startFailure(err)
	}
	h.mu.Lock()
	h.stdin, h.stdout, h.stderr = connection.Stdin, connection.Stdout, connection.Stderr
	h.wait, h.kill = connection.Wait, connection.Kill
	h.mu.Unlock()
	go h.readLoop(connection.Stdout)
	go h.stderrLoop(connection.Stderr)
	go h.waitLoop(connection.Wait)

	initCtx := ctx
	cancel := func() {}
	if h.cfg.StartupTimeout > 0 {
		initCtx, cancel = context.WithTimeout(ctx, h.cfg.StartupTimeout)
	}
	defer cancel()
	capabilities := map[string][]int{CapabilityToolProvider: {ToolProviderVersion}}
	if h.cfg.Placement == PlacementHost {
		capabilities = map[string][]int{CapabilityHookProvider: {HookProviderVersion}}
	}
	params := InitializeParams{
		RuntimeProtocol: RuntimeProtocol,
		Host: HostInfo{
			Name: h.cfg.HostName, Version: h.cfg.HostVersion,
			Placement: h.cfg.Placement, Workspace: h.cfg.Workspace,
		},
		SupportedCapabilities: capabilities,
		Config:                h.cfg.PluginConfig,
	}
	raw, err := h.call(initCtx, MethodInitialize, params)
	if err != nil {
		return h.startFailure(fmt.Errorf("external: initialize %q: %w", h.manifest.Name, err))
	}

	var result InitializeResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return h.startFailure(fmt.Errorf("external: initialize result: %w", err))
	}

	if result.Plugin.Name != h.manifest.Name {
		return h.startFailure(fmt.Errorf("external: initialize plugin name %q does not match manifest %q", result.Plugin.Name, h.manifest.Name))
	}
	if result.Plugin.Version == "" {
		return h.startFailure(errors.New("external: initialize result plugin.version is required"))
	}
	if err := h.acceptCapabilities(result.Capabilities); err != nil {
		return h.startFailure(fmt.Errorf("external: initialize capabilities: %w", err))
	}

	h.mu.Lock()
	h.plugin = result.Plugin
	failure := h.failure
	if failure == nil {
		h.ready = true
	}
	h.mu.Unlock()
	if failure != nil {
		return h.startFailure(failure)
	}
	return nil
}

func (h *Host) connect(ctx context.Context) (Connection, error) {
	if h.connector != nil {
		connection, err := h.connector(ctx)
		if err != nil {
			return Connection{}, fmt.Errorf("external: connect %q: %w", h.manifest.Name, err)
		}
		if connection.Stdin == nil ||
			connection.Stdout == nil ||
			connection.Stderr == nil ||
			connection.Wait == nil ||
			connection.Kill == nil {
			incompleteErr := errors.New("external: connector returned an incomplete connection")
			if connection.Kill != nil {
				incompleteErr = errors.Join(incompleteErr, connection.Kill())
			}
			for _, stream := range []io.Closer{connection.Stdin, connection.Stdout, connection.Stderr} {
				if stream != nil {
					incompleteErr = errors.Join(incompleteErr, stream.Close())
				}
			}
			return Connection{}, incompleteErr
		}
		return connection, nil
	}

	cmd := exec.Command(h.manifest.ResolvedEntrypoint(), h.manifest.Args...)
	setProcessGroup(cmd)
	cmd.Dir = h.cfg.WorkingDirectory
	if cmd.Dir == "" {
		cmd.Dir = h.manifest.Dir()
	}
	if cmd.Dir == "" {
		cmd.Dir = "."
	}
	cmd.Env = h.cfg.environment()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return Connection{}, fmt.Errorf("external: stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return Connection{}, fmt.Errorf("external: stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return Connection{}, fmt.Errorf("external: stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		return Connection{}, fmt.Errorf("external: start %q: %w", h.manifest.ResolvedEntrypoint(), err)
	}

	return Connection{
		Stdin: stdin, Stdout: stdout, Stderr: stderr,
		Wait: cmd.Wait,
		Kill: func() error {
			killProcess(cmd)
			return nil
		},
	}, nil
}

func (h *Host) startFailure(err error) error {
	h.fail(err)
	h.mu.Lock()
	startedProcess := h.wait != nil
	h.mu.Unlock()
	if !startedProcess {
		h.markProcessDone()
	}
	_ = h.terminate()
	return err
}

func (h *Host) acceptCapabilities(capabilities []Capability) error {
	if len(capabilities) == 0 {
		return errors.New("plugin advertised no supported capabilities")
	}
	seenCaps := make(map[string]bool, len(capabilities))
	var toolCfg *ToolProviderConfiguration
	for _, cap := range capabilities {
		key := fmt.Sprintf("%s/%d", cap.Type, cap.Version)
		if seenCaps[key] {
			return fmt.Errorf("duplicate capability %s", key)
		}
		seenCaps[key] = true
		if h.cfg.Placement == PlacementHost {
			if cap.Type != CapabilityHookProvider || cap.Version != HookProviderVersion {
				return fmt.Errorf("unsupported host capability %q/v%d", cap.Type, cap.Version)
			}
			var config HookProviderConfiguration
			if err := json.Unmarshal(cap.Configuration, &config); err != nil {
				return fmt.Errorf("hook_provider configuration: %w", err)
			}
			if len(config.Hooks) == 0 {
				return errors.New("hook_provider must advertise at least one hook")
			}
			for _, name := range config.Hooks {
				if !validHookName(name) || h.hooks[name] {
					return fmt.Errorf("invalid or duplicate hook %q", name)
				}
				h.hooks[name] = true
			}
			continue
		}
		cfg, err := validateCapability(cap, h.cfg.Placement, h.cfg.Limits.MaxTools)
		if err != nil {
			return err
		}
		if toolCfg != nil {
			return errors.New("multiple tool_provider capabilities are not supported on one connection")
		}
		toolCfg = &cfg
	}
	if h.cfg.Placement == PlacementHost {
		return nil
	}
	if toolCfg == nil {
		return errors.New("plugin did not advertise tool_provider/v1")
	}
	for _, tool := range toolCfg.Tools {
		h.tools[tool.Kind] = tool
	}
	limit := toolCfg.MaxConcurrency
	if h.cfg.Limits.MaxConcurrency > 0 && (limit == 0 || h.cfg.Limits.MaxConcurrency < limit) {
		limit = h.cfg.Limits.MaxConcurrency
	}
	if limit > 0 {
		h.toolSem = make(chan struct{}, limit)
	}
	return nil
}

// Tools returns the validated model-facing tool catalog in stable sorted order.
func (h *Host) Tools() []ToolDescription {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]ToolDescription, 0, len(h.tools))
	for _, tool := range h.tools {
		tool.InputSchema = append(json.RawMessage(nil), tool.InputSchema...)
		out = append(out, tool)
	}
	// Provider order is not semantically significant; sort for deterministic
	// callers and tests.
	sortTools(out)
	return out
}

func sortTools(tools []ToolDescription) {
	for i := 1; i < len(tools); i++ {
		for j := i; j > 0 && tools[j].Kind < tools[j-1].Kind; j-- {
			tools[j], tools[j-1] = tools[j-1], tools[j]
		}
	}
}

func (h *Host) HookNames() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	names := make([]string, 0, len(h.hooks))
	for name := range h.hooks {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// CallHook invokes one declared host hook with bounded input and output.
func (h *Host) CallHook(ctx context.Context, name string, event any, result any) error {
	h.mu.Lock()
	known := h.hooks[name]
	h.mu.Unlock()
	if !known {
		return fmt.Errorf("external: plugin %q does not provide hook %q", h.manifest.Name, name)
	}
	encoded, err := json.Marshal(hookWireEvent(event))
	if err != nil {
		return fmt.Errorf("external: encode hook event: %w", err)
	}
	if len(encoded) > h.cfg.Limits.MaxFrameBytes/2 {
		return errors.New("external: hook event exceeds frame limit")
	}
	callCtx, cancel := context.WithTimeout(ctx, h.cfg.CallTimeout)
	defer cancel()
	raw, err := h.call(callCtx, MethodHook, HookCallParams{Hook: name, Event: encoded})
	if err != nil {
		return err
	}
	if len(raw) > h.cfg.Limits.MaxResultBytes {
		return errors.New("external: hook result exceeds limit")
	}
	var returned HookCallResult
	if err := json.Unmarshal(raw, &returned); err != nil || len(returned.Patch) == 0 {
		return errors.New("external: malformed hook result")
	}
	decoder := json.NewDecoder(bytes.NewReader(returned.Patch))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(result); err != nil {
		return fmt.Errorf("external: malformed hook patch: %w", err)
	}
	return nil
}

// Execute adapts one hands action to tools/execute. Plugin/process failures
// are returned as unsuccessful ToolResults so the brain can observe them.
func (h *Host) Execute(ctx context.Context, action protocol.Action) (protocol.ToolResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	resultFailure := func(err error) (protocol.ToolResult, error) {
		return protocol.ToolResult{ActionID: action.ID, Kind: string(action.Kind), OK: false, Error: err.Error()}, nil
	}
	if action.Kind == "" {
		return resultFailure(errors.New("external: action kind is empty"))
	}
	if _, err := protocol.ObjectArgs(action.Args); err != nil {
		return resultFailure(fmt.Errorf("invalid action args: %w", err))
	}

	if len(bytes.TrimSpace(action.Args)) == 0 || bytes.Equal(bytes.TrimSpace(action.Args), []byte("null")) {
		action.Args = json.RawMessage(`{}`)
	}
	h.mu.Lock()
	tool, known := h.tools[string(action.Kind)]
	sem := h.toolSem
	h.mu.Unlock()
	if !known {
		return resultFailure(fmt.Errorf("plugin %q does not provide action kind %q", h.manifest.Name, action.Kind))
	}
	_ = tool // the lookup above is also the routing guard

	callCtx := ctx
	cancel := func() {}
	if h.cfg.CallTimeout > 0 {
		callCtx, cancel = context.WithTimeout(ctx, h.cfg.CallTimeout)
	}
	defer cancel()
	if sem != nil {
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
		case <-callCtx.Done():
			return resultFailure(callCtx.Err())
		}
	}

	raw, err := h.call(callCtx, MethodExecute, ExecuteParams{Action: action})
	if err != nil {
		return resultFailure(err)
	}

	if len(raw) > h.cfg.Limits.MaxResultBytes {
		return resultFailure(fmt.Errorf("plugin result exceeds %d bytes", h.cfg.Limits.MaxResultBytes))
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		if err == nil {
			err = errors.New("result must be a JSON object")
		}
		return resultFailure(fmt.Errorf("malformed tool result: %w", err))
	}
	var result protocol.ToolResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return resultFailure(fmt.Errorf("malformed tool result: %w", err))
	}

	// The request's action is the only trusted correlation identity. A plugin
	// cannot forge either the action id or the payload namespace in the core
	// transcript.
	result.ActionID = action.ID
	result.Kind = string(action.Kind)
	return result, nil
}

// Health performs the optional common health call. A plugin that does not
// implement it returns an error without changing the tool connection.
func (h *Host) Health(ctx context.Context) (HealthResult, error) {
	raw, err := h.call(ctx, MethodHealth, nil)
	if err != nil {
		return HealthResult{}, err
	}
	var result HealthResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return HealthResult{}, fmt.Errorf("external: health result: %w", err)
	}
	return result, nil
}

func (h *Host) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	h.mu.Lock()
	if h.failure != nil {
		err := h.failure
		h.mu.Unlock()
		return nil, err
	}
	if method != MethodInitialize && !h.ready {
		h.mu.Unlock()
		return nil, errors.New("external: plugin is not initialized")
	}
	if h.closing && method != MethodShutdown {
		h.mu.Unlock()
		return nil, errors.New("external: plugin is shutting down")
	}
	if len(h.pending) >= h.cfg.Limits.MaxPendingRequests {
		h.mu.Unlock()
		return nil, fmt.Errorf("external: pending request limit %d exceeded", h.cfg.Limits.MaxPendingRequests)
	}
	id := fmt.Sprintf("pons-%d", h.nextID.Add(1))
	p := &pendingCall{response: make(chan rpcResponse, 1)}
	h.pending[id] = p
	h.mu.Unlock()

	request := RPCRequest{JSONRPC: "2.0", ID: id, Method: method}
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			h.removePending(id)
			return nil, err
		}
		request.Params = b
	}

	if err := h.writeMessage(ctx, request); err != nil {
		h.removePending(id)
		h.fail(err)
		return nil, err
	}

	select {
	case result := <-p.response:
		if result.err != nil {
			return nil, result.err
		}
		if result.response.Error != nil {
			return nil, fmt.Errorf("plugin RPC error %d: %s", result.response.Error.Code, result.response.Error.Message)
		}
		if len(result.response.Result) == 0 {
			return nil, errors.New("plugin response has neither result nor error")
		}
		return result.response.Result, nil
	case <-ctx.Done():
		h.cancelPending(id)
		return nil, ctx.Err()
	}
}

func (h *Host) removePending(id string) {
	h.mu.Lock()
	delete(h.pending, id)
	h.mu.Unlock()
}

func (h *Host) cancelPending(id string) {
	h.mu.Lock()
	_, ok := h.pending[id]
	h.mu.Unlock()
	if !ok {
		return
	}
	writeCtx, cancel := context.WithTimeout(context.Background(), h.cfg.CancelGrace)
	_ = h.writeMessage(writeCtx, RPCRequest{JSONRPC: "2.0", Method: MethodCancel, Params: mustJSON(map[string]string{"id": id})})
	cancel()
	grace := h.cfg.CancelGrace
	if grace <= 0 {
		grace = 250 * time.Millisecond
	}
	go func() {
		t := time.NewTimer(grace)
		defer t.Stop()
		<-t.C
		h.mu.Lock()
		_, stillPending := h.pending[id]
		closed := h.closing
		h.mu.Unlock()
		if stillPending && !closed {
			h.fail(fmt.Errorf("external: request %s did not cancel within %s", id, grace))
		}
	}()
}

func (h *Host) writeMessage(ctx context.Context, message any) error {
	data, err := json.Marshal(message)
	if err != nil {
		return err
	}
	if len(data)+1 > h.cfg.Limits.MaxFrameBytes {
		return fmt.Errorf("external: outgoing frame exceeds %d bytes", h.cfg.Limits.MaxFrameBytes)
	}

	data = append(data, '\n')
	done := make(chan error, 1)
	go func() { done <- h.writeFrame(data) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		// A partially written frame makes the stream unusable. Failing the
		// connection also closes stdin, which unblocks the write goroutine.
		h.fail(fmt.Errorf("external: protocol write canceled: %w", ctx.Err()))
		return ctx.Err()
	}
}

func (h *Host) writeFrame(data []byte) error {
	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	h.mu.Lock()
	stdin := h.stdin
	failure := h.failure
	h.mu.Unlock()
	if failure != nil {
		return failure
	}
	if stdin == nil {
		return errors.New("external: plugin stdin is not open")
	}
	for len(data) > 0 {
		n, err := stdin.Write(data)
		if err != nil {
			return fmt.Errorf("external: write protocol frame: %w", err)
		}
		data = data[n:]
	}
	return nil
}

func (h *Host) readLoop(stdout io.Reader) {
	defer close(h.readerDone)
	reader := newFrameReader(stdout, h.cfg.Limits.MaxFrameBytes)
	for {
		frame, err := reader.Read()
		if err != nil {
			if h.isClosing() {
				return
			}
			if errors.Is(err, io.EOF) || errors.Is(err, os.ErrClosed) {
				h.fail(errors.New("external: plugin stdout closed"))
			} else {
				h.fail(fmt.Errorf("external: read protocol frame: %w", err))
			}
			return
		}
		if len(bytes.TrimSpace(frame)) == 0 {
			if !h.isClosing() {
				h.fail(errors.New("external: blank protocol frame"))
			}
			return
		}
		var response RPCResponse
		if err := json.Unmarshal(frame, &response); err != nil {
			if !h.isClosing() {
				h.fail(fmt.Errorf("external: malformed JSON-RPC response: %w", err))
			}
			return
		}
		if response.JSONRPC != "2.0" || response.ID == "" {
			if !h.isClosing() {
				h.fail(errors.New("external: invalid JSON-RPC response envelope"))
			}
			return
		}
		if (response.Error == nil) == (len(response.Result) == 0) {
			if !h.isClosing() {
				h.fail(errors.New("external: JSON-RPC response must contain exactly one result or error"))
			}
			return
		}
		if response.Error != nil && response.Error.Message == "" {
			if !h.isClosing() {
				h.fail(errors.New("external: JSON-RPC error is missing message"))
			}
			return
		}
		h.deliver(response)
	}
}

func (h *Host) deliver(response RPCResponse) {
	h.mu.Lock()
	p, ok := h.pending[response.ID]
	if ok {
		delete(h.pending, response.ID)
	}
	closing := h.closing
	h.mu.Unlock()
	if !ok {
		if !closing {
			h.fail(fmt.Errorf("external: response for unknown request id %q", response.ID))
		}
		return
	}
	p.response <- rpcResponse{response: response}
}

func (h *Host) stderrLoop(stderr io.Reader) {
	defer close(h.stderrDone)
	buf := make([]byte, 4096)
	total := 0
	tooLarge := false
	for {
		n, err := stderr.Read(buf)
		if n > 0 {
			total += n
			if total > h.cfg.Limits.MaxStderrBytes && !tooLarge {
				tooLarge = true
				if !h.isClosing() {
					h.fail(fmt.Errorf("external: plugin stderr exceeds %d bytes", h.cfg.Limits.MaxStderrBytes))
				}
			}
			h.stderrMu.Lock()
			if h.stderrBuf.Len() < h.cfg.Limits.MaxStderrBytes {
				remaining := h.cfg.Limits.MaxStderrBytes - h.stderrBuf.Len()
				if n > remaining {
					h.stderrBuf.Write(buf[:remaining])
				} else {
					h.stderrBuf.Write(buf[:n])
				}
			}
			h.stderrMu.Unlock()
		}
		if err != nil {
			if err != io.EOF && !h.isClosing() {
				h.fail(fmt.Errorf("external: read plugin stderr: %w", err))
			}
			break
		}
	}
	if tooLarge && !h.isClosing() {
		h.fail(fmt.Errorf("external: plugin stderr exceeds %d bytes", h.cfg.Limits.MaxStderrBytes))
	}
}

// Stderr returns the bounded stderr captured from the child, useful for
// diagnostics. It is never mixed into stdout protocol traffic.
func (h *Host) Stderr() string {
	h.stderrMu.Lock()
	defer h.stderrMu.Unlock()
	return h.stderrBuf.String()
}

func (h *Host) waitLoop(wait func() error) {
	err := wait()
	h.mu.Lock()
	h.waitErr = err
	h.mu.Unlock()
	h.markProcessDone()
	if h.isClosing() {
		// A fast exit can race the reader delivering the buffered shutdown
		// acknowledgement. Drain it before rejecting any unanswered request.
		waitForReaders(h.cfg.ShutdownTimeout, h.readerDone)
		h.rejectPending(errors.New("external: plugin exited during shutdown"))
		return
	}
	if err == nil {
		h.fail(errors.New("external: plugin exited unexpectedly"))
	} else {
		h.fail(fmt.Errorf("external: plugin exited: %w", err))
	}
}

func (h *Host) rejectPending(err error) {
	h.mu.Lock()
	pending := h.pending
	h.pending = make(map[string]*pendingCall)
	h.mu.Unlock()
	for _, p := range pending {
		p.response <- rpcResponse{err: err}
	}
}

func (h *Host) isClosing() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.closing
}

func (h *Host) fail(err error) {
	if err == nil {
		err = errors.New("external: plugin failed")
	}
	h.mu.Lock()
	if h.failure != nil {
		h.mu.Unlock()
		return
	}
	h.failure = err
	close(h.failed)
	pending := h.pending
	h.pending = make(map[string]*pendingCall)
	closing := h.closing
	kill, stdin := h.kill, h.stdin
	h.mu.Unlock()
	for _, p := range pending {
		p.response <- rpcResponse{err: err}
	}
	if stdin != nil {
		_ = stdin.Close()
	}
	if !closing && kill != nil {
		_ = kill()
	}
}

func (h *Host) markProcessDone() {
	h.processDoneOnce.Do(func() { close(h.processDone) })
}

func (h *Host) terminate() error {
	h.mu.Lock()
	kill, stdin := h.kill, h.stdin
	h.mu.Unlock()
	if stdin != nil {
		_ = stdin.Close()
	}
	if kill != nil {
		return kill()
	}
	return nil
}

// Failure returns the first checked failure observed by the host, if any.
func (h *Host) Failure() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.failure
}

// Close performs the protocol shutdown exchange, then closes stdin and waits
// a bounded grace period before terminating the child. It is idempotent.
func (h *Host) Close() error {
	h.closeOnce.Do(func() {
		h.startMu.Lock()
		defer h.startMu.Unlock()
		h.mu.Lock()
		started, ready, failed := h.started, h.ready, h.failure
		h.closing = true
		h.mu.Unlock()
		if !started {
			return
		}
		if ready && failed == nil {
			ctx, cancel := context.WithTimeout(context.Background(), h.cfg.ShutdownTimeout)
			_, err := h.call(ctx, MethodShutdown, nil)
			cancel()
			if err != nil && !errors.Is(err, context.Canceled) {
				h.closeErr = fmt.Errorf("external: shutdown: %w", err)
			}
		}
		h.mu.Lock()
		stdin, processDone := h.stdin, h.processDone
		startedProcess := h.wait != nil
		h.mu.Unlock()
		if stdin != nil {
			_ = stdin.Close()
		}
		wait := h.cfg.ShutdownTimeout
		if wait <= 0 {
			wait = 2 * time.Second
		}
		select {
		case <-processDone:
		case <-time.After(wait):
			h.closeErr = errors.Join(h.closeErr, h.terminate())
			<-processDone
		}
		h.mu.Lock()
		waitErr := h.waitErr
		h.mu.Unlock()
		if waitErr != nil {
			// A remote stream failure does not establish that its process exited.
			// Preserve the error so callers cannot checkpoint/reuse uncertain state.
			h.closeErr = errors.Join(h.closeErr, fmt.Errorf("external: wait for plugin: %w", waitErr), h.terminate())
		}
		// cmd.Wait closes the child pipes. Drain both readers before exposing
		// final diagnostics so a fast startup failure cannot lose stderr.
		if startedProcess && !waitForReaders(wait, h.readerDone, h.stderrDone) {
			h.closeErr = errors.Join(h.closeErr, errors.New("external: timed out draining child output"))
		}
		if h.closeErr == nil && failed != nil {
			h.closeErr = failed
		}
	})
	return h.closeErr
}

func waitForReaders(timeout time.Duration, readers ...<-chan struct{}) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for _, done := range readers {
		select {
		case <-done:
		case <-timer.C:
			return false
		}
	}
	return true
}

type frameReader struct {
	reader *bufio.Reader
	max    int
}

func newFrameReader(r io.Reader, max int) *frameReader {
	return &frameReader{reader: bufio.NewReaderSize(r, 4096), max: max}
}

func (r *frameReader) Read() ([]byte, error) {
	var frame []byte
	for {
		part, err := r.reader.ReadSlice('\n')
		frame = append(frame, part...)
		if len(frame) > r.max {
			return nil, fmt.Errorf("frame exceeds %d bytes", r.max)
		}
		if err == nil {
			if len(frame) == 0 || frame[len(frame)-1] != '\n' {
				return nil, errors.New("protocol frame is not newline terminated")
			}
			return bytes.TrimSuffix(frame, []byte{'\n'}), nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			if len(frame) == 0 {
				return nil, io.EOF
			}
			return nil, io.ErrUnexpectedEOF
		}
		return nil, err
	}
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
