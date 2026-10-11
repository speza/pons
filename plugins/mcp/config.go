// Package mcp is the built-in MCP client plugin. It connects to the remote
// MCP servers named in host configuration and registers the tools an agent is
// granted as Core tools named mcp__<server>__<tool>.
//
// Servers are untrusted: what they report is bounded and filtered, and what
// they return is data. Their secrets stay in this package's HTTP transport.
package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/samperrin/pons/internal/toolpattern"
)

// Approval modes are a server's default answer for its tools.
const (
	ApprovalAuto   = "auto"
	ApprovalAsk    = "ask"
	ApprovalWrites = "writes"
	ApprovalAllow  = "allow"
)

const (
	typeHTTP  = "http"
	typeStdio = "stdio"

	defaultStartupTimeout = 10 * time.Second
	defaultToolTimeout    = 60 * time.Second
	maxServerNameLength   = 32
)

// A name cannot end in an underscore or contain two in a row, so
// mcp__<server>__<tool> always splits at the first "__" after the prefix.
var serverNamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9_-]*[a-z0-9])?$`)

// ServerConfig is one entry of the host configuration's mcpServers map, in
// the shape other MCP clients use. Command, Args, Env, and OAuth are decoded
// only so they can be refused by name until they are supported.
type ServerConfig struct {
	Type              string            `json:"type,omitempty"`
	URL               string            `json:"url,omitempty"`
	Headers           map[string]string `json:"headers,omitempty"`
	Enabled           *bool             `json:"enabled,omitempty"`
	ApprovalMode      string            `json:"approvalMode,omitempty"`
	EnabledTools      []string          `json:"enabledTools,omitempty"`
	DisabledTools     []string          `json:"disabledTools,omitempty"`
	StartupTimeoutSec *int              `json:"startupTimeoutSec,omitempty"`
	ToolTimeoutSec    *int              `json:"toolTimeoutSec,omitempty"`

	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	OAuth   json.RawMessage   `json:"oauth,omitempty"`
}

// Filter selects tools by glob pattern. A tool passes when Enabled is empty
// or matches it, and Disabled does not match it.
type Filter struct {
	Enabled  []string
	Disabled []string
}

// Validate rejects malformed patterns.
func (f Filter) Validate() error {
	for _, pattern := range slices.Concat(f.Enabled, f.Disabled) {
		if !toolpattern.Valid(pattern) {
			return fmt.Errorf("tool pattern %q is malformed", pattern)
		}
	}
	return nil
}

func (f Filter) allows(tool string) bool {
	matches := func(pattern string) bool { return toolpattern.Match(pattern, tool) }
	if len(f.Enabled) > 0 && !slices.ContainsFunc(f.Enabled, matches) {
		return false
	}
	return !slices.ContainsFunc(f.Disabled, matches)
}

func (c ServerConfig) enabled() bool { return c.Enabled == nil || *c.Enabled }

func (c ServerConfig) filter() Filter {
	return Filter{Enabled: c.EnabledTools, Disabled: c.DisabledTools}
}

func (c ServerConfig) approvalMode() string {
	if c.ApprovalMode == "" {
		return ApprovalAuto
	}
	return c.ApprovalMode
}

func (c ServerConfig) startupTimeout() time.Duration {
	return timeoutSeconds(c.StartupTimeoutSec, defaultStartupTimeout)
}

func (c ServerConfig) toolTimeout() time.Duration {
	return timeoutSeconds(c.ToolTimeoutSec, defaultToolTimeout)
}

func timeoutSeconds(seconds *int, fallback time.Duration) time.Duration {
	if seconds == nil {
		return fallback
	}
	return time.Duration(*seconds) * time.Second
}

// ValidateServers checks every entry of an mcpServers map, in name order so
// the first error is stable. It does not resolve secrets or open connections.
func ValidateServers(servers map[string]ServerConfig) error {
	for _, name := range slices.Sorted(maps.Keys(servers)) {
		if err := validateServer(name, servers[name]); err != nil {
			return fmt.Errorf("mcpServers.%s: %w", name, err)
		}
	}
	return nil
}

func validateServer(name string, config ServerConfig) error {
	if !serverNamePattern.MatchString(name) || len(name) > maxServerNameLength || strings.Contains(name, "__") {
		return fmt.Errorf("server name must match %s, be at most %d characters, and not contain \"__\"",
			serverNamePattern, maxServerNameLength)
	}
	if config.Command != "" || len(config.Args) > 0 || len(config.Env) > 0 || config.Type == typeStdio {
		return errors.New("stdio servers are not supported yet; use a remote server with \"type\": \"http\"")
	}
	if len(config.OAuth) > 0 {
		return errors.New("oauth is not supported yet; authenticate with a token in \"headers\"")
	}
	if config.Type != typeHTTP {
		return fmt.Errorf("type must be %q, not %q", typeHTTP, config.Type)
	}
	if err := validateURL(config.URL); err != nil {
		return err
	}

	for header, value := range config.Headers {
		if !validHeaderName(header) {
			return fmt.Errorf("header name %q is not valid", header)
		}
		if err := validateHeaderValue(value); err != nil {
			return fmt.Errorf("header %q: %w", header, err)
		}
	}
	switch config.ApprovalMode {
	case "", ApprovalAuto, ApprovalAsk, ApprovalWrites, ApprovalAllow:
	default:
		return fmt.Errorf("approvalMode must be %q, %q, %q, or %q, not %q",
			ApprovalAuto, ApprovalAsk, ApprovalWrites, ApprovalAllow, config.ApprovalMode)
	}
	if err := config.filter().Validate(); err != nil {
		return err
	}
	for field, seconds := range map[string]*int{
		"startupTimeoutSec": config.StartupTimeoutSec,
		"toolTimeoutSec":    config.ToolTimeoutSec,
	} {
		if seconds != nil && *seconds <= 0 {
			return fmt.Errorf("%s must be positive", field)
		}
	}
	return nil
}

// validateURL requires HTTPS, or HTTP to this machine, so a token never
// crosses a network in the clear.
func validateURL(raw string) error {
	if raw == "" {
		return errors.New("url is required")
	}
	if strings.Contains(raw, "${") {
		return errors.New("secret references are supported only in headers, not in url")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return errors.New("url is not a valid absolute URL")
	}
	if parsed.User != nil {
		return errors.New("url must not contain credentials; use \"headers\"")
	}
	if parsed.Fragment != "" {
		return errors.New("url must not have a fragment")
	}

	switch parsed.Scheme {
	case "https":
		return nil
	case "http":
		host := parsed.Hostname()
		if ip := net.ParseIP(host); host == "localhost" || (ip != nil && ip.IsLoopback()) {
			return nil
		}
		return errors.New("url must use https unless the host is localhost or a loopback address")
	default:
		return fmt.Errorf("url scheme must be https, not %q", parsed.Scheme)
	}
}

func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if r <= ' ' || r >= 0x7f || strings.ContainsRune(`()<>@,;:\"/[]?={}`, r) {
			return false
		}
	}
	return true
}
