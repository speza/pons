# ADR-0007: Language-neutral external plugin runtime

**Status:** Accepted
**Date:** 2025-09-16
**Related:** ADR-0001 (plugin seam), ADR-0004 (hands boundary), ADR-0005 (session contract)

## Context

Pons calls Go values implementing `Plugin.Setup(*Core)` "plugins", but every
such plugin is imported and linked into the pons executable. Users cannot
install a capability without compiling a new pons binary. This is composition,
not a runtime plugin system.

We want independently built and installed extensions. Tool implementations
must also remain on the correct side of the brain/hands boundary, and the
extension mechanism should not require authors to use Go. A TypeScript, Rust,
or Python extension should be as valid as a Go extension.

The existing `Plugin` interface cannot itself cross a process boundary.
`Setup(*Core)` can register arbitrary Go closures, middleware, and hooks; none
of those values are serializable. An external extension therefore needs to
declare a finite set of capabilities for which pons has explicit contracts.

## Decision

### 1. An external plugin is an executable capability provider

An external plugin is an ordinary executable launched as a persistent child
process. Pons communicates with it using a versioned RPC protocol over stdio.
The executable may be implemented in any language.

"Plugin" names the independently distributed unit. A plugin advertises one or
more typed capabilities rather than receiving arbitrary access to `Core`.
Capability protocols are independently versioned on top of one common runtime
protocol.

Runtime protocol 1 standardizes `tool_provider/v1`: discovery and execution of
actions in the hands environment. `brain_provider` is the next intended
capability. Session stores, event sinks, durable recorders, and execution
policy are plausible future capabilities, but this ADR does not freeze their
contracts before they have been designed and exercised.

New capability types require an explicit host-side contract and adapter. An
external plugin cannot add arbitrary methods to the core at runtime. Unknown
capabilities are rejected or left inactive; capability negotiation makes the
runtime extensible without making unimplemented capability names normative.

### 2. In-process plugins remain supported

`Plugin.Setup(*Core)` remains the trusted Go composition API. Built-ins may
continue to use it, and applications embedding pons may define their own.

The external plugin loader is itself an in-process `Plugin`. It negotiates
capabilities with a child process and installs host-side proxies through the
same additive registration points. Thus the core loop does not distinguish a
built-in implementation from an external one.

The public, independently distributable extension mechanism is the external
protocol, not Go ABI loading.

### 3. Placement follows the capability

Plugins run in the environment where their capability belongs. Tool providers
run in the hands environment. Future control capabilities such as brains would
normally run in the control plane; future execution-policy capabilities would
run at the hands boundary.

When hands are local, the local runtime launches hands plugins. When hands are
in a container, VM, or remote process, that hands runtime discovers and
launches its installed plugins. Only tool descriptions, Actions, and
ToolResults cross back to the controller.

A plugin cannot choose a more privileged placement. Placement is composition
and deployment policy. A distribution exposing capabilities for incompatible
placements must be launched as separate instances in the relevant runtimes or
split into separate executables.

The sandbox encloses the hands runtime and all of its plugin children. A child
process is a crash boundary, not a security boundary: absent an OS sandbox, it
has the permissions of its parent environment.

### 4. Runtime transport is JSON-RPC 2.0 over NDJSON stdio

Each line on stdin or stdout is one UTF-8 JSON-RPC 2.0 message. Stdout is
reserved for protocol traffic; stderr is plugin logging. Request IDs permit
multiple calls to be in flight and responses may arrive out of order.

The common runtime protocol defines initialization, capability negotiation,
cancellation, health, and shutdown. Capability specifications define their
own methods and payloads. Protocol-level errors are distinct from domain
results such as a failed command represented by `ToolResult`.

The host owns deadlines and output limits. A crash, timeout, oversized frame,
or malformed response becomes a checked plugin failure. The host also
normalizes ToolResult action identity and payload namespace from the invoked
Action rather than trusting plugin fields. A hands plugin failure is returned
as an unsuccessful ToolResult where execution can safely continue;
failures of stateful control-plane capabilities follow that capability's
specified semantics.

### 5. Tool inputs are typed JSON described by JSON Schema

