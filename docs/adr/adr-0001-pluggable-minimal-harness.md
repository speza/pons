# ADR-0001: pons is a minimal, plugin-extensible agent harness

**Status:** Accepted
**Implementation:** Implemented
**Date:** 2025-09-14
**Related:** ADR-0003, ADR-0004, ADR-0006, ADR-0007, ADR-0008, ADR-0011

## Context

An agent harness has two different responsibilities. A brain plans and
interprets. Hands perform side effects. Keeping those responsibilities in one
loop makes execution policy difficult to audit, makes the loop hard to test,
and makes every new capability a change to the core.

pons needs a small orchestration layer whose available capabilities are
explicit at composition time. The brain must not receive execution handles,
and a tool must not receive the brain's history or goals.

## Decision

### 1. The core owns only contracts, orchestration, and composition

The core package depends on the dependency-free `protocol/` package and owns
three seams:

- `protocol/` defines the JSON contract: `Action`, `ToolResult`,
  `Observation`, and `TurnLog`.
- `ControlPort` is the brain interface:
  `Respond`, `Interpret`, and `Close`.
- `ToolPort` is the hands interface: `Execute` for one action.

`Core.Run` executes the fixed loop:

```text
respond -> execute -> interpret -> repeat
```

It supplies the turn budget, dispatches actions by kind, reserves the
`finish` action, and returns a `RunResult`. It does not contain task-specific
file, shell, session, provider, or policy behavior.

A newly created `Core` has no tools and no brain. Its capability surface is
exactly the set of plugins applied to it.

### 2. Plugins add capabilities through explicit registration

The plugin interface is:

```go
type Plugin interface { Setup(*Core) error }
```

`Setup` registers tools, a brain, middleware, lifecycle hooks, or named
capabilities for other trusted plugins. Registration
is additive and conflicts fail during composition:

- `AddTool` rejects an empty, reserved, duplicate, or nil-handler action.
- `SetBrain` rejects a nil or second brain.
- `WrapTool` composes execution middleware explicitly.
- `RegisterCapability` rejects empty names, nil values, and duplicate names;
  `Capability` lets another trusted plugin resolve a registered value.
- `AddHooks` composes typed agent, turn, and tool callbacks through one plugin
  registration value. `OnToolCallStart` is the sole pre-execution decision
  point; `OnAgentTurnEnd` replaces separate turn observers.

The composing application chooses the plugin set and order. The core does not
import or discover plugins.

### 3. The ports enforce the brain/hands boundary

A brain communicates with the harness by returning `protocol.Action` values
and by receiving `protocol.Observation` and `protocol.ToolResult` values. The
core gives it no tool implementation or direct execution operation.

A tool handler receives a context and one action. It has no core loop,
conversation history, model, or goal supplied by the hands interface. The
execution port is the enforcement point for tool policy; the brain's
`Danger` value is advisory and is not trusted.

`Action.Kind` is an open capability vocabulary. The core reserves only
`finish`; a tool plugin owns its action kind, arguments, guard, result payload,
and constructors. Unknown kinds become unsuccessful `ToolResult`
observations instead of crashing the loop.

### 4. Shared result data stays minimal

`ToolResult` contains only the universal status envelope, canonical text, and
an opaque plugin payload. The producing plugin renders the model-facing
observation, while the core preserves plugin-owned structured data. The
contract is defined by ADR-0003.

### 5. The loop is a usable API

`Run` returns:

- `Answer`, the final text from a finish action;
- `Turns` and `History`, the completed turn records; and
- `Exhausted`, which reports budget exhaustion without treating it as an
  error.

`OnEvent` exposes the loop stages (`agent_start`, `turn_start`, action start
and end, turn end, finish, stopped, and exhausted); a checked event sink can
fail the run before the next side effect. Tool calls within a turn execute
concurrently and are recorded in call order, as specified by ADR-0006.

Typed plugin hooks cover agent, agent-turn, permission, and tool-call phases.
Every hook takes an Input and returns an Output with a shared envelope (stop,
system message, additional context) plus at most one decision, as specified
by ADR-0014. When `OnToolCallStart` updates a call's arguments, a stateful
brain is reconciled so its transcript matches what ran.

The shipped LLM brain makes one provider call per turn, discovers the schemas
registered in `Core`, converts tool calls to `Action` values, and treats a
text-only provider response as the finish signal. Provider translation and
context compaction remain inside that brain plugin.

### 6. Execution capabilities are plugins; runtime persistence is not

External tool providers are supplied by the hands-side plugin runtime in
ADR-0007. That runtime uses the same action and result contracts as in-process
hands. Runtime persistence is infrastructure owned by the server composition
behind `runtime.Store`, not a core plugin, as specified by ADR-0011.

### 7. Brain/hands is architectural vocabulary, not the whole API taxonomy

**Brain** and **hands** name the core separation: planning and interpretation
on one side, execution on the other. They remain useful explanatory and
package-level terms, but new public APIs should prefer neutral names such as
control/planning, tool/execution, runtime, execution environment, message, and
provider.

The anatomy metaphor is not extended to channels, schedulers, sandboxes, or
other orchestration components. Existing names remain stable unless a later
breaking change has a concrete benefit.

## Consequences

**Positive**

- The loop is testable with deterministic brains and no provider calls.
- The composition site shows which capabilities and policies are installed.
- Tools can be removed without changing the core loop.
- In-process and external hands use the same brain/hands contract.
- Tool, runtime-store, and provider implementations remain outside the core.

**Accepted risks**

- The core does not provide an operating-system security boundary. A shell or
  other privileged tool is safe only when its composing deployment supplies
  the required isolation; ADR-0004 defines that posture.
- Long-lived scheduling, persistence, and client delivery add a separate
  runtime layer above the finite core, as specified by ADR-0008.

## References

- `core.go` — loop, ports, registry, and hooks
- `protocol/protocol.go` — brain/hands wire types
- `runtime/` — long-lived orchestration and its store contract
- `plugins/` — brain and execution capabilities
