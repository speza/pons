package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/samperrin/pons"
	"github.com/samperrin/pons/internal/hookeval"
	"github.com/samperrin/pons/plugins/actionpolicy"
	"github.com/samperrin/pons/plugins/external"
	"github.com/samperrin/pons/plugins/hostconfig"
)

type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

// runEval implements `pons eval [flags] <plugin-id>`: it builds the plugin
// from the global config, exactly as a run would, and scores its hooks
// against labeled cases without executing any tool.
func runEval(ctx context.Context, args []string, stdout, stderr io.Writer, home string, registry *hostconfig.Registry) error {
	flags := flag.NewFlagSet("pons eval", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var casePaths stringList
	flags.Var(&casePaths, "cases", "eval case file or directory (repeatable; default: an installed plugin's evals directory)")
	only := flags.String("case", "", "run only the case with this ID")
	threshold := flags.Float64("threshold", 0.9, "classifier plugins only: minimum safe confidence for an allow")
	jsonOutput := flags.Bool("json", false, "print a machine-readable JSON report")
	pluginPath := flags.String("plugin-path", "", "PATH supplied to external hook plugins, as for runs")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "usage: pons eval [flags] <plugin-id>")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		flags.Usage()
		return errors.New("exactly one plugin ID is required")
	}
	id := flags.Arg(0)
	thresholdSet := false
	flags.Visit(func(f *flag.Flag) { thresholdSet = thresholdSet || f.Name == "threshold" })

	global, err := loadSettings(home, "")
	if err != nil {
		return err
	}
	providers, err := trustedPluginProviders(global, map[string]bool{}, "anthropic", "", "", nil)
	if err != nil {
		return fmt.Errorf("plugin providers: %w", err)
	}
	options, err := registry.BuildPlugin(global.Plugins, id, os.Getenv, providers)
	if err != nil {
		return err
	}

	launch := hookLaunch{Path: *pluginPath}
	if global.PluginMaxResultBytes != nil {
		launch.MaxResultBytes = *global.PluginMaxResultBytes
	}
	core := pons.New()
	var defaultCases string
	for _, plugin := range options.Plugins {
		if plugin.Host != nil {
			if err := core.Use(plugin.Host); err != nil {
				return err
			}
			continue
		}
		manifest, err := external.LoadManifest(plugin.Manifest)
		if err != nil {
			return err
		}
		if manifest.Placement != external.PlacementHost {
			return fmt.Errorf("plugin %q provides tools, not hooks; only hook plugins can be evaluated", plugin.ID)
		}
		hooks, err := launch.start(plugin.Manifest, plugin.Config, "")
		if err != nil {
			return err
		}
		defer hooks.Close()
		if err := core.Use(hooks); err != nil {
			return err
		}
		if plugin.ID == id {
			defaultCases = filepath.Join(manifest.Dir(), "evals")
		}
	}
	if _, ok := core.Capability(actionpolicy.ClassifierCapability(id)); ok {
		// A classifier makes no decision by itself. Evaluate it through a
		// bare policy that accepts its confidence, which measures where its
		// own allow boundary falls.
		if err := core.Use(actionpolicy.Policy{
			ClassifierID: id, MinSafeConfidence: *threshold, AllowGeneratedConfidence: true,
		}); err != nil {
			return err
		}
	} else if thresholdSet {
		return fmt.Errorf("-threshold applies only to classifier plugins; %q is not one", id)
	}

	if len(casePaths) == 0 {
		if defaultCases == "" {
			return fmt.Errorf("-cases is required for built-in plugin %q", id)
		}
		casePaths = stringList{defaultCases}
	}
	cases, err := hookeval.LoadCaseFiles(casePaths...)
	if err != nil {
		return err
	}
	if *only != "" {
		var selected []hookeval.Case
		for _, item := range cases {
			if item.ID == *only {
				selected = append(selected, item)
			}
		}
		if len(selected) == 0 {
			return fmt.Errorf("case %q was not found", *only)
		}
		cases = selected
	}

	var onOutcome func(hookeval.Outcome)
	if !*jsonOutput {
		onOutcome = func(outcome hookeval.Outcome) { printEvalOutcome(stdout, outcome) }
	}
	report, err := hookeval.Run(ctx, core, cases, onOutcome)
	if err != nil {
		return err
	}
	if *jsonOutput {
		encoded, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(stdout, string(encoded))
	} else {
		printEvalSummary(stdout, report)
	}
	if report.Passed != report.Total {
		return fmt.Errorf("%d of %d cases failed", report.Total-report.Passed, report.Total)
	}
	return nil
}

func printEvalOutcome(w io.Writer, outcome hookeval.Outcome) {
	status := "PASS"
	if !outcome.Passed {
		status = "FAIL"
	}
	fmt.Fprintf(w, "%-4s %-43s expected=%-5s permission=%-5s", status, outcome.ID, outcome.ExpectedPermission, outcome.Permission)
	if outcome.Risk != "" {
		fmt.Fprintf(w, " risk=%-6s reference=%-6s confidence=%.2f", outcome.Risk, outcome.ReferenceRisk, outcome.Confidence)
	}
	fmt.Fprintf(w, " latency=%-8s reason=%s\n", outcome.Duration.Round(time.Millisecond), outcome.Reason)
	for _, mismatch := range outcome.Mismatches {
		fmt.Fprintf(w, "     mismatch: %s\n", mismatch)
	}
	for _, hookErr := range outcome.HookErrors {
		fmt.Fprintf(w, "     hook error: %s\n", hookErr)
	}
}

func printEvalSummary(w io.Writer, report hookeval.Report) {
	fmt.Fprintf(w, "%d/%d passed; false allows=%d, false asks=%d, hook errors=%d\n",
		report.Passed, report.Total, report.FalseAllows, report.FalseAsks, report.HookErrors)
	if report.ReferenceMismatches > 0 || report.FalseSafeAssessments > 0 {
		fmt.Fprintf(w, "reference risk mismatches=%d (false-safe assessments=%d)\n",
			report.ReferenceMismatches, report.FalseSafeAssessments)
	}
	fmt.Fprintf(w, "latency: wall=%s mean=%s p50=%s p95=%s min=%s max=%s\n",
		report.Timing.Wall.Round(time.Millisecond), report.Timing.Mean.Round(time.Millisecond),
		report.Timing.P50.Round(time.Millisecond), report.Timing.P95.Round(time.Millisecond),
		report.Timing.Min.Round(time.Millisecond), report.Timing.Max.Round(time.Millisecond))
}
