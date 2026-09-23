// Package sdk is a small Go serving SDK for pons external hands plugins.
//
// A Server owns only tool handlers and typed JSON action bytes. It never sees
// a Core or brain. Application logging must go to os.Stderr (or another
// stream); Serve writes stdout exclusively as JSON-RPC/NDJSON.
package sdk

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/samperrin/pons/plugins/external"
	"github.com/samperrin/pons/protocol"
)

// Tool is one tool_provider/v1 capability owned by a plugin.
type Tool struct {
	Kind        string
	Description string
	InputSchema json.RawMessage
	Handler     func(context.Context, protocol.Action) (protocol.ToolResult, error)
}

// Server serves one plugin process connection.
type Server struct {
	Name    string
	Version string
	Tools   []Tool
	// MaxConcurrency defaults to one. Set Unbounded to advertise the
	// protocol's explicit zero (still bounded by the host's pending limit).
	MaxConcurrency  int
	Unbounded       bool
	MaxFrameBytes   int
	ShutdownTimeout time.Duration
}

// Serve runs the persistent protocol loop until shutdown, EOF, or ctx
// cancellation. It is suitable for a plugin's main function:
//
//	func main() { _ = sdk.Serve(context.Background(), os.Stdin, os.Stdout, server) }
func Serve(ctx context.Context, in io.Reader, out io.Writer, server Server) error {
	return server.Serve(ctx, in, out)
}

