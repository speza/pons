# pons runtime v1 design

**Status:** Design target
**Related:** [ADR-0008](adr-0008-runtime-orchestration-layer.md)

This document turns ADR-0008 into the smallest useful local runtime shape. It
is an implementation guide, not a second agent protocol.

## Goals

- keep the pons core finite and reusable;
- accept messages through one local HTTP channel;
- make the CLI an HTTP client;
- persist accepted work across process restarts;
- stream live assistant output when possible; and
- resume a conversation by hydrating a fresh brain from its transcript.

## Out of scope

The first runtime does not include:

- WhatsApp, Slack, Telegram, or other remote channels;
- cross-channel identity linking or handoff;
- multiple agents;
- multi-process workers or distributed leases;
- authentication beyond loopback binding;
- image, audio, or file parts; or
- replay of historical token deltas.

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
provider emits multiple output blocks.

### Tool result

The observation for one tool call. It is correlated by the tool-call ID and is
persisted separately from the assistant message. A provider adapter may group
several results into the provider-specific request shape.

## Conversation lifecycle

```text
create conversation
        ↓
accept inbound message into durable inbox
        ↓
hydrate a fresh Core/Brain from the transcript
        ↓
run agent turns
        ↓
stream assistant/tool events
        ↓
persist the complete assistant response
        ↓
mark inbound work complete and tear down Core/Brain
        ↓
wait for the next inbound message
```

A message arriving while tools are running is placed in the same
conversation's mailbox. The active run waits for every tool in the current
batch, records results in planned call order, then presents the pending user
messages as separate messages before the next provider request.

A message arriving after a text-only response starts the next run. It uses the
same conversation transcript but a newly constructed Core and Brain.

## Core input seam

Initial and steering messages use one source abstraction:

```go
type MessageSource interface {
    GetMessages(ctx context.Context) ([]protocol.UserMessage, error)
}
```

The method returns currently pending messages in arrival order and claims them
atomically. An empty result is valid at a turn boundary. The runtime seeds the
source with the first accepted message; there is no separate initial-message
API.

The existing string-based one-shot API can remain as a convenience wrapper
around a source containing one text message.

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
incomplete assistant response.

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

## Runtime state

Conversation history remains in the existing append-only session transcript.
Operational state is separate:

```text
runtime/<conversation-id>/state.json
```

The state snapshot contains only runtime concerns:

- pending and processing inbox records;
- outbox records for completed assistant responses;
- processing/error state;
- event sequence/current durable state; and
- completed inbound IDs for deduplication.

Each update writes a temporary file, flushes it, and atomically renames it into
place. The first runtime is single-process and protects each conversation with
an in-process lock. A multi-process runtime requires a shared store with
atomic claims or leases and is not part of v1.

Completed inbound IDs remain for the lifetime of the conversation. The message
body can be removed from operational state after the response is durably
queued; the transcript remains the historical source. A deleted or archived
conversation may delete its deduplication state.

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

### Subscribe to events

```http
GET /v1/conversations/{conversation_id}/events
Accept: text/event-stream
```

The client may connect before submitting a message. Reconnecting clients use
an event ID/cursor where available and receive current durable state. Live
assistant deltas and tool-progress events are best-effort and do not need to
be replayed.

The initial event vocabulary is:

```text
message.accepted
assistant.started
assistant.delta
assistant.completed
tool.started
tool.completed
run.error
run.idle
```

Each event has an event ID plus correlation fields where applicable:

```text
conversation_id
run_id
inbound_message_id
response_id
tool_call_id
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
create conversation
connect SSE
submit message
consume events
```

A combined convenience endpoint can be added later; it is not the primitive
contract.

## Future extension points

When the first vertical slice is stable, add extensions in this order:

1. richer assistant/provider content parts;
2. a second runtime channel;
3. explicit cross-channel identity linking and handoff;
4. durable multi-process claims/leases; and
5. authenticated remote HTTP deployment.

A future handoff always forks a completed active transcript path into a new
conversation. It does not roll back or clone real-world tool effects.
