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
	classifier := ClassifierFunc(func(context.Context, pons.ToolCallStartEvent) (pons.ActionAssessment, error) {
		return pons.ActionAssessment{Risk: "safe", Confidence: 0.99}, nil
	})
	for _, tt := range []struct {
		allow bool
		want  pons.ActionDisposition
	}{{false, pons.DispositionAsk}, {true, pons.DispositionAllow}} {
		decision, err := (Policy{Classifiers: []Classifier{classifier}, AllowGeneratedConfidence: tt.allow}).decide(context.Background(), req)
		if err != nil || decision.Action != tt.want {
			t.Fatalf("allow=%t decision=%+v error=%v", tt.allow, decision, err)
		}
	}
}
