# ADR-0016: Persistent agents run finite activations and start private tasks

**Status:** Proposed
**Implementation:** Not implemented
**Date:** 2026-09-22
**Related:** ADR-0001, ADR-0008 through ADR-0012, ADR-0014, ADR-0017
through ADR-0022

## Context

pons currently composes one agent and exposes durable conversations above
finite `Core.Run` executions. Its SQLite runtime already provides most of a
local asynchronous mailbox: durable submissions, transactional claims,
conversation and workspace exclusion, bounded scheduling, event persistence,
and replay after reconnect.

An owner-named personal agent should handle day-to-day, coding, and research
work through this same execution model. Focused work, such as a coding task
in another repository, needs a clean context of its own without holding a
model call open or requiring a human to route the result.

A **message** is the durable transport which activates an agent. A **task**
is a bounded request run in a private child conversation, by the same agent
or another, with a lifecycle and an eventual result; the runtime records it
as a delegation. A **lineage** is one external submission together with all
work it causes.

All agents in one runtime belong to the trusted administrative domain defined
by ADR-0017. Per-agent capabilities provide least privilege inside that
domain; they are not tenant isolation.

## Decision

### 1. Persistent identity, finite execution

An agent is a stable runtime principal with an identity, configuration, owned
conversations, mailbox, memory under ADR-0019, and workspace policy under
ADR-0022. Persistence does not mean a resident goroutine or model connection.
Each activation remains finite:

```text
queued submission -> claim resources -> construct configured Core
                  -> run -> persist result -> release resources
```

Idle agents consume no worker. `Core` remains the finite
`respond -> execute -> interpret` kernel and does not own identity, routing,
scheduling, or persistence.

### 2. One agent shape and revisions

There are no agent kinds. Every agent has the same definition: a stable
non-secret ID, a persona, a provider slot, tools and grants, workspace and
memory policy, task profiles (section 7), and run limits. The runtime never
branches on what kind of agent something is. pons ships one default agent,
which the owner names and shapes through conversation; context isolation
comes from tasks, not from more agents.

The persona lives in a host-owned, versioned `PERSONA.md`, optionally seeded
from configuration. It changes only when the owner edits it or accepts an
exact proposal the agent made in the owner's conversation. The agent's hands
never receive a grant to it, so under a sandboxed provider it cannot write
it; the unsandboxed development composition trusts its owner instead.
Learned memory under ADR-0019 never becomes persona by itself.

Every change to a definition, including the persona, is an immutable revision
with a deterministic, non-secret fingerprint. Submissions and runs keep the
revision they were accepted under; new work uses the current one. A revision
that cannot be resolved fails closed rather than running under changed
authority or falling back to another agent.

A definition references provider and credential slots by name and never
contains secrets. Models cannot create or edit definitions. Additional agents,
for example one per household member, use the same definitions and are
managed through an owner-only surface when they arrive; until then the one
agent comes from configuration and `PERSONA.md`.

### 3. Conversation ownership and workspaces

Every conversation has exactly one owning agent and records `agent_id`,
`workspace_id`, and, for a delegated child, its parent conversation and
delegation. Only the owner is hydrated from a conversation. An agent's mailbox
is the set of queued submissions across its conversations, not one shared
inbox transcript.

Conversation exclusion remains one active run per conversation. Workspace
exclusion uses the stable `workspace_id`, which replaces the current use of
conversation ID as `environment.Spec.WorkspaceID`. ADR-0022 defines how agent
policy resolves it. Per-agent active-run capacity defaults to one.

ADR-0022 also defines how workspace IDs are validated against their backing
resources.

### 4. One trusted submission envelope

Human, system, and agent inputs use the same acceptance and claim path.
Submissions carry this canonical runtime-owned envelope. ADR-0018 defines how
ingress adapters populate the adapter fields, and ADR-0021 adds
`work_item_id`.

