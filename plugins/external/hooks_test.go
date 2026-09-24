package external_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
			names := []string{external.HookToolCallStart}
			if os.Getenv("PONS_EXTERNAL_HOOK_ALL") == "1" {
				names = []string{
					external.HookAgentStart, external.HookAgentEnd, external.HookAgentError,
					external.HookAgentTurnStart, external.HookAssistantResponse,
					external.HookAgentTurnEnd, external.HookAgentTurnError,
					external.HookToolCallStart, external.HookToolCallEnd,
					external.HookToolCallError, external.HookToolCallDenied,
					external.HookApprovalRequest, external.HookApprovalResolved,
				}
			}
			config, _ := json.Marshal(external.HookProviderConfiguration{Hooks: names})
			result, _ := json.Marshal(external.InitializeResult{
				Plugin:       external.PluginInfo{Name: "hook.test", Version: "1.0.0"},
				Capabilities: []external.Capability{{Type: external.CapabilityHookProvider, Version: 1, Configuration: config}},
			})
			_ = encoder.Encode(external.RPCResponse{JSONRPC: "2.0", ID: request.ID, Result: result})
		case external.MethodHook:
			var params external.HookCallParams
			_ = json.Unmarshal(request.Params, &params)
			value := json.RawMessage(`{}`)
			switch params.Hook {
			case external.HookToolCallStart:
				if os.Getenv("PONS_EXTERNAL_HOOK_ALL") != "1" {
					value = json.RawMessage(`{"Decision":{"Action":"ask","ReasonCode":"external_review"}}`)
				}
			case external.HookAgentEnd:
				if os.Getenv("PONS_EXTERNAL_HOOK_ALL") == "1" {
					var event pons.AgentEndEvent
					_ = json.Unmarshal(params.Event, &event)
					event.Result.Answer = "external end"
					value, _ = json.Marshal(struct{ Result pons.RunResult }{event.Result})
				}
			case external.HookToolCallEnd:
				if os.Getenv("PONS_EXTERNAL_HOOK_ALL") == "1" {
					var event pons.ToolCallEndEvent
					_ = json.Unmarshal(params.Event, &event)
					event.Result.Output = "external result"
					value, _ = json.Marshal(struct{ Result protocol.ToolResult }{event.Result})
				}
			}
			result, _ := json.Marshal(external.HookCallResult{Patch: value})
			_ = encoder.Encode(external.RPCResponse{JSONRPC: "2.0", ID: request.ID, Result: result})
		case external.MethodShutdown:
			_ = encoder.Encode(external.RPCResponse{JSONRPC: "2.0", ID: request.ID, Result: json.RawMessage(`{}`)})
			return
		}
	}
}

func TestExternalHookProviderDecision(t *testing.T) {
	entrypoint, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "plugin.json")
	manifest := fmt.Sprintf(`{"manifest_version":1,"name":"hook.test","entrypoint":%q,"args":["-test.run=^TestExternalHookHelper$"],"runtime_protocol":1,"placement":"host"}`, entrypoint)
	if err := os.WriteFile(path, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	plugin, err := external.NewHooks(path, external.HostConfig{Env: []string{"PONS_EXTERNAL_HOOK_HELPER=1"}, CallTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer plugin.Close()
	if err := plugin.Host().Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	request := pons.ToolCallStartEvent{Action: protocol.Action{ID: "real-id", Kind: "run", Args: json.RawMessage(`{}`)}}
	var response struct{ Decision pons.ActionDecision }
	if err := plugin.Host().CallHook(context.Background(), external.HookToolCallStart, request, &response); err != nil {
		t.Fatal(err)
	}
	if response.Decision.Action != pons.DispositionAsk || response.Decision.ReasonCode != "external_review" {
		t.Fatalf("hook decision: %+v", response)
	}
	if request.Action.ID != "real-id" {
		t.Fatal("hook mutated the host-owned request")
	}
	core := pons.New()
	if err := plugin.Setup(core); err != nil {
		t.Fatal(err)
	}
	called := false
	if err := core.AddTool("run", pons.ToolDef{Handler: func(context.Context, protocol.Action) (protocol.ToolResult, error) {
		called = true
		return protocol.ToolResult{OK: true}, nil
	}}); err != nil {
		t.Fatal(err)
	}
	if err := core.Use(scripted.New(scripted.Step{Actions: []protocol.Action{request.Action}})); err != nil {
		t.Fatal(err)
	}
	result, err := core.Run(context.Background(), "run it")
	if err != nil {
		t.Fatal(err)
	}
	if called || len(result.History) == 0 || result.History[0].Results[0].ActionID != "real-id" ||
		result.History[0].Results[0].OK {
		t.Fatalf("external policy was not applied: called=%v result=%+v", called, result)
	}
}

func TestExternalHookProviderAllEvents(t *testing.T) {
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
		Env: []string{"PONS_EXTERNAL_HOOK_HELPER=1", "PONS_EXTERNAL_HOOK_ALL=1"}, CallTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer plugin.Close()
	core := pons.New()
	if err := plugin.Setup(core); err != nil {
		t.Fatal(err)
	}
	if got := len(plugin.Host().HookNames()); got != 13 {
		t.Fatalf("registered %d hooks, want 13", got)
	}
	called := false
	if err := core.AddTool("run", pons.ToolDef{Handler: func(context.Context, protocol.Action) (protocol.ToolResult, error) {
		called = true
		return protocol.ToolResult{OK: true}, nil
	}}); err != nil {
		t.Fatal(err)
	}
	if err := core.Use(scripted.New(scripted.Step{Actions: []protocol.Action{{ID: "real-id", Kind: "run", Args: json.RawMessage(`{}`)}}})); err != nil {
		t.Fatal(err)
	}
	result, err := core.Run(context.Background(), "run it")
	if err != nil {
		t.Fatal(err)
	}
	if !called || result.Answer != "external end" ||
		len(result.History) == 0 || result.History[0].Results[0].Output != "external result" {
		t.Fatalf("external hooks not applied: called=%v result=%+v", called, result)
	}
}
