package external

import (
	"context"
	"fmt"

	"github.com/samperrin/pons"
)

const HookToolCallStart = "on_tool_call_start"

func validHookName(name string) bool { return name == HookToolCallStart }

// HookPlugin adapts an explicitly installed host-side hook provider.
// It receives tool-call context but no host credentials or Core handle.
type HookPlugin struct{ host *Host }

func (p *HookPlugin) Host() *Host { return p.host }

func NewHooks(manifestPath string, cfg HostConfig) (*HookPlugin, error) {
	manifest, err := LoadManifest(manifestPath)
	if err != nil {
		return nil, err
	}
	cfg.Placement = PlacementHost
	host, err := NewHost(manifest, cfg)
	if err != nil {
		return nil, err
	}
	return &HookPlugin{host: host}, nil
}

func (p *HookPlugin) Setup(core *pons.Core) error {
	if err := p.host.Start(context.Background()); err != nil {
		return err
	}
	names := p.host.HookNames()
	if len(names) != 1 || names[0] != HookToolCallStart {
		_ = p.host.Close()
		return fmt.Errorf("external: unsupported host hooks from %q", p.host.manifest.Name)
	}
	if err := core.AddHooks(pons.Hooks{OnToolCallStart: func(ctx context.Context, event *pons.ToolCallStartEvent) error {
		var decision pons.ActionDecision
		if err := p.host.CallHook(ctx, HookToolCallStart, event, &decision); err != nil {
			return err
		}
		event.Decision = decision
		return nil
	}}); err != nil {
		_ = p.host.Close()
		return err
	}
	return nil
}

func (p *HookPlugin) Close() error { return p.host.Close() }