The existing `Action.Args map[string]string` is too narrow for a public,
language-neutral extension contract. It cannot faithfully represent numbers,
booleans, arrays, nested objects, or null. Before `tool_provider/v1` ships,
`Action.Args` will become an opaque JSON object (`json.RawMessage` at the Go
wire boundary), and each tool will decode that object into its own typed input.

Tool providers describe inputs with a JSON Schema object. Version 1 guarantees
the practical subset shared by supported model providers: object properties,
required fields, primitive types, arrays, nested objects, descriptions, and
enums. The plugin protocol transports schemas without attempting to implement
or reinterpret them; the host validates the supported subset and may validate
arguments before dispatch.

This is intentionally a breaking protocol change made before an external
plugin ecosystem exists. Plugin-owned typed decoding keeps capability
vocabulary out of the core while preserving values across languages.

### 6. Installation and activation are explicit

A plugin is installed as a manifest plus its executable/artifacts. The manifest
contains launch information and a runtime protocol version; capabilities
reported by the initialization handshake are authoritative.

Initial activation is through explicit configuration or `--plugin` paths.
User-level plugin directories may be added later. Project-local plugins are
never executed merely because a repository contains a manifest; they require
explicit trust/approval.

Pons does not initially download or upload executable code into a hands
runtime. Container/VM builders install plugins in the image, or operators mount
an approved plugin directory. TypeScript plugins may depend on an installed
Node/Bun/Deno runtime or be distributed as standalone executables.

### 7. SDKs are conveniences, not the contract

The wire specification is language-neutral and normative. Small Go and
TypeScript SDKs provide serving loops, dispatch, cancellation, validation, and
error conversion. The Go SDK is covered by hermetic host tests; the TypeScript
SDK and example are smoke-tested, while a shared cross-language conformance
runner remains follow-up work.
A plugin may implement the wire protocol without an SDK.

## Why not the standard library `plugin` package?

Go shared-object plugins share values directly and could export a
`pons.Plugin`, but independently distributed plugins must normally match the
host's exact Go toolchain, build flags, and common dependency sources. Platform
support is limited, plugins cannot be unloaded, and plugin faults share the
host process. This is appropriate only when all artifacts are built together,
which does not meet this decision's distribution goal.

## Why not `hashicorp/go-plugin`?

`go-plugin` provides a mature subprocess lifecycle, handshakes, RPC/gRPC,
logging, and crash isolation. It remains a viable implementation option. Its
RPC adapters and, for cross-language use, protobuf/gRPC surface add substantial
machinery to a project whose existing domain seam is already JSON-serializable.
We choose a small protocol first and will reconsider `go-plugin` if lifecycle,
reattachment, streaming, or transport complexity grows beyond what pons can
keep small and auditable.

## Why not WASM?

WASM gives stronger portability and capability-oriented sandboxing, but tools
need controlled filesystem, process, network, and terminal access that varies
across WASI runtimes. It would make host bindings the dominant part of the
plugin API. WASM may become another transport/runtime later; it is not the
initial extension format.

## Consequences

### Positive

- Plugins can be built, installed, and upgraded independently of pons.
- Go, TypeScript, and other languages have equal protocol access.
- Plugin crashes do not corrupt the host process.
- Hands plugins naturally live inside the hands sandbox.
- Capability contracts and protocol versions make compatibility explicit.
- Existing in-process implementations and the core loop remain usable.

### Negative

- Every capability needs a wire contract and host adapter.
- Calls incur serialization and process-boundary overhead.
- Process supervision, cancellation, output limits, and malformed peers become
  pons responsibilities.
- A subprocess alone does not constrain malicious plugin effects.
- Multi-placement plugin distributions require explicit deployment handling.

## Follow-up specifications

- `external-plugin-protocol.md` — runtime, framing, lifecycle, manifests,
  capability negotiation, and the normative `tool_provider/v1` contract.
- A later `brain_provider` specification based on the exercised `ControlPort`.
- Other capability contracts only when their placement, lifecycle, and failure
  semantics have been separately designed.
- A shared conformance suite, followed by Go and TypeScript SDKs.

This decision supersedes ADR-0001's limitation to compile-time plugin
composition and supplies the plugin side of ADR-0004's pending transport work.
