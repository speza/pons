package external

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/samperrin/pons"
	"github.com/samperrin/pons/protocol"
)

// Config constructs an explicitly activated external hands plugin from a
// manifest path. No directory is scanned implicitly.
type Config struct {
	ManifestPath string
	Host         HostConfig
}

// Plugin is the in-process pons.Plugin adapter for one persistent external
// child. Setup starts the child, validates its authoritative capability
// handshake, and installs one proxy handler per discovered tool.
type Plugin struct {
	host      *Host
	mu        sync.Mutex
	setupDone bool
}

// New loads a manifest and constructs an external plugin. Setup performs the
// process launch, so callers can construct all plugins before composition.
func New(cfg Config) (*Plugin, error) {
	if cfg.ManifestPath == "" {
		return nil, fmt.Errorf("external: manifest path is required")
	}
	manifest, err := LoadManifest(cfg.ManifestPath)
	if err != nil {
		return nil, err
	}
	host, err := NewHost(manifest, cfg.Host)
	if err != nil {
		return nil, err
	}
	return &Plugin{host: host}, nil
}

// NewHands is an explicit hands-side spelling useful to embedding runtimes.
// It prevents a future control-plane capability from accidentally being
// composed with the tool provider adapter.
func NewHands(manifestPath string, cfg HostConfig) (*Plugin, error) {
	cfg.Placement = PlacementHands
	return New(Config{ManifestPath: manifestPath, Host: cfg})
}

// Host returns the persistent RPC host after construction. Tools are empty
// until Setup has completed successfully.
func (p *Plugin) Host() *Host { return p.host }

// Setup implements pons.Plugin. A failed additive registration closes the
// child so a conflict cannot leave a live unowned process behind.
func (p *Plugin) Setup(c *pons.Core) error {
	p.mu.Lock()
	if p.setupDone {
		p.mu.Unlock()
		return fmt.Errorf("external: plugin %q already set up", p.host.manifest.Name)
	}
	p.mu.Unlock()
	if err := p.host.Start(context.Background()); err != nil {
		return err
	}
	tools := p.host.Tools()
	info := p.host.PluginInfo()
	for _, tool := range tools {
		if c.HasTool(protocol.ActionKind(tool.Kind)) {
			_ = p.host.Close()
			return fmt.Errorf("external plugin %q tool %q: action kind %q already registered (plugin conflict)", p.host.manifest.Name, tool.Kind, tool.Kind)
		}
	}
	for _, tool := range tools {
		if err := c.AddTool(protocol.ActionKind(tool.Kind), pons.ToolDef{
			Description: tool.Description,
			InputSchema: append(json.RawMessage(nil), tool.InputSchema...),
			Source: pons.ToolSource{
				PluginName:        info.Name,
				PluginVersion:     info.Version,
				Capability:        CapabilityToolProvider,
				CapabilityVersion: ToolProviderVersion,
				Executable:        p.host.manifest.ResolvedEntrypoint(),
				External:          true,
			},
			Handler: func(ctx context.Context, action protocol.Action) (protocol.ToolResult, error) {
				return p.host.Execute(ctx, action)
			},
		}); err != nil {
			_ = p.host.Close()
			return fmt.Errorf("external plugin %q tool %q: %w", p.host.manifest.Name, tool.Kind, err)
		}
	}
	p.mu.Lock()
	p.setupDone = true
	p.mu.Unlock()
	return nil
}

// Close shuts down the child. Core does not own plugin lifetime, so an
// embedding application should defer this method after successful New.
func (p *Plugin) Close() error {
	if p == nil || p.host == nil {
		return nil
	}
	return p.host.Close()
}

// Tools exposes the validated discovery catalog for deployment/audit code.
func (p *Plugin) Tools() []ToolDescription {
	if p == nil || p.host == nil {
		return nil
	}
	return p.host.Tools()
}
