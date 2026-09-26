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
	HookToolCallEnd       = "on_tool_call_end"
	HookAgentTurnEnd      = "on_agent_turn_end"
	HookAgentEnd          = "on_agent_end"
)

func validHookName(name string) bool {
	switch name {
	case HookAgentStart, HookAgentTurnStart, HookAssistantResponse,
		HookToolCallStart, HookToolCallEnd, HookAgentTurnEnd, HookAgentEnd:
		return true
	default:
		return false
	}
}

// maxHookText bounds each tool output or error string sent to a hook, so
// event size does not grow with tool output.
const maxHookText = 16 << 10

// HookPlugin adapts an explicitly installed host-side hook provider. The
// executable is an untrusted boundary: it receives bounded event summaries
// but no host credentials or Core handle, every hook except on_tool_call_start
// is an observer, and its tool-call decisions can only add Ask or Deny.
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

// Wire events are explicit summaries rather than core structs, so history
// and tool output never make a hook frame grow with the run.
type (
	hookTurnStart struct {
		Turn    int
		Message string
	}
	hookToolCallEnd struct {
		Turn     int
		Action   protocol.Action
		Tool     *pons.ToolSpec
		Result   protocol.ToolResult
		Decision *pons.ActionDecision
	}
	hookToolOutcome struct {
		ActionID string
		Kind     string
		OK       bool
		Error    string
	}
	hookTurnEnd struct {
		Turn         int
		Actions      []protocol.Action
		Results      []hookToolOutcome
		ErrorMessage string
	}
	hookAgentEnd struct {
		Answer       string
		Turns        int
		Exhausted    bool
		Reason       pons.AgentEndReason
		StopReason   string
		ErrorMessage string
	}
)

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

func (p *HookPlugin) observe(ctx context.Context, name string, event any) error {
	var patch struct{}
	return p.host.CallHook(ctx, name, event, &patch)
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
		hooks.OnAgentStart = func(ctx context.Context, e *pons.AgentStartEvent) error {
			return p.observe(ctx, HookAgentStart, e)
		}
	}
	if registered[HookAgentTurnStart] {
		hooks.OnAgentTurnStart = func(ctx context.Context, e *pons.AgentTurnStartEvent) error {
			return p.observe(ctx, HookAgentTurnStart, hookTurnStart{Turn: e.Observation.Turn, Message: e.Observation.Message})
		}
	}
	if registered[HookAssistantResponse] {
		hooks.OnAssistantResponse = func(ctx context.Context, e *pons.AssistantResponseEvent) error {
			return p.observe(ctx, HookAssistantResponse, e)
		}
	}
	if registered[HookToolCallStart] {
		hooks.OnToolCallStart = func(ctx context.Context, e *pons.ToolCallStartEvent) error {
			var patch struct{ Decision *pons.ActionDecision }
			if err := p.host.CallHook(ctx, HookToolCallStart, e, &patch); err != nil {
				return err
			}
			if patch.Decision != nil {
				// Assessments are classifier evidence owned by trusted plugins.
				e.Decision = pons.ActionDecision{Action: patch.Decision.Action, ReasonCode: patch.Decision.ReasonCode}
			}
			return nil
		}
	}
	if registered[HookToolCallEnd] {
		hooks.OnToolCallEnd = func(ctx context.Context, e *pons.ToolCallEndEvent) error {
			return p.observe(ctx, HookToolCallEnd, hookToolCallEnd{
				Turn: e.Turn, Action: e.Action, Tool: e.Tool,
				Result: boundResult(e.Result), Decision: e.Decision,
			})
		}
	}
	if registered[HookAgentTurnEnd] {
		hooks.OnAgentTurnEnd = func(ctx context.Context, e *pons.AgentTurnEndEvent) error {
			event := hookTurnEnd{Turn: e.Turn, Actions: e.Log.Actions, ErrorMessage: errorMessage(e.Err)}
			for _, result := range e.Log.Results {
				event.Results = append(event.Results, hookToolOutcome{
					ActionID: result.ActionID, Kind: result.Kind, OK: result.OK, Error: boundText(result.Error),
				})
			}
			return p.observe(ctx, HookAgentTurnEnd, event)
		}
	}
	if registered[HookAgentEnd] {
		hooks.OnAgentEnd = func(ctx context.Context, e *pons.AgentEndEvent) error {
			return p.observe(ctx, HookAgentEnd, hookAgentEnd{
				Answer: e.Result.Answer, Turns: e.Result.Turns, Exhausted: e.Result.Exhausted,
				Reason: e.Reason, StopReason: e.StopReason, ErrorMessage: errorMessage(e.Err),
			})
		}
	}
	if err := core.AddHooks(hooks); err != nil {
		_ = p.host.Close()
		return err
	}
	return nil
}

func (p *HookPlugin) Close() error { return p.host.Close() }