```text
source.kind              human | agent | system
source.adapter_id        native | schedule | connector ID | internal
source.external_event_id adapter-scoped idempotency identity, when available
source.principal_id      verified principal from ADR-0018, when available
source.agent_id          agent source only
source.conversation_id   agent source only
source.delegation_id     delegation result only
work_item_id             work-item activation only (ADR-0021)
root_id                  lineage identity
causation_id             directly causing submission or transition
```

Trusted ingress or the runtime assigns this metadata; model-controlled action
arguments cannot. External input begins a lineage; delegation requests and
results inherit it. Provider adapters render attribution separately from
untrusted content, so labels in message text cannot forge identity.

### 5. Lineages complete explicitly

A durable lineage row is created with each external submission. It stays
`active` while any of its submissions is queued or running, any of its
delegations is non-terminal, a durable approval is pending under ADR-0021, or
a delegation result awaits processing.

When nothing remains, one compare-and-set transition ends it:

- `completed` if the latest root run produced an explicit final response, or
  a system root recorded `no_update` under ADR-0018;
- `failed` with `no_final_response` or the run's error otherwise; or
- `cancelled` under section 9.

A terminal lineage cannot be reopened by a stale run or callback. A final
root response while delegations are outstanding is progress, not the
lineage's answer. `lineage.updated` is emitted on the root conversation's
cursor. `pons client -message` follows its lineage to a terminal status and
returns the latest final response, and can resume following after reconnect.

### 6. Staged terminal decisions

Some host capabilities let a run declare how its lineage should end:
ADR-0018's `complete_no_update` and ADR-0021's `decide_work`. They share one
mechanism:

1. A host-side, run-scoped capability stages a typed decision on the current
   run. Staging never commits lineage or work-item state by itself.
2. A run stages at most one decision of each type. Replacing one requires its
   staged revision; otherwise the second call is rejected.
3. Only the latest successful root run's staged decisions commit, in the
   lineage's terminal transaction. Decisions from an earlier run are discarded
   if a delegation result reactivates the root.
4. A failed, cancelled, or fenced run cannot commit a staged decision.
5. Each decision type declares which roots may use it and what it conflicts
   with; the host validates both when staging.

Decision capabilities are host tools. They are not exposed to hands and add
no lifecycle behavior to `Core`.

### 7. Tasks: one level of private delegation

An agent hands work to a fresh, private context by starting a **task**. A
task is the runtime's delegation record: a child conversation with one run
and a result that returns to the parent. A root conversation's agent, when
granted tasks, receives one host-side tool:

```text
start_task(message, profile, workspace)
```

`message` is a self-contained request. `profile` names one of the agent's
task profiles (section 2), which decides who runs the task and with what. The
optional `workspace` names one of the workspaces that profile allows, such as
a configured repository; the host rejects anything else, and the model never
supplies a path. ADR-0020 defines an optional artifact-reference extension.

A task profile is owner configuration on the agent definition:

- **runs as** the agent itself or another allowed agent;
- **context**: the request only, never the parent's transcript;
- **tools**, usually a subset or a specialist set such as Git for coding;
- **workspace policy** and the named workspaces it may use under ADR-0022;
- **memory**: none, or the starting run's scopes read-only under ADR-0019;
  and
- provider slot and run limits.

A coding task, for example, runs as the same agent with shell, Git, and web
tools in an E2B workspace for the chosen repository, and reads the owner's
preferences from memory without being able to change them. The agent sees
its profiles' names and owner-written descriptions, not their configuration.

Tasks are one level deep. A task's run never receives `start_task`, so a
lineage is at most a root with direct children. Deeper trees are deferred
until a use case needs them.

The host derives the sender, lineage, and authorization from the scoped run.
A successful call returns a receipt with `delegation_id`, the private child
`conversation_id`, and `status = accepted`. Acceptance does not imply
completion; the sender may continue or finish.

