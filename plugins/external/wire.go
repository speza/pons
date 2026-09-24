package external

import (
	"encoding/json"

	"github.com/samperrin/pons/protocol"
)

const (
	CapabilityToolProvider = "tool_provider"
	ToolProviderVersion    = 1
	CapabilityHookProvider = "hook_provider"
	HookProviderVersion    = 1

	MethodInitialize = "plugin/initialize"
	MethodHealth     = "plugin/health"
	MethodShutdown   = "plugin/shutdown"
	MethodExecute    = "tools/execute"
	MethodHook       = "hooks/call"
	MethodCancel     = "$/cancelRequest"
)

// RPCRequest and RPCResponse are the small common JSON-RPC 2.0 envelope. V1
// uses string IDs so correlation remains straightforward in implementations
// that do not have a JavaScript-style number type.
type RPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      string          `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type RPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      string          `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

const (
	RPCParseError     = -32700
	RPCInvalidRequest = -32600
	RPCMethodNotFound = -32601
	RPCInvalidParams  = -32602
	RPCInternalError  = -32603
	RPCPluginFailure  = -32001
	RPCTooLarge       = -32002
)

// InitializeParams is sent once per process lifetime.
type InitializeParams struct {
	RuntimeProtocol       int              `json:"runtime_protocol"`
	Host                  HostInfo         `json:"host"`
	SupportedCapabilities map[string][]int `json:"supported_capabilities"`
	Config                json.RawMessage  `json:"config,omitempty"`
}

type HostInfo struct {
	Name      string    `json:"name"`
	Version   string    `json:"version"`
	Placement Placement `json:"placement"`
	Workspace string    `json:"workspace,omitempty"`
}

type InitializeResult struct {
	Plugin       PluginInfo   `json:"plugin"`
	Capabilities []Capability `json:"capabilities"`
}

type PluginInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type Capability struct {
	Type          string          `json:"type"`
	Version       int             `json:"version"`
	Configuration json.RawMessage `json:"configuration,omitempty"`
}

// ToolProviderConfiguration is the normative v1 capability configuration.
// An omitted max_concurrency means serial; an explicit JSON zero means
// unbounded subject to host limits.
type ToolProviderConfiguration struct {
	MaxConcurrency int               `json:"max_concurrency,omitempty"`
	Tools          []ToolDescription `json:"tools"`
}

type ToolDescription struct {
	Kind        string          `json:"kind"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type ExecuteParams struct {
	Action protocol.Action `json:"action"`
}

// HookProviderConfiguration names the hook callbacks implemented by a host
// plugin. Each call receives one event and returns a patch to mutable fields.
type HookProviderConfiguration struct {
	Hooks []string `json:"hooks"`
}

type HookCallParams struct {
	Hook  string          `json:"hook"`
	Event json.RawMessage `json:"event"`
}

type HookCallResult struct {
	Patch json.RawMessage `json:"patch"`
}

type HealthResult struct {
	Status string `json:"status"`
}
