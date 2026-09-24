# ADR-0007: External tool plugins use a language-neutral persistent runtime

**Status:** Accepted
**Implementation:** Implemented
**Date:** 2025-09-16
**Related:** ADR-0001, ADR-0004, ADR-0005

## Context

The Go `Plugin.Setup(*Core)` API is a trusted in-process composition API. It
requires the plugin to be compiled into the application and cannot carry
arbitrary Go values across a process boundary.

pons also needs independently built hands capabilities. The extension
mechanism must preserve the brain/hands boundary and must not require plugin
authors to use Go.

## Decision

### 1. The external runtime supports placement-specific capabilities

An external plugin is an executable launched as a persistent child process.
It advertises typed tools during initialization and implements their execution
methods. Any language that can speak the protocol can implement the child.

Runtime protocol 1 supports `tool_provider/v1` for hands-side tool execution
and `hook_provider/v1` for explicitly installed host-side Core hooks. A connection advertises the capability appropriate to its manifest
placement. Brain and session providers remain unsupported.

The independently distributed unit is the plugin. It does not receive a
`Core`, arbitrary Go closures, or a brain. It receives initialization data and
well-defined action requests.

### 2. In-process Go plugins remain supported

`Plugin.Setup(*Core)` remains the trusted composition API for built-in tools,
brains, sessions, and embedding applications.

`plugins/external` is itself an in-process `Plugin`. Its setup starts one
external child, validates its catalog, and registers one host proxy per
advertised tool through `Core.AddTool`. The core loop therefore uses the same
`ToolPort` for built-in and external tools, and duplicate action kinds fail
composition rather than using last-wins behavior.

### 3. External capabilities have explicit placement

A tool provider is launched with hands placement. A hook provider is launched
with host placement and receives bounded policy event context, including recent
source-labeled messages. It can request a tool decision but receives no `Core`
handle or inherited credentials. The host rejects a capability in the wrong
placement.

A child process is a crash boundary and a resource-control boundary, not a
permissions boundary. The operator supplies any OS, container, or VM
isolation required by ADR-0004.

### 4. Transport is JSON-RPC 2.0 over NDJSON stdio

Each stdin or stdout line is one UTF-8 JSON-RPC 2.0 message. Stdout is
protocol-only; plugin logs go to stderr. String request IDs permit concurrent
calls, and responses may arrive out of order.

The runtime defines initialization, capability negotiation, health,
cancellation, and shutdown. The host owns deadlines, frame/result/stderr
limits, pending-call limits, and process termination. A malformed frame,
timeout, oversized result, or child failure is a checked host failure. Once an
action is being executed, the adapter returns such a failure as
`ToolResult{OK:false}` when the connection can safely continue rather than
crashing the control loop.

The host normalizes `ActionID` and `Kind` in every returned `ToolResult` from
the action it invoked. A provider cannot forge correlation or payload
namespacing.

### 5. Tool arguments are typed JSON described by JSON Schema

`protocol.Action.Args` is an opaque JSON object (`json.RawMessage` at the Go
wire boundary). The host and SDK preserve numbers, booleans, arrays, nested
objects, and null values; they do not coerce arguments into strings. Each tool
owns decoding and semantic validation of its arguments.

A tool advertises an object-rooted JSON Schema. Version 1 accepts the subset
used by the shipped providers: object properties, required fields, primitive
types, arrays, nested objects, descriptions, enums, `items`, and
`additionalProperties:false`. The host validates this schema during
initialization and rejects unsupported schema keywords instead of silently
discarding them.

### 6. Installation and activation are explicit

A manifest is a versioned JSON file containing a stable plugin name, direct
entrypoint, literal arguments, and runtime protocol version. Relative
entrypoints and the child working directory resolve from the manifest
directory. The host never invokes the entrypoint through a shell and rejects
unknown manifest fields.

The CLI activates manifests supplied with `--plugin`, listed under
`plugins.external.manifests`, or installed at
`~/.pons/plugins/<id>/plugin.json` and enabled by that ID in global config. It
never discovers project-local manifests implicitly. The child environment is empty by default.
A caller may explicitly provide scoped environment variables or a safe `PATH`,
but credentials are not inherited accidentally. Workspace and placement are
passed by the host during initialization.

### 7. SDKs are conveniences, not the contract

The wire protocol is normative and language-neutral. The repository provides
small Go and dependency-free TypeScript serving SDKs with validation,
concurrent dispatch, cancellation, shutdown, result normalization, and
protocol-only stdout. A plugin may implement the protocol directly.

## Alternatives considered

- **Go's standard-library `plugin` package:** not selected because it shares
  the host process and normally requires matching toolchains, build settings,
  and platform support.
- **`hashicorp/go-plugin`:** not selected for runtime protocol 1 because its
  RPC and gRPC machinery is larger than the JSON-serializable domain seam
  requires.
- **WASM:** not selected because the filesystem, process, network, and
  terminal bindings needed by hands would make the host ABI the main plugin
  contract.
- **Implicit project-local activation:** not selected because a repository
  manifest is executable code and is not an approval signal.

## Consequences

- Go, TypeScript, and other language implementations can provide hands tools
  without rebuilding pons.
- Child crashes and malformed protocol data are contained at the adapter
  boundary and remain observable to the brain.
- Serialization, supervision, cancellation, and validation are host
  responsibilities.
- A subprocess alone does not protect the host from a malicious child;
  deployment isolation remains explicit under ADR-0004.
- New capabilities require a new host-side contract and adapter rather than
  adding arbitrary methods at runtime. `hook_provider/v1` supports the Core
  hook set with event-specific patch contracts.

## References

- `docs/reference/external-plugin-protocol.md` — normative runtime and tool-provider
  messages
- `plugins/external/` — host, manifest, schema validation, and adapter
- `plugins/external/sdk/` — Go and TypeScript serving SDKs
- `examples/external-echo/` — Go provider
- `examples/external-echo-ts/` — TypeScript provider
