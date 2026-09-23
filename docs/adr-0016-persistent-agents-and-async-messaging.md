# ADR-0016: Persistent agents delegate work through durable asynchronous messages

**Status:** Proposed
**Date:** 2026-09-22
**Related:** ADR-0001, ADR-0008, ADR-0009, ADR-0010, ADR-0011, ADR-0012, ADR-0014, ADR-0017

## Context

pons currently composes one agent and exposes durable conversations above
finite `Core.Run` executions. Its SQLite runtime already provides most of a
local asynchronous mailbox: durable submissions, transactional claims,
conversation and workspace exclusion, bounded scheduling, event persistence,
and replay after reconnect.

Coding, research, coordination, and personal agents should use this same
execution model. They differ in configuration, not in their core loop. They
also need to delegate work without holding a model call open or requiring a
human to route every result.

A **message** is the durable transport which activates an agent. A
**delegation** is a bounded request to another agent with a lifecycle and an
eventual result. The first product capability is delegation, not arbitrary
agent chat.

All agents in one runtime belong to the trusted administrative domain defined
by ADR-0017. Per-agent capabilities provide least privilege inside that
domain; they are not tenant isolation.

## Decision

### 1. Persistent identity, finite execution

An agent is a stable runtime principal with an identity, configuration, owned
conversations, mailbox, and future memory. Persistence does not mean a
resident goroutine or model connection. Each activation remains finite:

```text
queued submission -> claim resources -> construct configured Core
                  -> run -> persist result -> release resources
```

Idle agents consume no worker. `Core` remains the finite
`respond -> execute -> interpret` kernel and does not own identity, routing,
scheduling, or persistence.

### 2. Static agent directory and revisions

The composing application initially supplies a static agent directory. Each
definition contains:

- a stable non-secret ID and display name;
- instructions and a brain/provider profile;
- capabilities;
- workspace and execution-environment policy;
- authorization policy and allowed delegation recipients; and
- run and concurrency limits.

One definition is the default, preserving the simple single-agent experience.
IDs must be unique, non-empty, and safe in logs and URLs. Provider credentials
remain in the credential store and are referenced by named slots.

Each definition has a deterministic, non-secret revision fingerprint covering
instructions, provider selection, capabilities, workspace policy,
authorization policy, and limits. Submissions and runs record that revision.
After restart, work whose revision cannot be resolved fails closed and, when
applicable, notifies its parent; it never runs under changed authority or
falls back to the default agent. History remains readable if a definition is
removed.

The runtime directory exposes only identity, routing, scheduling, and policy
facts. Application composition remains responsible for constructing brains,
tools, and environments.

### 3. Conversation ownership and mailbox

Every conversation has exactly one owning agent and records:

```text
agent_id
workspace_id
workspace_path
parent_conversation_id  // empty for a root
delegation_depth        // zero for a root
```

Only its owner is hydrated from that conversation. An agent's mailbox is the
set of queued submissions across all conversations it owns, not one shared
inbox transcript.

Conversation exclusion remains one active run per conversation. Workspace
exclusion uses stable `workspace_id`, not agent ID or path spelling. Agent
definitions can share a workspace ID to serialize access to one resource.
Per-agent active-run capacity defaults to one but may allow independent
conversations to run concurrently when workspace policy permits.

For durable environments, `workspace_id` replaces the current incidental use
of conversation ID as `environment.Spec.WorkspaceID`. This allows one agent
computer, a shared project workspace, or conversation-scoped workspaces to be
configuration choices rather than different agent types.

### 4. One trusted submission envelope

Human and agent inputs use the same acceptance and claim path. Submissions add
runtime-owned metadata:

```text
source.kind             human | agent | system
source.agent_id         agent source only
source.conversation_id  agent source only
source.delegation_id    terminal delegation result only
root_id                 original external submission
causation_id            directly causing submission or transition
```

Trusted ingress or the runtime assigns this metadata; model-controlled action
arguments cannot. External input begins a lineage, while agent deliveries
inherit the active lineage. Idempotency remains scoped to the destination
conversation.

