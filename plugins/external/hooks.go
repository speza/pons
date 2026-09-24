package external

import (
	"context"
	"errors"
	"fmt"

	"github.com/samperrin/pons"
	"github.com/samperrin/pons/protocol"
)

const (
	HookAgentStart        = "on_agent_start"
	HookAgentEnd          = "on_agent_end"
	HookAgentError        = "on_agent_error"
	HookAgentStepStart    = "on_agent_step_start"
	HookAssistantResponse = "on_assistant_response"
	HookAgentStepEnd      = "on_agent_step_end"
	HookAgentStepError    = "on_agent_step_error"
	HookToolCallStart     = "on_tool_call_start"
	HookToolCallEnd       = "on_tool_call_end"
	HookToolCallError     = "on_tool_call_error"
	HookToolCallDenied    = "on_tool_call_denied"
	HookApprovalRequest   = "on_approval_request"
	HookApprovalResolved  = "on_approval_resolved"
)

func validHookName(name string) bool {
	switch name {
	case HookAgentStart, HookAgentEnd, HookAgentError, HookAgentStepStart,
		HookAssistantResponse, HookAgentStepEnd, HookAgentStepError,
		HookToolCallStart, HookToolCallEnd, HookToolCallError,
		HookToolCallDenied, HookApprovalRequest, HookApprovalResolved:
		return true
	default:
		return false
	}
}

// HookPlugin adapts an explicitly installed host-side hook provider.
// It receives bounded event data but no host credentials or Core handle.
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

// hookWireEvent exposes errors as text; encoding an error interface directly
// would otherwise produce an empty JSON object for most Go errors.
func hookWireEvent(event any) any {
	switch e := event.(type) {
	case *pons.AgentErrorEvent:
		return struct {
			Step         int
			ErrorMessage string
		}{e.Step, errorMessage(e.Err)}
	case *pons.AgentStepErrorEvent:
		return struct {
			Step         int
			ErrorMessage string
		}{e.Step, errorMessage(e.Err)}
	case *pons.AgentEndEvent:
		return struct {
			Result       pons.RunResult
			ErrorMessage string
			Reason       pons.AgentEndReason
			StopReason   string
		}{e.Result, errorMessage(e.Err), e.Reason, e.StopReason}
	case *pons.ToolCallErrorEvent:
		return struct {
			Step         int
			Action       protocol.Action
			Tool         *pons.ToolSpec
			Result       protocol.ToolResult
			ErrorMessage string
		}{e.Step, e.Action, e.Tool, e.Result, errorMessage(e.Err)}
	default:
		return event
	}
}

