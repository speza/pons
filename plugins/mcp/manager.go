package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"
)

const (
	defaultMaxResultBytes = 1 << 20
	// maxKindLength is the strictest tool-name limit among supported providers.
	maxKindLength      = 64
	maxDescriptionSize = 2 << 10
	maxSchemaSize      = 64 << 10
	// MaxToolsPerServer bounds the tools one server may add to a run.
	MaxToolsPerServer = 128
	// minResponseBytes keeps a small result limit from also cutting off a
	// large server's tool list.
	minResponseBytes = 8 << 20
	maxFailureText   = 512
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

	// connecting serializes opening a session. It is a channel so a waiter
	// can give up when its context ends, which a mutex cannot.
	connecting chan struct{}

	mu      sync.Mutex
	current session
	closed  bool
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
		entry := &server{name: name, config: config, connecting: make(chan struct{}, 1)}
		if config.enabled() {
			headers, secrets, err := resolveHeaders(config.Headers, options.Secrets)
			if err != nil {
				return nil, fmt.Errorf("mcpServers.%s: %w", name, err)
			}
			// A key in the URL's query is as secret as one in a header, and
			// transport errors quote the URL.
			if parsed, err := url.Parse(config.URL); err == nil {
				for _, values := range parsed.Query() {
					secrets = append(secrets, values...)
				}
			}
			entry.redact = newRedactor(secrets)
			// The transport cap sits above the result limit so an oversized
			// result is reported as one, not as a broken response.
			responseBytes := max(4*int64(maxResultBytes), minResponseBytes)
			entry.connect = sdkConnector(config.URL, newHTTPClient(headers, responseBytes))
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
		entry.mu.Lock()
		open := entry.current
		entry.current, entry.closed = nil, true
		entry.mu.Unlock()
		if open != nil {
			errs = append(errs, open.Close())
		}
	}
	return errors.Join(errs...)
}

// ToolInfo is one tool a server reports and what pons does with it.
type ToolInfo struct {
	Name     string
	ReadOnly bool
	// Status is "available", "filtered", or "skipped: " and the reason.
	Status string
}

// StatusAvailable marks a tool a run would register.
const StatusAvailable = "available"

// Tools connects to one enabled server and reports its tools against the
// host filters, in the order the server lists them. A run registers the
// available ones, unless there are more than MaxToolsPerServer.
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
	_, infos := entry.selectTools(tools, Filter{})
	return infos, nil
}

// selectTools applies the host filter, a grant's filter, and the limits on
// what a server may report, keeping the server's order. It returns the tools
// to register and a status for every tool listed.
func (s *server) selectTools(tools []tool, grant Filter) (selected []preparedTool, infos []ToolInfo) {
	host := s.config.filter()
	seen := make(map[string]bool, len(tools))
	for _, reported := range tools {
		info := ToolInfo{Name: reported.Name, ReadOnly: reported.ReadOnly, Status: StatusAvailable}
		prepared, reason := s.prepare(reported)
		switch {
		case !host.allows(reported.Name) || !grant.allows(reported.Name):
			info.Status = "filtered"
		case reason != "":
			info.Status = "skipped: " + reason
		case seen[reported.Name]:
			info.Status = "skipped: tool is listed twice"
		default:
			seen[reported.Name] = true
			selected = append(selected, prepared)
		}
		infos = append(infos, info)
	}
	return selected, infos
}

// open returns the shared session, or nil when there is none.
func (s *server) open() session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current
}

// session returns the shared session, opening it if needed.
func (s *server) session(ctx context.Context) (session, error) {
	if current := s.open(); current != nil {
		return current, nil
	}
	select {
	case s.connecting <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-s.connecting }()

	// Another caller may have connected while this one waited.
	if current := s.open(); current != nil {
		return current, nil
	}
	opened, err := s.connect(ctx)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		go func() { _ = opened.Close() }()
		return nil, errors.New("mcp: connections are closed")
	}
	s.current = opened
	return opened, nil
}

