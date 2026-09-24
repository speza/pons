# ADR-0004: Hands are the execution boundary; isolation is deployment policy

**Status:** Accepted
**Implementation:** Implemented boundary; isolation is opt-in by deployment
**Date:** 2026-09-15
**Related:** ADR-0001, ADR-0003, ADR-0007

## Context

Built-in tools execute in the pons process and therefore have the privileges
of that process. An allowlist, path check, or other in-process policy is useful
for limiting ordinary calls, but it is not an operating-system security
boundary: code running in the same process can use whatever host access the
process has.

The brain/hands split already gives pons a deployment seam. The brain talks in
`protocol.Action` values, and hands return `protocol.ToolResult` values. The
same seam supports built-in handlers and the persistent external tool
providers defined by ADR-0007.

## Decision

### 1. `ToolPort` is the execution boundary

The core never gives a brain a tool implementation or a direct I/O handle.
All capability execution goes through `ToolPort.Execute`. Built-in tools are
in-process implementations of that port. External tool providers are child
processes adapted to the same port by `plugins/external`.

The core does not implement a whole-hands socket, remote runtime, container,
or VM. Those are deployment choices outside the current pons runtime.

### 2. Isolation is supplied by deployment, not by tool policy

pons does not claim that a subprocess, path jail, or command allowlist is a
security boundary. When a task requires protection from the hands code, the
caller runs the hands side under an OS sandbox, container, or VM and grants it
only the intended workspace and capabilities.

The external host applies a clean child environment by default, accepts an
explicit workspace, and owns protocol-level timeouts, frame/result limits, and
process termination. These controls reduce accidental exposure and contain
failures; they do not replace OS isolation. Credentials are passed only by
explicit composition or a host-side broker.

### 3. Results and input are treated as untrusted data

`ToolResult` is the supported data path from hands back to the brain. In a
sandboxed deployment, the operator configures the sandbox's network and file
egress accordingly; pons itself cannot enforce that operating-system policy.

Workspace content in an observation is untrusted input and may contain prompt
injection. Sandboxing limits effects, not the meaning the brain assigns to
returned text. Brains and UIs must treat observations as data rather than
trusted instructions.

### 4. Failure belongs at the hands boundary

The control plane owns action deadlines and decides how to handle a dead,
killed, malformed, or oversized hands provider. Where execution can continue,
these failures become `ToolResult{OK:false}` observations. A tool's non-zero
command exit remains a domain result according to ADR-0003.

## Alternatives considered

- **Policy-only sandboxing:** rejected as a security story because in-process
  checks share the trust domain they are supposed to constrain.
- **Putting policy in the brain:** rejected because `Danger` is an advisory
  model output and cannot enforce execution policy.
- **Whole-agent container as the pons default:** rejected because it couples
  provider credentials and control logic to the sandbox. A caller may still
  choose that deployment when it is appropriate.

## Consequences

- The local in-process configuration remains simple and uses the same
  execution contract as external hands.
- External child processes provide a crash and resource-control boundary, but
  not a permissions boundary by themselves.
- Security-sensitive deployments must configure the OS/container/VM boundary
  explicitly.
- The protocol and tool result contract remain unchanged when deployment
  changes.

## References

- `core.go` — `ControlPort`, `ToolPort`, and dispatch
- `plugins/external` — persistent external hands adapter
- `docs/reference/external-plugin-protocol.md` — external tool-provider protocol
- `protocol/` — action and result contracts
