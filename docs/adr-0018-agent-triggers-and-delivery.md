# ADR-0018: Triggers and delivery surround durable agent submissions

**Status:** Proposed
**Date:** 2026-09-23
**Related:** ADR-0008, ADR-0010, ADR-0011, ADR-0012, ADR-0016, ADR-0017, ADR-0021

## Context

ADR-0016 gives an agent a stable identity and a mailbox of durable submissions.
That supports delegated tasks, but an ongoing assistant also needs trusted
ways to wake up and reliable ways to reach a person or system. A timer, chat
message, webhook, and child result should not each invent a runner. Nor should
ordinary replies depend on a model selecting a channel-send tool.

The native HTTP server is loopback-only and has no principal authorization.
External connectors must not expose it directly or treat transport fields as
trusted source identity. A scheduled check may also finish successfully with
nothing to report; an empty answer caused by an interrupted run is different.

## Decision

### 1. One ingress contract

Each trusted ingress adapter authenticates or validates its source, resolves
an agent and conversation, and submits through the same store acceptance path
as a human message. The accepted record carries runtime-owned metadata:

```text
source.kind              human | agent | system
source.adapter_id        native | schedule | connector ID | internal
source.external_event_id adapter-scoped idempotency identity, when available
source.principal_id      verified human or service identity, when available
root_id                  new external lineage or inherited child-result lineage
causation_id             event or transition which caused the submission
```

Adapters cannot set agent source or inherit another lineage. Raw sender
names, webhook bodies, and channel labels remain untrusted content. The host
records verified attribution separately and renders it separately to brains
and clients. An adapter must validate payload size and parts before accepting
work. A failure before acceptance returns to the source; after acceptance,
the runtime owns completion and delivery independently of the connection.

The native client path remains the first adapter. A connector may bind the
tuple `(adapter_id, external_account_id, external_thread_id)` to one agent and
root conversation, or to one configured work-item wake under ADR-0021. The
binding is stored and uniquely constrained; the first contract does not
fan out one event to several destinations. A connector cannot select an
arbitrary private child conversation. A new thread gets a new conversation.
A schedule may target a configured conversation or create one per firing.
Both choices create a fresh root lineage per firing.

This avoids making one ever-growing transcript the definition of agent memory.

### 2. Durable schedules and external events

Schedule definitions are administrator-owned configuration with stable IDs,
agent, conversation policy or work-item target, time zone, recurrence, payload
template, and enabled state. The store persists `next_fire_at` and the last
accepted slot. A work-item target must be owned by the configured agent. The
scheduler atomically advances the slot and accepts either
a direct submission or a durable pending work-item wake with idempotency key
`(schedule_id, nominal_fire_at)`. A restart or duplicate wake cannot execute
one slot twice. A delayed runtime accepts at most the latest
missed slot as one catch-up firing, records how many slots it skipped, and
computes the next future slot in the configured time zone. Disabling a
schedule prevents future acceptance but does not silently cancel accepted
lineages. For local wall-clock recurrences, a nonexistent time is skipped and
a repeated time fires once at its first occurrence. The nominal slot remains
the idempotency identity across time-zone changes.

External webhooks and channel adapters authenticate at their own boundary,
enforce rate and size limits, and deduplicate by adapter and stable event ID.
Binding policy allowlists verified principals or groups before routing them
to an agent.

If a source supplies no stable ID, the adapter must state its weaker delivery
guarantee; it cannot claim exactly-once acceptance. Connectors ignore their
own outbound messages and bound event loops. An authenticated connector or
gateway is required before accepting events from untrusted networks; native
loopback HTTP is not that gateway.

### 3. Terminal outcome is explicit

An interactive root lineage completes with a final assistant response as in
ADR-0016. A system-triggered root may instead complete with an explicit
`no_update` outcome. The runtime records an outcome kind, not an inference
from an empty string. `no_update` creates no outbound message and is visible
in lineage status and audit events. A stopped, exhausted, or crashed run with
no explicit outcome fails or waits according to ADR-0016; it never becomes a
successful silent check by accident.

The runner gains a host-side, run-scoped outcome capability for system roots.
Its model-facing operation is conceptually `complete_no_update()`; it stages
the outcome before the model finishes. Only the latest successful root run's
staged outcome can commit when the lineage becomes terminal; an earlier run's
directive is discarded if child results reactivate the root. A failed or stale
run cannot publish it. A human request or delegated child cannot use
`no_update` to evade its expected response. Staging `no_update` alongside a
final response is invalid. This capability is not handed to untrusted hands
or added as channel behavior in `Core`.

