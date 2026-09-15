# ADR-0003: ToolResult contract — status, canonical observation, typed payloads

**Status:** Accepted
**Date:** 2026-09-15
**Supersedes:** the flat `ToolResult` shape used until this date (`Stdout`,
`Stderr`, `Diff`, `Details map[string]string`).

## Context

`ToolResult` serves three audiences with different needs:

1. **The model** (via every LLM provider's tool-result wire format) — needs
   one consistent text rendering of what happened.
2. **The loop** — needs a control-flow signal distinguishing "harness-level
   failure" from "failed command as data" (a non-zero `exit 1` is an
   observation, not a crash).
3. **UIs / audit** — need tool-specific *structured* facts (the edit diff,
   truncation info, full-output paths) that the model doesn't need.

The initial shape conflated these: shell-flavored field names (`Stdout` even
for file reads), a tool-specific field (`Diff`) in the shared protocol, and
an untyped escape hatch (`Details map[string]string`). Rendering to
model-facing text was improvised per brain adapter — so provider responses
were only accidentally consistent.

## Decision

`ToolResult` is a **three-layer type**:

1. **Status envelope** — `ActionID`, `OK`, `Error`, `ExitCode`.
   - `OK=false` means harness-level failure (denied, crashed, unknown kind).
     A failed *command* is `OK=true` + `ExitCode` — an observation.
2. **Canonical observation** — `Output` is the model-facing text, *rendered
   by the tool plugin that produced it*; `Observation()` (protocol method)
   appends the failure note and is **the** function brains feed to providers.
   One result → identical text → every provider. Per-adapter string glue is
   forbidden.
3. **Structured payload** — `Kind` (the producing action kind; results are
   namespaced by the action that made them) + `Payload json.RawMessage`,
   decoded through **plugin-owned typed accessors** (`edit.AsEditResult`,
   `bash.AsExecResult`). The core never interprets payloads.

Dropped: `Stderr` (nothing set it; exec plugins merge streams by choice),
`Diff` (now `EditResult.Diff`), `Details` map (unstructured escape hatch).

## Alternatives considered

- **Open `Details` map** — one universal type with plugin-named keys. Works,
  but "structured, but with a name": untyped keys/values, no discoverability,
  and results stop being records. Rejected after review.
- **Closed union of payload types in core protocol** — rejected: violates
  plugin-owned vocabulary; adding a tool plugin could not require a core
  protocol change.
- **Per-tool result types without a shared envelope** — rejected: the loop,
  session recorder, and audit hooks need a uniform type.

## Consequences

**Positive:** provider consistency is structural (one rendering function);
payloads are typed at their edges via plugin accessors; the envelope stays
small and JSON-serializable for the future out-of-process hands.

**Negative / accepted risks**

- `Payload json.RawMessage` is opaque to the core — deliberate; consumers
  opt in by knowing the plugin.
- Contract now states: *tools render observations; providers translate them;
  nothing in between improvises string formats.*

## References

- `protocol/protocol.go` (`ToolResult`, `Observation()`)
- `plugins/edit` (`EditResult` + `AsEditResult`), `plugins/bash` (`ExecExtras` + `AsExecResult`)
- pi: `ToolResultMessage.content` vs `details` (the two-audience split this mirrors)
