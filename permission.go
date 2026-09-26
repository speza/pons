package pons

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

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

// repeatedDenialLimit circuit-breaks a brain that keeps proposing a call
// that was denied, rather than asking about it again.
const repeatedDenialLimit = 3

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