// drop forgets a session that stopped working, unless another caller already
// replaced it. It never waits on a connection being opened.
func (s *server) drop(stale session) {
	s.mu.Lock()
	dropped := s.current == stale
	if dropped {
		s.current = nil
	}
	s.mu.Unlock()

	if dropped {
		// Closing may wait on the server; nothing depends on its outcome.
		go func() { _ = stale.Close() }()
	}
}

// listTools lists the server's tools within its startup deadline. Listing
// changes nothing, so any failure is retried once on a new session. That
// also recovers a session the server lost without saying so the way the
// specification describes.
func (s *server) listTools(ctx context.Context) (tools []tool, err error) {
	ctx, cancel := context.WithTimeout(ctx, s.config.startupTimeout())
	defer cancel()

	for range 2 {
		var current session
		if current, err = s.session(ctx); err != nil {
			return nil, err
		}
		if tools, err = current.ListTools(ctx); err == nil || ctx.Err() != nil {
			return tools, err
		}
		s.drop(current)
	}
	return nil, err
}

// callTool calls one tool within the server's tool deadline. A call is sent
// again, once and on a new session, only when it provably did not run: the
// server refused this very request for its session, or the request was never
// sent. A session ending fails every call in flight on it, and one of those
// may already have run.
func (s *server) callTool(ctx context.Context, name string, args []byte) (callResult, error) {
	ctx, cancel := context.WithTimeout(ctx, s.config.toolTimeout())
	defer cancel()

	for attempt := 0; ; attempt++ {
		current, err := s.session(ctx)
		if err != nil {
			return callResult{}, err
		}
		state := &callState{}
		result, err := current.CallTool(withCallState(ctx, state), name, args)
		lost := state.refused.Load() || errors.Is(err, errSessionGone) || errors.Is(err, errSessionBroken)
		if err == nil || !lost {
			return result, err
		}

		s.drop(current)
		if didNotRun := state.refused.Load() || !state.sent.Load(); attempt > 0 || !didNotRun || ctx.Err() != nil {
			return callResult{}, err
		}
	}
}

// failure renders an error for the owner and the brain. Server text is
// redacted, and a deadline is named for what it bounded.
func (s *server) failure(ctx context.Context, err error) error {
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		return fmt.Errorf("MCP server %q did not answer in time", s.name)
	}
	text := s.redact.text(err.Error())
	if len(text) > maxFailureText {
		text = strings.ToValidUTF8(text[:maxFailureText], "") + "…"
	}
	return fmt.Errorf("MCP server %q: %s", s.name, text)
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

	schema, reason := s.safeSchema(reported.InputSchema)
	if reason != "" {
		return preparedTool{}, reason
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

// safeSchema returns an input schema every supported provider accepts: an
// object schema with a properties member and no top-level combinator. One
// schema a provider rejects would fail every model request of the run, so a
// tool whose schema cannot be made safe is skipped instead.
func (s *server) safeSchema(raw json.RawMessage) (schema json.RawMessage, reason string) {
	if trimmed := bytes.TrimSpace(raw); len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		raw = json.RawMessage(`{}`)
	}
	if len(raw) > maxSchemaSize {
		return nil, fmt.Sprintf("input schema is larger than %d bytes", maxSchemaSize)
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil || members == nil {
		return nil, "input schema is not an object"
	}

	if kind, ok := members["type"]; ok && string(bytes.TrimSpace(kind)) != `"object"` {
		return nil, "input schema does not describe an object"
	}
	for _, combinator := range []string{"anyOf", "oneOf", "allOf", "not", "enum"} {
		if _, ok := members[combinator]; ok {
			return nil, "input schema uses a top-level " + combinator
		}
	}
	members["type"] = json.RawMessage(`"object"`)
	if _, ok := members["properties"]; !ok {
		members["properties"] = json.RawMessage(`{}`)
	}

	encoded, err := json.Marshal(members)
	if err != nil {
		return nil, "input schema cannot be encoded"
	}
	if schema = json.RawMessage(s.redact.text(string(encoded))); !json.Valid(schema) {
		return nil, "input schema cannot be encoded"
	}
	return schema, ""
}
