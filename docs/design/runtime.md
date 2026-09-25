# pons runtime v1 design

**Status:** Transactional runtime and client synchronization implemented
**Related:** [ADR-0008](../adr/adr-0008-runtime-orchestration-layer.md),
[ADR-0010](../adr/adr-0010-runtime-state-and-client-synchronization.md),
[ADR-0012](../adr/adr-0012-scalable-runtime-coordination.md),
[ADR-0017](../adr/adr-0017-one-runtime-one-trust-domain.md), and
[ADR-0023](../adr/adr-0023-runtime-event-log.md)

This document turns ADR-0008 into the smallest useful local runtime shape. It
is an implementation guide, not a second agent protocol.

One runtime instance serves one trusted administrative domain. It is not a
multi-tenant boundary; hosted products isolate domains in separate runtime
cells as specified by ADR-0017.

## Goals

- keep the pons core finite and reusable;
- accept messages through one local HTTP channel;
- make the CLI an HTTP client;
- persist accepted work across process restarts;
- let clients hydrate a complete renderable conversation without event replay;
- stream live assistant output when possible; and
- resume a conversation from the durable event log, using a compaction
  checkpoint when one exists.

The runtime implementation lives in `runtime/`; the native wire adapter lives
in `runtime/httptransport`. `pons serve` composes them with the configured
agent and `pons client` uses that HTTP/SSE adapter. The default CLI invocation
bundles both lifecycles but still communicates through the same loopback HTTP
and SSE path.

Runs are globally bounded and SQLite claims runnable work only when its
conversation and workspace have no active run. This is transactional local
exclusion, not a renewable distributed lease. Each run starts a fresh hands
session in the configured execution environment, registers only its
discovered proxy tools, and closes the session before becoming idle. The
Seatbelt and E2B providers both launch `pons-hands`; the server has no
in-process hands and refuses to start without a provider.

The runtime persists one durable conversation event log and indexed
operational state in SQLite. Complete semantic messages are event log entries
used for both model context and client replay. It does not run a second session
recorder or write an independently authoritative JSONL transcript.

The manager exposes transport-independent operations for creation, submission,
snapshot reads, and subscriptions. HTTP routing, SSE framing, cursor headers,
and the native network client belong to `runtime/httptransport`. Future AG-UI,
ACP, or channel adapters project the same canonical state and events without
changing runtime persistence or scheduling.

## Black-box verification

The deterministic integration suite is run with:

```sh
make test-integration
make test-integration-race
```

It builds `cmd/pons` and `cmd/pons-hands` and launches the server, with hands
under Seatbelt, against a local, OpenAI-compatible fake provider, so it needs
macOS; on other platforms the targets fail rather than skip. The test then uses the real loopback API and
SSE stream, persists through SQLite, performs a real `read_file` tool
round-trip, kills and restarts the server, replays the durable event cursor,
checks idempotency, and continues the conversation through the compiled CLI.
It has no API-key or external-network dependency. The default `make test`
suite covers the same server flow over HTTP on any platform, serving the real
`pons-hands` tool host over in-memory pipes. The live model/provider and
Seatbelt smoke path remains intentionally separate under `make smoke-runtime`.

The runtime queues a message arriving during an active run as the next fresh
burst. It deliberately does not inject later submissions into an active run;
doing so would require a separate safe-boundary steering contract. Likewise,
v1 currently streams lifecycle and tool events; token-level `assistant.delta`
emission depends on provider streaming support.

## Out of scope

The first runtime does not include:

- WhatsApp, Slack, Telegram, or other remote channels;
- cross-channel identity linking or handoff;
- persistent agents and tasks (proposed in
  [ADR-0016](../adr/adr-0016-persistent-agents-and-async-messaging.md));
- mutually untrusted tenants in one runtime;
- multi-process workers or distributed leases;
- authentication beyond loopback binding;
- image, audio, or file parts; or
- persistence or replay of historical token deltas.

## Runtime terms

### Conversation

The public HTTP resource and logical interaction. Its opaque server-generated
ID scopes one canonical event log and its operational indexes in the runtime
store. A conversation also records its selected execution
environment; the server validates this opaque choice against the providers it
configured before persisting the conversation. The server configures one
provider, `seatbelt` or `e2b`, and an unset choice selects it.

`POST /v1/conversations` requires a host workspace or a Git repository with a
base commit or branch.
The environment may be selected alongside the source:

```json
{"workspace":"/absolute/path/to/workspace","environment":"seatbelt"}
```

