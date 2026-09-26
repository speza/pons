package pons

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sync"
	"unicode/utf8"

	"github.com/samperrin/pons/protocol"
)

// Permission is a decision about one exact pending tool call.
type Permission string

const (
	PermissionAllow Permission = "allow"
	PermissionAsk   Permission = "ask"
	PermissionDeny  Permission = "deny"
)

// ActionAssessment is advisory classifier evidence. It is never itself a grant.
type ActionAssessment struct {
	Risk                  string  `json:"risk"`
	Confidence            float64 `json:"confidence"`
	ProbabilityConfidence bool    `json:"probability_confidence,omitempty"` // a choice probability, not generated prose
	ReasonCode            string  `json:"reason_code,omitempty"`
	Classifier            string  `json:"classifier,omitempty"`
}

// PermissionDecision is the merged outcome for one call plus audit metadata.
// Reason is a safe machine-readable token; invalid values are replaced.
type PermissionDecision struct {
	Permission Permission        `json:"permission"`
	Reason     string            `json:"reason,omitempty"`
	Assessment *ActionAssessment `json:"assessment,omitempty"`
}

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

// ToolCallStartInput describes one exact pending tool call.
type ToolCallStartInput struct {
	Turn          int                 `json:"turn"`
	Message       string              `json:"message"`
	Workspace     string              `json:"workspace"`
	Platform      string              `json:"platform,omitempty"`
	Environment   ActionEnvironment   `json:"environment"`
	Action        protocol.Action     `json:"action"`
	Tool          *ToolSpec           `json:"tool,omitempty"`
	Resources     []ToolResource      `json:"resources,omitempty"`
	ResourceError bool                `json:"resource_error,omitempty"` // projection failed; hooks can still deny
	RecentContext []ActionContextItem `json:"recent_context,omitempty"`
}

// ActionEnvironment is trusted host-supplied execution context. It is
// descriptive input to policy, not proof that a sandbox enforces containment.
type ActionEnvironment struct {
	Provider string `json:"provider,omitempty"`
	Network  string `json:"network,omitempty"`
}

// ApprovalHandler resolves an Ask with Allow or Deny for this invocation only.
// It runs after OnPermissionRequest hooks leave the request unresolved.
type ApprovalHandler func(context.Context, PermissionRequestInput) (Permission, error)

// SetApprovalHandler installs the application's interactive approval seam.
// Without one, an unresolved Ask is denied. It never sees a hard Deny.
func (c *Core) SetApprovalHandler(fn ApprovalHandler) error {
	if fn == nil {
		return errors.New("pons: cannot install a nil approval handler")
	}
	if c.approver != nil {
		return errors.New("pons: approval handler already set")
	}
	c.approver = fn
	return nil
}

const (
	repeatedDenialLimit = 3
	maxInputUpdates     = 8
)

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

var reasonCodePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

func safeReasonCode(code, fallback string) string {
	if reasonCodePattern.MatchString(code) {
		return code
	}
	return fallback
}

func deniedResult(a protocol.Action, reason string) protocol.ToolResult {
	return protocol.ToolResult{
		ActionID: a.ID,
		Kind:     string(a.Kind),
		OK:       false,
		Error:    fmt.Sprintf("tool call was not allowed (reason: %s)", safeReasonCode(reason, "tool_call_denied")),
	}
}

