# Pons external plugin protocol

**Status:** Accepted
**Protocol:** `pons.plugin.runtime/1`
**Related:** ADR-0004, ADR-0007

This document is the normative specification for the external plugin runtime
shipped in `plugins/external`. Runtime protocol 1 supports `tool_provider/v1`
in hands placement and `hook_provider/v1` in host placement.

## 1. Roles and topology

A **host** is the pons process, or an embedding process using the external
adapter. It launches one **plugin process** and adapts its advertised tools or
hooks to the corresponding Core registration point.

A hands plugin receives no `Core`, brain, or conversation history. A host hook
plugin receives the event for its subscribed hook, which may include recent
source-labeled conversation context. Neither placement receives a `Core` handle
or inherited provider credentials.

## 2. Installation manifest

A manifest is UTF-8 JSON. Relative entrypoints are resolved from the manifest
directory. The host executes the entrypoint directly; no shell performs word
splitting or expansion.

```json
{
  "manifest_version": 1,
  "name": "acme.github",
  "entrypoint": "./pons-plugin-github",
  "args": [],
  "runtime_protocol": 1,
  "placement": "hands"
}
```

Required fields:

- `manifest_version`: manifest schema version;
- `name`: lower-case reverse-DNS-style plugin identifier;
- `entrypoint`: executable or interpreter path;
- `runtime_protocol`: requested runtime protocol major version.

Optional `config_version` declares the version of the plugin-owned config
contract (default `1.0.0`); the global plugin entry must name that version.

`args` is optional and contains literal strings. Hosts reject unknown manifest
fields. The host sets the child working directory to the manifest directory.
The hands workspace is passed explicitly during initialization and is never
inferred from the child working directory.
Omitted `placement` means `hands`. Set it to `host` for hook providers. An
installed plugin lives at `~/.pons/plugins/<name>/plugin.json` and is activated
by an enabled global config entry with the same ID; its `config` object is
delivered in `plugin/initialize`. Project config cannot activate it. The
`-plugin` flag can select an explicit manifest path.

An interpreted distribution can use an executable interpreter as the
entrypoint:

```json
{
  "manifest_version": 1,
  "name": "acme.github",
  "entrypoint": "/usr/bin/env",
  "args": ["node", "dist/index.js"],
  "runtime_protocol": 1
}
```

The default child environment is empty. A host may explicitly add variables or
replace `PATH` with a safe runtime path. Credentials are not inherited by
default. Hosts must not execute a project-local manifest merely because it
exists.

## 3. Framing

- Transport is the child's stdin and stdout.
- Encoding is UTF-8 JSON-RPC 2.0.
- Each physical line is exactly one complete JSON-RPC message.
- JSON strings contain escaped, not literal, newlines.
- Stdout is protocol-only; logs go to stderr.
- Requests and responses may be interleaved.
- Responses may arrive in a different order from requests.
- The host applies maximum frame and stderr sizes.

The plugin must keep reading while calls are in flight so a full pipe cannot
deadlock the host.

## 4. Lifecycle

```text
host starts process
host -> plugin/initialize
host <- plugin metadata and tool catalog
host <-> health, cancellation, and tool calls
host -> plugin/shutdown
plugin exits
```

No capability call is sent before successful initialization. There is one
initialization exchange per process lifetime.

### 4.1 Initialize

Request:

```json
{
  "jsonrpc": "2.0",
  "id": "init-1",
  "method": "plugin/initialize",
  "params": {
    "runtime_protocol": 1,
    "host": {
      "name": "pons",
      "version": "0.1.0",
      "placement": "hands",
      "workspace": "/workspace"
    },
    "supported_capabilities": {
      "tool_provider": [1]
    },
    "config": {}
  }
}
```

Response:

```json
{
  "jsonrpc": "2.0",
  "id": "init-1",
  "result": {
    "plugin": {
      "name": "acme.github",
      "version": "1.2.0"
    },
    "capabilities": [
      {
        "type": "tool_provider",
        "version": 1,
        "configuration": {
          "max_concurrency": 4,
          "tools": [
            {
              "kind": "github_create_issue",
              "description": "Create a GitHub issue",
              "input_schema": {
                "type": "object",
                "properties": {
                  "title": {"type": "string", "description": "Issue title"},
                  "labels": {"type": "array", "items": {"type": "string"}}
                },
                "required": ["title"],
                "additionalProperties": false
              }
            }
          ]
        }
      }
    ]
  }
}
```

