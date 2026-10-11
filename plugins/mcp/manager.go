package mcp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"unicode/utf8"
)

const (
	defaultMaxResultBytes = 1 << 20
	// maxKindLength is the strictest tool-name limit among supported providers.
	maxKindLength      = 64
	maxDescriptionSize = 2 << 10
	maxSchemaSize      = 64 << 10
	maxToolsPerServer  = 128
)

var toolNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// Options configures a Manager.
type Options struct {
	Servers map[string]ServerConfig
	// Secrets are the named values ${secret:<name>} references resolve to.
	Secrets map[string]string
	// MaxResultBytes bounds one tool result; zero uses 1 MiB.
	MaxResultBytes int
}

// Manager owns the connections to the configured servers. One Manager serves
// every run of a pons server; each run takes its own Plugin from it.
type Manager struct {
	servers        map[string]*server
	maxResultBytes int
}

// server is one configured MCP server and its shared session.
type server struct {
	name    string
	config  ServerConfig
	redact  redactor
	connect connector

	// gate guards current. It is a channel so a waiter can give up when its
	// context ends, which a mutex cannot.
	gate    chan struct{}
	current session
}

// NewManager validates the servers and resolves the secrets of the enabled
// ones. It opens no connection; sessions start on first use.
func NewManager(options Options) (*Manager, error) {
	if err := ValidateServers(options.Servers); err != nil {
		return nil, err
	}
	maxResultBytes := options.MaxResultBytes
	if maxResultBytes <= 0 {
		maxResultBytes = defaultMaxResultBytes
	}

	manager := &Manager{servers: make(map[string]*server, len(options.Servers)), maxResultBytes: maxResultBytes}
	for _, name := range slices.Sorted(maps.Keys(options.Servers)) {
		config := options.Servers[name]
		entry := &server{name: name, config: config, gate: make(chan struct{}, 1)}
		if config.enabled() {
			headers, secrets, err := resolveHeaders(config.Headers, options.Secrets)
			if err != nil {
				return nil, fmt.Errorf("mcpServers.%s: %w", name, err)
			}
			entry.redact = newRedactor(secrets)
			// The transport cap sits above the result limit so an oversized
			// result is reported as one, not as a broken response.
			entry.connect = sdkConnector(config.URL, newHTTPClient(headers, 4*int64(maxResultBytes)))
		}
		manager.servers[name] = entry
	}
	return manager, nil
}

// ServerInfo describes a configured server without its headers.
type ServerInfo struct {
	Name         string
	Host         string
	ApprovalMode string
	Enabled      bool
}

// Servers lists the configured servers in name order.
func (m *Manager) Servers() []ServerInfo {
	if m == nil {
		return nil
	}
	infos := make([]ServerInfo, 0, len(m.servers))
	for _, name := range slices.Sorted(maps.Keys(m.servers)) {
		entry := m.servers[name]
		host := ""
		if parsed, err := url.Parse(entry.config.URL); err == nil {
			host = parsed.Host
		}
		infos = append(infos, ServerInfo{
			Name: name, Host: host,
			ApprovalMode: entry.config.approvalMode(), Enabled: entry.config.enabled(),
		})
	}
	return infos
}

// Configured reports whether a server of this name is in the configuration,
// enabled or not.
func (m *Manager) Configured(name string) bool {
	if m == nil {
		return false
	}
	_, ok := m.servers[name]
	return ok
}

// Close ends every open session.
func (m *Manager) Close() error {
	if m == nil {
		return nil
	}
	var errs []error
	for _, entry := range m.servers {
		entry.gate <- struct{}{}
		if entry.current != nil {
			errs = append(errs, entry.current.Close())
			entry.current = nil
		}
		<-entry.gate
	}
	return errors.Join(errs...)
}

// ToolInfo is one tool a server reports and what pons would do with it.
type ToolInfo struct {
	Name     string
	ReadOnly bool
	// Status is "available", "filtered", or the reason the tool is skipped.
	Status string
}

