package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/samperrin/pons"
	"github.com/samperrin/pons/plugins/external"
	"github.com/samperrin/pons/plugins/hostconfig"
)

// TestEvalHookPluginHelper is the installed hook plugin used by the eval
// tests: it asks about bash and has no objection to anything else.
func TestEvalHookPluginHelper(t *testing.T) {
	if !slices.Contains(os.Args, "eval-hook-helper") {
		return
	}
	encoder := json.NewEncoder(os.Stdout)
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		var request external.RPCRequest
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			return
		}
		var result any
		switch request.Method {
		case external.MethodInitialize:
			config, _ := json.Marshal(external.HookProviderConfiguration{Hooks: []string{external.HookToolCallStart}})
			result = external.InitializeResult{
				Plugin:       external.PluginInfo{Name: "test.policy", Version: "1.0.0"},
				Capabilities: []external.Capability{{Type: external.CapabilityHookProvider, Version: 1, Configuration: config}},
			}
		case external.MethodHook:
			var params external.HookCallParams
			var input pons.ToolCallStartInput
			_ = json.Unmarshal(request.Params, &params)
			_ = json.Unmarshal(params.Event, &input)
			var output pons.ToolCallStartOutput
			if input.Action.Kind == "bash" {
				output = pons.ToolCallStartOutput{Permission: pons.PermissionAsk, Reason: "shell_review"}
			}
			patch, _ := json.Marshal(output)
			result = external.HookCallResult{Patch: patch}
		case external.MethodShutdown:
			_ = encoder.Encode(external.RPCResponse{JSONRPC: "2.0", ID: request.ID, Result: json.RawMessage(`{}`)})
			return
		}
		encoded, _ := json.Marshal(result)
		_ = encoder.Encode(external.RPCResponse{JSONRPC: "2.0", ID: request.ID, Result: encoded})
	}
}

const evalCases = `[
	{"id":"shell","hook":"on_tool_call_start","input":{"action":{"id":"a","kind":"bash","args":{"command":"git push"}}},"expect":{"permission":"ask","reason":"shell_review"}},
	{"id":"read","hook":"on_tool_call_start","input":{"action":{"id":"b","kind":"read_file","args":{"path":"README.md"}}},"expect":{"permission":"allow"}}
]`

// installEvalPlugin installs the helper as test.policy, disabled, with its
// own evals directory, and points HOME at it.
func installEvalPlugin(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	executable, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, ".pons", "plugins", "test.policy")
	if err := os.MkdirAll(filepath.Join(dir, "evals"), 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := fmt.Sprintf(`{"manifest_version":1,"name":"test.policy","entrypoint":%q,"args":["-test.run=^TestEvalHookPluginHelper$","eval-hook-helper"],"runtime_protocol":1,"placement":"host"}`, executable)
	files := map[string]string{
		filepath.Join(dir, "plugin.json"):           manifest,
		filepath.Join(dir, "evals", "policy.json"):  evalCases,
		filepath.Join(home, ".pons", "config.json"): `{"plugins":[{"id":"test.policy","version":"1.0.0","enabled":false,"config":{}}]}`,
	}
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

func TestEvalRunsInstalledPluginCases(t *testing.T) {
	home := installEvalPlugin(t)
	var stdout, stderr bytes.Buffer
	if err := runEval(context.Background(), []string{"test.policy"}, &stdout, &stderr, home, hostconfig.NewRegistry()); err != nil {
		t.Fatalf("eval: %v\n%s%s", err, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "2/2 passed; false allows=0, false asks=0, hook errors=0") {
		t.Fatalf("output:\n%s", stdout.String())
	}
}

func TestEvalReportsFailuresAsJSON(t *testing.T) {
	home := installEvalPlugin(t)
	cases := filepath.Join(t.TempDir(), "wrong.json")
	wrong := `[{"id":"shell","hook":"on_tool_call_start","input":{"action":{"id":"a","kind":"bash","args":{}}},"expect":{"permission":"allow"}}]`
	if err := os.WriteFile(cases, []byte(wrong), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	err := runEval(context.Background(), []string{"-json", "-cases", cases, "test.policy"}, &stdout, &stderr, home, hostconfig.NewRegistry())
	if err == nil || !strings.Contains(err.Error(), "1 of 1 cases failed") {
		t.Fatalf("err = %v", err)
	}
	var report struct {
		FalseAsks int `json:"false_asks"`
		Outcomes  []struct {
			Mismatches []string `json:"mismatches"`
		} `json:"outcomes"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("report %q: %v", stdout.String(), err)
	}
	if report.FalseAsks != 1 || len(report.Outcomes) != 1 || len(report.Outcomes[0].Mismatches) != 1 {
		t.Fatalf("report = %+v", report)
	}
}

func TestEvalRejectsMisuse(t *testing.T) {
	home := installEvalPlugin(t)
	for name, args := range map[string][]string{
		"no plugin":             {},
		"unconfigured plugin":   {"missing.policy"},
		"threshold on a policy": {"-threshold", "0.8", "test.policy"},
		"unknown case":          {"-case", "nope", "test.policy"},
	} {
		var stdout, stderr bytes.Buffer
		if err := runEval(context.Background(), args, &stdout, &stderr, home, hostconfig.NewRegistry()); err == nil {
			t.Errorf("%s: accepted %v", name, args)
		}
	}
}
