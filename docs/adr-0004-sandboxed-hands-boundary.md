# ADR-0004: Hands are a sandbox boundary — the protocol is the deployment seam

**Status:** Accepted (design); transport implementation tracked as follow-up
**Date:** 2026-09-15
**Related:** ADR-0001 (brain/hands ports), ADR-0003 (ToolResult wire shape)

## Context

Built-in hands run in-process with the user's privileges. Pi's `security.md`
is explicit about why a *policy* layer is not a security boundary: an
in-process policy check shares the trust domain it gates, and real isolation
must come from an OS, container, or VM boundary. Pi achieves that today by
running the whole agent in a container, or by routing tool execution into a
micro-VM via a tool-overriding extension (Gondolin).

The brain/hands split (`ControlPort` / `ToolPort`) makes the execution
boundary architectural in `pons`: the hands layer can move out of process
without touching the brain, the loop, or the protocol.

## Decision

**The hands layer is deployable out-of-process** — subprocess first, then
container/VM — with `protocol.Action` / `protocol.ToolResult` as the wire
over framed JSON (NDJSON, pi-RPC-style) on stdio/unix-socket/vsock. The
brain and harness are unchanged by where hands live.

Posture rules for the sandboxed configuration:

1. **The observation channel is the sandbox's only egress.** Information
   leaves the VM exclusively as `ToolResult` values; the control plane may
   inspect, truncate, redact, or deny them. Therefore: *secrets never enter
   the sandbox* (credentials stay host-side or proxy-side — pi's Docker
   Sandboxes sentinel-credential pattern), because anything inside can exit
   as an observation.
2. **Observations are untrusted.** Workspace content flows to the brain as
   observation text; prompt injection via that channel is expected and is
   treated as an input-hygiene problem, not something the sandbox prevents.
   The sandbox guarantees the *effects* boundary (OS-enforced), not input
   safety.
3. **Deadlines are owned by the control plane.** A remote hands process
   cannot be trusted to report its own hang; per-action timeouts move to the
   harness/transport, not the tool.
4. **Size discipline.** Large outputs should be truncated/referenced by the
   producing side (artifact path in the payload), because observations are
   the serialization cost of every crossing.
5. **Hands death is an observation.** A crashed or killed hands process
   surfaces as `ToolResult{OK:false}`, never as a control-plane crash.

## Alternatives considered

- **Whole-agent-in-container as the default** (pi's Plain Docker pattern) —
  simple, but provider credentials enter the sandbox and the split is
  all-or-nothing. Kept as an *option*, not the design.
- **Policy-only sandbox (in-process checks as the security story)** —
  explicitly rejected: a partial in-process sandbox is easy to mistake for a
  security boundary while depending on the host shell, filesystem, and
  package managers (pi's own `security.md` argument, which we adopt).
- **Policy in the brain** (the model self-reports risk) — rejected: the
  brain's `Danger` self-assessment is advisory; enforcement lives at
  execution, where it can't be bypassed by prompt injection.

## Consequences

**Positive:** the local/in-process case is a degenerate deployment of the
same design (in-memory dispatch); sandbox strength scales from "subprocess
with jail" to "micro-VM" without core changes; the audit trail records
exactly what crossed the boundary.

**Negative / accepted risks**

- Latency and serialization cost on every crossing — motivates size
  discipline and eventually streaming partial results.
- A follow-up **transport ADR** is required before implementation: framing,
  lifecycle (per-task vs per-session VM), health/reconnect, and payload
  kind registries across the wire.
- Policy plugins (allowlists) remain useful for *convenience and audit* but
  are explicitly not the security mechanism.

## References

- pi `security.md` ("no built-in sandbox, on purpose"), `containerization.md`
  (Gondolin: tool execution routed into a micro-VM)
- `harness` ports: `pons.ControlPort`, `pons.ToolPort`; `protocol/` (the wire)
