# ADR-0023: One durable event log for conversation history and client events

**Status:** Accepted
**Supersedes:** ADR-0010's separate semantic-message tables and disposable event outbox

## Context

The original runtime stored each complete semantic message in `messages` and
`message_parts`, then copied it into an `events` row for client delivery. The
same conversation content therefore had two durable representations. The event
outbox was allowed to expire, so it could not be used to reconstruct model
context or explain what a run had processed.

## Decision

Each conversation has one append-only, cursor-ordered durable event log in
`events`. `input.accepted`, `assistant.output.committed`, and
`tool.outcome.recorded` hold distinct content facts. Client messages and future
model context are projections of those events. Content IDs are indexed and
unique within the conversation. Event log entries are retained
for the life of the conversation; dropping old entries requires a future
checkpoint and retention design that preserves context and replay semantics.

The same log also records discrete private `agent.*` entries for agent and
iteration boundaries, completed model output, tool execution start, and
context compaction. The brain's prepared input, including its environment
header, is another private event. Each event type has its own payload:
iteration number, ordered model content, tool execution identity, or compaction
summary. Model output keeps ordered text, tool calls, and provider-native items
when returned by the provider. A provider-native reasoning item may be
opaque or encrypted; the runtime does not invent readable thinking. Checked
event hooks persist these entries before the loop advances. `agent.*` entries
share the durable cursor but are excluded from client SSE replay and snapshots;
the client derives messages and tool state from public facts. The store
offers an internal agent-event reader for inspection and future projections.

Input acceptance and admission are separate facts. `input.accepted` makes a
new input visible to clients when accepted. It carries the content, idempotency
key, trusted source kind and adapter, optional source principal or agent,
causation ID, target agent, and selected agent revision. Current HTTP ingress
sets `human/http` and records the conversation's named agent and selected
definition revision. Internal scheduled and delegated ingress uses the
same acceptance path with `system` or `agent` sources. `input.admitted` only
references the accepted input ID when a run claims it; it never copies content.
Model history includes previously admitted user
messages and complete assistant/tool messages in event log order; it excludes
queued input even if that input arrived before the active run finished. An
active run still does not accept interjections.

Submissions, runs, and tool calls remain indexed operational state for
idempotency, scheduling, workspace exclusion, and recovery. The tool-call index
stores status and identity; arguments and outcomes live in events and are
projected for snapshots. Their transitions
and corresponding facts commit in the same SQLite transaction. Run lifecycle
events are `run.started`, `run.completed`, `run.failed`, and `run.stopped`;
a failed or stopped run projects a notice message for clients. An assistant output
with tool calls records the requested tools; a tool outcome records their
terminal result, including an interrupted outcome whose execution is uncertain.
Client snapshots are projected from the same ordered public events that live
clients replay. The operational indexes are not read to assemble the snapshot.
Messages retain event cursor order, matching live delivery even when new input
arrives during an active run. Clients fetch
a snapshot with cursor N, then subscribe after N. Transient token deltas, tool
progress, and heartbeats have no cursor and are not persisted.

On SQLite open, Pons rebuilds the submission, run, and tool-call indexes from
the ordered event log in one transaction, then recovers any run left active by
the previous process. Invalid or inconsistent events abort the rebuild and
leave the prior indexes intact. A recovered run records interrupted outcomes
for tools still requested; completed tool outcomes are preserved. This makes
the log sufficient to restore scheduling state without retrying an uncertain
tool execution. Startup cost grows with the number of operational events until
an indexed checkpoint or incremental reconciliation is introduced.

Compaction events include the full provider-neutral context after compaction,
including opaque provider items. On a later run, the runtime starts from the
latest compaction checkpoint and projects admitted input, committed model
output, and tool results after it. A model response with no committed assistant
message is excluded from replay. Prepared input events replace the plain
admitted message in projected context. Without a checkpoint, the same
projection starts at the beginning of the log. The store gives the runner one
ordered context value, with checkpoint context followed by projected events.
Committed assistant output and accepted input cover runners that do not emit
private model or prepared-input events.

The event log is linear for now. Fork heads and mid-run input admission are
separate decisions. Pons has no
runtime approval gate today, so there is no approval decision to record yet;
any future gate must append request and decision events before tool execution.

## Consequences

- There is one durable content fact per accepted input, committed assistant
  output, or resolved tool, and one replay cursor.
- The log contains private agent detail without exposing provider state or
  internal execution stages on the client stream.
- Context projection scans the event log at claim time. Large histories will
  need indexed projections or checkpoints before this becomes a scale problem.
- Older SQLite runtime state directories use an unsupported schema and must be
  recreated under the project's pre-compatibility policy.
