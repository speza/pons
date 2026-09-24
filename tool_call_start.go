package pons

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
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
	Decision      ActionDecision // set to allow, ask, deny, or replace arguments
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
	// UpdatedArgs replaces the proposed tool arguments before execution. The
	// core reruns every start hook on the changed call before it can proceed.
	UpdatedArgs json.RawMessage
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

func (c *Core) evaluateToolCallStart(ctx context.Context, req ToolCallStartEvent, denials map[string]int) (*ActionDecision, protocol.Action, error) {
	hasStartHook := false
	for _, registered := range c.hooks {
		if registered.OnToolCallStart != nil {
			hasStartHook = true
			break
		}
	}
	if !hasStartHook {
		return nil, req.Action, nil
	}
	if err := c.emit(Event{Type: EventActionPreflight, Turn: req.Turn, Action: &req.Action, Tool: req.Tool}); err != nil {
		return nil, req.Action, fmt.Errorf("event action_preflight: %w", err)
	}
	decision := ActionDecision{Action: DispositionAllow}
	for pass := range 8 {
		key := denialKey(req.Action)
		if denials[key] >= repeatedDenialLimit {
			decision = ActionDecision{Action: DispositionDeny, ReasonCode: "repeated_denial"}
			break
		}
		decision = ActionDecision{Action: DispositionAllow}
		req.Resources, req.ResourceError = nil, false
		if project := c.resources[req.Action.Kind]; project != nil {
			projectedAction := req.Action
			projectedAction.Args = slices.Clone(req.Action.Args)
			resources, err := project(projectedAction)
			if err != nil {
				req.ResourceError = true
			} else {
				req.Resources = resources
			}
		}
		changed := false
		for _, registered := range c.hooks {
			if registered.OnToolCallStart == nil {
				continue
			}
			event := cloneToolCallStartEvent(req)
			err := registered.OnToolCallStart(ctx, &event)
			candidate := event.Decision
			if err != nil {
				candidate = ActionDecision{Action: DispositionAsk, ReasonCode: "tool_call_start_unavailable"}
			} else if candidate.Action == "" {
				candidate.Action = DispositionAllow
			} else if candidate.Action != DispositionAllow && candidate.Action != DispositionAsk && candidate.Action != DispositionDeny {
				candidate = ActionDecision{Action: DispositionAsk, ReasonCode: "tool_call_start_uncertain"}
			}
			if err := ctx.Err(); err != nil {
				return nil, req.Action, err
			}
			if len(candidate.UpdatedArgs) > 0 && candidate.Action != DispositionDeny && decision.Action != DispositionDeny {
				if !json.Valid(candidate.UpdatedArgs) {
					candidate = ActionDecision{Action: DispositionDeny, ReasonCode: "invalid_tool_call_update"}
				} else if !bytes.Equal(req.Action.Args, candidate.UpdatedArgs) {
					req.Action.Args = slices.Clone(candidate.UpdatedArgs)
					changed = true
					break
				}
			}
			candidate.UpdatedArgs = nil
			if candidate.Action == DispositionDeny ||
				(candidate.Action == DispositionAsk && decision.Action == DispositionAllow) ||
				(candidate.Action == DispositionAllow && decision.Action == DispositionAllow && decision.ReasonCode == "") {
				decision = candidate
			}
		}
		if changed {
			if pass == 7 {
				decision = ActionDecision{Action: DispositionDeny, ReasonCode: "tool_call_update_limit"}
				break
			}
			continue
		}
		if req.ResourceError && decision.Action != DispositionDeny {
			decision = ActionDecision{Action: DispositionAsk, ReasonCode: "resource_projection_failed"}
		}
		break
	}
	if err := ctx.Err(); err != nil {
		return nil, req.Action, err
	}
	if decision.ReasonCode != "" {
		fallback := "reason_unavailable"
		if decision.Action == DispositionDeny {
			fallback = "tool_call_denied"
		}
		decision.ReasonCode = safeReasonCode(decision.ReasonCode, fallback)
	} else if decision.Action == DispositionDeny {
		decision.ReasonCode = "tool_call_denied"
	}
	if decision.Assessment.ReasonCode != "" {
		decision.Assessment.ReasonCode = safeReasonCode(decision.Assessment.ReasonCode, "reason_unavailable")
	}
	observed := decision
	if err := c.emit(Event{Type: EventActionDecision, Turn: req.Turn, Action: &req.Action,
		Tool: req.Tool, Decision: &observed}); err != nil {
		return nil, req.Action, fmt.Errorf("event action_decision: %w", err)
	}
	if decision.Action == DispositionAsk {
		pending := decision
		if err := c.emit(Event{Type: EventApprovalRequest, Turn: req.Turn, Action: &req.Action,
			Tool: req.Tool, Decision: &pending}); err != nil {
			return nil, req.Action, fmt.Errorf("event approval_request: %w", err)
		}
		approval := ApprovalRequest{ToolCallStartEvent: cloneToolCallStartEvent(req), Assessment: decision.Assessment}
		hookDecision, err := c.resolveApprovalHooks(ctx, approval)
		if err != nil {
			return nil, req.Action, err
		}
		decision.Action = DispositionDeny
		if hookDecision != nil {
			decision.Action = *hookDecision
			if decision.Action == DispositionDeny {
				decision.ReasonCode = "approval_denied"
			}
		} else if c.approver != nil {
			answer, err := c.approver(ctx, approval)
			if err == nil && answer == DispositionAllow {
				decision.Action = DispositionAllow
			} else if err != nil {
				decision.ReasonCode = "approval_unavailable"
			} else if answer != DispositionDeny {
				decision.ReasonCode = "invalid_approval"
			} else if decision.ReasonCode == "" {
				decision.ReasonCode = "approval_denied"
			}
		} else {
			decision.ReasonCode = "approval_required"
		}
		if err := c.resolveApprovalResolvedHooks(ctx, approval, &decision); err != nil {
			return nil, req.Action, err
		}
		if err := c.emit(Event{Type: EventApprovalResolved, Turn: req.Turn, Action: &req.Action,
			Tool: req.Tool, Decision: &decision}); err != nil {
			return nil, req.Action, fmt.Errorf("event approval_resolved: %w", err)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, req.Action, err
	}
	if decision.Action == DispositionDeny {
		denials[denialKey(req.Action)]++
	}
	return &decision, req.Action, nil
}

func (c *Core) resolveApprovalHooks(ctx context.Context, request ApprovalRequest) (*ActionDisposition, error) {
	var errs []error
	var allowed, denied bool
	for i, registered := range c.hooks {
		if registered.OnApprovalRequest == nil {
			continue
		}
		event := ApprovalRequestEvent{Request: request}
		if err := registered.OnApprovalRequest(ctx, &event); err != nil {
			errs = append(errs, fmt.Errorf("approval request hook %d: %w", i+1, err))
		}
		if event.Decision == nil {
			continue
		}
		if *event.Decision != DispositionAllow && *event.Decision != DispositionDeny {
			errs = append(errs, fmt.Errorf("approval request hook %d: invalid decision", i+1))
			continue
		}
		if *event.Decision == DispositionDeny {
			denied = true
		} else {
			allowed = true
		}
	}
	if err := ctx.Err(); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	if denied {
		deny := DispositionDeny
		return &deny, nil
	}
	if allowed {
		allow := DispositionAllow
		return &allow, nil
	}
	return nil, nil
}

func (c *Core) resolveApprovalResolvedHooks(ctx context.Context, request ApprovalRequest, decision *ActionDecision) error {
	original := decision.Action
	denied := original == DispositionDeny
	var errs []error
	for i, registered := range c.hooks {
		if registered.OnApprovalResolved == nil {
			continue
		}
		event := ApprovalResolvedEvent{Request: request, Decision: decision.Action}
		if err := registered.OnApprovalResolved(ctx, &event); err != nil {
			errs = append(errs, fmt.Errorf("approval resolved hook %d: %w", i+1, err))
		}
		if event.Decision != DispositionAllow && event.Decision != DispositionDeny {
			errs = append(errs, fmt.Errorf("approval resolved hook %d: invalid decision", i+1))
			continue
		}
		if event.Decision == DispositionAllow && decision.Action == DispositionDeny {
			errs = append(errs, fmt.Errorf("approval resolved hook %d: cannot override a denial", i+1))
			continue
		}
		denied = denied || event.Decision == DispositionDeny
	}
	if denied {
		decision.Action = DispositionDeny
		if original == DispositionAllow {
			decision.ReasonCode = "approval_denied"
		}
	}
	if err := ctx.Err(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
