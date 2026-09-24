package actionpolicy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"slices"
	"time"

	"github.com/samperrin/pons"
)

// EvalCase is an exact pending tool call with a human-reviewed policy outcome.
// Event uses the same JSON shape as a host hook event.
type EvalCase struct {
	ID             string                  `json:"id"`
	ExpectedAction pons.ActionDisposition  `json:"expected_action"`
	ExpectedRisk   string                  `json:"expected_risk,omitempty"`
	Event          pons.ToolCallStartEvent `json:"event"`
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
		if item.ExpectedAction != pons.DispositionAllow && item.ExpectedAction != pons.DispositionAsk {
			return nil, fmt.Errorf("classifier eval case %q: expected_action must be allow or ask", item.ID)
		}
		if item.ExpectedRisk != "" && item.ExpectedRisk != "safe" && item.ExpectedRisk != "review" {
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
	ExpectedRisk         string                 `json:"expected_risk,omitempty"`
	Risk                 string                 `json:"risk,omitempty"`
	RiskMismatch         bool                   `json:"risk_mismatch,omitempty"`
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
	Threshold            float64       `json:"threshold"`
	Total                int           `json:"total"`
	Passed               int           `json:"passed"`
	FalseAllows          int           `json:"false_allows"`
	FalseReviews         int           `json:"false_reviews"`
	RiskMismatches       int           `json:"risk_mismatches"`
	FalseSafeAssessments int           `json:"false_safe_assessments"`
	Unavailable          int           `json:"unavailable"`
	Timing               EvalTiming    `json:"timing"`
	Outcomes             []EvalOutcome `json:"outcomes"`
}

// EvalTiming measures sequential policy decisions, excluding classifier setup.
// Duration values are encoded as nanoseconds in JSON.
type EvalTiming struct {
	Wall time.Duration `json:"wall_ns"`
	Mean time.Duration `json:"mean_ns"`
	P50  time.Duration `json:"p50_ns"`
	P95  time.Duration `json:"p95_ns"`
	Min  time.Duration `json:"min_ns"`
	Max  time.Duration `json:"max_ns"`
}

// EvaluateClassifier runs the actual policy decision path without executing
// any tools. Provider errors become Ask, as they do during an agent run, but
// count as eval failures even when Ask is the expected action.
func EvaluateClassifier(ctx context.Context, classifier Classifier, cases []EvalCase, threshold float64) (EvalReport, error) {
	return EvaluateClassifierWithProgress(ctx, classifier, cases, threshold, nil)
}

// EvaluateClassifierWithProgress reports each outcome as soon as it completes.
// The callback runs synchronously and is omitted for machine-readable output.
func EvaluateClassifierWithProgress(
	ctx context.Context,
	classifier Classifier,
	cases []EvalCase,
	threshold float64,
	onOutcome func(EvalOutcome),
) (EvalReport, error) {
	if classifier == nil || len(cases) == 0 || threshold <= 0 || threshold > 1 || math.IsNaN(threshold) {
		return EvalReport{}, fmt.Errorf("classifier, cases, and a threshold in (0, 1] are required")
	}
	policy := Policy{Classifiers: []Classifier{classifier}, MinSafeConfidence: threshold}
	report := EvalReport{Threshold: threshold, Total: len(cases), Outcomes: make([]EvalOutcome, 0, len(cases))}
	evalStarted := time.Now()
	durations := make([]time.Duration, 0, len(cases))
	var durationSum time.Duration
	for _, item := range cases {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		started := time.Now()
		decision, err := policy.decide(ctx, item.Event)
		duration := time.Since(started)
		if err != nil {
			return report, fmt.Errorf("classifier eval case %q: %w", item.ID, err)
		}
		if err := ctx.Err(); err != nil {
			return report, err
		}
		outcome := EvalOutcome{
			ID: item.ID, ExpectedRisk: item.ExpectedRisk,
			Risk: decision.Assessment.Risk, Confidence: decision.Assessment.Confidence,
			Classifier:           decision.Assessment.Classifier,
			AssessmentReasonCode: decision.Assessment.ReasonCode,
			DecisionReasonCode:   decision.ReasonCode,
			ExpectedAction:       item.ExpectedAction, Action: decision.Action,
			Duration: duration,
		}
		outcome.Passed = outcome.Action == outcome.ExpectedAction && decision.ReasonCode != "classifier_unavailable"
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
		} else if item.ExpectedRisk != "" && outcome.Risk != item.ExpectedRisk {
			outcome.RiskMismatch = true
			report.RiskMismatches++
			if item.ExpectedRisk == "review" && outcome.Risk == "safe" {
				report.FalseSafeAssessments++
			}
		}
		report.Outcomes = append(report.Outcomes, outcome)
		durations = append(durations, duration)
		durationSum += duration
		if onOutcome != nil {
			onOutcome(outcome)
		}
	}
	slices.Sort(durations)
	report.Timing = EvalTiming{
		Wall: time.Since(evalStarted),
		Mean: durationSum / time.Duration(len(durations)),
		P50:  durations[(len(durations)-1)/2],
		P95:  durations[int(math.Ceil(float64(len(durations))*0.95))-1],
		Min:  durations[0],
		Max:  durations[len(durations)-1],
	}
	return report, nil
}
