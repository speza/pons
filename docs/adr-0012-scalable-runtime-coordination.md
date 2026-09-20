# ADR-0012: Runtime work is claimed durably and scheduled lazily

**Status:** Accepted
**Date:** 2026-09-20
**Related:** ADR-0008, ADR-0009, ADR-0010, ADR-0011

## Context

The first server-centred runtime deliberately optimizes for one local process.
At startup `runtime.Manager` lists every conversation, creates one in-memory
`managedConversation` for each, recovers its running state, and launches one
worker goroutine for every conversation with queued work. The manager also
owns process-local conversation mutexes, a global semaphore, workspace locks,
subscriber lists, running flags, and cancellation functions.

This is a useful local implementation, but several properties do not scale
with the number of dormant conversations and do not form a distributed
coordination contract:

- startup and idle memory are proportional to all stored conversations;
- the number of waiting goroutines can be proportional to runnable
  conversations rather than configured concurrency;
- conversation and workspace exclusion are process-local;
- another process cannot distinguish a live worker from a stale one;
- a stale worker is not fenced from committing after ownership changes; and
- process-local wakeups and subscriber broadcasts do not cross server
  instances.

SQLite transactionally claims the next submission within one conversation,
but that transition alone does not establish renewable ownership or safe
multi-process recovery. We must not describe an in-memory mutex, a `running`
status, or a SQLite transaction as a distributed lease.

At the same time, scaling coordination must not move canonical conversation
state into an execution sandbox. Messages, submissions, runs, tool calls, and
the durable client-event outbox remain server-owned state under ADR-0010 and
ADR-0011. Sandboxes and future remote executors are replaceable runner or
environment concerns.

## Decision

### 1. Separate durable state, durable coordination, and live acceleration

The runtime has three kinds of state:

| State | Owner | Examples |
|---|---|---|
| Canonical durable state | `runtime.Store` | messages, parts, submissions, runs, tool calls, event outbox |
| Durable coordination state | coordination operations in `runtime.Store` | claim owner, lease identity, lease expiry, fencing generation, workspace ownership |
| Ephemeral live state | one server process | cancellation functions, connected subscribers, coalesced wake signals, bounded active-run cache |

Ephemeral state may reduce latency but is never proof of ownership and is not
required to reconstruct a conversation. A process crash may discard all live
state without losing canonical data or making a stale worker authoritative.

### 2. Preserve explicit scheduling invariants

The coordination contract must enforce:

1. At most one active run owns a conversation.
2. At most one active run owns an exclusive workspace.
3. Submission acceptance remains idempotent by conversation and client key.
4. A run mutation is accepted only from the current claim generation.
5. Durable events remain ordered per conversation and commit atomically with
   their authoritative state change.
6. No tool executes before its assistant message and tool intent commit.
7. A stale worker cannot complete, fail, or append tool results to a run after
   its claim has been replaced.

Conversation and workspace ownership must be acquired in the same transaction
as the queued-to-running transition. Checking eligibility and claiming in
separate operations is only a hint-plus-compare-and-swap design; the claim
transaction remains the authority.

### 3. Claim runnable work globally and lazily

The manager must not enumerate every conversation at startup. It asks the
store to atomically claim one eligible item when a configured execution slot
is available:

```text
wake scheduler
  -> reserve one of N local execution slots
  -> store claims oldest eligible queued submission
       with conversation + workspace ownership
  -> hydrate only that claimed run
  -> execute
  -> commit terminal state
  -> release slot and wake scheduler again
```

The local scheduler has at most `MaxConcurrent` active run goroutines. A
coalescing notification channel wakes it on startup, submission, and run
completion. It drains eligible claims only while capacity exists. It does not
create a goroutine or actor for every queued or stored conversation.

Conversation live state is created lazily for an active run, an API request,
or a subscriber. A later cache policy may evict entries with no active run,
subscriber, or API reference. Cache eviction is an optimization and must not
change correctness.

The initial local store operation can have this shape:

```go
ClaimRunnable(ctx context.Context) (*ClaimedRun, error)
```

It atomically returns the oldest eligible submission or `nil`. It replaces
`ConversationIDs`, per-conversation `HasPending`, and the split discovery plus
`ClaimNext` startup path.

The distributed form extends the claim, rather than adding distributed locks
to `Manager`:

```go
type ClaimRequest struct {
    WorkerID      string
    LeaseDuration time.Duration
}

type ClaimToken struct {
    RunID          string
    WorkerID       string
    LeaseID        string
    Fence          uint64
    LeaseExpiresAt time.Time
}

ClaimRunnable(ctx context.Context, request ClaimRequest) (*ClaimedRun, error)
RenewClaim(ctx context.Context, token ClaimToken, extendBy time.Duration) (ClaimToken, error)
```

Every run-scoped mutation must eventually carry the claim token and perform a
compare-and-swap on lease identity and fence. A claim token is operational
authority, not semantic conversation history and not a client protocol type.

### 4. Wakeups are hints; the database decides

The scheduler must not busy poll.

