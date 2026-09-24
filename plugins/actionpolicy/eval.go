package actionpolicy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"time"

	"github.com/samperrin/pons"
)

// EvalCase is an exact pending tool call with a human-reviewed risk label.
// Event uses the same JSON shape as a host hook event.
type EvalCase struct {
	ID           string                  `json:"id"`
	ExpectedRisk string                  `json:"expected_risk"`
	Event        pons.ToolCallStartEvent `json:"event"`
}

// LoadEvalCases decodes a strict, nonempty JSON array of labeled calls.
func LoadEvalCases(reader io.Reader) ([]EvalCase, error) {
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	var cases []EvalCase
	if err := decoder.Decode(&cases); err != nil {
		return nil, fmt.Errorf("decode classifier eval cases: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("classifier eval cases: trailing data")
	}
	if len(cases) == 0 {
		return nil, fmt.Errorf("classifier eval cases must not be empty")
	}
	ids := make(map[string]bool, len(cases))
	for i, item := range cases {
		if item.ID == "" || ids[item.ID] {
			return nil, fmt.Errorf("classifier eval case %d has an empty or duplicate id", i)
		}
		ids[item.ID] = true
		if item.ExpectedRisk != "safe" && item.ExpectedRisk != "review" {
			return nil, fmt.Errorf("classifier eval case %q: expected_risk must be safe or review", item.ID)
		}
		if item.Event.Message == "" || item.Event.Action.Kind == "" || item.Event.Action.ID == "" {
			return nil, fmt.Errorf("classifier eval case %q: message, action id, and kind are required", item.ID)
		}
		if args := bytes.TrimSpace(item.Event.Action.Args); len(args) == 0 || args[0] != '{' || !json.Valid(args) {
			return nil, fmt.Errorf("classifier eval case %q: action args must be a JSON object", item.ID)
		}
	}
	return cases, nil
}

type EvalOutcome struct {
	ID                   string                 `json:"id"`
	ExpectedRisk         string                 `json:"expected_risk"`
	Risk                 string                 `json:"risk,omitempty"`
	Confidence           float64                `json:"confidence"`
	Classifier           string                 `json:"classifier,omitempty"`
	AssessmentReasonCode string                 `json:"assessment_reason_code,omitempty"`
	DecisionReasonCode   string                 `json:"decision_reason_code,omitempty"`
	ExpectedAction       pons.ActionDisposition `json:"expected_action"`
	Action               pons.ActionDisposition `json:"action"`
	Passed               bool                   `json:"passed"`
	Duration             time.Duration          `json:"duration_ns"`
}

type EvalReport struct {
	Threshold    float64       `json:"threshold"`
	Total        int           `json:"total"`
	Passed       int           `json:"passed"`
	FalseAllows  int           `json:"false_allows"`
	FalseReviews int           `json:"false_reviews"`
	Unavailable  int           `json:"unavailable"`
	Outcomes     []EvalOutcome `json:"outcomes"`
}

// EvaluateClassifier runs the actual policy decision path without executing
// any tools. Provider errors become Ask, as they do during an agent run.
func EvaluateClassifier(ctx context.Context, classifier Classifier, cases []EvalCase, threshold float64) (EvalReport, error) {
	if classifier == nil || len(cases) == 0 || threshold <= 0 || threshold > 1 || math.IsNaN(threshold) {
		return EvalReport{}, fmt.Errorf("classifier, cases, and a threshold in (0, 1] are required")
	}
	policy := Policy{Classifiers: []Classifier{classifier}, MinSafeConfidence: threshold}
	report := EvalReport{Threshold: threshold, Total: len(cases), Outcomes: make([]EvalOutcome, 0, len(cases))}
	for _, item := range cases {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		started := time.Now()
		decision, err := policy.decide(ctx, item.Event)
		if err != nil {
			return report, fmt.Errorf("classifier eval case %q: %w", item.ID, err)
		}
		if err := ctx.Err(); err != nil {
			return report, err
		}
		expectedAction := pons.DispositionAsk
		if item.ExpectedRisk == "safe" {
			expectedAction = pons.DispositionAllow
		}
		outcome := EvalOutcome{
			ID: item.ID, ExpectedRisk: item.ExpectedRisk,
			Risk: decision.Assessment.Risk, Confidence: decision.Assessment.Confidence,
			Classifier:           decision.Assessment.Classifier,
			AssessmentReasonCode: decision.Assessment.ReasonCode,
			DecisionReasonCode:   decision.ReasonCode,
			ExpectedAction:       expectedAction, Action: decision.Action,
			Duration: time.Since(started),
		}
		outcome.Passed = outcome.Risk == outcome.ExpectedRisk && outcome.Action == outcome.ExpectedAction
		if outcome.Passed {
			report.Passed++
		}
		if outcome.ExpectedAction != pons.DispositionAllow && outcome.Action == pons.DispositionAllow {
			report.FalseAllows++
		}
		if outcome.ExpectedAction == pons.DispositionAllow && outcome.Action != pons.DispositionAllow {
			report.FalseReviews++
		}
		if decision.ReasonCode == "classifier_unavailable" {
			report.Unavailable++
		}
		report.Outcomes = append(report.Outcomes, outcome)
	}
	return report, nil
}
