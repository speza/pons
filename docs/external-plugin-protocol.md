# Pons external plugin protocol

**Status:** Draft
**Protocol:** `pons.plugin.runtime/1`
**Related:** ADR-0007

This document specifies the common runtime used by independently built pons
plugins and the normative `tool_provider/v1` capability. Other capability
names are reserved only as design direction; they gain wire contracts in later
specifications.

## 1. Roles and topology

A **runtime host** is either the pons control process or a hands runtime. It
launches a **plugin process** and adapts the plugin's advertised capabilities
to pons interfaces.

```text
control host                         hands host / sandbox
  brain provider                       tool provider
  session store                        execution policy
  event sink
```

One process connection has exactly one host and one plugin. A plugin may
advertise multiple capabilities valid in that host's placement.

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
  "runtime_protocol": 1
}
```

Required fields:

- `manifest_version`: manifest schema version;
- `name`: stable, reverse-DNS-style plugin identifier;
- `entrypoint`: executable or interpreter path;
- `runtime_protocol`: requested major runtime protocol.

Hosts reject unknown manifest fields so typos cannot silently change launch
policy. Optional launch arguments are literal strings. Secret values do not belong in
the manifest. The host controls inherited environment, credentials, resource limits, and
placement. The process working directory is the manifest directory so
interpreted plugins can find packaged assets. A hands workspace is passed
explicitly during initialization and must not be inferred from process cwd.

An interpreted TypeScript distribution could instead use:

```json
{
  "manifest_version": 1,
  "name": "acme.github",
  "entrypoint": "/usr/bin/env",
  "args": ["node", "dist/index.js"],
  "runtime_protocol": 1
}
```

The default host environment is empty so credentials do not cross into hands.
An embedding host supplies only a safe runtime path, for example
`HostConfig{Path: "/usr/local/bin:/usr/bin:/bin"}`; the CLI equivalent is
`pons --plugin plugin.json --plugin-path /usr/local/bin:/usr/bin:/bin`.
This supplies `PATH` without inheriting the rest of the host environment.

Hosts must not automatically execute a project-local manifest merely because
it exists.

## 3. Framing

- Transport is stdin/stdout.
- Encoding is UTF-8 JSON-RPC 2.0.
- Each physical line is exactly one complete JSON-RPC message (NDJSON).
- JSON messages must not contain literal unescaped newlines.
- Stdout is protocol-only. Human and structured logs go to stderr.
- Request and response messages may be interleaved.
- Responses may arrive in an order different from requests.
- The host imposes configurable maximum frame and stderr sizes.

A protocol implementation must continuously read while calls are in flight so
that a full pipe cannot deadlock the peer.

## 4. Lifecycle

```text
host starts process
host -> initialize
host <- metadata and capabilities
host activates supported capabilities
host <-> capability calls and notifications
host -> shutdown
plugin exits
```

No capability call may be sent before successful initialization. There is one
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
    }
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
capability declarations, unsupported versions, capabilities invalid for the
configured placement, and duplicate tool/action kinds. Capability
configuration is capability-specific.

The initialization request contains no secrets by default. Credentials are
provided only through explicit deployment policy, such as scoped environment
variables or a host-side broker.

### 4.2 Readiness

A successful initialize response means the plugin is ready. Plugins must
complete required startup work before responding. There is no separate ready
notification in version 1.

### 4.3 Health and cancellation

A host may call `plugin/health` after initialization. A healthy tool provider
responds with a successful result such as `{"status":"ok"}`; health is an
optional diagnostic call and does not add a capability.

The host may send the JSON-RPC notification convention used by LSP:

```json
{
  "jsonrpc": "2.0",
  "method": "$/cancelRequest",
  "params": {"id": "call-17"}
}
```

A plugin should promptly cancel the associated work. The host still owns the
deadline and may kill an unresponsive process. A response racing with
cancellation is valid; the host accepts at most one terminal outcome.

### 4.4 Shutdown

The host requests `plugin/shutdown`, waits for its response, closes stdin, and
allows a bounded grace period for process exit. After the grace period it
terminates the process. Unexpected EOF or process exit fails all pending calls.
Plugins cannot be reattached in runtime protocol 1.

## 5. Request and error semantics

JSON-RPC `error` means the request could not be processed as a protocol call:
unknown method, invalid parameters, incompatible state, or internal plugin
failure before a domain result existed.

Expected domain failures remain successful RPC responses. For example, a tool
that runs a command exiting 1 returns a `ToolResult`; it does not return a
JSON-RPC error. This preserves the existing distinction between observations
and harness failures.

The host validates correlation fields and does not trust a plugin-supplied
action ID, capability identity, or result kind to route a response.

Unknown JSON object fields should be ignored unless a capability specification
marks the object closed. This permits additive minor evolution. Major
incompatible changes use a new integer capability or runtime version.

## 6. Concurrency

Every request has a unique string ID. A plugin advertises `max_concurrency`
per stateful capability:

- absent or `1`: host serializes calls to that capability;
- greater than `1`: host may issue that many concurrent calls;
- `0`: concurrency is unbounded, subject to host limits.

The host may independently apply a lower limit. Calls to different
capabilities may overlap unless their specifications say otherwise.

## 7. Capability contracts

### 7.1 Tool provider (normative)

Capability identifier: `tool_provider`, version `1`.

Placement: hands.

Initialization configuration contains model-facing tool descriptions. Every
tool has a non-empty action `kind`, a `description`, and an `input_schema`.
The schema root must be an object. Version 1 supports the interoperable JSON
Schema subset used by model tool APIs: `type`, `properties`, `required`,
`description`, `enum`, `items`, nested objects, and
`additionalProperties:false`. Unsupported keywords cause composition to fail
rather than being silently discarded.

Action arguments are a JSON object carried without string coercion. At the Go
wire boundary they are `json.RawMessage`; a producing or consuming plugin
decodes them into its own typed structure. Consequently booleans, numbers,
arrays, and nested objects retain their JSON types.

Execution method:

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

The response result is a `protocol.ToolResult`. The hands host enforces the
deadline and overwrites both `action_id` and `kind` from the invoked Action;
plugin output cannot redirect correlation or forge payload namespacing. Plugin
death, timeout, or malformed output becomes an unsuccessful ToolResult so the
brain can observe the unavailable capability when safe.

### 7.2 Anticipated capabilities (non-normative)

A later specification is expected to define `brain_provider` from the existing
`ControlPort`, including tool-catalog configuration, planning, interpretation,
and fatal failure semantics. Session stores, event sinks, durable turn
recorders, and execution policy are possible capability types, but runtime
protocol 1 assigns them no methods or payloads. Implementations must not claim
those capability names until their contracts are accepted.

## 8. Discovery and routing

The runtime responsible for a placement loads configured manifests. A remote
hands runtime aggregates tool descriptions from its local providers and sends
the catalog to the controller as part of the hands transport handshake. The
controller registers proxy handlers that forward Actions to that hands
runtime; it never launches the remote hands plugin locally.

```text
Core
  -> remote tool proxy
    -> hands transport
      -> hands registry
        -> external plugin RPC