The manifest name and handshake name must match. The host rejects duplicate
capabilities, unsupported versions, invalid placement, multiple tool-provider
declarations, duplicate tool kinds, and invalid tool schemas. The handshake
catalog is authoritative for the tools registered in the core.

Initialization contains no secrets by default. Explicit deployment policy is
required for any credential, such as a scoped environment variable or a
host-side broker.

### 4.2 Readiness

A successful initialize response means the plugin is ready. Startup work must
finish before the response. Runtime protocol 1 has no separate ready
notification.

### 4.3 Health and cancellation

After initialization the host may send:

```json
{
  "jsonrpc": "2.0",
  "id": "health-1",
  "method": "plugin/health"
}
```

A healthy tool provider returns a successful result such as
`{"status":"ok"}`. Health is a diagnostic method and is not a capability.

The host cancels work with the JSON-RPC notification convention used by LSP:

```json
{
  "jsonrpc": "2.0",
  "method": "$/cancelRequest",
  "params": {"id": "call-17"}
}
```

The plugin cancels the associated handler. The host still owns the deadline
and may terminate an unresponsive process. A response racing with
cancellation is valid; the host accepts only one terminal outcome for a call.

### 4.4 Shutdown

The host sends `plugin/shutdown`, waits for its response, closes stdin, and
allows a bounded grace period for process exit. It terminates the process when
the grace period expires. Unexpected EOF or process exit fails pending calls.
The host does not reattach to a process in runtime protocol 1.

## 5. Request and error semantics

A request and response use JSON-RPC 2.0 envelopes. Request IDs are non-empty
strings and identify one in-flight request. A response contains exactly one of
`result` or `error`.

A JSON-RPC `error` means the request could not be processed as a protocol call:
unknown method, invalid parameters, invalid lifecycle state, or a plugin
failure before a domain result existed.

Expected domain failures are successful RPC results. A command that exits 1,
for example, returns a `protocol.ToolResult`; it is not a JSON-RPC error.

The host validates response envelopes and correlation IDs. For tool calls it
overwrites `action_id` and `kind` in the returned `ToolResult` from the action
it routed. Plugin output cannot redirect a response or forge its payload
namespace.

Unknown object fields are ignored unless the object is explicitly closed by
this specification or its capability schema. Major incompatible changes use a
new protocol or capability version.

## 6. Concurrency

Every request has a unique string ID. A provider advertises
`max_concurrency` in its tool-provider configuration:

- omitted or `1`: the host serializes calls;
- greater than `1`: the host may issue that many concurrent calls; and
- explicit `0`: calls are unbounded by the provider, subject to host limits.

The host may apply a lower limit. Calls to different capabilities are not
relevant in runtime protocol 1 because only one capability is defined.

## 7. Tool provider

Capability identifier: `tool_provider`, version `1`. Placement: `hands`.

The capability configuration contains `tools`. Every tool has a non-empty
`kind`, a description, and an `input_schema`. The schema root is an object.
Version 1 supports these JSON Schema keywords:

- `type`, `properties`, `required`, and `description`;
- `enum`;
- `items`; and
- `additionalProperties:false`.

The host rejects unsupported keywords. Nested object and array schemas use the
same subset.

Action arguments are a JSON object and retain their JSON types. At the Go wire
boundary they are `json.RawMessage`; providers decode them into their own
input types. Missing or null arguments are normalized to `{}` by the Go host,
while a provider-facing execution request uses an object.

Execution request:

```json
{
  "jsonrpc": "2.0",
  "id": "call-17",
  "method": "tools/execute",
  "params": {
    "action": {
      "id": "tool-use-9",
      "kind": "github_create_issue",
      "args": {
        "title": "Crash on startup",
        "labels": ["bug", "startup"]
      }
    }
  }
}
```