New human input does not cancel older descendants. Late results retain root,
causation, and authenticated source attribution so clients and agents can
distinguish concurrent work. Provider adapters render attribution separately
from untrusted content; labels in message text cannot forge identity.

Future schedules, webhooks, approvals, and connectors should enter through
this envelope rather than alternate runners or queues.

### 5. Host-side `delegate` capability

An enabled agent receives a host-side tool with exactly two model arguments:

```text
delegate(agent_id, message)
```

The recipient is one configured agent and `message` is a self-contained text
request. The first contract has no broadcast, attachment, priority, deadline,
arbitrary metadata, agent discovery, or destination conversation argument.

The host derives sender identity, source conversation, submission, lineage,
and authorization from the scoped run. The tool is not exposed through an
untrusted external hands process or execution sandbox.

The sender sees only the IDs, display names, and bounded administrator-written
role descriptions of its allowed recipients. This is a curated catalog, not
dynamic discovery, and does not expose private recipient configuration.

A successful call returns an acceptance receipt:

```text
delegation_id
conversation_id  // private child
submission_id
status = accepted
```

Acceptance does not imply completion. The sender may continue or finish; the
scheduler later claims the recipient independently. A scoped callback in the
run request lets the host plugin invoke the runtime transition without giving
`Core`, the brain, or hands access to the manager or store.

### 6. Private child conversations

Each accepted delegation atomically:

1. validates that the sender run and every ancestor delegation are still
   active, then checks recipient, directed policy, and bounds;
2. creates a `pending` delegation;
3. creates a recipient-owned child conversation linked to its parent;
4. sets depth to parent depth plus one;
5. accepts the agent-sourced message and queued submission; and
6. appends the corresponding durable events.

The active-run and ancestry checks occur in the same transaction as creation.
Cancellation or terminal failure committed first denies a late host callback;
creation committed first makes the new child visible to the cancellation or
failure transition. Context cancellation alone is not an acceptance fence.

The child receives only its own definition, the explicit request, its own
history, and future explicitly authorized artifact references. It does not
implicitly receive the parent's transcript, reasoning, tool output, secrets,
workspace, or permissions.

Addressing an arbitrary existing conversation is deferred. One private child
per request avoids shared-assistant transcript semantics, accidental context
leakage, and a larger authorization contract.

### 7. Delegation lifecycle and waiting

The durable states are:

```text
pending -> running -> waiting -> running -> completed
                   \                     -> failed
                    ----------------------> cancelled
```

- `pending`: child work is queued.
- `running`: a run owns the child conversation.
- `waiting`: its latest run finished but direct child work or later queued
  input remains unresolved.
- `completed`: it produced a final response with neither unresolved direct
  children nor earlier queued inputs.
- `failed`: its run failed terminally.
- `cancelled`: its creator or an authorized operator stopped it.

The runtime initially infers waiting; there is no model `wait` action. On run
completion it checks for non-terminal direct delegations and submissions
queued or running after the current one. If either exists, progress remains in
the private transcript and the delegation waits. Otherwise the latest final
assistant response completes it.

This check prevents a parent delegation from completing after processing only
one of several already-queued descendant results. Each activation receives a
bounded trusted summary of its direct delegations—ID, recipient, and status—so
it can reason about outstanding work without receiving child transcripts.
Root agents may still send progress to human clients.

An agent that wants to complete before its children must cancel them. Explicit
suspend/complete controls are deferred until external waits or detached child
work require them.

### 8. Transactional result routing

When a child becomes terminal, the same store transaction which commits its
response and terminal state accepts one agent-sourced submission in the parent
containing the result or a bounded failure/cancellation notice. It preserves
lineage and authenticated child attribution.

If the parent is idle, the scheduler may claim it. If the parent is running,
the result waits for a fresh activation and is never inserted into an active
model request. The child needs no `reply` tool. Intermediate progress does not
wake ancestors.

A terminal child failure stores internal detail for audit, routes only a safe
notice, and cancels non-terminal descendants. The same transition fails any
queued submissions in the terminal child's conversation, including results
already delivered by children which became terminal first. Their messages
remain readable for audit, but cannot start another run. Claims exclude child
conversations whose parent delegation is terminal. A failed child is not
automatically retried. A root conversation has no parent delivery.