// Tools connects to one enabled server and reports its tools against the
// host filters, in the order the server lists them.
func (m *Manager) Tools(ctx context.Context, name string) ([]ToolInfo, error) {
	entry, ok := m.servers[name]
	if !ok {
		return nil, fmt.Errorf("MCP server %q is not configured", name)
	}
	if entry.connect == nil {
		return nil, fmt.Errorf("MCP server %q is disabled", name)
	}
	tools, err := entry.listTools(ctx)
	if err != nil {
		return nil, entry.failure(ctx, err)
	}

	infos := make([]ToolInfo, 0, len(tools))
	for _, reported := range tools {
		info := ToolInfo{Name: reported.Name, ReadOnly: reported.ReadOnly, Status: "available"}
		if _, reason := entry.prepare(reported); reason != "" {
			info.Status = "skipped: " + reason
		} else if !entry.config.filter().allows(reported.Name) {
			info.Status = "filtered"
		}
		infos = append(infos, info)
	}
	return infos, nil
}

// session returns the shared session, opening it if needed.
func (s *server) session(ctx context.Context) (session, error) {
	select {
	case s.gate <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-s.gate }()

	if s.current == nil {
		opened, err := s.connect(ctx)
		if err != nil {
			return nil, err
		}
		s.current = opened
	}
	return s.current, nil
}

// drop forgets a session that stopped working, unless another caller already
// replaced it.
func (s *server) drop(stale session) {
	s.gate <- struct{}{}
	dropped := s.current == stale
	if dropped {
		s.current = nil
	}
	<-s.gate

	if dropped {
		// Closing may wait on the server; nothing depends on its outcome.
		go func() { _ = stale.Close() }()
	}
}

// listTools lists the server's tools within its startup deadline. Listing
// changes nothing, so it is retried once on a new session whenever the old
// one stopped working.
func (s *server) listTools(ctx context.Context) ([]tool, error) {
	ctx, cancel := context.WithTimeout(ctx, s.config.startupTimeout())
	defer cancel()

	for attempt := 0; ; attempt++ {
		current, err := s.session(ctx)
		if err != nil {
			return nil, err
		}
		tools, err := current.ListTools(ctx)
		if !errors.Is(err, errSessionGone) && !errors.Is(err, errSessionBroken) {
			return tools, err
		}
		s.drop(current)
		if attempt == 1 {
			return nil, err
		}
	}
}

// callTool calls one tool within the server's tool deadline. Only a call the
// server refused for a missing session is retried: any other failure may
// have run the tool.
func (s *server) callTool(ctx context.Context, name string, args []byte) (callResult, error) {
	ctx, cancel := context.WithTimeout(ctx, s.config.toolTimeout())
	defer cancel()

	for attempt := 0; ; attempt++ {
		current, err := s.session(ctx)
		if err != nil {
			return callResult{}, err
		}
		result, err := current.CallTool(ctx, name, args)
		switch {
		case errors.Is(err, errSessionGone) && attempt == 0:
			s.drop(current)
		case errors.Is(err, errSessionGone), errors.Is(err, errSessionBroken):
			s.drop(current)
			return callResult{}, err
		default:
			return result, err
		}
	}
}

// failure renders an error for the owner and the brain. Server text is
// redacted, and a deadline is named for what it bounded.
func (s *server) failure(ctx context.Context, err error) error {
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		return fmt.Errorf("MCP server %q did not answer in time", s.name)
	}
	return fmt.Errorf("MCP server %q: %s", s.name, s.redact.text(err.Error()))
}

// preparedTool is a reported tool made safe to register.
type preparedTool struct {
	tool
	Kind string
}

// prepare bounds what an untrusted server reports about one tool. A non-empty
// reason means the tool cannot be registered.
func (s *server) prepare(reported tool) (prepared preparedTool, reason string) {
	kind := "mcp__" + s.name + "__" + reported.Name
	switch {
	case !toolNamePattern.MatchString(reported.Name):
		return preparedTool{}, "tool name has unsupported characters"
	case len(kind) > maxKindLength:
		return preparedTool{}, fmt.Sprintf("full tool name is longer than %d characters", maxKindLength)
	}

	schema := bytes.TrimSpace(reported.InputSchema)
	switch {
	case len(schema) == 0 || bytes.Equal(schema, []byte("null")):
		schema = []byte(`{"type":"object"}`)
	case schema[0] != '{':
		return preparedTool{}, "input schema is not an object"
	case len(schema) > maxSchemaSize:
		return preparedTool{}, fmt.Sprintf("input schema is larger than %d bytes", maxSchemaSize)
	}

	description := s.redact.text(reported.Description)
	if len(description) > maxDescriptionSize {
		description = description[:maxDescriptionSize]
		for !utf8.ValidString(description) {
			description = description[:len(description)-1]
		}
	}
	reported.Description, reported.InputSchema = description, schema
	return preparedTool{tool: reported, Kind: kind}, ""
}