Each accepted task atomically checks that the sender run and lineage are
active and that policy and bounds allow the request, then creates the
delegation, a child conversation owned by the profile's agent, and its
single queued submission. Cancellation or failure committed first denies a
late callback. Idempotency derives from source run ID plus tool-call ID.

The child receives only its profile, the request, and artifact references
authorized under ADR-0020. It does not receive the parent's transcript,
reasoning, tool output, workspace, secrets, or permissions, and receives
memory only as its profile allows. Clients may read or cancel a child
conversation but cannot submit messages to it.

### 8. Task lifecycle and result routing

A child conversation has exactly one submission, so a task is one run:

```text
pending -> running | failed | cancelled
running -> completed | failed | cancelled
```

`pending -> failed` occurs when the running agent's revision cannot be
resolved.
A run that stops without an explicit final response fails with
`no_final_response`; an exhausted run fails with its turn-limit error. A
child cannot use `no_update`.

The transaction that makes a delegation terminal also accepts one
agent-sourced submission in the parent conversation containing the result or
a bounded failure or cancellation notice. If the parent is running, the
result waits for a fresh activation and is never inserted into an active
model request. Failures store internal detail for audit and route only a safe
notice. A failed child is not retried automatically.

If the process crashes after acceptance but before the sender's tool result
is recorded, recovery fails the interrupted sender run without replaying its
tool call. The accepted child continues and its result reactivates the root.

### 9. Cancellation

The sender may cancel its own tasks through a host capability; an
authorized client may cancel one task or a whole lineage. Cancellation
is durable and idempotent. Cancelling a delegation marks it cancelled, removes
its queued submission from eligibility, requests context cancellation of a
running child, and routes one notice to the parent. Cancelling a lineage also
cancels its root's queued and running work and all its delegations, fences
their completion, and wakes no one. Exactly one terminal transition wins.
Disconnecting a client cancels nothing.

### 10. Authorization, isolation, and eligibility

A task is accepted only when the relevant revisions resolve, the sender holds
the task grant, the profile and workspace are ones the agent defines, and
size, lineage, and egress policy allow the request. The egress policy sees
host-derived sender, profile, running agent, lineage, and request text,
and may allow, deny, or redact; audit records the decision and a payload
hash.

A task transfers a request, not authority. It uses only its profile's
tools, secrets, workspace, and policy, and its response is untrusted
input to the parent. Until ADR-0021's durable approvals exist, a policy that
requires approval for starting a task denies with `approval_unavailable`.

Per-agent secret isolation requires process boundaries: brain credentials
stay in the host, and hands receive only that agent's grants. pons must not
claim per-agent secret isolation until this is enforced and tested.

The native HTTP server has no object authorization, so the private-child rule
holds only if hands cannot reach it. A delegation-enabled composition must
ensure each agent's hands cannot reach the runtime listener or read the
runtime database, checkpoint and artifact stores, agent definitions, or
credential store, and can see only the memory scopes ADR-0019 mounts for the
run. Against current providers:

- **Seatbelt** is eligible when network is denied and no granted path
  contains the runtime state directory (default `~/.pons/runtime/`) or
  credential store.
- **E2B** is eligible unless a host port is exposed into the sandbox.
- **Unsandboxed in-process tools**, including unrestricted `bash`, are
  ineligible.

Composition rejects an ineligible delegation setup at startup and reports
why. Delegation also requires long-lived `pons serve`; the bundled ephemeral
server rejects it.

### 11. Bounds and scheduling

The server enforces a maximum request size (initially 64 KiB of UTF-8) and a
maximum number of delegations per lineage (initially 16). Because each result
reactivates the root, the delegation cap also bounds root activations.
Denials use stable codes such as `task_profile_not_allowed` and
`lineage_delegation_limit` without exposing policy detail.