// Serve is the method form of Serve.
func (s Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if s.Name == "" {
		return errors.New("external sdk: Name is required")
	}
	if s.Version == "" {
		s.Version = "0.1.0"
	}
	if s.MaxConcurrency < 0 {
		return errors.New("external sdk: MaxConcurrency must be non-negative")
	}
	if s.Unbounded && s.MaxConcurrency != 0 {
		return errors.New("external sdk: Unbounded requires MaxConcurrency=0")
	}
	if s.MaxFrameBytes <= 0 {
		s.MaxFrameBytes = 1 << 20
	}
	if s.ShutdownTimeout <= 0 {
		s.ShutdownTimeout = 2 * time.Second
	}
	tools := make(map[string]Tool, len(s.Tools))
	for _, tool := range s.Tools {
		if tool.Kind == "" || tool.Kind == "finish" {
			return fmt.Errorf("external sdk: invalid tool kind %q", tool.Kind)
		}
		if tool.Description == "" {
			return fmt.Errorf("external sdk: tool %q has empty description", tool.Kind)
		}
		if tool.Handler == nil {
			return fmt.Errorf("external sdk: tool %q has nil handler", tool.Kind)
		}
		if _, exists := tools[tool.Kind]; exists {
			return fmt.Errorf("external sdk: duplicate tool kind %q", tool.Kind)
		}
		if err := external.ValidateToolSchema(tool.InputSchema); err != nil {
			return fmt.Errorf("external sdk: tool %q: %w", tool.Kind, err)
		}
		tools[tool.Kind] = tool
	}

	maxConcurrency := s.MaxConcurrency
	if !s.Unbounded && maxConcurrency == 0 {
		maxConcurrency = 1
	}
	state := &serveState{
		ctx:      ctx,
		tools:    tools,
		maxFrame: s.MaxFrameBytes,
		shutdown: s.ShutdownTimeout,
		maxConc:  maxConcurrency,
		cancels:  make(map[string]context.CancelFunc),
		out:      out,
		fatal:    make(chan error, 1),
	}

	reader := newFrameReader(in, s.MaxFrameBytes)
	type frameResult struct {
		frame []byte
		err   error
	}
	frames := make(chan frameResult, 1)
	stopReader := make(chan struct{})
	go func() {
		for {
			frame, err := reader.Read()
			select {
			case frames <- frameResult{frame: frame, err: err}:
			case <-stopReader:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	defer close(stopReader)
	defer func() {
		if closer, ok := in.(io.Closer); ok {
			_ = closer.Close()
		}
	}()

	initialized := false
	shuttingDown := false
	for {
		var next frameResult
		select {
		case <-ctx.Done():
			state.cancelAll()
			return ctx.Err()
		case err := <-state.fatal:
			state.cancelAll()
			return err
		case next = <-frames:
		}
		frame, err := next.frame, next.err
		if err != nil {
			if errors.Is(err, io.EOF) {
				state.cancelAll()
				if shuttingDown {
					return nil
				}
				return nil
			}
			state.cancelAll()
			return fmt.Errorf("external sdk: read frame: %w", err)
		}

		if len(bytes.TrimSpace(frame)) == 0 {
			return errors.New("external sdk: blank frame")
		}
		var request external.RPCRequest
		if err := json.Unmarshal(frame, &request); err != nil {
			if writeErr := state.errorResponse("", external.RPCParseError, err.Error()); writeErr != nil {
				return writeErr
			}
			continue
		}
		if request.JSONRPC != "2.0" || request.Method == "" {
			if writeErr := state.errorResponse(request.ID, external.RPCInvalidRequest, "invalid JSON-RPC request"); writeErr != nil {
				return writeErr
			}
			continue
		}
		if request.Method == external.MethodCancel {
			state.cancelRequest(request.Params)
			continue
		}
		if request.ID == "" {
			if writeErr := state.errorResponse("", external.RPCInvalidRequest, "request id must be a string"); writeErr != nil {
				return writeErr
			}
			continue
		}

		if !initialized {
			if request.Method != external.MethodInitialize {
				if writeErr := state.errorResponse(request.ID, external.RPCInvalidRequest, "plugin must be initialized first"); writeErr != nil {
					return writeErr
				}
				continue
			}
			result, err := initialize(s, request.Params)
			if err != nil {
				if writeErr := state.errorResponse(request.ID, external.RPCInvalidParams, err.Error()); writeErr != nil {
					return writeErr
				}
				continue
			}
			if err := state.response(request.ID, result); err != nil {
				return err
			}
			initialized = true
			continue
		}

		switch request.Method {
		case external.MethodExecute:
			var params external.ExecuteParams
			if err := json.Unmarshal(request.Params, &params); err != nil || params.Action.Kind == "" {
				if err == nil {
					err = errors.New("action.kind is required")
				}
				if writeErr := state.errorResponse(request.ID, external.RPCInvalidParams, err.Error()); writeErr != nil {
					return writeErr
				}
				continue
			}
			if _, err := protocol.ObjectArgs(params.Action.Args); err != nil {
				if writeErr := state.errorResponse(request.ID, external.RPCInvalidParams, "action.args: "+err.Error()); writeErr != nil {
					return writeErr
				}
				continue
			}

			tool, ok := tools[string(params.Action.Kind)]
			if !ok {
				// Unknown action kinds are domain observations, not protocol
				// failures. This keeps a provider failure visible to the brain.
				tr := protocol.ToolResult{ActionID: params.Action.ID, Kind: string(params.Action.Kind), OK: false,
					Error: fmt.Sprintf("no tool %q", params.Action.Kind)}
				if err := state.response(request.ID, tr); err != nil {
					return err
				}
				continue
			}
			state.startCall(request.ID, params.Action, tool)
		case external.MethodHealth:
			if err := state.response(request.ID, external.HealthResult{Status: "ok"}); err != nil {
				return err
			}
		case external.MethodShutdown:
			if err := state.response(request.ID, map[string]any{}); err != nil {
				return err
			}
			shuttingDown = true
			state.cancelAll()
			wait := state.shutdownWait()
			if wait > 0 {
				finished := make(chan struct{})
				go func() { state.wg.Wait(); close(finished) }()
				select {
				case <-finished:
				case <-time.After(wait):
				}
			}
			return nil
		default:
			if err := state.errorResponse(request.ID, external.RPCMethodNotFound, "method not found"); err != nil {
				return err
			}
		}
	}
}

type serveState struct {
	ctx      context.Context
	tools    map[string]Tool
	maxFrame int
	shutdown time.Duration
	maxConc  int

	outMu    sync.Mutex
	out      io.Writer
	mu       sync.Mutex
	cancels  map[string]context.CancelFunc
	wg       sync.WaitGroup
	sem      chan struct{}
	fatal    chan error
	failOnce sync.Once
}

func initialize(s Server, raw json.RawMessage) (external.InitializeResult, error) {
	var params external.InitializeParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return external.InitializeResult{}, err
	}
	if params.RuntimeProtocol != external.RuntimeProtocol {
		return external.InitializeResult{}, fmt.Errorf("unsupported runtime_protocol %d", params.RuntimeProtocol)
	}
	if params.Host.Placement != external.PlacementHands {
		return external.InitializeResult{}, fmt.Errorf("tool_provider is only valid in hands placement")
	}
	supported := false
	for _, version := range params.SupportedCapabilities[external.CapabilityToolProvider] {
		if version == external.ToolProviderVersion {
			supported = true
		}
	}
	if !supported {
		return external.InitializeResult{}, errors.New("host does not support tool_provider/v1")
	}

	tools := make([]external.ToolDescription, 0, len(s.Tools))
	for _, tool := range s.Tools {
		tools = append(tools, external.ToolDescription{
			Kind: tool.Kind, Description: tool.Description,
			InputSchema: append(json.RawMessage(nil), tool.InputSchema...),
		})
	}
	maxConcurrency := s.MaxConcurrency
	if s.Unbounded {
		// Explicit zero is meaningful on the wire; json omitempty would
		// otherwise collapse it into the serial-by-default omission.
		maxConcurrency = 0
	} else if maxConcurrency == 0 {
		maxConcurrency = 1
	}
	var (
		configuration []byte
		err           error
	)
	if s.Unbounded {
		// ToolProviderConfiguration uses omitempty so its zero value means
		// omitted/serial for ordinary Go callers. The SDK's explicit
		// Unbounded option writes the zero field deliberately.
		configuration, err = json.Marshal(struct {
			MaxConcurrency int                        `json:"max_concurrency"`
			Tools          []external.ToolDescription `json:"tools"`
		}{MaxConcurrency: 0, Tools: tools})
	} else {
		config := external.ToolProviderConfiguration{MaxConcurrency: maxConcurrency, Tools: tools}
		configuration, err = json.Marshal(config)
	}
	if err != nil {
		return external.InitializeResult{}, err
	}

	return external.InitializeResult{
		Plugin:       external.PluginInfo{Name: s.Name, Version: s.Version},
		Capabilities: []external.Capability{{Type: external.CapabilityToolProvider, Version: external.ToolProviderVersion, Configuration: configuration}},
	}, nil
}

func (s *serveState) startCall(id string, action protocol.Action, tool Tool) {
	callCtx, cancel := context.WithCancel(s.ctx)
	s.mu.Lock()
	s.cancels[id] = cancel
	s.mu.Unlock()
	s.wg.Go(func() {
		defer func() {
			s.mu.Lock()
			delete(s.cancels, id)
			s.mu.Unlock()
			cancel()
		}()
		if s.maxConc > 0 {
			s.mu.Lock()
			if s.sem == nil {
				s.sem = make(chan struct{}, s.maxConc)
			}
			sem := s.sem
			s.mu.Unlock()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-callCtx.Done():
				s.respondResult(id, protocol.ToolResult{ActionID: action.ID, Kind: string(action.Kind), OK: false, Error: "canceled"})
				return
			}
		}

		result, err := callTool(callCtx, tool.Handler, action)
		if err != nil {
			result = protocol.ToolResult{ActionID: action.ID, Kind: string(action.Kind), OK: false, Error: err.Error()}
		}

		// The SDK is also an untrusted boundary for handler output: always
		// normalize both correlation and payload namespace from the request.
		result.ActionID = action.ID
		result.Kind = string(action.Kind)
		s.respondResult(id, result)
	})
}

func callTool(ctx context.Context, handler func(context.Context, protocol.Action) (protocol.ToolResult, error), action protocol.Action) (result protocol.ToolResult, err error) {
	defer func() {
		if value := recover(); value != nil {
			err = fmt.Errorf("tool panic: %v", value)
		}
	}()
	return handler(ctx, action)
}

func (s *serveState) respondResult(id string, result protocol.ToolResult) {
	if err := s.response(id, result); err != nil {
		s.failOnce.Do(func() { s.fatal <- err })
	}
}

func (s *serveState) cancelRequest(raw json.RawMessage) {
	var params struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &params) != nil || params.ID == "" {
		return
	}
	s.mu.Lock()
	cancel := s.cancels[params.ID]
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *serveState) cancelAll() {
	s.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(s.cancels))
	for _, cancel := range s.cancels {
		cancels = append(cancels, cancel)
	}
	s.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

func (s *serveState) shutdownWait() time.Duration { return s.shutdown }

func (s *serveState) response(id string, result any) error {
	data, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return s.write(external.RPCResponse{JSONRPC: "2.0", ID: id, Result: data})
}

func (s *serveState) errorResponse(id string, code int, message string) error {
	return s.write(external.RPCResponse{JSONRPC: "2.0", ID: id, Error: &external.RPCError{Code: code, Message: message}})
}

func (s *serveState) write(message external.RPCResponse) error {
	data, err := json.Marshal(message)
	if err != nil {
		return err
	}
	if len(data)+1 > s.maxFrame {
		return fmt.Errorf("external sdk: response frame exceeds %d bytes", s.maxFrame)
	}
	data = append(data, '\n')
	s.outMu.Lock()
	defer s.outMu.Unlock()
	for len(data) > 0 {
		n, err := s.out.Write(data)
		if err != nil {
			return err
		}
		data = data[n:]
	}
	return nil
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