### 9. Idempotency and recovery

Delegation idempotency derives from source run ID plus tool-call ID. Repeating
the same accepted action returns its original receipt and cannot create a
second child. Terminal delivery uses delegation ID plus terminal state, so a
retried transition cannot enqueue duplicate parent input.

If acceptance commits before the sender's tool result and the process crashes,
the generic tool outcome may remain unknown. Startup recovery fails the
interrupted sender run and does not replay its tool call. A root sender's
accepted child may continue and deliver its result to the root conversation.
If the sender is itself a child, its terminal failure cancels its non-terminal
descendants, including the newly accepted child. The idempotency key rejoins
an accepted action if it is repeated while the original run is still active;
it is not a promise of replay after a crash. This retains the existing
at-most-one-automatic-attempt policy for requested tools.

Child completion and parent acceptance are atomic. A future distributed store
may implement an idempotent outbox, but must preserve the invariant that a
terminal child result is either delivered or explicitly recorded as
permanently undeliverable.

### 10. Events and observability

Events retain independent per-conversation cursors; there is no global event
log. A transaction may emit child and parent events. The manager groups and
publishes committed events by `ConversationID` and emits one coalesced
scheduler wake hint. Reconciliation remains authoritative if a wake is lost.

Views expose agent ownership and structured source attribution.
`delegation.updated` is emitted on the parent cursor. A conversation view
includes its parent delegation and direct delegations; a tree projection reads
the same canonical rows. Child transcript access remains an authorized normal
conversation read. `lineage.updated` is emitted on the root conversation's
cursor when its completion status changes. Progress is not copied into
ancestor histories.

### 11. Authorization and credential isolation

A delegation is accepted only when:

- both definitions and the sender's recorded revision are available;
- the sender has the host delegation capability;
- the recipient is on its directed allowlist;
- sender and recipient differ; and
- size, depth, lineage, and egress policy allow the request.

Delegation transfers a request, not authority. The recipient uses only its own
tools, secrets, workspace, network policy, and authorization policy. Its
response is authenticated but still untrusted input to the parent. Native
action authorization from ADR-0014 applies before delegation, while recipient
policy remains a non-overridable hard check.

Delegation is a data-egress seam. Policy receives destination, message,
source trust domain, and declared artifact references and may allow, deny, or
redact. The audit record stores the decision and payload hash, not hidden
credentials. An allowlisted route does not imply every payload is safe.
Unattended delegation does not wait on a human approval inside an active run:
a policy requiring approval denies with `approval_unavailable` until a later
design provides durable approval suspension and resumption. ADR-0014 may
still authorize a directly attended action before the host tool runs.

Secret isolation requires process boundaries. Brain credentials stay in host
composition; hands and external plugins receive only grants for that agent and
must not inherit the server's full environment. pons must not claim per-agent
secret isolation until this launch path is enforced and tested.

### 12. Bounds and scheduling

The server initially enforces:

- 64 KiB maximum UTF-8 request text;
- depth of at most 8 child edges;
- 64 accepted delegations per root lineage;
- 256 claimed activations per root lineage; and
- one recipient per delegation action.

Checks are transactional and use runtime-owned lineage. Human input begins a
new lineage and budget. Return delivery is always accepted so results are not
lost. If its next activation exceeds budget, the submission and any owning
non-terminal delegation fail with `lineage_budget_exceeded`, and one bounded
notice propagates upward rather than leaving work waiting forever.

Depth follows ancestry; returning to a parent does not increase it. Agent IDs
may recur, but depth and lineage limits bound cycles. Stable denial codes
include `recipient_not_allowed`, `delegation_depth_exceeded`, and
`lineage_budget_exceeded` without exposing raw policy detail.

The manager reserves one of its global `MaxConcurrent` slots before calling
`ClaimRunnable`, as in ADR-0012. The claim transaction enforces conversation
and workspace exclusion and the matching agent revision's active-run cap.
Missing revisions are resolved through terminal failure routing, not skipped
or reassigned. Fair queuing, token/cost lineage budgets, and distributed
workers are deferred.

