# ADR-0021: Ongoing work and approvals wait durably, not in a running agent

**Status:** Proposed
**Implementation:** Not implemented
**Date:** 2026-09-23
**Related:** ADR-0008, ADR-0012, ADR-0014, ADR-0016, ADR-0018, ADR-0019

## Context

A lineage in ADR-0016 answers one external submission and all attached child
work. It is not an agent's lifetime. A coding task may need approval after the
requester disconnects; an ongoing assistant may monitor a subject for weeks
and wake on several schedules or events. Keeping a goroutine, model request,
workspace lock, or approval handler blocked for that duration defeats finite
execution and durable recovery.

The first ADR-0016 contract has no detached child work or explicit wait. An
agent must finish or cancel its children before its lineage completes. That
remains the rule for delegation. Ongoing intent needs a separate resource
whose lifetime can span several completed root lineages.

## Decision

### 1. A work item spans activations

A `work_item` represents one bounded, inspectable ongoing intent. It has an
owning agent, creator, task text, optional root conversation, status, revision,
limits, creation time, and latest lineage. The creator is a verified client,
system trigger, or policy-authorized agent; model text cannot forge it. A
work item is not a transcript, memory namespace, worker, or workflow DAG.
Client creation is idempotent by client key; agent creation is idempotent by
source run and action ID and requires an explicit detached-work grant. The
default agent policy denies creating work that can outlive its current
lineage. Creation returns an accepted receipt and records the creator's
lineage for audit, without making the new item a child delegation.

The optional host-side `create_work(task, initial_wake)` operation always
creates work owned by the calling agent; it cannot select a different agent
or destination conversation through model arguments. A scoped
`cancel_work(id)` operation lets the owning agent stop an item it is
authorized to manage; it cannot cancel another agent's work.

```text
open -> running | waiting | cancelled
running -> waiting | completed | failed | cancelled
waiting -> running | cancelled
```

One work item has at most one active root lineage at a time. Each activation
is a normal submission with `work_item_id` and its own `root_id`. The runtime
claims it through ADR-0016's conversation, workspace, and agent limits.
Other work items and unrelated conversations may run independently.

On a successful activation, the agent may stage one host-validated work
decision: `complete`, `wait_until(time)`, or `wait_for(configured_event_key,
optional_deadline)`. The decision and bounded updated task summary commit
with the terminal lineage transition, after its attached delegations finish.
Only the latest successful root run's staged decision can commit. A decision
from an earlier run is discarded if child results reactivate the root.
The model-facing operation is conceptually
`decide_work(decision, condition, summary)` and is available only while
running an item. A second decision in one run replaces the first only under
an explicit revision check; otherwise it is rejected as a conflicting
transition.

Without an explicit decision, an ordinary interactive work item completes;
a configured recurring item returns to its existing schedule. A failed or
cancelled lineage cannot stage a new wait. Work creation and decisions have
policy limits on count, frequency, duration, and allowed event keys. An agent
cannot create an unbounded self-wake loop or wait on an arbitrary URL.

A waiting item has no claimed run or workspace. The store owns its wake
condition. Timer and event adapters from ADR-0018 atomically match and accept
the next submission; repeated wakes use stable event IDs. Events arriving
while a lineage is active remain bounded pending wakes. Schedule slots may
coalesce according to ADR-0018; distinct authenticated events are not silently
discarded. An adapter acknowledges a pending wake only after it is durable;
its verified route sends the event either here or to a direct conversation,
not both. The next wake begins a fresh run and receives the work item,
bounded task summary, event attribution, and normal agent memory policy.
It does not restore a live model context.

Cancellation marks the item terminal, removes future wake eligibility, and
cancels its active lineage using ADR-0016. A terminal item cannot be reopened
by a late timer or callback. A later retry or continuation is a new item or
an explicit authorized revision, never an implicit resurrection. The audit
view links all root lineages to the item. External messages about the same
project are not automatically folded into it without a verified binding.

### 2. Durable approval is a pause in one lineage

ADR-0014's `Ask` decision may outlive a client connection. The runtime first
commits the complete assistant turn and exact requested tool intents, as it
already does before execution. Preflight runs in plan order before any sibling
action executes. If a tool needs approval, the store records a pending
approval with the run, lineage, action ID, exact arguments, agent revision,
policy/resource fingerprint, authorized approver scope, and expiry. The run
becomes `waiting_approval`; it releases the worker, workspace claim, and hands
session. Later queued submissions in that conversation do not pass the paused
run. A dedicated work-item conversation avoids blocking unrelated threads.
The lineage remains active and may be cancelled.

