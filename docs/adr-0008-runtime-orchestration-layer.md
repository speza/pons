# ADR-0008: Long-lived orchestration is a pluggable runtime above the pons kernel

**Status:** Accepted
**Date:** 2026-09-18
**Related:** ADR-0001, ADR-0002, ADR-0005, ADR-0007

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
- a durable inbox and outbox using local JSON state.

The public API calls the resource a **conversation**. Its opaque identifier is
also the underlying pons session identifier in v1; the storage package may
continue to call it a session. The internal distinction is not exposed as a
second public identifier.

The first HTTP shape is:

```text
POST /v1/conversations
GET  /v1/conversations/{id}/events
POST /v1/conversations/{id}/messages
```

Conversation creation is server-owned. A message uses a `parts` array even
though v1 accepts only `{type: "text"}` parts. The message endpoint is
asynchronous and returns `202 Accepted`; an idempotency key identifies retries.
SSE is a separate subscription primitive. The CLI opens it before posting a
message, while reconnecting clients use event identity/current durable state.

The initial HTTP server is local-only. Exposing it beyond loopback requires a
future authentication decision.

### 5. Messages steer only at safe turn boundaries

Messages arriving while tools are executing are held in a per-conversation
mailbox. The runtime waits for the complete tool batch, records results in
planned call order, then the next agent decision receives the pending messages
as separate user messages.

A text-only assistant response is a response boundary: it is streamed to the
channel, persisted as an assistant message, and the active core is torn down.
The runtime then waits for another inbound message and creates a fresh core
and brain from the durable conversation. It does not make an autonomous model
call with no new input.

The generic input seam is intentionally small:

```go
type MessageSource interface {
    GetMessages(ctx context.Context) ([]protocol.UserMessage, error)
}
```

`GetMessages` returns and claims currently pending messages in arrival order;
an empty result is valid after a tool batch. The runtime seeds the source with
the first message, so initial and steering input use the same path.

### 6. Assistant output is not a `finish` action

An assistant response is an `AssistantMessage`, not a core `finish` action.
An assistant message may contain ordered text and tool-call blocks. Text may be
streamed while a complete provider response is being assembled; tool calls are
not executed until the complete assistant message is durable.

The core/runtime lifecycle determines whether that assistant message ends a
one-shot run or yields to an idle continuous conversation. Runtime shutdown
and cancellation are separate lifecycle controls.

### 7. Persistence and crash recovery are explicit

The transcript remains the durable conversation history. Runtime operational
state is separate and is stored as an atomically replaced JSON snapshot per
conversation. It contains pending inbox entries, processing state, outbox
entries, and inbound deduplication IDs. The runtime is single-process in v1;
multiple processes require a shared store with atomic claims or leases.

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
incomplete response is executed. Token and tool-progress events are live and
best-effort. The completed assistant response and current runtime state are
durable and replayable; reconnecting clients need not receive old token
deltas.

### 8. Channels remain runtime plugins

The HTTP channel is the first built-in transport. Future channels and triggers
adapt to the normalized runtime message model. A scheduler is another inbound
source and follows the same message-to-agent path.

Cross-channel identity linking, handoff, and conversation forking are deferred.
When eventually added, handoff will fork a completed active transcript path
into a new conversation; it will not roll back or fork real-world tool side
effects.

## Alternatives considered

- **Put messaging and scheduling in `Core`:** not selected. This couples the
  finite agent loop to transport, lifecycle, and delivery concerns.
- **Make every channel a model tool:** not selected for ordinary replies. It
  gives the model responsibility for delivery semantics and makes reliable
  request/reply handling difficult.
- **Use a persistent brain while idle:** not selected for v1. Idle cores and
  brains are torn down; the runtime hydrates them from the conversation.
- **Use JSONL for mutable runtime state:** not selected for v1. The transcript
  is append-only JSONL, but queue and delivery state is a small mutable JSON
  snapshot with atomic replacement.
- **Build cross-channel continuation immediately:** not selected. Channel
  conversation identity is a safer initial session boundary than guessing
  identity or context across transports.

## Consequences

- pons can remain a small, useful agent harness without committing to a
  particular messaging product.
- The first always-on runtime can be implemented and tested locally without
  authentication, a database, or multiple workers.
- The same normalized message path supports human messages, future webhooks,
  and future schedules.
- Durable inbox recovery must distinguish incomplete agent processing from
  tool execution; arbitrary tool side effects cannot be made exactly once by
  the runtime.
- A provider-neutral assistant turn is richer than the current action-only
  control interface and will require a deliberate breaking API change when
  implemented.
- The HTTP API exposes a conversation resource rather than storage internals,
  leaving future session backends and conversation forks possible.

## References

- `docs/runtime-v1.md` — concrete v1 runtime and HTTP design
- `core.go` — finite agent loop and core plugin composition
- `protocol/` — brain/hands wire types
- `sessions/` — transcript and session storage contract
- `docs/adr-0001-pluggable-minimal-harness.md` — core architecture
- `docs/adr-0005-transcript-model-vs-engine.md` — session model and composition
- `docs/adr-0007-language-neutral-plugin-runtime.md` — external hands plugins
