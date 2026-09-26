// Package actionpolicy composes trusted host-side rules and optional
// classifiers into the core's tool-call-start hook.
package actionpolicy

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"

	"github.com/samperrin/pons"
)

// Rule matches an exact pending request. Rules must come from trusted host
// configuration, never from model output or repository content.
type Rule struct {
	Action     pons.ActionDisposition
	ReasonCode string
	Match      func(pons.ToolCallStartEvent) bool
}

// Classifier provides advisory evidence. Its implementation may be
// deterministic or model-backed; it cannot make a hard denial.
type Classifier interface {
	Assess(context.Context, pons.ToolCallStartEvent) (pons.ActionAssessment, error)
}

// ClassifierFunc adapts a function to Classifier.
type ClassifierFunc func(context.Context, pons.ToolCallStartEvent) (pons.ActionAssessment, error)

func (f ClassifierFunc) Assess(ctx context.Context, req pons.ToolCallStartEvent) (pons.ActionAssessment, error) {
	return f(ctx, req)
}

// ToolRule matches calls to the named tool kinds; "*" matches every tool.
func ToolRule(action pons.ActionDisposition, reasonCode string, kinds ...string) Rule {
	return Rule{Action: action, ReasonCode: reasonCode, Match: func(req pons.ToolCallStartEvent) bool {
		return slices.ContainsFunc(kinds, func(kind string) bool {
			return kind == "*" || kind == string(req.Action.Kind)
		})
	}}
}

// Policy applies Deny, Ask, then Allow rules, independent of declaration
// order. Unmatched requests pass through classifiers in order. A valid "safe"
// assessment at or above MinSafeConfidence is allowed when its confidence is a
// returned probability, or when AllowGeneratedConfidence accepts a
// model-generated estimate; everything else, including a request no rule or
// classifier settles, requires approval.
type Policy struct {
	Rules                    []Rule
	Classifiers              []Classifier
	ClassifierID             string  // resolves a classifier capability registered by another plugin
	MinSafeConfidence        float64 // zero uses 0.9
	AllowGeneratedConfidence bool
}

func (p Policy) Setup(c *pons.Core) error {
	if p.ClassifierID != "" {
		if len(p.Classifiers) != 0 {
			return errors.New("actionpolicy: use ClassifierID or Classifiers, not both")
		}
		value, ok := c.Capability(ClassifierCapability(p.ClassifierID))
		if !ok {
			return fmt.Errorf("actionpolicy: classifier %q is not registered", p.ClassifierID)
		}
		classifier, ok := value.(Classifier)
		if !ok {
			return fmt.Errorf("actionpolicy: capability %q is not a classifier", p.ClassifierID)
		}
		p.Classifiers = []Classifier{classifier}
	}
	for i, rule := range p.Rules {
		if rule.Match == nil {
			return fmt.Errorf("actionpolicy: rule %d has no matcher", i)
		}
		switch rule.Action {
		case pons.DispositionDeny, pons.DispositionAsk, pons.DispositionAllow:
		default:
			return fmt.Errorf("actionpolicy: rule %d has invalid action %q", i, rule.Action)
		}
	}
	for i, classifier := range p.Classifiers {
		if classifier == nil {
			return fmt.Errorf("actionpolicy: classifier %d is nil", i)
		}
	}
	if p.MinSafeConfidence < 0 || p.MinSafeConfidence > 1 || math.IsNaN(p.MinSafeConfidence) {
		return errors.New("actionpolicy: MinSafeConfidence must be between 0 and 1")
	}
	return c.AddHooks(pons.Hooks{OnToolCallStart: p.onToolCallStart})
}

func (p Policy) onToolCallStart(ctx context.Context, event *pons.ToolCallStartEvent) error {
	decision, err := p.decide(ctx, *event)
	event.Decision = decision
	return err
}

func (p Policy) decide(ctx context.Context, req pons.ToolCallStartEvent) (pons.ActionDecision, error) {
	for _, action := range []pons.ActionDisposition{
		pons.DispositionDeny, pons.DispositionAsk, pons.DispositionAllow,
	} {
		for _, rule := range p.Rules {
			if rule.Action == action && rule.Match(req) {
				return pons.ActionDecision{Action: action, ReasonCode: rule.ReasonCode}, nil
			}
		}
	}
	threshold := p.MinSafeConfidence
	if threshold == 0 {
		threshold = 0.9
	}
	for _, classifier := range p.Classifiers {
		assessment, err := classifier.Assess(ctx, req)
		if err != nil || !validAssessment(assessment) {
			if ctx.Err() != nil {
				break
			}
			continue
		}
		decision := pons.ActionDecision{
			Action:     pons.DispositionAsk,
			Assessment: assessment,
			ReasonCode: "classifier_review",
		}
		if assessment.Risk == "safe" && assessment.Confidence >= threshold {
			if assessment.ProbabilityConfidence || p.AllowGeneratedConfidence {
				decision.Action = pons.DispositionAllow
				decision.ReasonCode = "classifier_safe"
			} else {
				decision.ReasonCode = "classifier_generated_confidence"
			}
		}
		return decision, nil
	}
	if len(p.Classifiers) == 0 {
		return pons.ActionDecision{Action: pons.DispositionAsk, ReasonCode: "no_matching_rule"}, nil
	}
	return pons.ActionDecision{Action: pons.DispositionAsk, ReasonCode: "classifier_unavailable"}, nil
}
