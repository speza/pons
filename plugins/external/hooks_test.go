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
			config, _ := json.Marshal(external.HookProviderConfiguration{Hooks: []string{external.HookToolCallStart}})
			result, _ := json.Marshal(external.InitializeResult{
				Plugin:       external.PluginInfo{Name: "hook.test", Version: "1.0.0"},
				Capabilities: []external.Capability{{Type: external.CapabilityHookProvider, Version: 1, Configuration: config}},
			})
			_ = encoder.Encode(external.RPCResponse{JSONRPC: "2.0", ID: request.ID, Result: result})
		case external.MethodHook:
			var params external.HookCallParams
			_ = json.Unmarshal(request.Params, &params)
			value, _ := json.Marshal(pons.ActionDecision{Action: pons.DispositionAsk, ReasonCode: "external_review"})
			result, _ := json.Marshal(external.HookCallResult{Decision: value})
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
	var response pons.ActionDecision
	if err := plugin.Host().CallHook(context.Background(), external.HookToolCallStart, request, &response); err != nil {
		t.Fatal(err)
	}
	if response.Action != pons.DispositionAsk || response.ReasonCode != "external_review" {
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