func errorMessage(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func callPatch[T any](p *HookPlugin, ctx context.Context, name string, event any) (T, error) {
	var patch T
	err := p.host.CallHook(ctx, name, event, &patch)
	return patch, err
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
			patch, err := callPatch[struct{ Message *string }](p, ctx, HookAgentStart, e)
			if err == nil && patch.Message != nil {
				e.Message = *patch.Message
			}
			return err
		}
	}
	if registered[HookAgentEnd] {
		hooks.OnAgentEnd = func(ctx context.Context, e *pons.AgentEndEvent) error {
			patch, err := callPatch[struct{ Result *pons.RunResult }](p, ctx, HookAgentEnd, e)
			if err == nil && patch.Result != nil {
				e.Result = *patch.Result
			}
			return err
		}
	}
	if registered[HookAgentError] {
		hooks.OnAgentError = func(ctx context.Context, e *pons.AgentErrorEvent) error {
			patch, err := callPatch[struct{ ErrorMessage *string }](p, ctx, HookAgentError, e)
			if err == nil && patch.ErrorMessage != nil {
				if *patch.ErrorMessage == "" {
					return errors.New("external: replacement error must be nonempty")
				}
				e.Err = errors.New(*patch.ErrorMessage)
			}
			return err
		}
	}
	if registered[HookAgentStepStart] {
		hooks.OnAgentStepStart = func(ctx context.Context, e *pons.AgentStepStartEvent) error {
			patch, err := callPatch[struct {
				Observation *struct {
					Message *string
					History *[]protocol.StepLog
				}
			}](p, ctx, HookAgentStepStart, e)
			if err == nil && patch.Observation != nil {
				if patch.Observation.Message != nil {
					e.Observation.Message = *patch.Observation.Message
				}
				if patch.Observation.History != nil {
					e.Observation.History = *patch.Observation.History
				}
			}
			return err
		}
	}
	if registered[HookAssistantResponse] {
		hooks.OnAssistantResponse = func(ctx context.Context, e *pons.AssistantResponseEvent) error {
			patch, err := callPatch[struct{ Response *pons.AssistantResponse }](p, ctx, HookAssistantResponse, e)
			if err == nil && patch.Response != nil {
				e.Response = *patch.Response
			}
			return err
		}
	}
	if registered[HookAgentStepEnd] {
		hooks.OnAgentStepEnd = func(ctx context.Context, e *pons.AgentStepEndEvent) error {
			_, err := callPatch[struct{}](p, ctx, HookAgentStepEnd, e)
			return err
		}
	}
	if registered[HookAgentStepError] {
		hooks.OnAgentStepError = func(ctx context.Context, e *pons.AgentStepErrorEvent) error {
			patch, err := callPatch[struct{ ErrorMessage *string }](p, ctx, HookAgentStepError, e)
			if err == nil && patch.ErrorMessage != nil {
				if *patch.ErrorMessage == "" {
					return errors.New("external: replacement error must be nonempty")
				}
				e.Err = errors.New(*patch.ErrorMessage)
			}
			return err
		}
	}
	if registered[HookToolCallStart] {
		hooks.OnToolCallStart = func(ctx context.Context, e *pons.ToolCallStartEvent) error {
			patch, err := callPatch[struct{ Decision *pons.ActionDecision }](p, ctx, HookToolCallStart, e)
			if err == nil && patch.Decision != nil {
				e.Decision = *patch.Decision
			}
			return err
		}
	}
	if registered[HookToolCallEnd] {
		hooks.OnToolCallEnd = func(ctx context.Context, e *pons.ToolCallEndEvent) error {
			patch, err := callPatch[struct{ Result *protocol.ToolResult }](p, ctx, HookToolCallEnd, e)
			if err == nil && patch.Result != nil {
				e.Result = *patch.Result
			}
			return err
		}
	}
	if registered[HookToolCallError] {
		hooks.OnToolCallError = func(ctx context.Context, e *pons.ToolCallErrorEvent) error {
			patch, err := callPatch[struct{ Result *protocol.ToolResult }](p, ctx, HookToolCallError, e)
			if err == nil && patch.Result != nil {
				e.Result = *patch.Result
			}
			return err
		}
	}
	if registered[HookToolCallDenied] {
		hooks.OnToolCallDenied = func(ctx context.Context, e *pons.ToolCallDeniedEvent) error {
			patch, err := callPatch[struct{ Result *protocol.ToolResult }](p, ctx, HookToolCallDenied, e)
			if err == nil && patch.Result != nil {
				e.Result = *patch.Result
			}
			return err
		}
	}
	if registered[HookApprovalRequest] {
		hooks.OnApprovalRequest = func(ctx context.Context, e *pons.ApprovalRequestEvent) error {
			patch, err := callPatch[struct{ Decision *pons.ActionDisposition }](p, ctx, HookApprovalRequest, e)
			if err == nil && patch.Decision != nil {
				e.Decision = patch.Decision
			}
			return err
		}
	}
	if registered[HookApprovalResolved] {
		hooks.OnApprovalResolved = func(ctx context.Context, e *pons.ApprovalResolvedEvent) error {
			patch, err := callPatch[struct{ Decision *pons.ActionDisposition }](p, ctx, HookApprovalResolved, e)
			if err == nil && patch.Decision != nil {
				e.Decision = *patch.Decision
			}
			return err
		}
	}
	if err := core.AddHooks(hooks); err != nil {
		_ = p.host.Close()
		return err
	}
	return nil
}

func (p *HookPlugin) Close() error { return p.host.Close() }