The manager reserves a global `MaxConcurrent` slot before calling
`ClaimRunnable`, as in ADR-0012. The claim enforces conversation and workspace
exclusion and the agent's active-run cap. Per-agent token budgets arrive with
schedules; fair queuing and distributed workers are deferred.

### 12. Runtime and HTTP boundaries

Conversation creation accepts an optional `agent_id`; omission selects the
default. Views expose ownership, source attribution, a conversation's
delegations, and lineage status; `delegation.updated` is emitted on the
parent's cursor. The API adds delegation and lineage cancellation and a
lineage status read. Remote authentication is a separate decision.

## Store changes

The store exposes domain transitions, not generic queue CRUD: create a
delegation with its child and submission, finish a child while routing its
result, cancel a delegation or lineage, and end a lineage. The store adds
`agent_revisions`, `delegations`, and `lineages`;
conversations, submissions, and runs gain agent, revision, workspace, and
envelope fields. The SQLite schema version
increases; old databases may need recreation under the pre-compatibility
policy.

[`docs/persistent-agents-v1.md`](../persistent-agents-v1.md) orders the work:
identity (sections 1–3) and lineages (sections 4–6) first, tasks
(sections 7–11) after the assistant path.

## Verification requirements

Deterministic tests, without provider credentials or network, prove:

- a persona or definition change creates a new revision, in-flight work
  keeps its starting revision, and an unresolvable revision fails closed;
- the agent cannot change `PERSONA.md` except through an accepted proposal,
  and no model-facing tool can create or edit a definition;
- an empty or exhausted run cannot complete a lineage or delegation, and an
  early root response does not end the client's wait while delegations are
  outstanding;
- staged decisions from a failed, cancelled, or superseded run never commit;
- a task receives the request but no parent context, cannot start tasks, and
  gets only its profile's tools, workspace, and memory access;
- a model-supplied profile or workspace outside the agent's configuration is
  rejected;
- results arriving during a parent run stay queued and attributed;
- duplicate actions and crashes around acceptance create one child and one
  result, with no replay of interrupted tools;
- cancellation and failure races produce exactly one terminal state, and a
  late callback cannot create a child after either;
- profile, workspace, egress, size, and per-lineage limits fail closed; and
- an ineligible hands provider is rejected when delegation is enabled.

Black-box coverage exercises a scripted agent and a task through HTTP,
SQLite, the scheduler, restart, and event replay.

## Alternatives and non-goals

A standalone broker is rejected: SQLite submissions are already the durable
queue and audit log. One goroutine per agent and direct brain-to-brain calls
are rejected because they waste idle resources and bypass durability, policy,
and recovery. Blocking delegation is rejected because it holds worker
capacity across unbounded work. Multi-level delegation is deferred because
no current use case needs it and it concentrates most of the lifecycle
complexity. Predefined specialist agents (a coding agent, a research agent)
are rejected as the default: they make the owner design roles up front,
while task profiles give the same context isolation inside one named agent.

This ADR does not introduce shared transcripts, addressing existing
conversations, agent-to-agent chat, model-driven agent creation or discovery,
broadcasts, priorities, workflow DAGs, or exactly-once external effects.

## Consequences

One lightweight architecture lets a single named agent handle day-to-day,
coding, and research work through tasks, extending existing queueing,
persistence, recovery, and events rather than adding a service. Idle agents stay cheap, and private children with
profile-scoped authority reduce leakage.

Tasks need self-contained requests. Definition changes apply to new work
only, and the generic tool crash window can still report an accepted
delegation's receipt as outcome unknown.

## Deferred questions

- Which use case, if any, justifies tasks that start further tasks?

## References

- `core.go` — finite brain/hands loop
- `runtime/store.go`, `runtime/scheduler.go` — runtime transitions and
  scheduling
- `runtime/sqlite/store.go` — SQLite claims and event outbox
- `cmd/pons/runtime_mode.go` — current composition
- `docs/runtime-v1.md` — implemented one-agent runtime
