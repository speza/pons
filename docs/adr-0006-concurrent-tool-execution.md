# ADR-0006: Within-turn tool execution is concurrent; the record is call-ordered

**Status:** Accepted
**Date:** 2026-09-15
**Related:** ADR-0001 (loop), ADR-0003 (ToolResult)

## Context

When the model emits several independent tool calls in one turn, executing
them sequentially wastes latency (each waits for the previous). But
execution order and *recorded* order are different concerns: the audit
trail, session tree, and brain context must stay deterministic or the
transcript becomes unreliable.

## Decision

1. **Concurrent execution**: all non-finish actions of a turn run
   concurrently (goroutine per call). Everything before a `finish` action
   executes; `finish` stops the turn as before.
2. **Call-order record**: results are indexed by call position — never by
   completion order. The `TurnLog`, session tree, brain context, and
   `action_end` events replay in call order after the last call completes;
   only wall-clock timing is nondeterministic.
3. **Safety rules that follow**:
   - tool handlers and `WrapTool` middleware must be goroutine-safe;
   - multi-step mutations of the same file are the producing plugin's
     responsibility to serialize (the edit tool is a read-modify-write);
   - the recorder runs after the turn completes — persistence stays
     sequential by construction.

## Alternatives considered

- **Sequential** (the original design) — deterministic for free, but slow
  when the model plans independent calls; rejected now that the audit
  order is preserved by indexing instead of execution order.
- **Completion-ordered recording** — rejected: turns the audit trail and
  session tree nondeterministic, breaking replay/resume.

## Consequences

Latency tracks the slowest call in a turn, not the sum. The deterministic
test locks both properties at once: the first tool blocks on a signal only
the second tool can send (sequential execution would deadlock), and the
recorded results must still come back in call order.

## References

- `core.go` (`Run` — the concurrent dispatch), `core_test.go`
  (`TestConcurrentExecutionOrderedResults`)
