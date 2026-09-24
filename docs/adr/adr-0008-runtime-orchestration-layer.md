# ADR-0008: Long-lived orchestration is a pluggable runtime above the pons kernel

**Status:** Accepted; state, storage, and coordination refined by ADR-0010 through ADR-0012, persistent agents and tasks proposed by ADR-0016, and trust-domain scope defined by ADR-0017
**Implementation:** Local runtime implemented; other channel adapters remain proposed
**Date:** 2026-09-18
**Related:** ADR-0001, ADR-0007, ADR-0009, ADR-0010, ADR-0011, ADR-0012, ADR-0016, ADR-0017

## Context

pons is a minimal agent harness. Its core is intentionally concerned with one
finite run:

```text
message -> plan -> execute -> reflect -> answer
```

The core provides the brain/hands boundary, tool composition, execution
policy, and loop lifecycle. It does not need to know who sent a message, which
channel carried it, whether a run was scheduled, or how a response is
redelivered.

An always-on assistant introduces a different set of concerns:

- message and event ingress from channels, webhooks, and local clients;
- scheduling and wake-ups;
- routing an event to an agent and conversation;
- serializing work for one conversation;
- durable inbox, session mapping, and delivery state;
- retries, deduplication, cancellation, and process lifecycle; and
- translating an agent result into a channel response.

These concerns are orchestration, not brain/hands execution. Putting them in
`Core`, or representing ordinary replies as model-selected tools, would make
the kernel larger and would mix transport reliability with agent behavior.

## Decision

### 1. The pons core remains a finite agent kernel

`Core.Run` remains the unit of agent execution. The core does not acquire
concepts for channels, users, schedules, delivery targets, or long-lived
runtime processes.

The existing brain/hands and tool plugin contracts remain the primary
extension mechanism for execution capabilities, as defined by ADR-0001.

A future run input source may provide a batch of provider-neutral user
messages. A single message is simply a batch containing one message; the core
does not need a separate initial-message concept.

### 2. A separate runtime owns long-lived orchestration

A runtime package and/or long-lived command is built above `Core`. Its
conceptual flow is:

```text
channel / webhook / scheduler
              |
       normalized inbound message
              |
       routing and session lookup
              |
        serialized Core.Run
              |
       result and runtime events
              |
       outbound message / delivery
```

The runtime owns the orchestration spine: event correlation, conversation
queueing, cancellation and deadlines, session selection, delivery state,
retries, and shutdown. It may construct a fresh core per active burst or
otherwise ensure that a conversation is not driven concurrently.

The runtime has a separate composition boundary from `Core`. Runtime
extensions are not required to be hands plugins and do not receive a `Core`
handle unless the composing application explicitly gives them one.

### 3. Chat/message is a built-in runtime capability

The runtime defines a normalized message model for the common request/reply
path. The model is provider-neutral and initially supports text parts only.
A user-facing message contains an identity and ordered parts; future parts may
represent images, audio, or attachments without changing the batch shape.

The semantic conversation items are:

```text
UserMessage
AssistantMessage   // ordered text and tool-call blocks
ToolResult         // correlated to one tool call
```

A tool result is not semantically a user message. Providers may require it to
be encoded as one: Anthropic receives `tool_result` blocks in a following user
message, while OpenAI Responses receives `function_call_output` items. Those
are adapter details.

The built-in `chat`/message capability consumes an inbound message, resolves
(or creates) the corresponding pons session, invokes an agent run, and emits
an outbound message. It is shipped and enabled by default, but uses the same
runtime registration seam where practical so it can be tested, replaced, or
embedded without changing `Core`.

The runtime message model is not part of the channel transport contract.
Channel and sender metadata belong to the runtime layer. The provider-neutral
content types cross the runtime-to-brain boundary; provider-native content
blocks remain inside brain adapters.

### 4. Runtime v1 is deliberately narrow

The first runtime composition is:

- one configured agent, named `pons`;
- one HTTP channel, bound to loopback with no authentication;
- the CLI as an HTTP client rather than a separate transport;
- one process with per-conversation serialization;
- one conversation per channel conversation/thread; and
- a durable inbox and outbox in the transactional conversation store selected
  by ADR-0010.

The public API calls the resource a **conversation**. Its opaque identifier is
the canonical runtime-store identity; there is no separate session identifier
or legacy session store.

The first HTTP shape is:

```text
POST /v1/conversations
GET  /v1/conversations/{id}
GET  /v1/conversations/{id}/events
POST /v1/conversations/{id}/messages
```

Conversation creation is server-owned. A message uses a `parts` array even
though v1 accepts only `{type: "text"}` parts. The message endpoint is
asynchronous and returns `202 Accepted`; an idempotency key identifies retries.
The conversation `GET` returns a renderable snapshot and its event cursor. SSE
is a separate subscription primitive. The CLI may open it before posting a
message; a reconnecting client first renders a snapshot and then subscribes
after that snapshot's cursor. Durable changes are replayable, while token
deltas and progress are transient.

The initial HTTP server is local-only. Exposing it beyond loopback requires a
future authentication decision.

HTTP and SSE are one transport adapter, not manager concerns. The runtime
surface exposes conversation creation, submission, coherent snapshots, and
event subscriptions using provider-neutral types. Native HTTP/SSE lives in
`runtime/httptransport`; future protocols translate at the same boundary.