For the single-process SQLite backend, these events coalesce onto an in-memory
wake channel:

- manager startup after recovery;
- successful non-duplicate submission;
- completion or failure of a run; and
- release of local capacity.

The manager also performs a low-frequency bounded repair scan. This covers a
lost local signal, a durable write performed through another in-process store
consumer, and work which becomes eligible when an earlier run releases its
conversation or workspace. The scan uses the same bounded claim operation; it
does not enumerate or hydrate every conversation.

A distributed backend needs cross-process notification. PostgreSQL should use
`LISTEN`/`NOTIFY` (or an equivalent broker) to announce that runnable state or
outbox state may have changed. Notifications carry identifiers or cursors, not
canonical payloads. They may be duplicated or lost; workers always re-check
the database. Startup, reconnect, notification-channel failure, and lease
expiry trigger a bounded reconciliation scan. A low-frequency safety scan is
acceptable; tight polling is not.

Scheduler wakeups and client event delivery are related but distinct:

- runnable notifications wake workers to claim submissions;
- outbox notifications wake an HTTP/SSE instance to fetch committed events.

Until cross-process outbox notification exists, live SSE subscribers and the
writer serving them must remain in the same process, or clients must reconnect
through the durable cursor path.

### 5. Use leases and fencing for distributed ownership

A distributed store records, at minimum:

```text
worker_id
lease_id
lease_expires_at
fence
```

The claim transaction:

1. selects an eligible queued submission;
2. proves there is no unexpired conversation owner;
3. proves there is no unexpired workspace owner;
4. increments the fencing generation;
5. creates the running attempt and both ownership records; and
6. appends run/submission outbox events.

PostgreSQL can implement selection with row locking such as `FOR UPDATE SKIP
LOCKED`, plus unique/conditional constraints or explicit lease rows. The exact
SQL is adapter-owned; the invariants belong to `runtime.Store`.

Workers renew before expiry. Every assistant-turn, tool-result, finish, and
failure transition verifies the lease identity and fence. Once a lease expires
or a higher fence is issued, the old worker may still consume CPU or perform an
external action, but it cannot mutate authoritative runtime state.

Fencing database writes does not make arbitrary external side effects exactly
once. That limitation must remain visible.

### 6. Recovery chooses safety over automatic repetition of unknown effects

Delivery and execution guarantees differ by boundary:

- message submission is effectively exactly-once for one conversation and
  idempotency key;
- a queued submission is at-least-once claimable until a run begins;
- model computation before durable tool intent may be retried after lease
  expiry, accepting duplicated model cost;
- an external tool side effect cannot generally be proven at-most-once after a
  crash or timeout.

If a lease expires after a requested tool call has committed, the runtime marks
the tool and run interrupted with **outcome unknown** and does not
automatically replay the submission. This is an at-most-one-automatic-attempt
policy, not a claim of exactly-once execution. A tool may later opt into safe
retry by exposing an idempotency key and reconciliation contract.

If no tool intent exists, a future policy may create a new fenced attempt for
the same submission. Attempts must remain auditable rather than rewriting the
old run in place.

Recovery is bounded and store-driven. It searches expired leases, not every
conversation. A stale attempt is fenced first, then its requested tools and run
are resolved transactionally or through idempotent recovery transitions.

### 7. SQLite remains a local, exclusive-manager backend

SQLite continues to be the default local store. The first coordination
refactor makes runnable selection, conversation exclusion, and workspace
exclusion one immediate transaction. This is useful defence and is tested with
multiple database connections.

However, the supported SQLite topology remains:

```text
one runtime database
one Manager process
bounded local run concurrency
in-process wakeups and subscribers
```

SQLite will not initially implement renewable worker leases, fencing tokens,
cross-process notification, or safe recovery while another manager may still
be alive. Startup recovery therefore assumes exclusive ownership of the local
database. Running two long-lived managers against it is unsupported even if an
individual claim transaction happens to serialize correctly.

PostgreSQL or another coordination-capable backend is required before pons
advertises distributed workers. Backend capabilities must be explicit during
composition; the manager must not infer distributed safety from successful
SQL transactions.

### 8. Keep dormant and historical data out of process memory

Dormant conversations remain rows, not eagerly created actors. The target
manager memory model scales approximately with:

```text
active runs + connected subscribers + bounded hot API entries
```

not total conversations or queued submissions. The implemented local manager
creates `liveConversation` delivery state lazily, but does not yet evict an
entry after it has been accessed; eviction remains a bounded-cache
optimization rather than a correctness mechanism.

Canonical messages remain durable and directly queryable. Today an active run
materializes its complete eligible history before the LLM brain applies
provider-side compaction. A future bounded context view will instead assemble
recent complete messages plus durable summary/checkpoint state while retaining
full history for audit and UI pagination.

Bounded context hydration requires its own semantic policy and durable summary
contract. The first scheduler refactor stops eager dormant-conversation loading
but retains current full-history hydration to avoid silently changing model
context. This remaining bound is explicit follow-up work.

Likewise, a full UI snapshot may need pagination for very large conversations;
that is independent of worker coordination and event catch-up.

