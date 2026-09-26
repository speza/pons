package actionpolicy

import (
	"context"
	"errors"
	"testing"

	"github.com/samperrin/pons"
	"github.com/samperrin/pons/protocol"
)

func TestPolicyPrecedenceAndClassifierFallback(t *testing.T) {
	request := pons.ToolCallStartEvent{Action: protocol.Action{Kind: "run"}}
	called := 0
	classifier := ClassifierFunc(func(context.Context, pons.ToolCallStartEvent) (pons.ActionAssessment, error) {
		called++
		return pons.ActionAssessment{Risk: "safe", Confidence: 0.95, ProbabilityConfidence: true}, nil
	})
	match := func(pons.ToolCallStartEvent) bool { return true }
	tests := []struct {
		name  string
		rules []Rule
		want  pons.ActionDisposition
	}{
		{"hard deny wins", []Rule{{Action: pons.DispositionAllow, Match: match}, {Action: pons.DispositionAsk, Match: match}, {Action: pons.DispositionDeny, Match: match}}, pons.DispositionDeny},
		{"ask wins allow", []Rule{{Action: pons.DispositionAllow, Match: match}, {Action: pons.DispositionAsk, Match: match}}, pons.DispositionAsk},
		{"explicit allow", []Rule{{Action: pons.DispositionAllow, Match: match}}, pons.DispositionAllow},
		{"classifier", nil, pons.DispositionAllow},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := called
			decision, err := (Policy{Rules: tt.rules, Classifiers: []Classifier{classifier}}).decide(context.Background(), request)
			if err != nil || decision.Action != tt.want {
				t.Fatalf("decision=%+v error=%v", decision, err)
			}
			if (len(tt.rules) > 0 && called != before) || (len(tt.rules) == 0 && called != before+1) {
				t.Fatalf("classifier calls: before=%d after=%d", before, called)
			}
		})
	}
}

func TestPolicyClassifierFailureAndUncertaintyAsk(t *testing.T) {
	req := pons.ToolCallStartEvent{Action: protocol.Action{Kind: "run"}}
	tests := []struct {
		name        string
		classifiers []Classifier
		want        pons.ActionDisposition
	}{
		{"missing", nil, pons.DispositionAsk},
		{"failure", []Classifier{ClassifierFunc(func(context.Context, pons.ToolCallStartEvent) (pons.ActionAssessment, error) {
			return pons.ActionAssessment{}, errors.New("unavailable")
		})}, pons.DispositionAsk},
		{"low confidence", []Classifier{ClassifierFunc(func(context.Context, pons.ToolCallStartEvent) (pons.ActionAssessment, error) {
			return pons.ActionAssessment{Risk: "safe", Confidence: 0.5}, nil
		})}, pons.DispositionAsk},
		{"risky", []Classifier{ClassifierFunc(func(context.Context, pons.ToolCallStartEvent) (pons.ActionAssessment, error) {
			return pons.ActionAssessment{Risk: "dangerous", Confidence: 1}, nil
		})}, pons.DispositionAsk},
		{"fallback", []Classifier{
			ClassifierFunc(func(context.Context, pons.ToolCallStartEvent) (pons.ActionAssessment, error) {
				return pons.ActionAssessment{}, errors.New("first unavailable")
			}),
			ClassifierFunc(func(context.Context, pons.ToolCallStartEvent) (pons.ActionAssessment, error) {
				return pons.ActionAssessment{Risk: "safe", Confidence: 0.95, ProbabilityConfidence: true}, nil
			}),
		}, pons.DispositionAllow},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			decision, err := (Policy{Classifiers: tt.classifiers}).decide(context.Background(), req)
			if err != nil || decision.Action != tt.want {
				t.Fatalf("decision=%+v error=%v", decision, err)
			}
		})
	}
}

func TestPolicyInvalidAssessmentFallsBack(t *testing.T) {
	req := pons.ToolCallStartEvent{Action: protocol.Action{Kind: "run"}}
	called := false
	policy := Policy{Classifiers: []Classifier{
		ClassifierFunc(func(context.Context, pons.ToolCallStartEvent) (pons.ActionAssessment, error) {
			return pons.ActionAssessment{Risk: "unknown", Confidence: 0.99}, nil
		}),
		ClassifierFunc(func(context.Context, pons.ToolCallStartEvent) (pons.ActionAssessment, error) {
			called = true
			return pons.ActionAssessment{Risk: "safe", Confidence: 0.95, ProbabilityConfidence: true}, nil
		}),
	}}
	decision, err := policy.decide(context.Background(), req)
	if err != nil || !called || decision.Action != pons.DispositionAllow {
		t.Fatalf("decision=%+v fallback called=%t error=%v", decision, called, err)
	}
}

func TestGeneratedConfidenceRequiresOptIn(t *testing.T) {
	req := pons.ToolCallStartEvent{Action: protocol.Action{Kind: "run"}}
	for _, tt := range []struct {
		name        string
		probability bool
		optIn       bool
		want        pons.ActionDisposition
		reason      string
	}{
		{"generated estimate", false, false, pons.DispositionAsk, "classifier_generated_confidence"},
		{"generated estimate opted in", false, true, pons.DispositionAllow, "classifier_safe"},
		{"choice probability", true, false, pons.DispositionAllow, "classifier_safe"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			classifier := ClassifierFunc(func(context.Context, pons.ToolCallStartEvent) (pons.ActionAssessment, error) {
				return pons.ActionAssessment{Risk: "safe", Confidence: 0.99, ProbabilityConfidence: tt.probability}, nil
			})
			policy := Policy{Classifiers: []Classifier{classifier}, AllowGeneratedConfidence: tt.optIn}
			decision, err := policy.decide(context.Background(), req)
			if err != nil || decision.Action != tt.want || decision.ReasonCode != tt.reason {
				t.Fatalf("decision=%+v error=%v", decision, err)
			}
		})
	}
}

func TestToolRulesWithoutClassifier(t *testing.T) {
	policy := Policy{Rules: []Rule{
		ToolRule(pons.DispositionAllow, "rule_allow", "*"),
		ToolRule(pons.DispositionAsk, "shell_review", "bash"),
	}}
	for kind, want := range map[protocol.ActionKind]pons.ActionDisposition{
		"bash": pons.DispositionAsk, "read": pons.DispositionAllow,
	} {
		decision, err := policy.decide(context.Background(), pons.ToolCallStartEvent{Action: protocol.Action{Kind: kind}})
		if err != nil || decision.Action != want {
			t.Fatalf("%s: decision=%+v error=%v", kind, decision, err)
		}
	}

	unmatched := Policy{Rules: []Rule{ToolRule(pons.DispositionDeny, "no_bash", "bash")}}
	decision, err := unmatched.decide(context.Background(), pons.ToolCallStartEvent{Action: protocol.Action{Kind: "read"}})
	if err != nil || decision.Action != pons.DispositionAsk || decision.ReasonCode != "no_matching_rule" {
		t.Fatalf("unmatched decision=%+v error=%v", decision, err)
	}
}
