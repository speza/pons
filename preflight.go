package pons

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/samperrin/pons/protocol"
)

// ToolResource is a tool-owned projection of an argument relevant to preflight checks.
type ToolResource struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

// ActionContextSource identifies the origin of recent context. Only
// user messages can express user intent; tool output remains untrusted data.
type ActionContextSource string

const (
	ContextUser       ActionContextSource = "user"
	ContextAssistant  ActionContextSource = "assistant"
	ContextAction     ActionContextSource = "action"
	ContextToolResult ActionContextSource = "tool_result"
)

// ActionContextItem is one bounded recent message or action. Text is
// the original content, truncated only to keep classifier input bounded.
type ActionContextItem struct {
	Source   ActionContextSource `json:"source"`
	ActionID string              `json:"action_id,omitempty"`
	Kind     protocol.ActionKind `json:"kind,omitempty"`
	Text     string              `json:"text"`
}

// ResourceProjection extracts hook-facing resources from typed tool arguments.
// Errors fail closed through the approval path.
type ResourceProjection func(protocol.Action) ([]ToolResource, error)

// StringArgResource projects one string argument without interpreting its
// contents. The tool chooses the resource kind and argument name.
func StringArgResource(kind, argument string) ResourceProjection {
	return func(action protocol.Action) ([]ToolResource, error) {
		value, err := protocol.StringArg(action.Args, argument)
		if err != nil {
			return nil, err
		}
		return []ToolResource{{Kind: kind, Value: value}}, nil
	}
}

// maxInputUpdates bounds how often start hooks may rewrite one call.
const maxInputUpdates = 8

const (
	maxActionContextItems = 24
	maxActionContextText  = 2048
)

func boundedActionContext(items []ActionContextItem) []ActionContextItem {
	if len(items) > maxActionContextItems {
		items = items[len(items)-maxActionContextItems:]
	}
	out := make([]ActionContextItem, len(items))
	copy(out, items)
	for i := range out {
		if len(out[i].Text) > maxActionContextText {
			out[i].Text = out[i].Text[:maxActionContextText]
			for !utf8.ValidString(out[i].Text) {
				out[i].Text = out[i].Text[:len(out[i].Text)-1]
			}
		}
	}
	return out
}

// cloneStartInput gives each hook its own slices, so one hook cannot change
// what another sees.
func cloneStartInput(in ToolCallStartInput) ToolCallStartInput {
	in.Action.Args = slices.Clone(in.Action.Args)
	in.Resources = slices.Clone(in.Resources)
	in.RecentContext = slices.Clone(in.RecentContext)
	if in.Tool != nil {
		tool := *in.Tool
		tool.Params = slices.Clone(tool.Params)
		tool.InputSchema = slices.Clone(tool.InputSchema)
		in.Tool = &tool
	}
	return in
}

// callDecision is the preflight outcome for one call. Its input is the call
// as it will run: updated arguments and their projected resources.
type callDecision struct {
	input    ToolCallStartInput
	decision PermissionDecision
	fx       hookEffects
	errs     []error
}

// ToolCallCheck is the start-hook outcome for one call.
type ToolCallCheck struct {
	Action     protocol.Action // with any UpdatedInput applied
	Decision   PermissionDecision
	Output     HookOutput // first stop request; context and messages joined by newlines
	HookErrors []error
}

// CheckToolCall runs the OnToolCallStart hooks for one call and merges their
// decisions exactly as Run does before execution. It does not resolve an Ask,
// execute the call, count repeated denials, or emit events. Evals and dry
// runs use it; Resources in the input are used when the tool registers no
// projection.
func (c *Core) CheckToolCall(ctx context.Context, in ToolCallStartInput) (ToolCallCheck, error) {
	call := c.decideToolCall(ctx, in)
	if err := ctx.Err(); err != nil {
		return ToolCallCheck{}, err
	}
	return ToolCallCheck{
		Action:   call.input.Action,
		Decision: call.decision,
		Output: HookOutput{
			Stop:              call.fx.stop,
			StopReason:        call.fx.stopReason,
			SystemMessage:     strings.Join(call.fx.messages, "\n"),
			AdditionalContext: strings.Join(call.fx.context, "\n"),
		},
		HookErrors: call.errs,
	}, nil
}

