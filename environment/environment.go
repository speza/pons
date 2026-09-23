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

// WorkspacePlan selects provider-controlled provisioning for a logical
// workspace. SourceRef is a non-secret source identifier; for git/v1 it is a
// credential-free HTTPS repository URL. BaseRevision is an immutable commit ID
// or a fully qualified branch ref, resolved when the workspace is provisioned.
// A zero plan preserves the provider's archive/v1 behavior.
type WorkspacePlan struct {
	Strategy     WorkspaceStrategy
	SourceRef    string
	BaseRevision string
}

// Spec is the explicit policy and launch input for one hands environment.
// WorkspaceID is the independent logical workspace identity. WorkspacePath is
// the host-local path used directly by local providers or once as an archive
// source. Command[0] must be an absolute pons-hands executable path.
// Environment is a clean, explicit list of KEY=VALUE entries; the parent
// environment is never inherited.
type Spec struct {
	WorkspaceID        string
	WorkspacePath      string
	RunID              string
	Command            []string
	ReadOnly           []string
	Network            NetworkPolicy
	Environment        []string
	Limits             ResourceLimits
	WorkspacePlan      WorkspacePlan
	GitAllRepositories bool
	// ReportProgress emits transient, per-run setup stages to the caller.
	ReportProgress func(string) error
}

// Metadata identifies the effective backend and policy for audit and UI use.
// WorkspacePath and Platform describe hands, not the host-local source checkout.
type Metadata struct {
	Provider      string
	EnvironmentID string
	WorkspaceID   string
	WorkspacePath string
	Platform      string // GOOS/GOARCH of the execution environment
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
