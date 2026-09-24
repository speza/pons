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
	if len(cases) != 12 {
		t.Fatalf("loaded %d cases, want 12", len(cases))
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
		`[{"id":"one","expected_risk":"maybe","event":{"Message":"task","Action":{"id":"a","kind":"run","args":{}}}}]`,
		`[{"id":"one","expected_risk":"safe","event":{"Message":"task","Action":{"id":"a","kind":"run","args":null}}}]`,
		`[{"id":"one","expected_risk":"safe","event":{"Message":"task","Action":{"id":"a","kind":"run","args":{}}}},{"id":"one","expected_risk":"review","event":{"Message":"task","Action":{"id":"b","kind":"run","args":{}}}}]`,
	} {
		if _, err := LoadEvalCases(strings.NewReader(input)); err == nil {
			t.Fatalf("accepted invalid eval cases: %s", input)
		}
	}
}

func TestEvaluateClassifierReportsUnsafeAllowsAndUnavailable(t *testing.T) {
	cases := []EvalCase{
		{ID: "safe", ExpectedRisk: "safe", Event: pons.ToolCallStartEvent{Action: protocol.Action{ID: "safe"}}},
		{ID: "review", ExpectedRisk: "review", Event: pons.ToolCallStartEvent{Action: protocol.Action{ID: "review"}}},
		{ID: "false-allow", ExpectedRisk: "review", Event: pons.ToolCallStartEvent{Action: protocol.Action{ID: "false-allow"}}},
		{ID: "unavailable", ExpectedRisk: "safe", Event: pons.ToolCallStartEvent{Action: protocol.Action{ID: "unavailable"}}},
	}
	classifier := ClassifierFunc(func(_ context.Context, event pons.ToolCallStartEvent) (pons.ActionAssessment, error) {
		switch event.Action.ID {
		case "review":
			return pons.ActionAssessment{Risk: "review", Confidence: 0.99, ReasonCode: "needs_review"}, nil
		case "unavailable":
			return pons.ActionAssessment{}, errors.New("provider unavailable")
		default:
			return pons.ActionAssessment{Risk: "safe", Confidence: 0.99, ReasonCode: "looks_safe"}, nil
		}
	})
	report, err := EvaluateClassifier(context.Background(), classifier, cases, 0.9)
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed != 2 || report.FalseAllows != 1 || report.FalseReviews != 1 || report.Unavailable != 1 {
		t.Fatalf("unexpected eval counts: %+v", report)
	}
	if report.Outcomes[2].Action != pons.DispositionAllow || report.Outcomes[2].Passed ||
		report.Outcomes[3].DecisionReasonCode != "classifier_unavailable" {
		t.Fatalf("unexpected eval outcomes: %+v", report.Outcomes)
	}
}
