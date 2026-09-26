// Package hookeval runs labeled hook cases against a composed Core without
// executing tools. A case pairs a hook's Input with the Output fields it
// expects, in the same JSON as the hook_provider/v1 wire format, so plugin
// authors in any language can ship evals as data.
package hookeval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"time"

	"github.com/samperrin/pons"
)

// HookToolCallStart is the hook the runner supports; others are rejected
// until Core has a dry-run entry point for them.
const HookToolCallStart = "on_tool_call_start"

// Case is one labeled hook invocation. Expect is matched partially: every
// field it names must equal the actual output's, recursively for objects.
// ReferenceRisk is an optional human label for a classifier's raw risk; it is
// reported but does not decide whether the case passes.
type Case struct {
	ID            string          `json:"id"`
	Hook          string          `json:"hook"`
	Input         json.RawMessage `json:"input"`
	Expect        json.RawMessage `json:"expect"`
	ReferenceRisk string          `json:"reference_risk,omitempty"`

	input pons.ToolCallStartInput
}

// LoadCases decodes a strict, nonempty JSON array of cases.
func LoadCases(reader io.Reader) ([]Case, error) {
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	var cases []Case
	if err := decoder.Decode(&cases); err != nil {
		return nil, fmt.Errorf("decode eval cases: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("eval cases: trailing data")
	}
	if len(cases) == 0 {
		return nil, errors.New("eval cases must not be empty")
	}
	for i := range cases {
		if err := cases[i].validate(); err != nil {
			return nil, fmt.Errorf("eval case %d (%q): %w", i, cases[i].ID, err)
		}
	}
	return cases, nil
}

func (c *Case) validate() error {
	if c.ID == "" {
		return errors.New("id is required")
	}
	if c.Hook != HookToolCallStart {
		return fmt.Errorf("hook %q is not supported; use %s", c.Hook, HookToolCallStart)
	}
	decoder := json.NewDecoder(bytes.NewReader(c.Input))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&c.input); err != nil {
		return fmt.Errorf("input: %w", err)
	}
	if c.input.Action.ID == "" || c.input.Action.Kind == "" {
		return errors.New("input action id and kind are required")
	}
	if args := bytes.TrimSpace(c.input.Action.Args); len(args) == 0 || args[0] != '{' || !json.Valid(args) {
		return errors.New("input action args must be a JSON object")
	}
	var expect map[string]any
	if err := json.Unmarshal(c.Expect, &expect); err != nil || len(expect) == 0 {
		return errors.New("expect must be a nonempty object")
	}
	if c.ReferenceRisk != "" && c.ReferenceRisk != "safe" && c.ReferenceRisk != "review" {
		return errors.New("reference_risk must be safe or review")
	}
	return nil
}

// LoadCaseFiles loads cases from files and directories; a directory
// contributes its *.json files in name order. IDs must be unique overall.
func LoadCaseFiles(paths ...string) ([]Case, error) {
	var files []string
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			files = append(files, path)
			continue
		}
		matches, err := filepath.Glob(filepath.Join(path, "*.json"))
		if err != nil {
			return nil, err
		}
		sort.Strings(matches)
		files = append(files, matches...)
	}
	if len(files) == 0 {
		return nil, errors.New("no eval case files found")
	}

	var cases []Case
	ids := make(map[string]string)
	for _, file := range files {
		loaded, err := loadCaseFile(file)
		if err != nil {
			return nil, err
		}
		for _, item := range loaded {
			if other, exists := ids[item.ID]; exists {
				return nil, fmt.Errorf("eval case %q in %s duplicates one in %s", item.ID, file, other)
			}
			ids[item.ID] = file
		}
		cases = append(cases, loaded...)
	}
	return cases, nil
}

func loadCaseFile(path string) ([]Case, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	cases, err := LoadCases(file)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cases, nil
}

// Checker is the dry-run surface of a composed Core.
type Checker interface {
	CheckToolCall(context.Context, pons.ToolCallStartInput) (pons.ToolCallCheck, error)
}

type Outcome struct {
	ID                 string          `json:"id"`
	Passed             bool            `json:"passed"`
	Output             json.RawMessage `json:"output"`
	Mismatches         []string        `json:"mismatches,omitempty"`
	HookErrors         []string        `json:"hook_errors,omitempty"`
	ExpectedPermission pons.Permission `json:"expected_permission,omitempty"`
	Permission         pons.Permission `json:"permission"`
	Reason             string          `json:"reason,omitempty"`
	ReferenceRisk      string          `json:"reference_risk,omitempty"`
	Risk               string          `json:"risk,omitempty"`
	Confidence         float64         `json:"confidence,omitempty"`
	Duration           time.Duration   `json:"duration_ns"`
}

// Report summarizes a run. FalseAllows counts cases that expected ask or deny
// but were allowed; FalseAsks counts expected allows that were not.
// ReferenceMismatches compare a classifier's raw risk with reference_risk.
type Report struct {
	Total                int       `json:"total"`
	Passed               int       `json:"passed"`
	FalseAllows          int       `json:"false_allows"`
	FalseAsks            int       `json:"false_asks"`
	HookErrors           int       `json:"hook_errors"`
	ReferenceMismatches  int       `json:"reference_mismatches"`
	FalseSafeAssessments int       `json:"false_safe_assessments"`
	Timing               Timing    `json:"timing"`
	Outcomes             []Outcome `json:"outcomes"`
}

