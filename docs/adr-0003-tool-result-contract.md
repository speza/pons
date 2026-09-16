# ADR-0003: ToolResult has a status envelope, canonical observation, and typed payload

**Status:** Accepted
**Date:** 2026-09-15
**Supersedes:** the earlier flat `ToolResult` shape (`Stdout`, `Stderr`,
`Diff`, and `Details map[string]string`)
**Related:** ADR-0001, ADR-0007

## Context

A tool result serves three audiences:

1. the brain, which needs a consistent text observation;
2. the loop, which needs to distinguish a hands or harness failure from a
   command failure represented as data; and
3. UIs and audit consumers, which need tool-specific structured facts such as
   an edit diff or output-truncation metadata.

A shell-specific result shape and an untyped details map do not provide those
audiences with a stable contract. Provider adapters also should not invent
slightly different renderings of the same tool result.

## Decision

`protocol.ToolResult` has three layers.

### 1. Status envelope

The universal fields are `ActionID`, `OK`, `Error`, and `ExitCode`.

`OK` reports whether the hands side produced a usable result for the harness.
A tool may represent a domain failure as data: for example, a command that
exits non-zero can return `OK: true`, an `ExitCode`, and an error description.
Denials, unavailable tools, panics, malformed results, and other harness-level
failures return `OK: false`.

### 2. Canonical observation

`Output` is text rendered by the tool that produced the result.
`ToolResult.Observation()` is the only model-facing rendering: it returns the
output and appends the error note when one is present. Brains pass this value
to their provider adapters. Provider adapters translate the same observation
into their provider-specific wire format; they do not re-render tool output.

### 3. Plugin-owned structured payload

`Kind` identifies the action kind that produced the result and `Payload` is
opaque `json.RawMessage`. The core never decodes or assigns meaning to a
payload beyond preserving its action-kind namespace. Consumers opt in through
accessors owned by the producing plugin, such as `edit.AsEditResult` and
`bash.AsExecResult`.

The core fills an omitted kind from the dispatched action. The external hands
adapter also overwrites the action identity and kind from the invoked action,
so an untrusted provider cannot redirect correlation or payload namespacing.

The shared protocol therefore contains no shell-only fields, edit-specific
fields, or open-ended string map.

## Alternatives considered

- **A universal `Details` map:** rejected because untyped keys and values hide
  the result schema and turn records into conventions.
- **A closed union of payload types in `protocol/`:** rejected because every
  new tool would require a core protocol change.
- **Per-tool results without an envelope:** rejected because the loop,
  recorder, and audit hooks require uniform status and correlation fields.

## Consequences

- Every provider receives the same canonical observation text.
- Structured facts remain available to UIs and audit code without coupling the
  core to tool vocabulary.
- The protocol remains JSON-serializable across the external hands boundary.
- Consumers that need a payload must know which plugin owns its schema; the
  core deliberately cannot interpret opaque payloads.

## References

- `protocol/protocol.go` — `ToolResult` and `Observation()`
- `plugins/edit` — `EditResult`
- `plugins/bash` — `ExecExtras`
- `plugins/external` — external result normalization
