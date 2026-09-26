package hookeval

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samperrin/pons"
	"github.com/samperrin/pons/protocol"
)

func TestShippedCasesLoad(t *testing.T) {
	cases, err := LoadCaseFiles("../../plugins/actionpolicy/evals")
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) < 10 {
		t.Fatalf("loaded %d cases", len(cases))
	}
}

func TestLoadCasesRejectsInvalidCases(t *testing.T) {
	valid := `"hook":"on_tool_call_start","input":{"action":{"id":"a","kind":"run","args":{}}},"expect":{"permission":"ask"}`
	for name, input := range map[string]string{
		"empty":          `[]`,
		"missing id":     `[{` + valid + `}]`,
		"other hook":     `[{"id":"x","hook":"on_agent_end","input":{"action":{"id":"a","kind":"run","args":{}}},"expect":{"permission":"ask"}}]`,
		"missing kind":   `[{"id":"x","hook":"on_tool_call_start","input":{"action":{"id":"a","args":{}}},"expect":{"permission":"ask"}}]`,
		"array args":     `[{"id":"x","hook":"on_tool_call_start","input":{"action":{"id":"a","kind":"run","args":[]}},"expect":{"permission":"ask"}}]`,
		"unknown input":  `[{"id":"x","hook":"on_tool_call_start","input":{"prompt":"old","action":{"id":"a","kind":"run","args":{}}},"expect":{"permission":"ask"}}]`,
		"empty expect":   `[{"id":"x","hook":"on_tool_call_start","input":{"action":{"id":"a","kind":"run","args":{}}},"expect":{}}]`,
		"bad reference":  `[{"id":"x",` + valid + `,"reference_risk":"maybe"}]`,
		"unknown field":  `[{"id":"x",` + valid + `,"expected_action":"ask"}]`,
		"trailing value": `[{"id":"x",` + valid + `}] []`,
	} {
		if _, err := LoadCases(strings.NewReader(input)); err == nil {
			t.Errorf("%s: accepted %s", name, input)
		}
	}
}

func TestLoadCaseFilesRejectsDuplicateIDs(t *testing.T) {
	dir := t.TempDir()
	body := `[{"id":"same","hook":"on_tool_call_start","input":{"action":{"id":"a","kind":"run","args":{}}},"expect":{"permission":"ask"}}]`
	for _, name := range []string{"a.json", "b.json"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := LoadCaseFiles(dir); err == nil || !strings.Contains(err.Error(), "duplicates") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunScoresOutputsAndMetrics(t *testing.T) {
	core := pons.New()
	if err := core.AddHooks(pons.Hooks{OnToolCallStart: func(_ context.Context, in pons.ToolCallStartInput) (pons.ToolCallStartOutput, error) {
		switch in.Action.ID {
		case "allowed-risky":
			return pons.ToolCallStartOutput{
				Permission: pons.PermissionAllow, Reason: "classifier_safe",
				Assessment: &pons.ActionAssessment{Risk: "safe", Confidence: 0.99},
			}, nil
		case "asked-routine":
			return pons.ToolCallStartOutput{Permission: pons.PermissionAsk, Reason: "classifier_review"}, nil
		case "outage":
			return pons.ToolCallStartOutput{}, errors.New("classifier down")
		case "rewritten":
			return pons.ToolCallStartOutput{UpdatedInput: protocol.MustArgsJSON(map[string]string{"path": "safe.txt"})}, nil
		default:
			return pons.ToolCallStartOutput{
				Permission: pons.PermissionAsk, Reason: "classifier_review",
				Assessment: &pons.ActionAssessment{Risk: "review", Confidence: 0.8},
			}, nil
		}
	}}); err != nil {
		t.Fatal(err)
	}
	cases, err := LoadCases(strings.NewReader(`[
		{"id":"allowed-risky","hook":"on_tool_call_start","input":{"action":{"id":"allowed-risky","kind":"bash","args":{}}},"expect":{"permission":"ask"},"reference_risk":"review"},
		{"id":"asked-routine","hook":"on_tool_call_start","input":{"action":{"id":"asked-routine","kind":"read","args":{}}},"expect":{"permission":"allow"}},
		{"id":"outage","hook":"on_tool_call_start","input":{"action":{"id":"outage","kind":"bash","args":{}}},"expect":{"permission":"ask"}},
		{"id":"rewritten","hook":"on_tool_call_start","input":{"action":{"id":"rewritten","kind":"write","args":{"path":"x"}}},"expect":{"permission":"allow","updated_input":{"path":"safe.txt"}}},
		{"id":"reviewed","hook":"on_tool_call_start","input":{"action":{"id":"reviewed","kind":"bash","args":{}}},"expect":{"permission":"ask","assessment":{"risk":"review"}},"reference_risk":"review"}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	var progress []string
	report, err := Run(context.Background(), core, cases, func(outcome Outcome) { progress = append(progress, outcome.ID) })
	if err != nil {
		t.Fatal(err)
	}
	passed := map[string]bool{}
	for _, outcome := range report.Outcomes {
		passed[outcome.ID] = outcome.Passed
	}
	want := map[string]bool{"allowed-risky": false, "asked-routine": false, "outage": false, "rewritten": true, "reviewed": true}
	for id, ok := range want {
		if passed[id] != ok {
			t.Errorf("%s passed = %v, want %v (%+v)", id, passed[id], ok, report.Outcomes)
		}
	}
	if report.Total != 5 || report.Passed != 2 || report.FalseAllows != 1 || report.FalseAsks != 1 ||
		report.HookErrors != 1 || report.ReferenceMismatches != 1 || report.FalseSafeAssessments != 1 {
		t.Fatalf("report = %+v", report)
	}
	if mismatch := report.Outcomes[0].Mismatches; len(mismatch) != 1 || mismatch[0] != `permission: expected "ask", got "allow"` {
		t.Fatalf("mismatches = %v", mismatch)
	}
	if len(progress) != 5 || report.Timing.Min > report.Timing.Max {
		t.Fatalf("progress = %v, timing = %+v", progress, report.Timing)
	}
}