```

Action kind conflicts fail composition. Selection is never last-wins.
Plugin name, version, executable identity, capability version, and action kind
should be included in audit metadata.

## 9. Security requirements

- Treat manifests and executables as code, not data.
- Require explicit activation or prior approval.
- Do not invoke entrypoints through a shell.
- Do not trust plugin schemas, logs, results, or payload sizes.
- Put hands plugins inside the same OS sandbox as the hands runtime.
- Do not mistake a subprocess boundary for a permissions boundary.
- Keep credentials out of hands by default; use scoped credentials or brokers.
- Apply deadlines and size limits in the host.
- A plugin cannot request or elevate its placement.

## 10. SDK and conformance requirements

The protocol repository should provide fixture-driven tests that can launch any
plugin executable and verify:

- initialization and version rejection;
- stdout framing and stderr logging;
- out-of-order concurrent responses;
- cancellation and shutdown;
- malformed and oversized output handling;
- capability schema validation;
- process death with pending requests.

The Go SDK is covered by the hermetic host black-box tests in
`plugins/external`; the dependency-free TypeScript SDK in
`plugins/external/sdk/typescript` is exercised by
`examples/external-echo-ts` (including initialize, typed execution, and
shutdown). A shared fixture runner for both languages remains a follow-up.
Both SDKs expose cancellation to handlers and must not print application logs
to stdout.

## 11. Open follow-ups

1. Define the controller-to-hands discovery handshake in the ADR-0004
   transport specification.
2. Decide manifest checksum/signature fields and user approval storage.
3. Define restart policy for stateless versus stateful capabilities.
4. Publish machine-readable schemas for runtime and `tool_provider/v1`
   messages.
5. Specify `brain_provider` separately after exercising the tool runtime.
