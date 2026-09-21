// Package environment defines the deployment seam between the pons host and
// a complete tool host behind the hands boundary. Providers own placement, policy, connectivity,
// health, and teardown; the core continues to see an ordinary ToolPort.
package environment

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/samperrin/pons"
	"github.com/samperrin/pons/plugins/external"
	"github.com/samperrin/pons/protocol"
)

// NetworkPolicy controls network access inside an environment.
type NetworkPolicy string

const (
	NetworkDisabled NetworkPolicy = "disabled"
	NetworkEnabled  NetworkPolicy = "enabled"
)

// ResourceLimits are the host-enforceable limits for one tool endpoint.
// Provider-specific CPU, memory, process, and disk limits can be added without
// changing the action/result contract.
type ResourceLimits struct {
	CallTimeout        time.Duration
	MaxFrameBytes      int
	MaxStderrBytes     int
	MaxResultBytes     int
	MaxTools           int
	MaxPendingRequests int
	MaxConcurrency     int
}

// Spec is the explicit policy and launch input for one hands environment.
// Command[0] must be an absolute pons-hands executable path. Environment is a
// clean, explicit list of KEY=VALUE entries; the parent environment is never
// inherited.
type Spec struct {
	Workspace   string
	LeaseID     string
	Command     []string
	ReadOnly    []string
	Network     NetworkPolicy
	Environment []string
	Limits      ResourceLimits
}

// Metadata identifies the effective backend and policy for audit and UI use.
type Metadata struct {
	Provider      string
	EnvironmentID string
	Workspace     string
	Network       NetworkPolicy
}

// Provider provisions a hands environment and establishes its endpoint.
type Provider interface {
	Start(context.Context, Spec) (HandsSession, error)
}

// HandsSession is one live, discovered hands endpoint.
type HandsSession interface {
	pons.ToolPort
	Catalog() []external.ToolDescription
	Metadata() Metadata
	Close() error
}

// Proxy exposes a discovered hands session as an additive Core plugin.
// Closing the session remains the caller's responsibility.
func Proxy(session HandsSession) pons.Plugin {
	return &proxy{session: session}
}

type proxy struct {
	session HandsSession
	mu      sync.Mutex
	setup   bool
}

func (p *proxy) Setup(core *pons.Core) error {
	if p.session == nil {
		return errors.New("environment: nil hands session")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.setup {
		return errors.New("environment: hands session already registered")
	}
	tools := p.session.Catalog()
	for _, tool := range tools {
		if core.HasTool(protocol.ActionKind(tool.Kind)) {
			return fmt.Errorf("environment: action kind %q already registered (plugin conflict)", tool.Kind)
		}
	}
	meta := p.session.Metadata()
	for _, tool := range tools {
		if err := core.AddTool(protocol.ActionKind(tool.Kind), pons.ToolDef{
			Description: tool.Description,
			InputSchema: slices.Clone(tool.InputSchema),
			Source: pons.ToolSource{
				PluginName:        "pons.hands",
				PluginVersion:     "0.1.0",
				Capability:        external.CapabilityToolProvider,
				CapabilityVersion: external.ToolProviderVersion,
				Executable:        meta.Provider,
				External:          true,
			},
			Handler: p.session.Execute,
		}); err != nil {
			return fmt.Errorf("environment: register tool %q: %w", tool.Kind, err)
		}
	}
	p.setup = true
	return nil
}