### 9. Sandboxes remain execution adapters

Canonical conversation and coordination state stay in the server store.
`Runner` owns one claimed agent burst. `Environment.Provider` owns placement of
the untrusted hands endpoint for that burst. A future whole-agent sandbox,
micro-VM, Kubernetes job, Ax adapter, or Substrate adapter may implement a
runner or environment boundary, but it does not become the conversation
database or the lease authority merely because it hosts execution.

This decision does not change `protocol.Action`, the brain/hands split, or the
`Environment.Provider` contract.

## Implemented local slice

The implemented first slice is intentionally smaller than the target
distributed contract:

1. Eager `ConversationIDs` startup loading and per-conversation `HasPending`
   checks are replaced by atomic store-level `ClaimRunnable`.
2. Goroutine-per-pending-conversation scheduling is replaced by a coalesced wake
   channel, a low-frequency bounded repair scan, and at most `MaxConcurrent`
   active run goroutines.
3. Process-local `liveConversation` delivery state is created lazily for API
   access, subscribers, and claimed runs.
4. SQLite's claim transaction rejects work when either its conversation or
   workspace already has a running submission.
5. Abandoned SQLite runs are recovered once during exclusive manager startup,
   without enumerating conversations in the manager.
6. Local subscriber delivery is preserved by broadcasting committed event batches
   through the lazily created conversation state.
7. Tests cover bounded scheduling, lost-wakeup repair, conversation
   serialization, workspace exclusion, restart recovery,
   idempotency/event ordering, and competing claims through two SQLite
   connections.

This slice does **not** add lease renewal or claim tokens. Consequently it does
not enable a supported second manager process. Naming and documentation must
make that boundary unambiguous.

## Staged migration

### Stage 1 — bounded local coordination (implemented)

The initial slice above keeps SQLite deterministic and preserves the existing
HTTP/SSE contract.

### Stage 2 — lease-capable store contract

Add claim owner, expiry, attempt identity, and fence to the runtime domain.
Require claim tokens on every run-scoped mutation. Add clock-controlled
contract tests for renewal, expiry, stale writes, and recovery policy.

### Stage 3 — PostgreSQL distributed adapter

Implement atomic claims, workspace lease rows, `SKIP LOCKED` selection,
renewals, fencing, bounded expired-lease recovery, and `LISTEN`/`NOTIFY` hints.
Run the same store contract suite against PostgreSQL with multiple worker
processes.

### Stage 4 — distributed live delivery

Notify server instances of committed outbox cursors, fetch events from the
store, and test reconnect/failover without sticky clients. Add outbox retention
and snapshot resynchronization.

### Stage 5 — bounded context and remote execution

Design durable summaries/checkpoints and paginated UI history. Prototype
remote runners or environments, including micro-VM and Kubernetes placement,
without transferring canonical state ownership into them.

## Non-goals

- Exactly-once arbitrary tool side effects.
- Moving messages, inboxes, events, or leases into a sandbox.
- Treating an in-memory mutex or channel as distributed coordination.
- Adding Ax, Substrate, Kubernetes, or a micro-VM provider in this change.
- Changing `protocol.Action`, brain/hands semantics, or tool plugin contracts.
- Selecting a hosted database vendor or general-purpose workflow engine.
- Solving context summarization, UI pagination, or event retention in the
  initial scheduler refactor.

## Consequences

- Local startup and idle memory no longer need to grow with all historical
  conversations after Stage 1.
- Scheduling concurrency becomes bounded by construction rather than by a
  semaphore behind an unbounded set of waiting workers.
- Conversation and workspace eligibility move closer to the durable authority.
- SQLite remains simple and deterministic, with an explicit single-manager
  limitation instead of accidental distributed claims.
- A real distributed deployment requires a lease-capable store contract,
  fencing on every write, cross-process wakeups, and distributed outbox
  notification.
- Full canonical history remains durable while active-run memory can be bounded
  independently in a later semantic-context change.

## Open questions before Stage 2

1. Is workspace exclusivity always required, or should a workspace declare a
   bounded concurrency policy?
2. What lease duration and renewal margin fit model calls and long-running
   tools without causing false expiry?
3. Which tool kinds can support idempotency keys or post-crash reconciliation?
4. Should a pre-tool expired attempt retry automatically, require policy, or
   wait for operator confirmation?
5. Does distributed SSE use database notifications directly, a broker, or a
   dedicated gateway consuming the outbox?
6. What durable summary/checkpoint format can bound LLM hydration without
   making provider-specific context canonical?

## References

- `runtime/manager.go` — runtime lifecycle and lazy live-state lookup
- `runtime/scheduler.go` — current bounded local scheduler
- `runtime/subscriptions.go` — process-local subscriber delivery state
- `runtime/store.go` — durable domain transition contract
- `runtime/sqlite/store.go` — current local transactional adapter
- `docs/adr-0010-runtime-state-and-client-synchronization.md` — canonical state
  and outbox ownership
- `docs/adr-0011-server-centred-runtime-storage.md` — injected store and
  server-centred application
