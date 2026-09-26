package external_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samperrin/pons"
	"github.com/samperrin/pons/plugins/brain/scripted"
	"github.com/samperrin/pons/plugins/external"
	"github.com/samperrin/pons/protocol"
)

func TestExternalHookHelper(t *testing.T) {
	if os.Getenv("PONS_EXTERNAL_HOOK_HELPER") != "1" {
		return
	}
	encoder := json.NewEncoder(os.Stdout)
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var request external.RPCRequest
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			return
		}
		switch request.Method {
		case external.MethodInitialize:
			mode := os.Getenv("PONS_EXTERNAL_HOOK_MODE")
			config := external.HookProviderConfiguration{Hooks: []string{external.HookToolCallStart}}
			switch mode {
			case "all":
				config.Hooks = []string{
					external.HookAgentStart, external.HookAgentTurnStart, external.HookAssistantResponse,
					external.HookToolCallStart, external.HookPermissionRequest, external.HookToolCallEnd,
					external.HookAgentTurnEnd, external.HookAgentEnd,
				}
			case "filtered":
				config.Tools = []string{"other"}
			case "approve":
				config.Hooks = []string{external.HookToolCallStart, external.HookPermissionRequest}
			case "malformed":
				config.Hooks = []string{external.HookToolCallEnd}
			}
			encoded, _ := json.Marshal(config)
			result, _ := json.Marshal(external.InitializeResult{
				Plugin:       external.PluginInfo{Name: "hook.test", Version: "1.0.0"},
				Capabilities: []external.Capability{{Type: external.CapabilityHookProvider, Version: 1, Configuration: encoded}},
			})
			_ = encoder.Encode(external.RPCResponse{JSONRPC: "2.0", ID: request.ID, Result: result})
		case external.MethodHook:
			var params external.HookCallParams
			_ = json.Unmarshal(request.Params, &params)
			value := json.RawMessage(`{}`)
			switch os.Getenv("PONS_EXTERNAL_HOOK_MODE") {
			case "all":
				// Tool output is large; every input must stay bounded.
				if len(params.Event) > 64<<10 {
					_ = encoder.Encode(external.RPCResponse{JSONRPC: "2.0", ID: request.ID,
						Error: &external.RPCError{Code: -32000, Message: "unbounded " + params.Hook}})
					continue
				}
				switch params.Hook {
				case external.HookAgentStart:
					value = json.RawMessage(`{"additional_context":"from external","system_message":"external ready"}`)
				case external.HookToolCallEnd:
					value = json.RawMessage(`{"result":{"ok":true,"output":"external result"}}`)
				}
			case "approve":
				value = json.RawMessage(`{"permission":"ask","reason":"external_review"}`)
				if params.Hook == external.HookPermissionRequest {
					value = json.RawMessage(`{"permission":"allow"}`)
				}
			case "malformed":
				value = json.RawMessage(`{"bogus":true}`)
			default:
				value = json.RawMessage(`{"permission":"ask","reason":"external_review"}`)
			}
			result, _ := json.Marshal(external.HookCallResult{Patch: value})
			_ = encoder.Encode(external.RPCResponse{JSONRPC: "2.0", ID: request.ID, Result: result})
		case external.MethodShutdown:
			_ = encoder.Encode(external.RPCResponse{JSONRPC: "2.0", ID: request.ID, Result: json.RawMessage(`{}`)})
			return
		}
	}
}

func startHookPlugin(t *testing.T, mode string) *external.HookPlugin {
	t.Helper()
	entrypoint, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "plugin.json")
	manifest := fmt.Sprintf(`{"manifest_version":1,"name":"hook.test","entrypoint":%q,"args":["-test.run=^TestExternalHookHelper$"],"runtime_protocol":1,"placement":"host"}`, entrypoint)
	if err := os.WriteFile(path, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	plugin, err := external.NewHooks(path, external.HostConfig{
		Env: []string{"PONS_EXTERNAL_HOOK_HELPER=1", "PONS_EXTERNAL_HOOK_MODE=" + mode}, CallTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = plugin.Close() })
	return plugin
}