`GET /v1/options` returns the environment names and default exposed by the
server so clients do not have to guess which providers are available.

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

The observation for one tool call. It is correlated by the tool-call ID within
its run and is persisted separately from the assistant message. Provider IDs
need not be globally unique; durable and client identity is the pair of run ID
and tool-call ID. A provider adapter may group several results into the
provider-specific request shape.

## Conversation lifecycle

```text
create conversation
        ↓
atomically append input.accepted and index its submission
        ↓
hydrate a fresh Core/Brain from the latest compaction checkpoint and later
committed events, or project from the start of the log without a checkpoint
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

## Core response seam

Brains implement `ControlPort.Respond` and return an `AssistantResponse`
containing both executable actions and the ordered provider-neutral assistant
parts which produced them. The core emits that complete response through its
checked event sink before emitting any action-start event. The runtime commits
the assistant message and every requested tool-call record atomically at that
boundary.

## Active-run steering

Injecting messages into an already-active run remains follow-up work. No
steering interface is part of the current contract: the runtime claims one
durable submission per fresh run. A future design must define the safe model
boundary, durable claim semantics, and provider-neutral message shape before
introducing such an interface.

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
submissions
runs
tool_calls
events             // canonical log; client replay is a filtered projection
```

`input.accepted`, `assistant.output.committed`, and `tool.outcome.recorded`
hold the canonical conversation content. The runtime projects admitted input,
prepared input, committed model output, and tool results from the log, starting
at the latest compaction checkpoint when one exists. The HTTP snapshot projects messages,
including queued user input, into the client view in event cursor order.

Submissions index acceptance and idempotency for claiming. The accepted-input
event carries the idempotency key, trusted source, causation ID, target agent,
selected revision, and content. HTTP ingress records a human
source; trusted internal ingress can record scheduled or agent input through
the same path. `input.admitted` references that accepted input when claimed.
Runs index execution state.
Tool-call records index identity and terminal status for recovery; arguments and
results are projected from assistant-output and tool-outcome events. Snapshots
project all client state from public events in cursor order. Each
operational change and its event log entry commit in the same
transaction.
SQLite rebuilds the submission, run, and tool-call indexes from the log when
the store opens. A malformed log aborts startup; a valid active run is then
failed during recovery, with still-requested tools marked interrupted rather
than executed again.

The event log supports catch-up between a snapshot and a live SSE
subscription. It is retained as conversation history. Token deltas, tool
progress and heartbeats bypass it and are transient. Curated
environment setup steps are durable events and also appear in snapshots so a
client can reopen the setup log after a disconnect.

Discrete private `agent.*` entries share the log cursor. Their typed payloads
record agent and iteration boundaries, prepared user input, complete model
output (including opaque provider items when available), tool execution stages,
and compaction summaries. Client replay filters them out; the internal store
reader can inspect them. Compaction events
also contain the context after compaction. The store gives each claimed run one
ordered context projected from the latest checkpoint and committed events that
followed it, or from the start of the log. The runner has no separate message
history or compaction fallback. The current runtime has no approval gate.

The manager depends on the consumer-owned `runtime.Store` interface. Its
operations describe atomic runtime transitions rather than SQL tables, so a
PostgreSQL implementation can replace SQLite without changing scheduling,
transport, or agent execution. The composing process constructs the store and
owns its lifecycle.

The initial local transactional implementation is `runtime/sqlite.Store`. The
composition root imports both packages and injects the store into the manager.
The runtime remains single-process in v1. The manager claims work lazily rather
than loading every conversation at startup, creates live conversation state
only for active/API/subscriber use, and starts no more than `MaxConcurrent` run
goroutines. SQLite atomically enforces one running submission per conversation
and exclusive workspace ownership. Process-local locks protect subscriber
delivery and live object state; they are not execution ownership.

Submissions and run completions send coalesced wake hints to the scheduler. A
low-frequency bounded repair scan uses the same atomic claim operation, so a
lost hint cannot strand durable work and does not require enumerating every
conversation. SQLite startup recovery assumes one exclusive manager and marks
abandoned requested tools interrupted with an unknown outcome; it never
silently repeats them. Multiple live processes require renewable leases,
fencing on every run mutation, and cross-process runnable/outbox notification.
The SQLite store holds an exclusive advisory lock on the state directory for
its lifetime. A second process cannot open the same state while the first is
running; after a process exits, the OS releases the lock for restart recovery.

### Implementation