### 13. Cancellation

The creator may cancel a direct delegation through a host capability; an
authorized client or operator may cancel one delegation or a root lineage.
Cancellation is durable and idempotent. It atomically marks the selected
delegation and non-terminal descendants cancelled, prevents queued work from
being claimed, resolves their queued submissions, and requests context
cancellation for local running work. The acceptance fence in section 6 also
prevents an in-flight host callback from creating a descendant after the
cancellation transaction commits.

Exactly one terminal transition wins. Completion committed first is retained;
cancellation committed first fences stale completion. Requested tool effects
retain outcome-unknown semantics. One bounded parent notice is delivered, but
cancelled descendants do not each trigger ancestor model runs. Disconnecting
a client does not cancel accepted work.

### 14. Runtime and HTTP boundaries

Conversation creation accepts an optional `agent_id`; omission selects the
default. Conversation views include ownership. Human message submission keeps
its existing route for root conversations. Private child conversations accept
only runtime-routed descendant results; clients may read or cancel them when
authorized, but cannot submit a message to them. Clients cannot set agent
source, lineage, or depth metadata, and there is initially no public
agent-mailbox message route.

The API adds explicit delegation and lineage cancellation operations. Agent
listing, remote authentication, external channel identities, and mutation of
agent definitions are separate decisions. The cancellation routes are
conceptually `POST /v1/delegations/{id}/cancel` and
`POST /v1/conversations/{id}/lineages/{root_id}/cancel`.

A root lineage remains active while any submission with that `root_id` is
queued or running, any descendant delegation is non-terminal, or a terminal
result still awaits processing in the root conversation. Once those are
resolved, it is `completed` with the latest final root response for that
`root_id`, `failed` if its last root run failed, or `cancelled` if the lineage
was cancelled. The runtime projects this status from canonical rows. A
conceptual
`GET /v1/conversations/{id}/lineages/{root_id}` returns status, latest final
root response ID, and error code without exposing child transcripts.

`pons client -message` follows the root lineage created by its submission
through its terminal status. An early final root response while children are
outstanding is progress, not the command's terminal answer; the client returns
the latest final root response once the lineage completes. It reports a failed
or cancelled lineage instead of treating an earlier progress response as
success. Snapshot plus durable cursor allows a client to resume following the
same `root_id` after reconnect. Other human submissions start independent
lineages and do not change this completion condition.

Delegation requires a process lifecycle independent of a client request. It
is enabled in long-lived `pons serve`. The bundled ephemeral server rejects a
delegation-enabled multi-agent composition rather than exiting with accepted
child work queued. SSE disconnect does not stop work; snapshot plus durable
cursor reconstructs results on reconnect.

### 15. Store and runner changes

The store continues to expose domain transitions, not generic queue CRUD. New
atomic transitions create a delegation with its child and submission, cancel a
delegation or lineage, and finish/fail a child while routing parent input.

Claims carry `AgentID`, `AgentRevision`, `WorkspaceID`, and workspace path.
The runner resolves the definition, creates a fresh brain, registers exactly
its tools, and supplies the scoped delegation callback only when enabled.

The minimum durable additions are:

```text
conversations: agent_id, workspace_id, parent_conversation_id,
               parent_delegation_id, delegation_depth
submissions:   agent_revision, source fields, source_run_id,
               source_action_id, root_id, causation_id
runs:          agent_id, agent_revision, root_id
delegations:   id, root_id, parent/child conversation IDs,
               sender/recipient IDs, source run/action IDs, status,
               result_message_id, error, timestamps
```

`(source_run_id, source_action_id)` uniquely identifies a delegation and
rejoins its child and initial submission on conflict. Lineage limits count
canonical delegation and run rows. Automatic result delivery consumes no
delegation slot, while a claimed parent activation consumes its run budget.

The SQLite schema version increases. Under the pre-compatibility policy, old
non-empty runtime databases may require recreation; no dual schema is retained
solely for compatibility.

## Implementation sequence