### 5. Messages arriving during a run queue the next run

Each accepted inbound message creates one durable queued submission. A message
arriving while another run owns the conversation remains queued; it is not
injected into the active core. When the current run commits its terminal state,
the scheduler may claim the next submission and hydrate a fresh core and brain
from canonical messages through that submission.

This preserves a clear response boundary and prevents later queued messages
from leaking into an earlier run's provider context. Active-run steering at a
safe model boundary would require a separate runner contract and is not part of
the current runtime.

### 6. Runtime output is semantic even though the finite core has a finish signal

The finite core retains its internal `finish` action as the signal that a
bounded `Core.Run` has produced its answer. The runtime projects that answer as
a final semantic `AssistantMessage`; clients and durable history do not consume
the core action.

Non-final assistant turns may contain ordered text and tool-call blocks. Tool
calls are not executed until the complete assistant turn and requested tool
intent are durable. Runtime shutdown and cancellation remain separate
lifecycle controls.

### 7. Persistence and crash recovery are explicit

Semantic messages are the durable conversation history and are read directly
for both LLM hydration and client snapshots. Submissions, runs, and tool calls
own operational state. These records and a client event outbox share one
transactional store, as specified by ADR-0010.

The persistence ordering for a tool turn is:

```text
persist UserMessage
  -> persist AssistantMessage with all tool calls
  -> execute tools
  -> persist one ToolResult per call
  -> continue to the next model decision
```

If a process crashes with an assistant tool call that has no result, recovery
adds a normal failed result whose text says that execution was interrupted and
its outcome is unknown. The provider then receives a complete tool-call/result
pair and the agent decides whether to issue a new action. The runtime never
automatically retries a tool action.

LLM provider requests may be retried with backoff. An incomplete streamed
assistant response is discarded and regenerated; no tool call from an
incomplete response is executed. Token deltas, tool progress, and heartbeats
are live, best-effort, and never part of the durable event outbox. Complete
assistant messages, tool intent/results, and current run state are durable.
Reconnect reconstructs the UI from a conversation snapshot, not historical
token deltas or a full event replay.

### 8. Channels remain runtime adapters

The HTTP channel is the first built-in transport adapter. Future channels and
triggers adapt to the normalized runtime message model. They are not core
plugins: a scheduler is another inbound adapter and follows the same
message-to-agent path.

Cross-channel identity linking, handoff, and conversation forking are deferred.
When eventually added, handoff will fork a completed active transcript path
into a new conversation; it will not roll back or fork real-world tool side
effects.

ADR-0016 separately proposes a persistent, owner-named agent that starts
private tasks over durable messages and child conversations. That design extends the durable submission path without
changing this ADR's finite-core boundary.

ADR-0017 defines one runtime instance as one trusted administrative domain.
Hosted tenancy belongs in a control plane which provisions and routes isolated
runtime cells, not in `Core` or the local conversation contract.

## Alternatives considered

- **Put messaging and scheduling in `Core`:** not selected. This couples the
  finite agent loop to transport, lifecycle, and delivery concerns.
- **Make every channel a model tool:** not selected for ordinary replies. It
  gives the model responsibility for delivery semantics and makes reliable
  request/reply handling difficult.
- **Use a persistent brain while idle:** not selected for v1. Idle cores and
  brains are torn down; the runtime hydrates them from the conversation.
- **Use JSONL for mutable runtime state:** not selected. ADR-0010 replaces the
  original JSONL-plus-JSON baseline with implemented transactional semantic
  and operational state plus an event outbox.
- **Build cross-channel continuation immediately:** not selected. Channel
  conversation identity is a safer initial session boundary than guessing
  identity or context across transports.

## Consequences

- pons can remain a small, useful agent harness without committing to a
  particular messaging product.
- The first always-on runtime is local and single-process; it uses SQLite and
  loopback-only HTTP without remote authentication.
- The same normalized message path supports human messages, future webhooks,
  and future schedules.
- Durable inbox recovery must distinguish incomplete agent processing from
  tool execution; arbitrary tool side effects cannot be made exactly once by
  the runtime.
- The runtime runner adapts the finite action-based core into provider-neutral
  assistant turns without adding transport or persistence concerns to `Core`.
- The HTTP API exposes a conversation resource rather than storage internals,
  leaving future store adapters and conversation forks possible.

## References

- `docs/runtime-v1.md` — concrete v1 runtime and HTTP design
- `docs/adr/adr-0010-runtime-state-and-client-synchronization.md` — transactional
  runtime state, snapshots, durable events, and transient streaming
- `docs/adr/adr-0016-persistent-agents-and-async-messaging.md` — proposed persistent
  identities, mailboxes, and asynchronous delegation
- `docs/adr/adr-0017-one-runtime-one-trust-domain.md` — single-domain scope and
  isolated-cell hosting boundary
- `core.go` — finite agent loop and core plugin composition
- `protocol/` — brain/hands wire types
- `runtime/`, `runtime/sqlite/` — runtime manager, store contract, and SQLite store
- `docs/adr/adr-0001-pluggable-minimal-harness.md` — core architecture
- `docs/adr/adr-0011-server-centred-runtime-storage.md` — server-owned runtime storage
- `docs/adr/adr-0007-language-neutral-plugin-runtime.md` — external hands plugins