func denialKey(a protocol.Action) string {
	if len(bytes.TrimSpace(a.Args)) == 0 || bytes.Equal(bytes.TrimSpace(a.Args), []byte("null")) {
		return string(a.Kind) + "\x00{}"
	}
	var args any
	decoder := json.NewDecoder(bytes.NewReader(a.Args))
	decoder.UseNumber()
	if err := decoder.Decode(&args); err == nil {
		if normalized, err := json.Marshal(args); err == nil {
			return string(a.Kind) + "\x00" + string(normalized)
		}
	}
	return string(a.Kind) + "\x00" + string(a.Args)
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

// callDecision is the preflight outcome for one call.
type callDecision struct {
	action   protocol.Action // arguments may have been updated
	decision PermissionDecision
	fx       hookEffects
	errs     []error
}

// preflight decides every pending call of one turn before any of them runs.
// Start hooks for different calls run concurrently; hook errors and messages
// are then reported and asks resolved one at a time in call order. A hook's
// Stop request denies the calls that are still undecided.
func (c *Core) preflight(ctx context.Context, requests []ToolCallStartInput, denials map[string]int, fx *hookEffects) ([]callDecision, error) {
	calls := make([]callDecision, len(requests))
	if !slices.ContainsFunc(c.hooks, func(h Hooks) bool { return h.OnToolCallStart != nil }) {
		for i, req := range requests {
			calls[i] = callDecision{action: req.Action, decision: PermissionDecision{Permission: PermissionAllow}}
		}
		return calls, nil
	}

	var wg sync.WaitGroup
	for i, req := range requests {
		if denials[denialKey(req.Action)] >= repeatedDenialLimit {
			calls[i] = callDecision{action: req.Action, decision: PermissionDecision{Permission: PermissionDeny, Reason: "repeated_denial"}}
			continue
		}
		wg.Go(func() { calls[i] = c.decideToolCall(ctx, req) })
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	for i := range calls {
		call := &calls[i]
		fx.context = append(fx.context, call.fx.context...)
		fx.messages = append(fx.messages, call.fx.messages...)
		if call.fx.stop {
			fx.add(HookOutput{Stop: true, StopReason: call.fx.stopReason})
		}
		if err := c.reportHooks(requests[i].Turn, call.errs, fx); err != nil {
			return nil, err
		}
	}

	for i := range calls {
		call, req := &calls[i], requests[i]
		req.Action = call.action
		if fx.stop && call.decision.Permission != PermissionDeny {
			call.decision = PermissionDecision{Permission: PermissionDeny, Reason: "run_stopped"}
		}
		if call.decision.Permission == PermissionAsk {
			decision, err := c.requestPermission(ctx, PermissionRequestInput{ToolCallStartInput: req, Decision: call.decision}, fx)
			if err != nil {
				return nil, err
			}
			call.decision = decision
		}
		if call.decision.Permission == PermissionDeny {
			denials[denialKey(call.action)]++
		}
		observed := call.decision
		action := call.action
		if err := c.emit(Event{Type: EventActionDecision, Turn: req.Turn, Action: &action,
			Tool: req.Tool, Decision: &observed}); err != nil {
			return nil, fmt.Errorf("event action_decision: %w", err)
		}
	}
	return calls, nil
}

// decideToolCall runs every start hook on one call and merges their answers.
// When a hook updates the arguments, every hook runs again on the new call.
func (c *Core) decideToolCall(ctx context.Context, req ToolCallStartInput) callDecision {
	for range maxInputUpdates {
		call := callDecision{action: req.Action, decision: PermissionDecision{Permission: PermissionAllow}}
		req.Resources, req.ResourceError = nil, false
		if project := c.resources[req.Action.Kind]; project != nil {
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
				call.decision = PermissionDecision{Permission: PermissionDeny, Reason: "invalid_tool_call_update"}
				return call
			}
			req.Action.Args = slices.Clone(trimmed)
			continue
		}
		if req.ResourceError && call.decision.Permission != PermissionDeny {
			call.decision = PermissionDecision{Permission: PermissionAsk, Reason: "resource_projection_failed"}
		}
		call.decision = sanitizeDecision(call.decision)
		return call
	}
	return callDecision{action: req.Action, decision: PermissionDecision{Permission: PermissionDeny, Reason: "tool_call_update_limit"}}
}

// hookDecision normalizes one start hook's answer. An empty permission means
// the hook has no objection; an unknown one asks.
func hookDecision(out ToolCallStartOutput) PermissionDecision {
	switch out.Permission {
	case "":
		return PermissionDecision{Permission: PermissionAllow}
	case PermissionAllow, PermissionAsk, PermissionDeny:
		return PermissionDecision{Permission: out.Permission, Reason: out.Reason, Assessment: out.Assessment}
	default:
		return PermissionDecision{Permission: PermissionAsk, Reason: "tool_call_start_uncertain"}
	}
}

// mergeDecision keeps the stricter of two decisions: Deny over Ask over
// Allow. Between equally strict decisions the first explained one wins.
func mergeDecision(current, candidate PermissionDecision) PermissionDecision {
	rank := map[Permission]int{PermissionAllow: 0, PermissionAsk: 1, PermissionDeny: 2}
	switch {
	case rank[candidate.Permission] > rank[current.Permission]:
		return candidate
	case rank[candidate.Permission] == rank[current.Permission] && current.Reason == "" && current.Assessment == nil:
		return candidate
	default:
		return current
	}
}

func sanitizeDecision(decision PermissionDecision) PermissionDecision {
	if decision.Permission == PermissionDeny {
		decision.Reason = safeReasonCode(decision.Reason, "tool_call_denied")
	} else if decision.Reason != "" {
		decision.Reason = safeReasonCode(decision.Reason, "reason_unavailable")
	}
	if decision.Assessment != nil && decision.Assessment.ReasonCode != "" {
		assessment := *decision.Assessment
		assessment.ReasonCode = safeReasonCode(assessment.ReasonCode, "reason_unavailable")
		decision.Assessment = &assessment
	}
	return decision
}

// requestPermission resolves an Ask through OnPermissionRequest hooks and
// then the approval handler. Any Deny or hook failure denies; the call runs
// only on an explicit Allow.
func (c *Core) requestPermission(ctx context.Context, in PermissionRequestInput, fx *hookEffects) (PermissionDecision, error) {
	decision := in.Decision
	var allowed, denied bool
	errs := runHooks(ctx, c.hooks, "permission request",
		func(h Hooks) func(context.Context, PermissionRequestInput) (PermissionRequestOutput, error) {
			return h.OnPermissionRequest
		},
		in, fx,
		func(out PermissionRequestOutput) {
			switch out.Permission {
			case "":
			case PermissionAllow:
				allowed = true
			default:
				denied = true
			}
		},
	)
	if err := ctx.Err(); err != nil {
		return decision, err
	}
	if err := c.reportHooks(in.Turn, errs, fx); err != nil {
		return decision, err
	}

	switch {
	case len(errs) > 0:
		decision.Permission, decision.Reason = PermissionDeny, "permission_hook_failed"
	case denied:
		decision.Permission, decision.Reason = PermissionDeny, "approval_denied"
	case allowed:
		decision.Permission = PermissionAllow
	case c.approver == nil:
		decision.Permission, decision.Reason = PermissionDeny, "approval_required"
	default:
		answer, err := c.approver(ctx, in)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return decision, ctxErr
		}
		switch {
		case err != nil:
			decision.Permission, decision.Reason = PermissionDeny, "approval_unavailable"
		case answer == PermissionAllow:
			decision.Permission = PermissionAllow
		case answer == PermissionDeny:
			decision.Permission, decision.Reason = PermissionDeny, "approval_denied"
		default:
			decision.Permission, decision.Reason = PermissionDeny, "invalid_approval"
		}
	}
	return decision, nil
}
