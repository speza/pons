// Command pons-eval runs labeled action-policy cases against a live classifier.
// It only assesses proposed calls; it never executes their tools.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/samperrin/pons/plugins/actionpolicy"
	"github.com/samperrin/pons/plugins/brain/llm"
)

func main() {
	casesPath := flag.String("cases", "plugins/actionpolicy/testdata/classifier_eval.json", "labeled classifier eval cases")
	kind := flag.String("classifier", "codex", "classifier: codex, openai, or typesafe-jev")
	model := flag.String("model", "", "classifier model (required for codex; otherwise provider default)")
	authID := flag.String("auth-id", "codex", "Codex auth-store entry")
	keyEnv := flag.String("api-key-env", "", "API key environment variable for OpenAI or TypeSafe")
	threshold := flag.Float64("threshold", 0.9, "minimum safe confidence for an allow")
	timeout := flag.Duration("timeout", 20*time.Second, "timeout per classifier call")
	only := flag.String("case", "", "run only the case with this ID")
	jsonOutput := flag.Bool("json", false, "print a machine-readable JSON report")
	flag.Parse()

	if err := run(*casesPath, *kind, *model, *authID, *keyEnv, *threshold, *timeout, *only, *jsonOutput); err != nil {
		fmt.Fprintln(os.Stderr, "pons-eval:", err)
		os.Exit(1)
	}
}

func run(casesPath, kind, model, authID, keyEnv string, threshold float64, timeout time.Duration, only string, jsonOutput bool) error {
	file, err := os.Open(casesPath)
	if err != nil {
		return err
	}
	defer file.Close()
	cases, err := actionpolicy.LoadEvalCases(file)
	if err != nil {
		return err
	}
	if only != "" {
		var selected []actionpolicy.EvalCase
		for _, item := range cases {
			if item.ID == only {
				selected = append(selected, item)
			}
		}
		if len(selected) == 0 {
			return fmt.Errorf("case %q was not found", only)
		}
		cases = selected
	}
	classifier, err := newClassifier(kind, model, authID, keyEnv, timeout)
	if err != nil {
		return err
	}
	var onOutcome func(actionpolicy.EvalOutcome)
	if !jsonOutput {
		onOutcome = printOutcome
	}
	report, err := actionpolicy.EvaluateClassifierWithProgress(
		context.Background(), classifier, cases, threshold, onOutcome,
	)
	if err != nil {
		return err
	}
	if jsonOutput {
		encoded, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(encoded))
	} else {
		fmt.Printf("%d/%d passed; false allows=%d, false reviews=%d, unavailable=%d\n",
			report.Passed, report.Total, report.FalseAllows, report.FalseReviews, report.Unavailable)
		fmt.Printf("raw risk mismatches=%d (false-safe assessments=%d)\n",
			report.RiskMismatches, report.FalseSafeAssessments)
		fmt.Printf("latency: wall=%s mean=%s p50=%s p95=%s min=%s max=%s\n",
			report.Timing.Wall.Round(time.Millisecond), report.Timing.Mean.Round(time.Millisecond),
			report.Timing.P50.Round(time.Millisecond), report.Timing.P95.Round(time.Millisecond),
			report.Timing.Min.Round(time.Millisecond), report.Timing.Max.Round(time.Millisecond))
	}
	if report.Passed != report.Total {
		return fmt.Errorf("classifier eval failed")
	}
	return nil
}

func printOutcome(outcome actionpolicy.EvalOutcome) {
	status := "PASS"
	if !outcome.Passed {
		status = "FAIL"
	}
	reason := outcome.AssessmentReasonCode
	if reason == "" {
		reason = outcome.DecisionReasonCode
	}
	fmt.Printf("%-4s %-43s expected=%-5s action=%-5s risk=%-6s reference=%-6s confidence=%.2f latency=%-8s reason=%s\n",
		status, outcome.ID, outcome.ExpectedAction, outcome.Action, outcome.Risk, outcome.ExpectedRisk, outcome.Confidence,
		outcome.Duration.Round(time.Millisecond), reason)
}

func newClassifier(kind, model, authID, keyEnv string, timeout time.Duration) (actionpolicy.Classifier, error) {
	if timeout <= 0 {
		return nil, fmt.Errorf("timeout must be positive")
	}
	switch kind {
	case "codex":
		if model == "" {
			return nil, fmt.Errorf("-model is required for codex eval; select the configured classifier model")
		}
		client, err := llm.NewProviderClient(llm.Fallback{ID: authID, Provider: "codex", Model: model})
		if err != nil {
			return nil, err
		}
		return actionpolicy.NewCodexClassifier(client, model, timeout)
	case "openai", "typesafe-jev":
		if keyEnv == "" {
			keyEnv = "OPENAI_API_KEY"
			if kind == "typesafe-jev" {
				keyEnv = "TYPESAFE_API_KEY"
			}
		}
		config := actionpolicy.RemoteConfig{APIKey: os.Getenv(keyEnv), Model: model, Timeout: timeout}
		if config.APIKey == "" {
			return nil, fmt.Errorf("%s is not set", keyEnv)
		}
		if kind == "openai" {
			return actionpolicy.NewOpenAIClassifier(config)
		}
		return actionpolicy.NewTypeSafeClassifier(config)
	default:
		return nil, fmt.Errorf("unknown classifier %q", kind)
	}
}