type hookRun struct {
	result pons.RunResult
	events []pons.Event
	ran    bool
}

func runWithHookPlugin(t *testing.T, plugin *external.HookPlugin, output string) hookRun {
	t.Helper()
	core := pons.New()
	if err := plugin.Setup(core); err != nil {
		t.Fatal(err)
	}
	var run hookRun
	if err := core.AddTool("run", pons.ToolDef{Handler: func(context.Context, protocol.Action) (protocol.ToolResult, error) {
		run.ran = true
		return protocol.ToolResult{OK: true, Output: output}, nil
	}}); err != nil {
		t.Fatal(err)
	}
	if err := core.Use(scripted.New(scripted.Step{Actions: []protocol.Action{{ID: "real-id", Kind: "run", Args: json.RawMessage(`{}`)}}})); err != nil {
		t.Fatal(err)
	}
	core.OnEvent(func(event pons.Event) { run.events = append(run.events, event) })
	result, err := core.Run(context.Background(), "run it")
	if err != nil {
		t.Fatal(err)
	}
	run.result = result
	return run
}

func eventTexts(events []pons.Event, eventType pons.EventType) []string {
	var texts []string
	for _, event := range events {
		if event.Type == eventType {
			texts = append(texts, event.Text)
		}
	}
	return texts
}

func TestExternalHookProviderDecision(t *testing.T) {
	plugin := startHookPlugin(t, "decision")
	run := runWithHookPlugin(t, plugin, "real")
	if run.ran || len(run.result.History) == 0 || run.result.History[0].Results[0].ActionID != "real-id" ||
		!strings.Contains(run.result.History[0].Results[0].Error, "approval_required") {
		t.Fatalf("external policy was not applied: %+v", run)
	}
}

func TestExternalHookProviderToolFilter(t *testing.T) {
	plugin := startHookPlugin(t, "filtered")
	if run := runWithHookPlugin(t, plugin, "real"); !run.ran {
		t.Fatalf("filtered hook applied to an unlisted tool: %+v", run.result)
	}
}

func TestExternalHookProviderCanApprove(t *testing.T) {
	plugin := startHookPlugin(t, "approve")
	if run := runWithHookPlugin(t, plugin, "real"); !run.ran {
		t.Fatalf("external permission hook did not approve: %+v", run.result)
	}
}

func TestExternalHookProviderAllHooksWithBoundedInputs(t *testing.T) {
	plugin := startHookPlugin(t, "all")
	run := runWithHookPlugin(t, plugin, strings.Repeat("x", 256<<10))
	if got := len(plugin.Host().HookNames()); got != 8 {
		t.Fatalf("registered %d hooks, want 8", got)
	}
	if errs := eventTexts(run.events, pons.EventHookError); len(errs) != 0 {
		t.Fatalf("hook errors: %v", errs)
	}
	if !run.ran || run.result.History[0].Results[0].Output != "external result" {
		t.Fatalf("external hooks not applied: %+v", run.result)
	}
	if messages := eventTexts(run.events, pons.EventSystemMessage); len(messages) != 1 || messages[0] != "external ready" {
		t.Fatalf("system messages = %v", messages)
	}
}

func TestExternalHookMalformedOutputIsReported(t *testing.T) {
	plugin := startHookPlugin(t, "malformed")
	run := runWithHookPlugin(t, plugin, "real")
	errs := eventTexts(run.events, pons.EventHookError)
	if len(errs) != 1 || !strings.Contains(errs[0], "malformed hook patch") ||
		!strings.Contains(run.result.History[0].Results[0].Error, "withheld") {
		t.Fatalf("errors = %v, result = %+v", errs, run.result.History[0].Results[0])
	}
}
