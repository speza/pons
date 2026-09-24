package actionpolicy

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/samperrin/pons"
	"github.com/samperrin/pons/protocol"
)

func TestClassifierEvalCasesLoad(t *testing.T) {
	file, err := os.Open("testdata/classifier_eval.json")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	cases, err := LoadEvalCases(file)
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 16 {
		t.Fatalf("loaded %d cases, want 16", len(cases))
	}
	for _, item := range cases {
		if len(item.Event.RecentContext) == 0 {
			t.Fatalf("case %q has no source-labeled context", item.ID)
		}
	}
}

func TestLoadEvalCasesRejectsInvalidLabelsAndCalls(t *testing.T) {
	for _, input := range []string{
		`[]`,
		`[{"id":"one","expected_action":"maybe","event":{"Message":"task","Action":{"id":"a","kind":"run","args":{}}}}]`,
		`[{"id":"one","expected_action":"ask","expected_risk":"maybe","event":{"Message":"task","Action":{"id":"a","kind":"run","args":{}}}}]`,
		`[{"id":"one","expected_action":"allow","event":{"Message":"task","Action":{"id":"a","kind":"run","args":null}}}]`,
		`[{"id":"one","expected_action":"allow","event":{"Message":"task","Action":{"id":"a","kind":"run","args":{}}}},{"id":"one","expected_action":"ask","event":{"Message":"task","Action":{"id":"b","kind":"run","args":{}}}}]`,
	} {
		if _, err := LoadEvalCases(strings.NewReader(input)); err == nil {
			t.Fatalf("accepted invalid eval cases: %s", input)
		}
	}
}

func TestEvaluateClassifierReportsUnsafeAllowsAndUnavailable(t *testing.T) {
	cases := []EvalCase{
		{ID: "safe", ExpectedAction: pons.DispositionAllow, Event: pons.ToolCallStartEvent{Action: protocol.Action{ID: "safe"}}},
		{ID: "review", ExpectedAction: pons.DispositionAsk, Event: pons.ToolCallStartEvent{Action: protocol.Action{ID: "review"}}},
		{ID: "uncertain-safe", ExpectedAction: pons.DispositionAsk, ExpectedRisk: "review", Event: pons.ToolCallStartEvent{Action: protocol.Action{ID: "uncertain-safe"}}},
		{ID: "false-allow", ExpectedAction: pons.DispositionAsk, ExpectedRisk: "review", Event: pons.ToolCallStartEvent{Action: protocol.Action{ID: "false-allow"}}},
		{ID: "unavailable", ExpectedAction: pons.DispositionAllow, Event: pons.ToolCallStartEvent{Action: protocol.Action{ID: "unavailable"}}},
		{ID: "unavailable-ask", ExpectedAction: pons.DispositionAsk, Event: pons.ToolCallStartEvent{Action: protocol.Action{ID: "unavailable-ask"}}},
	}
	classifier := ClassifierFunc(func(_ context.Context, event pons.ToolCallStartEvent) (pons.ActionAssessment, error) {
		switch event.Action.ID {
		case "review":
			return pons.ActionAssessment{Risk: "review", Confidence: 0.99, ReasonCode: "needs_review"}, nil
		case "uncertain-safe":
			return pons.ActionAssessment{Risk: "safe", Confidence: 0.58, ReasonCode: "uncertain"}, nil
		case "unavailable", "unavailable-ask":
			return pons.ActionAssessment{}, errors.New("provider unavailable")
		default:
			return pons.ActionAssessment{Risk: "safe", Confidence: 0.99, ReasonCode: "looks_safe"}, nil
		}
	})
	var completed []string
	report, err := EvaluateClassifierWithProgress(
		context.Background(), classifier, cases, 0.9,
		func(outcome EvalOutcome) { completed = append(completed, outcome.ID) },
	)
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed != 3 || report.FalseAllows != 1 || report.FalseReviews != 1 || report.Unavailable != 2 ||
		report.RiskMismatches != 2 || report.FalseSafeAssessments != 2 {
		t.Fatalf("unexpected eval counts: %+v", report)
	}
	if !report.Outcomes[2].Passed || report.Outcomes[2].Risk != "safe" || !report.Outcomes[2].RiskMismatch ||
		report.Outcomes[3].Action != pons.DispositionAllow || report.Outcomes[3].Passed ||
		report.Outcomes[4].DecisionReasonCode != "classifier_unavailable" || report.Outcomes[5].Passed {
		t.Fatalf("unexpected eval outcomes: %+v", report.Outcomes)
	}
	if len(completed) != len(cases) || report.Timing.Wall <= 0 ||
		report.Timing.Min > report.Timing.P50 || report.Timing.P50 > report.Timing.P95 ||
		report.Timing.P95 > report.Timing.Max {
		t.Fatalf("unexpected progress or timing: completed=%v timing=%+v", completed, report.Timing)
	}
	for i, item := range cases {
		if completed[i] != item.ID {
			t.Fatalf("progress order: completed=%v", completed)
		}
	}
}
