package external

import (
	"context"
	"fmt"
	"unicode/utf8"

	"github.com/samperrin/pons"
	"github.com/samperrin/pons/protocol"
)

const (
	HookAgentStart        = "on_agent_start"
	HookAgentTurnStart    = "on_agent_turn_start"
	HookAssistantResponse = "on_assistant_response"
	HookToolCallStart     = "on_tool_call_start"
	HookPermissionRequest = "on_permission_request"
	HookToolCallEnd       = "on_tool_call_end"
	HookAgentTurnEnd      = "on_agent_turn_end"
	HookAgentEnd          = "on_agent_end"
)

func validHookName(name string) bool {
	switch name {
	case HookAgentStart, HookAgentTurnStart, HookAssistantResponse, HookToolCallStart,
		HookPermissionRequest, HookToolCallEnd, HookAgentTurnEnd, HookAgentEnd:
		return true
	default:
		return false
	}
}

// maxHookText bounds each tool output or error string sent to a hook, so
// hook inputs do not grow with tool output.
const maxHookText = 16 << 10

// HookPlugin adapts an explicitly installed host-side hook provider. Like a
// Go plugin's hooks, it is trusted user code: its outputs are applied with
// the same effect. The process still receives no host credentials or Core
// handle, and its inputs omit run history and bound tool output.
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

func boundText(text string) string {
	if len(text) <= maxHookText {
		return text
	}
	text = text[:maxHookText]
	for !utf8.ValidString(text) {
		text = text[:len(text)-1]
	}
	return text
}

func boundResult(result protocol.ToolResult) protocol.ToolResult {
	result.Output, result.Error = boundText(result.Output), boundText(result.Error)
	if len(result.Payload) > maxHookText {
		result.Payload = nil
	}
	return result
}

func errorMessage(err error) string {
	if err == nil {
		return ""
	}
	return boundText(err.Error())
}

func callHook[Out any](ctx context.Context, p *HookPlugin, name string, in any) (Out, error) {
	var out Out
	err := p.host.CallHook(ctx, name, in, &out)
	return out, err
}

func (p *HookPlugin) Setup(core *pons.Core) error {
	if err := p.host.Start(context.Background()); err != nil {
		return err
	}
	registered := make(map[string]bool)
	for _, name := range p.host.HookNames() {
		if !validHookName(name) {
			_ = p.host.Close()
			return fmt.Errorf("external: unsupported host hook %q", name)
		}
		registered[name] = true
	}

	var hooks pons.Hooks
	if registered[HookAgentStart] {
		hooks.OnAgentStart = func(ctx context.Context, in pons.AgentStartInput) (pons.AgentStartOutput, error) {
			return callHook[pons.AgentStartOutput](ctx, p, HookAgentStart, in)
		}
	}
	if registered[HookAgentTurnStart] {
		hooks.OnAgentTurnStart = func(ctx context.Context, in pons.AgentTurnStartInput) (pons.AgentTurnStartOutput, error) {
			in.Observation.History = nil
			return callHook[pons.AgentTurnStartOutput](ctx, p, HookAgentTurnStart, in)
		}
	}
	if registered[HookAssistantResponse] {
		hooks.OnAssistantResponse = func(ctx context.Context, in pons.AssistantResponseInput) (pons.AssistantResponseOutput, error) {
			return callHook[pons.AssistantResponseOutput](ctx, p, HookAssistantResponse, in)
		}
	}
	if registered[HookToolCallStart] {
		hooks.OnToolCallStart = func(ctx context.Context, in pons.ToolCallStartInput) (pons.ToolCallStartOutput, error) {
			if !p.host.HookWantsTool(string(in.Action.Kind)) {
				return pons.ToolCallStartOutput{}, nil
			}
			return callHook[pons.ToolCallStartOutput](ctx, p, HookToolCallStart, in)
		}
	}
	if registered[HookPermissionRequest] {
		hooks.OnPermissionRequest = func(ctx context.Context, in pons.PermissionRequestInput) (pons.PermissionRequestOutput, error) {
			if !p.host.HookWantsTool(string(in.Action.Kind)) {
				return pons.PermissionRequestOutput{}, nil
			}
			return callHook[pons.PermissionRequestOutput](ctx, p, HookPermissionRequest, in)
		}
	}
	if registered[HookToolCallEnd] {
		hooks.OnToolCallEnd = func(ctx context.Context, in pons.ToolCallEndInput) (pons.ToolCallEndOutput, error) {
			if !p.host.HookWantsTool(string(in.Action.Kind)) {
				return pons.ToolCallEndOutput{}, nil
			}
			in.Result = boundResult(in.Result)
			return callHook[pons.ToolCallEndOutput](ctx, p, HookToolCallEnd, in)
		}
	}
	if registered[HookAgentTurnEnd] {
		hooks.OnAgentTurnEnd = func(ctx context.Context, in pons.AgentTurnEndInput) (pons.AgentTurnEndOutput, error) {
			results := make([]protocol.ToolResult, len(in.Log.Results))
			for i, result := range in.Log.Results {
				results[i] = boundResult(result)
			}
			in.Log.Results = results
			return callHook[pons.AgentTurnEndOutput](ctx, p, HookAgentTurnEnd, struct {
				pons.AgentTurnEndInput
				Error string `json:"error,omitempty"`
			}{in, errorMessage(in.Err)})
		}
	}
	if registered[HookAgentEnd] {
		hooks.OnAgentEnd = func(ctx context.Context, in pons.AgentEndInput) (pons.AgentEndOutput, error) {
			in.Result.History = nil
			return callHook[pons.AgentEndOutput](ctx, p, HookAgentEnd, struct {
				pons.AgentEndInput
				Error string `json:"error,omitempty"`
			}{in, errorMessage(in.Err)})
		}
	}
	if err := core.AddHooks(hooks); err != nil {
		_ = p.host.Close()
		return err
	}
	return nil
}

func (p *HookPlugin) Close() error { return p.host.Close() }