### 4. Delivery is a second durable boundary

An inbound binding may carry a host-configured delivery target. The model
cannot supply an arbitrary destination in its answer. Acceptance pins the
target and binding revision to the root lineage; a later binding edit cannot
silently redirect its result. When a root lineage
reaches a deliverable terminal result, the same transaction writes an
outbound intent keyed by `(root_id, target_id)`. The intent contains a bounded
projection of the final response and authorized artifact
references, never child transcripts or hidden credentials. A delivery worker
reads intents after commit, independently of agent worker slots. It does not
rerun the agent on transport failure.

```text
pending -> sending -> delivered
                   -> retryable | unknown | permanently_failed
retryable -> sending
```

Adapters with provider idempotency send the stable outbound key and may retry
timeouts with bounded backoff. If the provider cannot deduplicate and a send
may have succeeded before its acknowledgement was lost, the intent becomes
`unknown` and is not automatically resent. Operators or a connector-specific
reconciler may resolve it. Definitive rejection becomes
`permanently_failed`; transient failures retry within configured limits.
Status and safe error codes are durable and visible to the owning client.
Agent success and delivery success are separate facts.

Only terminal root responses are delivered by default. Explicit progress
delivery needs its own bounded, authorized policy and cannot quietly turn
every internal event into an external message. Native SSE remains a cursor
view of runtime events, not this outbound transport outbox.

### 5. Security and lifecycle

Connector credentials stay in host composition and are never exposed to
brains or hands. Ingress and egress policies use verified principal, binding,
agent, destination, and content. A recipient allowlist for delegation does
not grant a channel delivery target. Revoking a binding prevents new ingress
and sends, including sends from already accepted work. The delivery worker
rechecks revocation immediately before send and records a safe terminal
failure when it suppresses an intent. Accepted work remains auditable. A
cancelled lineage produces no
new outbound intent; a previously sent message cannot be retracted.

The first implementation may support one local schedule adapter and one
scripted delivery adapter. Remote chat providers and webhook gateways can be
added without changing the canonical submission or outbox transitions.

## Store and implementation sequence

Minimum durable additions are schedule definitions and slots, connector
bindings, verified source attribution, explicit root outcome kind, and
outbound intents with attempts and status. Store operations accept a trigger
slot or external event atomically with its submission, and commit a terminal
lineage with its outbound intent atomically. Reconciliation scans due slots
and pending delivery in bounded batches; in-memory wakeups are hints.
Trusted management surfaces create, inspect, and disable schedules and
bindings, and expose delivery status; model-controlled hands cannot call them.

1. Add source attribution, binding resolution, and explicit root outcomes.
2. Add due-schedule acceptance and restart reconciliation.
3. Add outbound intents and a scripted delivery adapter.
4. Add authenticated connectors only with source verification, delivery
   reconciliation, and loop controls.

## Verification requirements

Deterministic tests without provider credentials or external network prove:

- duplicate schedule ticks and connector events create one submission;
- restart catches up at most one missed schedule slot and records skipped
  slots across time-zone transitions;
- source labels in payload text cannot forge verified attribution or select a
  private child conversation;
- a system root explicitly reports `no_update`, while an empty or exhausted
  run does not complete successfully;
- one deliverable completed lineage with a bound target creates one outbound
  intent despite retries and cancellation races; `no_update` creates none;
- delivery failure never reruns the agent, and unknown non-idempotent sends
  are not retried automatically;
- a disabled schedule or revoked binding rejects new work without erasing
  already accepted history; and
- no model-controlled hands process can reach connector credentials or the
  native runtime listener in a delegation-enabled composition.

## Consequences and non-goals

Ongoing agents can wake from human or system events while `Core.Run` remains
finite. Accepted work and outbound delivery have separate durable lifecycles.
The additional store rows and adapter-specific authentication are the cost of
honest restart and delivery behavior.

This ADR does not define cross-channel identity linking, public webhook
hosting, arbitrary model-selected destinations, exactly-once delivery to
providers without idempotency, or autonomous goal planning. ADR-0021 owns
durable work that spans multiple root lineages.