The result is a `protocol.ToolResult`. A command-level failure is represented
inside that result. A plugin panic, process failure, timeout, malformed result,
or oversized result is converted to an unsuccessful `ToolResult` when the
host can safely continue; a broken protocol connection also fails pending
calls and is not reused.

## 8. Discovery and routing

The host loads only manifests explicitly supplied by the application or
installed by ID under `~/.pons/plugins`. It starts each child, validates its
initialization catalog, and registers tool handlers with `Core.AddTool` or
hook callbacks with `Core.AddHooks`. Action-kind conflicts fail composition;
selection is never last-wins.

The host keeps plugin name, version, capability version, executable, and action
kind available in tool metadata for presentation and audit. The core routes an
action to the proxy, and the proxy routes it to the child provider.

### 8.1 Host hook provider

A host manifest sets `"placement":"host"`. Initialization advertises only
`hook_provider: [1]`; the plugin returns a `hook_provider/v1` capability with
a nonempty `hooks` list. Supported names are `on_agent_start`, `on_agent_end`,
`on_agent_error`, `on_agent_turn_start`, `on_assistant_response`,
`on_agent_turn_end`, `on_agent_turn_error`, `on_tool_call_start`,
`on_tool_call_end`, `on_tool_call_error`, `on_tool_call_denied`,
`on_approval_request`, and `on_approval_resolved`. Duplicate or unknown names
and mixed hands/host capabilities are rejected. Only advertised hooks are called.

The host sends `hooks/call` with the `hook` name and the JSON event. Go event
field names retain their capitalized form. Error events expose `ErrorMessage`
as text instead of Go's `Err` interface. The plugin returns
`{"patch":{...}}`; an observer returns `{"patch":{}}`. Only the fields in
this table are accepted in a patch:

| Hook | Patch fields |
| --- | --- |
| `on_agent_start` | `Message` |
| `on_agent_end` | `Result` |
| `on_agent_error`, `on_agent_turn_error` | `ErrorMessage` (nonempty) |
| `on_agent_turn_start` | `Observation` (message and history only) |
| `on_assistant_response` | `Response` |
| `on_agent_turn_end` | none |
| `on_tool_call_start` | `Decision` |
| `on_tool_call_end`, `on_tool_call_error`, `on_tool_call_denied` | `Result` |
| `on_approval_request`, `on_approval_resolved` | `Decision` |

For example, tool preflight can return `{"patch":{"Decision":{"Action":
"ask","ReasonCode":"needs_review"}}}`. It may allow, ask, deny, or
replace arguments under Core's normal validation and replay rules. Host-owned
action identity, workspace, and resource projection cannot be patched. Failed
tool-start calls request approval and deny execution when no approval handler
is installed. Other hook failures follow Core's normal error collection and
run termination behavior. Host deadlines and frame/result limits apply.

## 9. Security requirements

- Treat manifests and executables as code, not data.
- Require explicit activation or prior approval.
- Execute entrypoints directly, never through a shell.
- Treat plugin schemas, logs, results, and payloads as untrusted.
- Apply deadlines, frame/result limits, stderr limits, pending-call limits, and
  process termination in the host.
- Keep credentials out of the child by default; use scoped credentials or a
  broker when necessary.
- Put an untrusted hands child inside the hands deployment's OS sandbox. Host
  hooks are explicitly installed control-plane code and receive policy context;
  operators must trust or separately isolate their executable.
- Do not allow a plugin to elevate its placement.

## 10. Implementations and tests

The Go host and SDK implement runtime protocol 1 and are covered by
hermetic tests in `plugins/external`. The dependency-free TypeScript SDK is
smoke-tested when Bun is available. The examples in
`examples/external-echo` and `examples/external-echo-ts` are explicit plugin
providers, not implicit registrations.

The tests cover manifest and schema validation, clean environments, typed
arguments, discovery, correlation normalization, concurrent calls,
cancellation, shutdown, process death, malformed frames, oversized frames,
and protocol write failures. Both SDKs keep application logs off stdout.

## 11. Capability scope

Runtime protocol 1 defines no external brain or session-store capability.
The host hook capability exposes the full typed Core hook set. A future hook
or a change to these patch contracts requires a new capability version.
