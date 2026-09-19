# pons runtime v1 design

**Status:** Transactional runtime and client synchronization implemented
**Related:** [ADR-0008](adr-0008-runtime-orchestration-layer.md),
[ADR-0010](adr-0010-runtime-state-and-client-synchronization.md)

This document turns ADR-0008 into the smallest useful local runtime shape. It
is an implementation guide, not a second agent protocol.

## Goals

- keep the pons core finite and reusable;
- accept messages through one local HTTP channel;
- make the CLI an HTTP client;
- persist accepted work across process restarts;
- let clients hydrate a complete renderable conversation without event replay;
- stream live assistant output when possible; and
- resume a conversation by loading provider-neutral semantic messages directly.

The runtime implementation lives in `runtime/`; the native wire adapter lives
in `runtime/httptransport`. `pons serve` composes them with the configured
agent and `pons client` uses that HTTP/SSE adapter. The default CLI invocation
bundles both lifecycles but still communicates through the same loopback HTTP
and SSE path.

Runs are globally bounded and hold an exclusive workspace lease. When an
execution-environment provider is configured, each run starts a fresh hands
session, registers only its discovered proxy tools, and closes the session
before becoming idle. The Seatbelt provider launches `pons-hands`; the
unsandboxed development composition registers in-process tools explicitly.

The runtime persists semantic messages, operational state, and a durable event
outbox in one SQLite database. It does not run a second session recorder or
write an independently authoritative JSONL transcript. JSONL may be offered as
an explicit export of canonical runtime messages later.

The manager exposes transport-independent operations for creation, submission,
snapshot reads, and subscriptions. HTTP routing, SSE framing, cursor headers,
and the native network client belong to `runtime/httptransport`. Future AG-UI,
ACP, or channel adapters project the same canonical state and events without
changing runtime persistence or scheduling.

The baseline queues a message arriving during an active run as the next fresh
burst. Injecting it at the next safe turn boundary requires the planned
`MessageSource`/provider-neutral assistant-turn change and remains follow-up
work. Likewise, v1 currently streams lifecycle and tool events; token-level
`assistant.delta` emission depends on provider streaming support.

## Out of scope

The first runtime does not include:

- WhatsApp, Slack, Telegram, or other remote channels;
- cross-channel identity linking or handoff;
- multiple agents;
- multi-process workers or distributed leases;
- authentication beyond loopback binding;
- image, audio, or file parts; or
- persistence or replay of historical token deltas.

## Runtime terms

### Conversation

The public HTTP resource and the logical interaction. In v1 its opaque ID is
the same value as the underlying pons session ID. A conversation is scoped to
the HTTP channel's conversation key.

### Inbound message

A durable user input. It has a stable ID for idempotency and an ordered list of
content parts. v1 accepts text parts only.

### Assistant message

A provider-neutral assistant output. It has ordered parts, including text and
tool-call parts. It is persisted as one semantic assistant item even when the
provider emits multiple output blocks. `complete` marks the durable semantic
boundary; `final` is reserved for the terminal text response which completes
an inbound submission.

### Tool result

The observation for one tool call. It is correlated by the tool-call ID and is
persisted separately from the assistant message. A provider adapter may group
several results into the provider-specific request shape.

## Conversation lifecycle

```text
create conversation
        ↓
atomically persist UserMessage, submission, and client event
        ↓
hydrate a fresh Core/Brain from semantic messages
        ↓
run agent turns
        ↓
commit each complete assistant tool turn before executing its tools
        ↓
persist tool results and stream durable entity updates
        ↓
persist the final text response
        ↓
mark inbound work complete and tear down Core/Brain
        ↓
wait for the next inbound message
```

A message arriving while tools are running is placed in the same
conversation's mailbox. The active run finishes without seeing that later
message. The runtime then claims it as the next fresh burst, ordered after all
semantic messages produced by the preceding run.

A message arriving after a text-only response starts the next run. It loads
the same semantic conversation but uses a newly constructed Core and Brain.

## Core planning seam

Brains may implement `PlanningControlPort` to return a `Plan` containing both
the executable actions and the ordered provider-neutral assistant parts which
produced them. The core emits that complete plan through its checked event
sink before emitting any action-start event. The runtime commits the assistant
message and every requested tool-call record atomically at that boundary.

Brains which only implement `ControlPort` remain valid: the core derives a
tool-call-only plan from their actions.

## Planned steering seam

Injecting messages into an already-active run remains follow-up work. The
planned abstraction is:

```go
type MessageSource interface {
    GetMessages(ctx context.Context) ([]protocol.UserMessage, error)
}
```

The method would return currently pending messages in arrival order and claim
them atomically. The current runtime instead claims one durable submission per
fresh run.

The native CLI submits through the same runtime API; there is no separate
one-shot persistence or execution path.

## Semantic conversation shape

The canonical sequence is:

```text
UserMessage
AssistantMessage [text, tool_call, text, ...]
ToolResult
ToolResult
AssistantMessage
...
```

A `ToolResult` is not semantically a user message, even though Anthropic's
wire format places `tool_result` blocks in a following user-role message.
OpenAI Responses represents the same result as a `function_call_output` item.
Provider adapters perform that translation.

### User messages

The initial public shape is:

```json
{
  "parts": [
    {"type": "text", "text": "hello"}
  ]
}
```

The part container is intentional even though only text is accepted in v1.
Future image or attachment parts must be added as semantic content, not as
provider-specific `image_url`, base64, or upload objects.

### Assistant messages

Assistant parts preserve provider order:

```text
text("I'll inspect that")
tool_call(id="call-1", kind="read_file", args={...})
text("Then I'll summarize it")
```

Text can be streamed to the channel before tools execute. The complete
assistant message is not committed until the provider response is complete.
An incomplete provider stream is discarded; tools are never executed from an
incomplete assistant response. Individual token deltas are transient and are
not part of durable conversation or event history.

