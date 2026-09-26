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

// ActionDisposition is the outcome requested by a host-side policy.
type ActionDisposition string

const (
	DispositionAllow ActionDisposition = "allow"
	DispositionAsk   ActionDisposition = "ask"
	DispositionDeny  ActionDisposition = "deny"
)

// ActionAssessment is advisory classifier evidence. It is never itself a grant.
type ActionAssessment struct {
	Risk                  string
	Confidence            float64
	ProbabilityConfidence bool // supplied as a choice probability, not generated prose
	ReasonCode            string
	Classifier            string
}

// ToolResource is a tool-owned projection of an argument relevant to preflight checks.
type ToolResource struct {
	Kind  string
	Value string
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
	Source   ActionContextSource
	ActionID string
	Kind     protocol.ActionKind
	Text     string
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

// ToolCallStartEvent describes one exact pending tool call.
type ToolCallStartEvent struct {
	Turn          int
	Message       string
	Workspace     string
	Platform      string
	Environment   ActionEnvironment
	Action        protocol.Action
	Tool          *ToolSpec
	Resources     []ToolResource
	ResourceError bool // projection failed; policy can still apply hard denies
	RecentContext []ActionContextItem
	Decision      ActionDecision // set to allow, ask, or deny
}

// ActionEnvironment is trusted host-supplied execution context. It is
// descriptive input to policy, not proof that a sandbox enforces containment.
type ActionEnvironment struct {
	Provider string
	Network  string
}

// ActionDecision is the policy's requested action plus audit metadata.
// ReasonCode must be a safe machine-readable token; invalid values are replaced.
type ActionDecision struct {
	Action     ActionDisposition
	Assessment ActionAssessment
	ReasonCode string
}

// ApprovalRequest is passed to the application for an exact pending action.
type ApprovalRequest struct {
	ToolCallStartEvent
	Assessment ActionAssessment
}

// ApprovalHandler resolves an Ask with Allow or Deny for this invocation only.
type ApprovalHandler func(context.Context, ApprovalRequest) (ActionDisposition, error)

// SetApprovalHandler installs the application's interactive approval seam.
// Without one, Ask is denied. A handler cannot override a hard Deny.
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

const repeatedDenialLimit = 3

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

func cloneToolCallStartEvent(req ToolCallStartEvent) ToolCallStartEvent {
	req.Action.Args = slices.Clone(req.Action.Args)
	req.Resources = slices.Clone(req.Resources)
	req.RecentContext = slices.Clone(req.RecentContext)
	if req.Tool != nil {
		tool := *req.Tool
		tool.Params = slices.Clone(tool.Params)
		tool.InputSchema = slices.Clone(tool.InputSchema)
		req.Tool = &tool
	}
	return req
}

func (c *Core) hasToolCallStartHooks() bool {
	return slices.ContainsFunc(c.hooks, func(h Hooks) bool { return h.OnToolCallStart != nil })
}

// preflight decides every pending call of one turn before any of them runs.
// Start hooks for different calls run concurrently; approvals are then
// resolved one at a time in call order. Without start hooks every call is
// allowed and preflight returns nil.
func (c *Core) preflight(ctx context.Context, requests []ToolCallStartEvent, denials map[string]int) ([]ActionDecision, error) {
	if !c.hasToolCallStartHooks() {
		return nil, nil
	}

	decisions := make([]ActionDecision, len(requests))
	var wg sync.WaitGroup
	for i, req := range requests {
		if denials[denialKey(req.Action)] >= repeatedDenialLimit {
			decisions[i] = ActionDecision{Action: DispositionDeny, ReasonCode: "repeated_denial"}
			continue
		}
		wg.Go(func() { decisions[i] = c.decideToolCall(ctx, req) })
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	for i, req := range requests {
		if decisions[i].Action == DispositionAsk {
			decisions[i] = c.resolveApproval(ctx, req, decisions[i])
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if decisions[i].Action == DispositionDeny {
			denials[denialKey(req.Action)]++
		}
		observed := decisions[i]
		if err := c.emit(Event{Type: EventActionDecision, Turn: req.Turn, Action: &req.Action,
			Tool: req.Tool, Decision: &observed}); err != nil {
			return nil, fmt.Errorf("event action_decision: %w", err)
		}
	}
	return decisions, nil
}

// decideToolCall runs every start hook on one call and merges their answers.
// Hook failures and unknown answers fail closed to Ask; a Deny always holds.
func (c *Core) decideToolCall(ctx context.Context, req ToolCallStartEvent) ActionDecision {
	if project := c.resources[req.Action.Kind]; project != nil {
		projected := req.Action
		projected.Args = slices.Clone(req.Action.Args)
		if resources, err := project(projected); err != nil {
			req.ResourceError = true
		} else {
			req.Resources = resources
		}
	}

	decision := ActionDecision{Action: DispositionAllow}
	for _, registered := range c.hooks {
		if registered.OnToolCallStart == nil {
			continue
		}
		event := cloneToolCallStartEvent(req)
		err := registered.OnToolCallStart(ctx, &event)
		decision = mergeDecision(decision, hookDecision(event.Decision, err))
	}
	if req.ResourceError && decision.Action != DispositionDeny {
		decision = ActionDecision{Action: DispositionAsk, ReasonCode: "resource_projection_failed"}
	}

	if decision.Action == DispositionDeny {
		decision.ReasonCode = safeReasonCode(decision.ReasonCode, "tool_call_denied")
	} else if decision.ReasonCode != "" {
		decision.ReasonCode = safeReasonCode(decision.ReasonCode, "reason_unavailable")
	}
	if decision.Assessment.ReasonCode != "" {
		decision.Assessment.ReasonCode = safeReasonCode(decision.Assessment.ReasonCode, "reason_unavailable")
	}
	return decision
}

// hookDecision normalizes one start hook's answer. An empty action means the
// hook has no objection.
func hookDecision(decision ActionDecision, err error) ActionDecision {
	switch decision.Action {
	case "":
		decision.Action = DispositionAllow
	case DispositionAllow, DispositionAsk, DispositionDeny:
	default:
		return ActionDecision{Action: DispositionAsk, ReasonCode: "tool_call_start_uncertain"}
	}
	if err != nil && decision.Action != DispositionDeny {
		return ActionDecision{Action: DispositionAsk, ReasonCode: "tool_call_start_unavailable"}
	}
	return decision
}

// mergeDecision keeps the stricter of two decisions: Deny over Ask over
// Allow. Between equally strict decisions the first explained one wins.
func mergeDecision(current, candidate ActionDecision) ActionDecision {
	rank := map[ActionDisposition]int{DispositionAllow: 0, DispositionAsk: 1, DispositionDeny: 2}
	switch {
	case rank[candidate.Action] > rank[current.Action]:
		return candidate
	case rank[candidate.Action] == rank[current.Action] && current.ReasonCode == "":
		return candidate
	default:
		return current
	}
}

// resolveApproval asks the application to allow or deny one exact call.
// Anything but an explicit Allow denies it.
func (c *Core) resolveApproval(ctx context.Context, req ToolCallStartEvent, decision ActionDecision) ActionDecision {
	decision.Action = DispositionDeny
	if c.approver == nil {
		decision.ReasonCode = "approval_required"
		return decision
	}

	answer, err := c.approver(ctx, ApprovalRequest{ToolCallStartEvent: cloneToolCallStartEvent(req), Assessment: decision.Assessment})
	switch {
	case err != nil:
		decision.ReasonCode = "approval_unavailable"
	case answer == DispositionAllow:
		decision.Action = DispositionAllow
	case answer == DispositionDeny:
		decision.ReasonCode = "approval_denied"
	default:
		decision.ReasonCode = "invalid_approval"
	}
	return decision
}
