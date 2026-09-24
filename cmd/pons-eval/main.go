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
	model := flag.String("model", "", "classifier model (defaults to gpt-6-luna or jev-latest)")
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
	report, err := actionpolicy.EvaluateClassifier(context.Background(), classifier, cases, threshold)
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
		for _, outcome := range report.Outcomes {
			status := "PASS"
			if !outcome.Passed {
				status = "FAIL"
			}
			reason := outcome.AssessmentReasonCode
			if reason == "" {
				reason = outcome.DecisionReasonCode
			}
			fmt.Printf("%-4s %-32s expected=%-6s got=%-6s confidence=%.2f action=%-5s reason=%s\n",
				status, outcome.ID, outcome.ExpectedRisk, outcome.Risk, outcome.Confidence, outcome.Action, reason)
		}
		fmt.Printf("%d/%d passed; false allows=%d, false reviews=%d, unavailable=%d\n",
			report.Passed, report.Total, report.FalseAllows, report.FalseReviews, report.Unavailable)
	}
	if report.Passed != report.Total {
		return fmt.Errorf("classifier eval failed")
	}
	return nil
}

func newClassifier(kind, model, authID, keyEnv string, timeout time.Duration) (actionpolicy.Classifier, error) {
	if timeout <= 0 {
		return nil, fmt.Errorf("timeout must be positive")
	}
	switch kind {
	case "codex":
		if model == "" {
			model = "gpt-6-luna"
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
