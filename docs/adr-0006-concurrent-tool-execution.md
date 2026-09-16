# ADR-0006: Tool calls within a turn execute concurrently and record in call order

**Status:** Accepted
**Date:** 2026-09-15
**Related:** ADR-0001, ADR-0003

## Context

A brain may return several independent tool calls in one plan. Running them
sequentially makes each call wait for the preceding call even when the tools
are independent. Completion order is nondeterministic, but the audit trail and
next brain context must remain deterministic.

## Decision

1. The core finds the first `finish` action in a plan. Actions before it run
   concurrently, with one goroutine per action. The finish action and any
   actions after it are loop control and do not execute.
2. Results are stored by the actions' original indexes, never by completion
   time. After all running calls complete, the core emits `action_end` events,
   writes the `TurnLog`, runs turn hooks, and gives results to the brain in
   call order.
3. Tool handlers and `WrapTool` middleware are responsible for being safe
   under concurrent calls. A plugin that performs a multi-step mutation of
   shared state must serialize that mutation; the edit plugin serializes its
   read-modify-write operation.
4. Turn recording is performed after the concurrent calls have joined, so the
   session recorder writes one deterministic turn at a time.

## Alternatives considered

- **Sequential execution:** rejected because it adds avoidable latency for
  independent calls.
- **Completion-ordered recording:** rejected because it makes audit history,
  session trees, and resumed brain context nondeterministic.

## Consequences

- Turn latency is approximately the slowest running call rather than the sum
  of all call durations.
- The transcript and provider context preserve plan order even when tools
  finish in a different order.
- A tool must not assume that another tool call in the same plan has already
  completed unless the plugin or application provides synchronization.
- A finish action records in the plan but has no corresponding tool result.

## References

- `core.go` — concurrent dispatch and ordered result collection
- `core_test.go` — `TestConcurrentExecutionOrderedResults`
- `plugins/edit/edit.go` — serialized read-modify-write