### Tool results

Each result has the matching tool-call ID, success/error observation, and
structured result payload where available. Tools execute concurrently, but
results are committed and presented to the provider in the original planned
call order.

The runtime never retries a tool action. A normal tool error is returned to
the brain, which may decide to issue another action.

## Crash recovery

The durable ordering is:

```text
1. persist the UserMessage;
2. persist the complete AssistantMessage and all tool calls;
3. execute tools;
4. persist one ToolResult per call;
5. continue the conversation.
```

On startup, recovery scans the active transcript for assistant tool calls with
no result. For each unresolved call it appends a normal failed result whose
observation explicitly says that execution was interrupted and the outcome is
unknown. This repairs the provider-required call/result pairing without
silently repeating a side effect. The agent decides what to do next.

Provider requests may be retried with backoff. A provider retry is not a tool
retry: it occurs before an incomplete assistant turn is committed or after a
complete tool-result batch has been returned to the model.

Background store failures are reported through the manager's supervision
callback. The shipped server treats them as fatal and closes active HTTP/SSE
connections, so a client receives a transport failure rather than waiting
forever on a worker that can no longer commit state. Durable recovery runs when
the server next opens the store.

## Durable state

The target runtime uses one transactional store with purpose-specific state:

```text
conversations
messages
message_parts
submissions
runs
tool_calls
events             // client delivery outbox
```

`messages` and `message_parts` are the canonical provider-neutral conversation
history. The runtime queries them directly when hydrating a brain, and the HTTP
API queries them directly when hydrating a client. Neither path rebuilds a
conversation by replaying events.

Submissions own message acceptance and idempotency. Runs own execution state.
Tool-call records own durable pre-execution intent and terminal outcome. Each
authoritative change and its durable client event commit in the same
transaction.

The event outbox supports bounded catch-up between a snapshot and a live SSE
subscription. It is not a second conversation history. Token deltas, tool
progress, and heartbeats bypass the outbox and are transient.

The manager depends on the consumer-owned `runtime.Store` interface. Its
operations describe atomic runtime transitions rather than SQL tables, so a
PostgreSQL implementation can replace SQLite without changing scheduling,
transport, or agent execution. The composing process constructs the store and
owns its lifecycle.

The initial local transactional implementation is `runtime/sqlite.Store`. The
composition root imports both packages and injects the store into the manager.
The runtime remains single-process in v1 and protects each conversation with an
in-process lock; multiple processes additionally require database-backed
atomic claims or leases.

### Implementation

The SQLite database is stored at `<state-dir>/runtime.db`. It uses WAL mode,
foreign keys, immediate write transactions, and full synchronous commits.
The runtime database is authoritative. SQLite-specific opening, migration, and
connection policy are confined to `runtime/sqlite`; the manager only consumes
the `runtime.Store` contract. Store shutdown belongs to the composing process,
not the manager.

## HTTP API

The first server binds to `127.0.0.1` and has no authentication. It must not
bind publicly without a future authentication decision.

### Create a conversation

```http
POST /v1/conversations
```

Response:

```json
{
  "conversation_id": "..."
}
```

The server creates the conversation and its underlying pons session. The
agent is the single configured agent, `pons`.

### Hydrate a conversation

```http
GET /v1/conversations/{conversation_id}
```

The response is a renderable snapshot containing conversation metadata,
semantic messages and ordered parts, submission status, active run/tool status, and an
`event_cursor`. Message history may be paginated, but the returned cursor and
state must represent one consistent read.

Both initial clients and reconnecting clients render this snapshot. They do
not reconstruct the UI from historical SSE events.

### Subscribe to events

```http
GET /v1/conversations/{conversation_id}/events
Accept: text/event-stream
```

The CLI may connect before submitting a message. A client reconnecting to an
existing conversation first fetches its snapshot, then subscribes after the
snapshot's `event_cursor`. Durable events committed in between are replayed.
If the requested cursor is older than the retained outbox, the server requires
a new snapshot.

Durable client events are entity-shaped updates that can be applied
idempotently to the same local model returned by the snapshot API:

```text
message.upserted
message.removed
submission.updated
tool_call.updated
run.updated
conversation.updated
```

Transient live events are:

```text
assistant.delta
tool.progress
heartbeat
```

Transient events are never persisted in the durable event outbox and do not
receive a replay guarantee or advance the durable cursor. The complete
assistant message and terminal tool state replace any live draft or progress
display.

Each durable event has a cursor plus correlation fields where applicable:

```text
conversation_id
run_id
inbound_message_id
message | submission | tool_call | run
```

### Submit a message

```http
POST /v1/conversations/{conversation_id}/messages
Idempotency-Key: <stable-client-key>
Content-Type: application/json
```

Body:

```json
{
  "parts": [
    {"type": "text", "text": "hello"}
  ]
}
```

The server durably accepts the message before returning:

```http
202 Accepted
```

with the conversation and inbound message IDs. Retrying with the same
idempotency key must not append a duplicate user message.

The CLI wraps the API as:

```text
new conversation: create -> connect SSE -> submit -> consume events
existing conversation: fetch snapshot -> render -> connect SSE after cursor
```

A combined convenience endpoint can be added later; it is not the primitive
contract.

## Future extension points

Continue the runtime in this order:

1. provider callbacks that feed `assistant.delta` into the implemented transient stream;
2. bounded event retention and the snapshot-required response for expired cursors;
3. a second runtime channel;
4. explicit cross-channel identity linking and handoff;
5. durable multi-process claims/leases; and
6. authenticated remote HTTP deployment.

A future handoff always forks a completed active transcript path into a new
conversation. It does not roll back or clone real-world tool effects.