Only an authenticated, authorized approver can allow or deny that exact
action. At resume, the runtime obtains a new claim/fence, resolves the pinned
agent revision, and rechecks hard policy, tool availability, workspace
identity, credentials, and approval expiry. A grant cannot override a new
hard deny or become a general permission. The runner resumes the persisted
plan without asking the brain to generate another action first. It records
one result for the exact action, then continues the finite loop. Other
actions in the plan are reauthorized before execution. A denial or expiry
records a normal denied tool result so the brain can respond. Cancellation
fences both approval and resumed execution.

The native loopback server may serve a trusted local approver. A remote
approval surface requires authenticated routing and object authorization;
untrusted hands cannot reach the approval endpoint. An approval request is
bound to the exact action and lineage, never to a general agent permission.

Approval is granted before an external side effect, not proof of that side
effect's outcome. If execution was requested and its result becomes unknown
after a crash, ADR-0012's at-most-one-automatic-attempt policy still applies.
It is not retried merely because approval exists. If no approver or durable
pause adapter is configured, unattended `Ask` still fails closed with
`approval_unavailable` as ADR-0016 specifies.

The resume operation is a runtime/runner contract over persisted turn state;
it does not serialize a live `Core` object or leave a goroutine blocked. The
finite core may need a resume entry point for a previously committed plan,
but does not acquire schedules, channel identities, or work item policy.

### 3. Results and visibility

Each work activation has an explicit terminal outcome under ADR-0018: a
response or, for a system root, `no_update`. Delivery is per lineage and
target, not a side effect of changing work status. A work item can therefore
stay waiting after sending an update, or complete silently after a check.
Client views show item state, next wake, latest lineage, pending approvals,
and safe errors; private child transcripts require their own authorization.
Trusted management surfaces create, inspect, and cancel work items and show
pending approvals; a scoped approval decision addresses one approval ID.

The work item summary is model-influenced data, not a trusted instruction.
It is versioned and bounded like memory, but serves only this item's
continuation. Agent memory in ADR-0019 stores reusable cross-item knowledge.
Artifacts in ADR-0020 carry large files. None of these stores is silently
hydrated as a system prompt.

## Store and implementation sequence

The store adds `work_items`, versioned task summaries, wake conditions,
pending wakes, and approvals. A work decision is staged by a run-scoped host
callback and committed only when the fenced lineage finishes. The same
transaction updates item state and emits its event. Timer/event matching,
approval resolution, and cancellation are idempotent store transitions.

1. Add work item creation, inspection, cancellation, and one-active-lineage
   exclusion; exercise a timer wake through ADR-0018's submission path.
2. Add explicit terminal work decisions, bounded pending wakes, and event
   matching; verify restart and revision conflicts.
3. Persist pending approvals and paused turns; add exact-action resume with
   new fencing and policy revalidation.
4. Add authorized client approval surfaces and delivery of status events.

## Verification requirements

Deterministic tests without provider credentials or external network prove:

- one work item spans several root lineages without retaining a worker or
  model context between them;
- duplicate timer or event wakes create one submission and a late wake cannot
  reopen a terminal item;
- pending events are bounded, schedule slots coalesce as specified, and
  unrelated work remains runnable;
- a work decision from a failed, stale, or cancelled run cannot commit;
- cancellation fences an active lineage, queued wake, and pending approval;
- a paused approval holds no worker or workspace claim but blocks later
  submissions in its own conversation;
- approval resumes the exact persisted action after revalidating hard policy,
  grants, revision, and workspace, without a new model-selected action;
- a denied or expired approval creates one model-visible result;
- crash recovery never automatically repeats an action whose external outcome
  is unknown; and
- one work item cannot escape its count, rate, duration, or event-key policy.

## Consequences and non-goals

Ongoing assistants and approval-heavy coding tasks can pause without tying up
runtime capacity. Work item state adds a second lifecycle above one lineage,
and exact-action approval resume adds substantial runner complexity. These
costs are explicit rather than hidden in a permanently running agent.

This ADR does not define an autonomous planner, arbitrary workflow DAGs,
shared team task lists, model-owned recurring schedules, detached
delegations, or a general external event broker. Those can build on work
items only with their own authorization and lifecycle decisions.