The SQLite database is stored at `<state-dir>/runtime.db`. It uses WAL mode,
foreign keys, immediate write transactions, and full synchronous commits.
Domain and store contracts use UTC `time.Time` values normalized to microsecond
precision. The SQLite adapter stores those instants as integer Unix
microseconds so indexed ordering and deadline comparisons are numeric; a future
PostgreSQL adapter should use `TIMESTAMPTZ` behind the same Go contract.
The runtime database is authoritative. SQLite-specific opening, schema
initialization, and connection policy are confined to `runtime/sqlite`; the
manager only consumes the `runtime.Store` contract. Store shutdown belongs to
the composing process, not the manager. During the current pre-compatibility
phase, schema versions are checked at startup and an older database is rejected
with instructions to recreate the runtime state directory rather than migrated.

## HTTP API

The first server binds to `127.0.0.1` and has no authentication. It must not
bind publicly without a future authentication decision. It rejects non-loopback
Host headers and cross-origin browser Origin headers to reduce DNS rebinding exposure. Other
processes running as the same user on the host remain trusted clients.

### Create a conversation

```http
POST /v1/conversations
Content-Type: application/json

{
  "workspace": "/absolute/path/to/workspace",
  "environment": "seatbelt"
}
```

For a Git-backed conversation, use:

```json
{
  "git_repository": "https://github.com/OWNER/REPO.git",
  "git_revision": "FULL_40_CHARACTER_COMMIT_ID",
  "git_all_repositories": false,
  "environment": "e2b"
}
```

To start from a branch, set `git_revision` to a fully qualified ref such as
`"refs/heads/main"` instead of a commit ID. The branch tip is fetched when the
workspace is first provisioned; subsequent runs restore its checkpoint.

The `environment` field may be omitted to use the default returned by
`GET /v1/options`.

Response:

```json
{
  "conversation_id": "...",
  "environment": "seatbelt"
}
```

The server creates the canonical conversation in the runtime store. Git source
and access options are immutable conversation metadata. Both repository fields
must be set together; `git_all_repositories` explicitly grants access to every
repository available to the server's GitHub App installation. Git conversations
do not specify a host workspace. Local, Seatbelt, and E2B archive conversations
instead provide `{"workspace":"/absolute/path/on/server"}`. The server
validates that path against its configured root and state directory at creation
and again before each run. It stores the canonical path so symlink aliases use
the same workspace lock. The agent is the single configured agent, `pons`.

### List conversations

```http
GET /v1/conversations
```

The response is an array of conversation metadata ordered newest first. It is
intended for clients that need to restore a conversation picker without
hydrating every conversation snapshot.

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
Durable event log entries remain replayable while the conversation exists.

Durable client events record facts or append curated environment progress.
Clients project messages, submissions, tool calls, and the active run from
them after loading a snapshot:

```text
input.accepted
input.admitted
assistant.output.committed
tool.outcome.recorded
run.started
run.completed
run.failed
environment.progress
```

Transient live events are:

```text
assistant.delta
tool.progress
heartbeat
```

Transient events are never persisted in the durable event log and do not
receive a replay guarantee or advance the durable cursor. The complete
assistant message and terminal tool state replace any live draft or progress
display.

`environment.progress` carries an `environment_progress` object with a stable
`step` identifier and a curated, human-readable `message`. Steps cover initial
workspace preparation, E2B startup, Git fetch and checkout, checkpoint restore,
and readiness. Warm runs that reconnect to the existing sandbox emit no setup
steps; replacing a sandbox emits restore and readiness steps. Each event is
scoped by `conversation_id`, `run_id`, and `inbound_message_id`, receives a
durable cursor, and appears in the snapshot's
`environment_events` list. The messages are deliberately authored by the
server and provider rather than copied from raw commands or provider output.

Each durable event has a cursor plus correlation fields where applicable:

```text
conversation_id
run_id
inbound_message_id
message | submission | tool_call | run | environment_progress
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

The server also serves the embedded browser client at `/` (with `/ui/` as an
alias). It uses the same snapshot, submission, and SSE endpoints described
above; it does not introduce a separate web-specific runtime model.

## Future extension points

Continue the runtime in this order:

1. provider callbacks that feed `assistant.delta` into the implemented transient stream;
2. bounded event retention and the snapshot-required response for expired cursors;
3. a second runtime channel;
4. explicit cross-channel identity linking and handoff;
5. renewable multi-process leases, fencing, and cross-process notification; and
6. authenticated remote HTTP deployment.

A future handoff always forks a completed active transcript path into a new
conversation. It does not roll back or clone real-world tool effects.
