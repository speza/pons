// Command external-policy is a minimal separately installed host hook plugin.
// It asks for approval on shell calls and allows other tools.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"

	"github.com/samperrin/pons"
	"github.com/samperrin/pons/plugins/external"
)

func main() {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var request external.RPCRequest
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
		var result any
		switch request.Method {
		case external.MethodInitialize:
			config, _ := json.Marshal(external.HookProviderConfiguration{Hooks: []string{external.HookToolCallStart}})
			result = external.InitializeResult{
				Plugin:       external.PluginInfo{Name: "example.policy", Version: "1.0.0"},
				Capabilities: []external.Capability{{Type: external.CapabilityHookProvider, Version: external.HookProviderVersion, Configuration: config}},
			}
		case external.MethodHook:
			var params external.HookCallParams
			if err := json.Unmarshal(request.Params, &params); err != nil || params.Hook != external.HookToolCallStart {
				result = external.HookCallResult{Decision: json.RawMessage(`{"Action":"ask","ReasonCode":"invalid_hook_request"}`)}
				break
			}
			var event pons.ToolCallStartEvent
			if err := json.Unmarshal(params.Event, &event); err != nil {
				result = external.HookCallResult{Decision: json.RawMessage(`{"Action":"ask","ReasonCode":"invalid_tool_call"}`)}
				break
			}
			decision := pons.ActionDecision{Action: pons.DispositionAllow}
			if event.Action.Kind == "bash" || event.Action.Kind == "shell" {
				decision = pons.ActionDecision{Action: pons.DispositionAsk, ReasonCode: "shell_review"}
			}
			encoded, _ := json.Marshal(decision)
			result = external.HookCallResult{Decision: encoded}
		case external.MethodShutdown:
			_ = encoder.Encode(external.RPCResponse{JSONRPC: "2.0", ID: request.ID, Result: json.RawMessage(`{}`)})
			return
		default:
			_ = encoder.Encode(external.RPCResponse{JSONRPC: "2.0", ID: request.ID, Error: &external.RPCError{Code: external.RPCMethodNotFound, Message: "method not found"}})
			continue
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
		if err := encoder.Encode(external.RPCResponse{JSONRPC: "2.0", ID: request.ID, Result: encoded}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
}