// decideCalls runs the start hooks for every pending call of one turn,
// concurrently across calls, and reports their errors and messages in call
// order. It returns checked=false, and allows every call, when no start hook
// is registered.
func (c *Core) decideCalls(ctx context.Context, requests []ToolCallStartInput, denials map[string]int, fx *hookEffects) (calls []callDecision, checked bool, err error) {
	calls = make([]callDecision, len(requests))
	if !slices.ContainsFunc(c.hooks, func(h Hooks) bool { return h.OnToolCallStart != nil }) {
		for i, req := range requests {
			calls[i] = callDecision{input: req, decision: PermissionDecision{Permission: PermissionAllow}}
		}
		return calls, false, nil
	}

	var wg sync.WaitGroup
	for i, req := range requests {
		if denials[denialKey(req.Action)] >= repeatedDenialLimit {
			calls[i] = callDecision{input: req, decision: PermissionDecision{Permission: PermissionDeny, Reason: "repeated_denial"}}
			continue
		}
		wg.Go(func() { calls[i] = c.decideToolCall(ctx, req) })
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, true, err
	}

	for i := range calls {
		call := &calls[i]
		fx.context = append(fx.context, call.fx.context...)
		fx.messages = append(fx.messages, call.fx.messages...)
		if call.fx.stop {
			fx.add(HookOutput{Stop: true, StopReason: call.fx.stopReason})
		}
		if err := c.reportHooks(requests[i].Turn, call.errs, fx); err != nil {
			return nil, true, err
		}
	}
	return calls, true, nil
}

// resolveCalls settles decided calls in call order: an Ask goes to the
// permission hooks and then the approval handler. A Stop requested by any
// start or permission hook denies every call of the turn that would
// otherwise run. Denials count toward the repeated-denial limit under the
// call the brain proposed, so a rewriting hook cannot evade it.
func (c *Core) resolveCalls(ctx context.Context, requests []ToolCallStartInput, calls []callDecision, denials map[string]int, fx *hookEffects) error {
	for i := range calls {
		call := &calls[i]
		if fx.stop {
			break
		}
		if call.decision.Permission == PermissionAsk {
			decision, err := c.requestPermission(ctx, PermissionRequestInput{ToolCallStartInput: call.input, Decision: call.decision}, fx)
			if err != nil {
				return err
			}
			call.decision = decision
		}
	}

	for i := range calls {
		call := &calls[i]
		if fx.stop && call.decision.Permission != PermissionDeny {
			call.decision = PermissionDecision{Permission: PermissionDeny, Reason: "run_stopped"}
		}
		if call.decision.Permission == PermissionDeny {
			denials[denialKey(requests[i].Action)]++
		}
		observed, action := call.decision, call.input.Action
		if err := c.emit(Event{Type: EventActionDecision, Turn: call.input.Turn, Action: &action,
			Tool: call.input.Tool, Decision: &observed}); err != nil {
			return fmt.Errorf("event action_decision: %w", err)
		}
	}
	return nil
}

// decideToolCall runs every start hook on one call and merges their answers.
// When a hook updates the arguments, every hook runs again on the new call.
func (c *Core) decideToolCall(ctx context.Context, req ToolCallStartInput) callDecision {
	for range maxInputUpdates {
		call := callDecision{decision: PermissionDecision{Permission: PermissionAllow}}
		// A registered projection owns the resources; otherwise any the
		// caller supplied (as CheckToolCall callers may) are kept.
		if project := c.resources[req.Action.Kind]; project != nil {
			req.Resources, req.ResourceError = nil, false
			projected := req.Action
			projected.Args = slices.Clone(req.Action.Args)
			if resources, err := project(projected); err != nil {
				req.ResourceError = true
			} else {
				req.Resources = resources
			}
		}

		var updated json.RawMessage
		for i, registered := range c.hooks {
			if registered.OnToolCallStart == nil {
				continue
			}
			out, err := registered.OnToolCallStart(ctx, cloneStartInput(req))
			if err != nil {
				// A failed hook's output is discarded, except that its Deny holds.
				call.errs = append(call.errs, fmt.Errorf("tool call start hook %d: %w", i+1, err))
				failed := PermissionDecision{Permission: PermissionAsk, Reason: "tool_call_start_unavailable"}
				if out.Permission == PermissionDeny {
					failed = PermissionDecision{Permission: PermissionDeny, Reason: out.Reason}
				}
				call.decision = mergeDecision(call.decision, failed)
				continue
			}
			call.fx.add(out.HookOutput)
			call.decision = mergeDecision(call.decision, hookDecision(out))
			if len(out.UpdatedInput) > 0 && out.Permission != PermissionDeny && !bytes.Equal(out.UpdatedInput, req.Action.Args) {
				updated = out.UpdatedInput
				break
			}
		}

		if updated != nil && call.decision.Permission != PermissionDeny {
			trimmed := bytes.TrimSpace(updated)
			if _, err := protocol.ObjectArgs(trimmed); err != nil || len(trimmed) == 0 || trimmed[0] != '{' {
				call.input = req
				call.decision = PermissionDecision{Permission: PermissionDeny, Reason: "invalid_tool_call_update"}
				return call
			}
			req.Action.Args = slices.Clone(trimmed)
			continue
		}
		if req.ResourceError && call.decision.Permission != PermissionDeny {
			call.decision = PermissionDecision{Permission: PermissionAsk, Reason: "resource_projection_failed"}
		}
		call.input = req
		call.decision = sanitizeDecision(call.decision)
		return call
	}
	return callDecision{input: req, decision: PermissionDecision{Permission: PermissionDeny, Reason: "tool_call_update_limit"}}
}
