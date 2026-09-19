# ADR-0010: Runtime conversations use transactional state and an event outbox

**Status:** Accepted
**Date:** 2026-09-19
**Related:** ADR-0002, ADR-0005, ADR-0008

## Context

The first long-lived runtime stores the semantic transcript through
`sessions.Store` and keeps inbox, outbox, idempotency, and reconnectable events
in a separate per-conversation JSON snapshot. That was sufficient to prove the
server, scheduling, recovery, and SSE shape, but it creates overlapping durable
representations and cannot atomically update the transcript and runtime state.

A web or terminal client has two different needs:

- load the current conversation efficiently when it connects or reconnects;
- observe changes such as new messages, tool status, and run status while it is
  connected.

Replaying every historical lifecycle or token event to build either the UI or
the next LLM request would make projection code part of every hot path. It would
also retain presentation details indefinitely and make old event schemas a
permanent reconstruction dependency.

## Decision

### 1. The runtime uses one transactional store, not one universal journal

Runtime persistence is composed of purpose-specific durable state in one
transactional store:

```text
conversations
messages
message_parts
submissions
runs
tool_calls
events             // delivery outbox
```

The initial local implementation uses SQLite. The runtime manager consumes an
injected, consumer-owned `runtime.Store`; it neither opens nor closes a
database. Other implementations, including PostgreSQL, can satisfy the same
domain-level transition contract, but must provide the atomicity and
consistent snapshot semantics described below.

"One source of truth" means that each fact has one authoritative owner. It
does not require every fact to be reconstructed from one append-only event
stream.

### 2. Semantic messages are canonical and directly queryable

`messages` and `message_parts` are the authoritative provider-neutral
conversation history. They contain stable identities and ordered semantic
parts for:

```text
UserMessage
AssistantMessage [text, tool_call, text, ...]
ToolResult
```

Both LLM hydration and client snapshots read this representation directly.
They do not replay runtime events to reconstruct it. Provider adapters translate
the semantic messages into provider-specific request shapes.

Only complete assistant messages participate in future LLM context. A complete
assistant message containing tool calls is committed before any of those calls
execute.

### 3. Operational state has explicit owners

Submissions own durable acceptance, idempotency keys, and pending/completed
state. Runs own execution lifecycle. Tool-call records own pre-execution intent,
outcome status, and the identity of the canonical `ToolResult` message.

Runtime recovery queries those records directly. For example, a committed tool
call without a committed result is resolved as interrupted with an unknown
outcome; the runtime does not infer that state by folding presentation events
and does not automatically repeat the side effect.

Ephemeral leases, semaphore occupancy, and live subscriber lists remain
in-memory coordination. They are not conversation history.

Message parts and event envelopes retain opaque metadata maps. Adapters may
preserve protocol-specific annotations there without making those protocols
part of the canonical runtime model. Unknown semantic part types remain an
explicit capability/versioning decision rather than being flattened into text.

### 4. Durable client events form a transactional outbox

Every externally observable durable mutation appends an event in the same
transaction as the authoritative state change. Examples include:

```text
message.upserted
message.removed
submission.updated
tool_call.updated
run.updated
conversation.updated
```

The event payload is entity-shaped and idempotent where practical, so clients
can upsert the same object shape returned by the snapshot API. An event is a
notification of committed state, not a competing copy of the conversation.

Events have a monotonically increasing cursor scoped to the conversation. The
outbox may be retained for a bounded period. If a requested cursor predates the
retained range, the server instructs the client to fetch a new snapshot.

### 5. Token streaming and progress are transient

The live stream may also carry presentation events such as:

```text
assistant.delta
tool.progress
heartbeat
```

These events are best-effort and are not written to the durable event outbox.
In particular, token deltas are never required to reconstruct an assistant
message. The complete assistant message is the durable boundary. Transient SSE
events do not advance the durable reconnect cursor.

