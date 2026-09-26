package actionpolicy

import (
	"context"
	"errors"
	"testing"

	"github.com/samperrin/pons"
	"github.com/samperrin/pons/protocol"
)

func TestPolicyPrecedenceAndClassifierFallback(t *testing.T) {
	request := pons.ToolCallStartInput{Action: protocol.Action{Kind: "run"}}
	called := 0
	classifier := ClassifierFunc(func(context.Context, pons.ToolCallStartInput) (pons.ActionAssessment, error) {
		called++
		return pons.ActionAssessment{Risk: "safe", Confidence: 0.95, ProbabilityConfidence: true}, nil
	})
	match := func(pons.ToolCallStartInput) bool { return true }
	tests := []struct {
		name  string
		rules []Rule
		want  pons.Permission
	}{
		{"hard deny wins", []Rule{{Action: pons.PermissionAllow, Match: match}, {Action: pons.PermissionAsk, Match: match}, {Action: pons.PermissionDeny, Match: match}}, pons.PermissionDeny},
		{"ask wins allow", []Rule{{Action: pons.PermissionAllow, Match: match}, {Action: pons.PermissionAsk, Match: match}}, pons.PermissionAsk},
		{"explicit allow", []Rule{{Action: pons.PermissionAllow, Match: match}}, pons.PermissionAllow},
		{"classifier", nil, pons.PermissionAllow},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := called
			decision, err := (Policy{Rules: tt.rules, Classifiers: []Classifier{classifier}}).decide(context.Background(), request)
			if err != nil || decision.Permission != tt.want {
				t.Fatalf("decision=%+v error=%v", decision, err)
			}
			if (len(tt.rules) > 0 && called != before) || (len(tt.rules) == 0 && called != before+1) {
				t.Fatalf("classifier calls: before=%d after=%d", before, called)
			}
		})
	}
}

func TestPolicyClassifierFailureAndUncertaintyAsk(t *testing.T) {
	req := pons.ToolCallStartInput{Action: protocol.Action{Kind: "run"}}
	tests := []struct {
		name        string
		classifiers []Classifier
		want        pons.Permission
		wantErr     bool // an outage is reported as a hook error
	}{
		{"missing", nil, pons.PermissionAsk, false},
		{"failure", []Classifier{ClassifierFunc(func(context.Context, pons.ToolCallStartInput) (pons.ActionAssessment, error) {
			return pons.ActionAssessment{}, errors.New("unavailable")
		})}, pons.PermissionAsk, true},
		{"low confidence", []Classifier{ClassifierFunc(func(context.Context, pons.ToolCallStartInput) (pons.ActionAssessment, error) {
			return pons.ActionAssessment{Risk: "safe", Confidence: 0.5}, nil
		})}, pons.PermissionAsk, false},
		{"risky", []Classifier{ClassifierFunc(func(context.Context, pons.ToolCallStartInput) (pons.ActionAssessment, error) {
			return pons.ActionAssessment{Risk: "dangerous", Confidence: 1}, nil
		})}, pons.PermissionAsk, true},
		{"fallback", []Classifier{
			ClassifierFunc(func(context.Context, pons.ToolCallStartInput) (pons.ActionAssessment, error) {
				return pons.ActionAssessment{}, errors.New("first unavailable")
			}),
			ClassifierFunc(func(context.Context, pons.ToolCallStartInput) (pons.ActionAssessment, error) {
				return pons.ActionAssessment{Risk: "safe", Confidence: 0.95, ProbabilityConfidence: true}, nil
			}),
		}, pons.PermissionAllow, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			decision, err := (Policy{Classifiers: tt.classifiers}).decide(context.Background(), req)
			if (err != nil) != tt.wantErr || decision.Permission != tt.want {
				t.Fatalf("decision=%+v error=%v", decision, err)
			}
		})
	}
}

func TestPolicyInvalidAssessmentFallsBack(t *testing.T) {
	req := pons.ToolCallStartInput{Action: protocol.Action{Kind: "run"}}
	called := false
	policy := Policy{Classifiers: []Classifier{
		ClassifierFunc(func(context.Context, pons.ToolCallStartInput) (pons.ActionAssessment, error) {
			return pons.ActionAssessment{Risk: "unknown", Confidence: 0.99}, nil
		}),
		ClassifierFunc(func(context.Context, pons.ToolCallStartInput) (pons.ActionAssessment, error) {
			called = true
			return pons.ActionAssessment{Risk: "safe", Confidence: 0.95, ProbabilityConfidence: true}, nil
		}),
	}}
	decision, err := policy.decide(context.Background(), req)
	if err != nil || !called || decision.Permission != pons.PermissionAllow {
		t.Fatalf("decision=%+v fallback called=%t error=%v", decision, called, err)
	}
}

func TestGeneratedConfidenceRequiresOptIn(t *testing.T) {
	req := pons.ToolCallStartInput{Action: protocol.Action{Kind: "run"}}
	for _, tt := range []struct {
		name        string
		probability bool
		optIn       bool
		want        pons.Permission
		reason      string
	}{
		{"generated estimate", false, false, pons.PermissionAsk, "classifier_generated_confidence"},
		{"generated estimate opted in", false, true, pons.PermissionAllow, "classifier_safe"},
		{"choice probability", true, false, pons.PermissionAllow, "classifier_safe"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			classifier := ClassifierFunc(func(context.Context, pons.ToolCallStartInput) (pons.ActionAssessment, error) {
				return pons.ActionAssessment{Risk: "safe", Confidence: 0.99, ProbabilityConfidence: tt.probability}, nil
			})
			policy := Policy{Classifiers: []Classifier{classifier}, AllowGeneratedConfidence: tt.optIn}
			decision, err := policy.decide(context.Background(), req)
			if err != nil || decision.Permission != tt.want || decision.Reason != tt.reason {
				t.Fatalf("decision=%+v error=%v", decision, err)
			}
		})
	}
}

func TestToolRulesWithoutClassifier(t *testing.T) {
	policy := Policy{Rules: []Rule{
		ToolRule(pons.PermissionAllow, "rule_allow", "*"),
		ToolRule(pons.PermissionAsk, "shell_review", "bash"),
	}}
	for kind, want := range map[protocol.ActionKind]pons.Permission{
		"bash": pons.PermissionAsk, "read": pons.PermissionAllow,
	} {
		decision, err := policy.decide(context.Background(), pons.ToolCallStartInput{Action: protocol.Action{Kind: kind}})
		if err != nil || decision.Permission != want {
			t.Fatalf("%s: decision=%+v error=%v", kind, decision, err)
		}
	}

	unmatched := Policy{Rules: []Rule{ToolRule(pons.PermissionDeny, "no_bash", "bash")}}
	decision, err := unmatched.decide(context.Background(), pons.ToolCallStartInput{Action: protocol.Action{Kind: "read"}})
	if err != nil || decision.Permission != pons.PermissionAsk || decision.Reason != "no_matching_rule" {
		t.Fatalf("unmatched decision=%+v error=%v", decision, err)
	}
}
