# ADR-0021: Ongoing work and approvals wait durably, not in a running agent

**Status:** Proposed
**Implementation:** Not implemented
**Date:** 2026-09-23
**Related:** ADR-0008, ADR-0012, ADR-0014, ADR-0016, ADR-0018, ADR-0019

> **Note (2026-09-25):** this ADR builds on ADR-0016's lineages and staged
> decisions, which are deferred. Revisit both before implementing work items.

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
owning agent, creator, task text, optional root conversation, optional
principal, optional delivery target, status, revision, limits, creation time,
and latest lineage. Its principal and delivery target behave as a schedule's
do under ADR-0018 section 2. An agent-created item inherits them from the
creating lineage; the model cannot supply either. The creator is a verified client,
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
The model-facing operation is conceptually
`decide_work(decision, condition, summary)` and is available only while
running an item. It is a staged terminal decision under ADR-0016 section 6,
which defines commit, replacement, and discard rules. It may be combined with
`complete_no_update` from ADR-0018.

When a `wait_for` deadline passes before its event arrives, the store accepts
one system wake with `source.adapter_id = internal` and a
`deadline_expired` cause, idempotent by work-item revision and deadline. The
activation decides whether to wait again, complete, or report. A matching
event committed first wins, and the deadline wake is not created.

Without an explicit decision, an ordinary interactive work item completes;
a configured recurring item returns to its existing schedule. A failed or
cancelled lineage cannot stage a new wait. Work creation and decisions have
policy limits on count, frequency, duration, and allowed event keys. An agent
cannot create an unbounded self-wake loop or wait on an arbitrary URL.

An agent may propose a recurring work item, such as a daily brief, from the
owner's conversation. It is a confirmation of the exact recurrence, target,
and task; nothing runs until the owner accepts it, and the accepted item then
belongs to the owner's configuration like any configured schedule.

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

This section is a decision summary. Its detailed contract is written when
phase 11 of [`docs/proposals/persistent-agents/plan.md`](../proposals/persistent-agents/plan.md)
begins, after ADR-0014's preflight authorization stage exists.

- ADR-0014's `Ask` may outlive a client connection. The runtime records a
  pending approval bound to the exact action, arguments, run, lineage, agent
  revision, approver scope, and expiry, then releases the worker, workspace
  claim, and hands session. Later submissions in that conversation wait
  behind it; the lineage stays active and cancellable.
- Only an authenticated, authorized approver can allow or deny that one
  action. An approval is never a general permission.
- Resume takes a new claim, revalidates hard policy, revision, workspace, and
  expiry, and executes the persisted action without asking the brain to plan
  again. A denial or expiry becomes a normal denied tool result.
- Approval does not change crash semantics: an action whose outcome is
  unknown is not retried automatically.
- Resume works from persisted turn state; it never serializes `Core` or
  blocks a goroutine. Without an approval surface, unattended `Ask` fails
  closed with `approval_unavailable`.

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

## Store changes

The store adds `work_items`, versioned task summaries, wake conditions,
pending wakes, and later approvals. A work decision is staged by a run-scoped host
callback and committed only when the fenced lineage finishes. The same
transaction updates item state and emits its event. Timer/event matching,
approval resolution, and cancellation are idempotent store transitions.

[`docs/proposals/persistent-agents/plan.md`](../proposals/persistent-agents/plan.md) places work items
in phase 8 and durable approvals in phase 11.

## Verification requirements

Deterministic tests without provider credentials or external network prove:

- one work item spans several root lineages without retaining a worker or
  model context between them;
- duplicate timer or event wakes create one submission and a late wake cannot
  reopen a terminal item;
- pending events are bounded, schedule slots coalesce as specified, and
  unrelated work remains runnable;
- a work decision from a failed, stale, or cancelled run cannot commit;
- a passed `wait_for` deadline creates one wake, and an event committed first
  suppresses it;
- a work item's results reach its pinned delivery target;
- cancellation fences an active lineage and any queued wake; and
- one work item cannot escape its count, rate, duration, or event-key policy.

## Consequences and non-goals

Ongoing assistants and approval-heavy coding tasks can pause without tying up
runtime capacity. Work item state adds a second lifecycle above one lineage,
and exact-action approval resume adds substantial runner complexity. These
costs are explicit rather than hidden in a permanently running agent.

This ADR does not define an autonomous planner, arbitrary workflow DAGs,
shared team task lists, recurring work without owner confirmation, detached
delegations, or a general external event broker. Those can build on work
items only with their own authorization and lifecycle decisions.