1. **Identity:** persist agent, revision, workspace, parent, and depth; resolve
   one default definition; carry identity through claims and run requests.
2. **Composition:** build brain, tools, environment, limits, and credential
   grants per agent; support agent selection; reject missing revisions.
3. **Delegation:** add source/lineage metadata, host tools, atomic child
   creation with an active-ancestry fence, waiting, result routing, terminal
   cleanup, cancellation, mixed-conversation event publication, policy,
   bounds, and concurrency enforcement.
4. **Client completion:** project root-lineage status, publish its event, and
   make the HTTP client follow a lineage through its terminal answer.
5. **Triggers:** adapt schedules, webhooks, connectors, and approvals to the
   same submission envelope without changing `Core`; durable approval
   suspension requires its own decision before enabling unattended `ask`.

## Verification requirements

Deterministic tests, requiring no provider credentials or network, must prove:

- ownership selects the non-default agent and asymmetric definitions produce
  different composition through the same `Core`;
- children receive the request but no parent history, and progress stays
  private until all direct work and queued results are resolved;
- results arriving during a parent run stay queued, attributed, and durable;
- missing revisions, recipient/egress denials, limits at both boundary values,
  repeated identity cycles, and credential separation fail closed;
- duplicate actions and crashes around acceptance create one child and one
  parent terminal delivery, with no automatic replay of interrupted tools;
- failure and cancellation races produce exactly one terminal state and no
  runnable work in terminal child conversations, including queued results;
- a late delegate callback cannot create a child after cancellation or failure;
- per-agent and workspace concurrency are enforced transactionally;
- child and parent events retain independent cursors and survive restart;
- an early root response does not end the client wait while delegated work is
  outstanding, and reconnect resumes the same lineage.

Black-box coverage should exercise two scripted agents through HTTP, SQLite,
the scheduler, restart, and event replay.

## Alternatives and non-goals

A standalone broker is rejected for the local runtime: SQLite submissions are
already the durable queue and audit log. A broker may later provide wake hints,
but not canonical state. One goroutine per agent and direct brain-to-brain calls
are rejected because they waste idle resources and bypass durability, policy,
and recovery. Blocking delegation is rejected because it holds bounded worker
capacity across unbounded work.

Shared multi-agent transcripts, singleton inbox conversations, and arbitrary
existing-conversation addressing are deferred because they leak unrelated
context or require substantially broader membership and lifecycle semantics.
Full mutable agent definitions are not copied into SQLite until managed
configuration defines version retention explicitly.

This ADR does not introduce shared memory, group chat, one-way social
messaging, dynamic agent creation or discovery, broadcasts, priorities,
attachments, artifact transfer, first-class goals or workflow DAGs,
exactly-once external effects, distributed leases, cross-tenant identity, or
product UI such as presence and computer takeover.

## Consequences

The same lightweight architecture supports coding, research, coordination,
and personal agents. It extends existing queueing, persistence, recovery, and
events rather than adding a service. Idle agents remain cheap, and private
contexts plus recipient-owned authority reduce accidental leakage.

The cost is more conversations and events. Agents must write self-contained
requests; direct-child results activate parents independently; static
definition changes require restart; removed definitions make historical
conversations temporarily non-runnable; and the generic tool crash window can
still report an accepted delegation's receipt as outcome unknown.

## Deferred questions

- When do goals, artifacts, cross-conversation memory, schedules, or continued
  child addressing deserve first-class resources?
- Should definitions become mutable API resources with retained revisions?
- How should lineage token and monetary budgets compose with retries and
  provider failover?
- What consistency does shared memory require for concurrent agent runs?

## References

- `core.go` — finite brain/hands loop
- `runtime/types.go`, `runtime/store.go` — runtime domain and transitions
- `runtime/manager.go`, `runtime/scheduler.go` — durable scheduling
- `runtime/sqlite/store.go` — SQLite claims and event outbox
- `cmd/pons/runtime_mode.go` — current composition
- `docs/runtime-v1.md` — implemented one-agent runtime
- ADR-0008, ADR-0010, ADR-0012, ADR-0014, ADR-0017