A reconnecting client may miss earlier deltas from an active response. It can
show that the assistant is responding and replace the draft when the complete
message commits. Persisted or in-memory draft snapshots may be added later as
an optimization without making deltas durable history.

Tool intent and terminal tool status are durable because they are required for
recovery. Tool progress between those states is transient.

### 6. Clients hydrate from snapshots, then consume events

The server exposes a conversation view containing:

- conversation metadata and current status;
- semantic messages and parts;
- active run and tool-call state needed for rendering; and
- the durable event cursor corresponding to that view.

A client connects or reconnects as follows:

```text
fetch conversation snapshot at cursor N
        -> render the snapshot
        -> subscribe to SSE after N
        -> apply durable entity updates and transient live events
```

The snapshot and cursor are read consistently. Because authoritative mutations
and outbox events commit together, durable changes occurring between the
snapshot response and SSE subscription are replayed after `N`.

The client renders one local conversation model. A snapshot replaces that
model; entity events update it. The client does not maintain a separate
historical-event renderer.

### 7. State changes and notifications commit together

Representative transactions are:

```text
accept message:
  insert UserMessage
  insert pending submission with idempotency key
  append message/submission events

commit assistant tool turn:
  insert complete AssistantMessage and ordered parts
  insert requested tool calls
  append message/tool-call events

complete tool call:
  insert ToolResult
  update tool-call outcome and result-message identity
  append tool-call/message events
```

No tool executes until its complete assistant turn and requested tool-call
state have committed.

## Relationship to legacy session storage

ADR-0002 and ADR-0005 describe the earlier finite harness and its JSONL session
tree. The server-centred application supersedes that composition. The server
does not register `sessionrecorder`, use `sessions.Store`, or write an
independently authoritative JSONL transcript beside its database.

Inspectable JSONL can be implemented as an explicit export of canonical
runtime messages. It is not a runtime storage backend and does not participate
in recovery, model hydration, or client hydration.

The store is an injected infrastructure adapter, not a `pons.Plugin`.
`pons.Plugin` is the composition seam for capabilities participating in the
finite brain/hands loop. Persistence must wrap message acceptance, run claims,
tool intent/results, and outbox events in transactions outside that loop; a
turn observer such as `sessionrecorder` cannot provide those guarantees.

This decision supersedes ADR-0008's v1 choice of a separate mutable JSON state
snapshot for the target runtime architecture. The implemented snapshot-based
baseline remains a migration source until the transactional store replaces it.

## Alternatives considered

- **Rebuild all state from one event journal:** not selected. It makes LLM and
  UI hydration depend on replay and projection-version compatibility in the
  hot path.
- **Persist transcript and runtime JSON independently:** used by the baseline
  but not selected as the target. Cross-store updates cannot be atomic and need
  reconciliation.
- **Use SSE history as the UI snapshot:** not selected. Event retention and UI
  hydration have different lifecycles, and transient deltas must remain
  discardable.
- **Keep all events forever:** not selected. Durable outbox events may be
  compacted or pruned after clients can safely fall back to a fresh snapshot.

## Consequences

- LLM and UI hydration are ordinary indexed reads rather than event replay.
- Durable state and its client notification cannot diverge across a committed
  transaction.
- Reconnecting clients get a coherent snapshot plus lossless durable catch-up.
- Token streaming remains low-latency and disposable.
- The shipped server gains a SQLite dependency and schema migrations. A future
  database adapter can replace it behind `runtime.Store`.
- Materialized state and the event outbox have explicit retention and ownership
  rules instead of acting as overlapping sources of truth.

## References

- `docs/adr-0008-runtime-orchestration-layer.md` — runtime ownership and API
- `docs/runtime-v1.md` — concrete runtime design and migration status
- `docs/adr-0005-transcript-model-vs-engine.md` — session model and engines
- `runtime/` — transactional state, snapshot API, and durable/transient stream