// Timing measures sequential hook decisions, excluding plugin setup.
// Durations are encoded as nanoseconds in JSON.
type Timing struct {
	Wall time.Duration `json:"wall_ns"`
	Mean time.Duration `json:"mean_ns"`
	P50  time.Duration `json:"p50_ns"`
	P95  time.Duration `json:"p95_ns"`
	Min  time.Duration `json:"min_ns"`
	Max  time.Duration `json:"max_ns"`
}

// Run evaluates cases in order. A case passes when its expected fields match
// and no hook failed, since a failed hook can only pass by accident. The
// callback, if any, runs as each case completes.
func Run(ctx context.Context, checker Checker, cases []Case, onOutcome func(Outcome)) (Report, error) {
	report := Report{Total: len(cases), Outcomes: make([]Outcome, 0, len(cases))}
	started := time.Now()
	durations := make([]time.Duration, 0, len(cases))
	for _, item := range cases {
		caseStarted := time.Now()
		check, err := checker.CheckToolCall(ctx, item.input)
		duration := time.Since(caseStarted)
		if err != nil {
			return report, fmt.Errorf("eval case %q: %w", item.ID, err)
		}

		outcome, err := score(item, check)
		if err != nil {
			return report, fmt.Errorf("eval case %q: %w", item.ID, err)
		}
		outcome.Duration = duration
		if outcome.Passed {
			report.Passed++
		}
		if len(outcome.HookErrors) > 0 {
			report.HookErrors++
		}
		if outcome.ExpectedPermission != "" {
			expectAllow, allowed := outcome.ExpectedPermission == pons.PermissionAllow, outcome.Permission == pons.PermissionAllow
			if !expectAllow && allowed {
				report.FalseAllows++
			}
			if expectAllow && !allowed {
				report.FalseAsks++
			}
		}
		if item.ReferenceRisk != "" && outcome.Risk != "" && outcome.Risk != item.ReferenceRisk {
			report.ReferenceMismatches++
			if item.ReferenceRisk == "review" && outcome.Risk == "safe" {
				report.FalseSafeAssessments++
			}
		}

		report.Outcomes = append(report.Outcomes, outcome)
		durations = append(durations, duration)
		if onOutcome != nil {
			onOutcome(outcome)
		}
	}
	report.Timing = timing(time.Since(started), durations)
	return report, nil
}

// score builds the case's actual Output, as a hook_provider plugin would see
// it, and matches it against the expectation.
func score(item Case, check pons.ToolCallCheck) (Outcome, error) {
	output := pons.ToolCallStartOutput{
		HookOutput: check.Output,
		Permission: check.Decision.Permission,
		Reason:     check.Decision.Reason,
		Assessment: check.Decision.Assessment,
	}
	if !bytes.Equal(check.Action.Args, item.input.Action.Args) {
		output.UpdatedInput = check.Action.Args
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		return Outcome{}, err
	}
	var actual, expect map[string]any
	if err := json.Unmarshal(encoded, &actual); err != nil {
		return Outcome{}, err
	}
	if err := json.Unmarshal(item.Expect, &expect); err != nil {
		return Outcome{}, err
	}

	outcome := Outcome{
		ID:            item.ID,
		Output:        encoded,
		Mismatches:    match("", expect, actual),
		Permission:    output.Permission,
		Reason:        output.Reason,
		ReferenceRisk: item.ReferenceRisk,
	}
	if permission, ok := expect["permission"].(string); ok {
		outcome.ExpectedPermission = pons.Permission(permission)
	}
	if output.Assessment != nil {
		outcome.Risk, outcome.Confidence = output.Assessment.Risk, output.Assessment.Confidence
	}
	for _, hookErr := range check.HookErrors {
		outcome.HookErrors = append(outcome.HookErrors, hookErr.Error())
	}
	outcome.Passed = len(outcome.Mismatches) == 0 && len(outcome.HookErrors) == 0
	return outcome, nil
}

// match reports every expected field that the actual value does not equal.
// Objects match partially; any other value must be equal.
func match(path string, expect, actual any) []string {
	expectObject, ok := expect.(map[string]any)
	if !ok {
		if reflect.DeepEqual(expect, actual) {
			return nil
		}
		return []string{fmt.Sprintf("%s: expected %s, got %s", displayPath(path), displayJSON(expect), displayJSON(actual))}
	}
	actualObject, _ := actual.(map[string]any)
	var mismatches []string
	keys := make([]string, 0, len(expectObject))
	for key := range expectObject {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		mismatches = append(mismatches, match(path+"."+key, expectObject[key], actualObject[key])...)
	}
	return mismatches
}

func displayPath(path string) string {
	if path == "" {
		return "output"
	}
	return path[1:]
}

func displayJSON(value any) string {
	if value == nil {
		return "nothing"
	}
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func timing(wall time.Duration, durations []time.Duration) Timing {
	if len(durations) == 0 {
		return Timing{Wall: wall}
	}
	sorted := slices.Clone(durations)
	slices.Sort(sorted)
	var sum time.Duration
	for _, duration := range sorted {
		sum += duration
	}
	return Timing{
		Wall: wall,
		Mean: sum / time.Duration(len(sorted)),
		P50:  sorted[(len(sorted)-1)/2],
		P95:  sorted[int(math.Ceil(float64(len(sorted))*0.95))-1],
		Min:  sorted[0],
		Max:  sorted[len(sorted)-1],
	}
}
