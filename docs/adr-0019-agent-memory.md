# ADR-0019: Agent memory is explicit, scoped, and separate from transcripts

**Status:** Proposed
**Date:** 2026-09-23
**Related:** ADR-0005, ADR-0008, ADR-0010, ADR-0016, ADR-0017, ADR-0018, ADR-0020

## Context

A persistent agent can own many conversations and run only when work arrives.
Neither one unbounded conversation nor a resident model process is a sound
memory system. Coding agents need project facts and decisions; ongoing
assistants need durable preferences and commitments. Both must be able to
inspect what was retained and distinguish remembered data from trusted
instructions.

Canonical conversations remain the audit history. Brain context compaction
changes only one provider request and does not create durable memory. Workspace
files and transferable artifacts have different ownership and lifecycle from
agent memory.

## Decision

### 1. Agent-owned records

The runtime stores small, versioned memory records under an owning `agent_id`.
A record contains:

```text
id, agent_id, scope_kind, scope_id, revision, text, status
source.kind, source.conversation_id, source.run_id, source.message_id
created_at, updated_at, optional expires_at
```

The first visibility scopes are agent-wide, workspace, conversation, and
verified principal. Agent-wide records are suitable only for facts that may
appear in any conversation owned by that agent. Workspace scope uses
ADR-0016's validated `workspace_id`, not path spelling. A principal scope
uses the adapter and verified principal identity from ADR-0018; two channel
accounts are not silently treated as one person. Human-sourced writes default
to their conversation scope. Widening one to workspace or agent-wide requires
an explicit grant or administrator action. Model arguments may request a scope
kind but cannot invent or override its runtime-derived identity.

Source pointers are runtime-derived and remain readable for audit, subject to
normal retention. Agent-generated text is untrusted data, even if the agent
later reads it. Administrator-written instructions stay in the agent
definition and are not mixed into this store. Credentials remain in credential
slots, not memory records. The memory API rejects configured secret patterns
and oversized content but does not claim to detect every secret.

Memory is private to its owner and visibility scope by default. Retrieval
requires the owning agent and a matching conversation, validated workspace,
or verified principal scope; agent-wide records require that agent's policy.
A system trigger with no principal does not receive personal records. A
delegated child receives no parent memory, and a parent receives no child
memory or inherited human principal scope. Intentional transfer
uses the bounded delegation request or an authorized artifact reference under
ADR-0020. Several conversations owned by one agent may access the same memory
namespace only through agent-wide, same-workspace, or same-principal records.
Record revisions prevent one run from silently overwriting another.

### 2. Explicit mutation and retrieval

An agent may request create, revise, or delete through a host-side memory
capability when its definition grants it. The host derives the agent and run
identity; model arguments cannot choose another namespace or forge source
pointers. Each mutation is idempotent by source run and action ID. Revisions
use compare-and-set, so a stale update reports a conflict for the agent to
resolve. A successful mutation is a durable side effect even if the enclosing
run later fails; crash recovery does not replay an unknown memory action.

The model-facing shape is conceptually `remember(text, scope)`,
`revise_memory(id, expected_revision, text)`,
`forget_memory(id, expected_revision)`, and `recall_memory(query)`. The host
may expose fewer operations to a particular agent. Reads return bounded text,
scope, revision, and provenance; writes return the accepted ID and revision.
No operation accepts an `agent_id` or arbitrary scope ID argument.
A run may revise or delete only records in its visible scopes, and policy
checks any requested widening to workspace or agent-wide scope.

Administrators can inspect, revise, pin, or delete records through a separate
authorized management surface. A pinned record is still data, not a system
instruction. Deletion removes it from future retrieval and records an audit
tombstone. A retention policy can later remove old content; deleting a
conversation or agent requires a defined cascading memory policy before that
operation is exposed. A run's selected revision set can reconstruct what it
saw only while those immutable revisions remain retained. After content purge,
audit keeps IDs and digests without claiming replay of erased text.

The host provides bounded read and search operations scoped to the active
agent and visible scopes. SQLite records are canonical. A search index or
embeddings may improve ranking, but are rebuildable projections and cannot
widen access. Automatic run context selects a bounded set of records according
to an explicit agent policy (for example pinned records plus recent relevant
records). The runner
records the selected IDs and revisions with the run, so a later memory change
does not rewrite what the model saw. Memory appears as attributed, lower-trust
context, never as a system or administrator message. Tool output and imported
text cannot promote themselves into instructions by being remembered.

The first implementation uses text records only. It enforces a configured
per-record size, per-agent record count, and per-run injection budget before
acceptance or hydration. A provider request can omit memory when no retrieval
policy is configured; the complete conversation remains available by its own
normal hydration rules.

### 3. Capture is deliberate

There is no automatic copying of every message or tool result into memory.
An agent or authorized administrator chooses facts worth retaining. Optional
summarization is a separate, auditable run whose proposed writes pass the
same policy, provenance, size, and revision checks. Untrusted web pages,
repositories, and channel messages may inform memory but do not gain higher
authority through that path.

Memory is for compact reusable knowledge, not task status or a large file
store. ADR-0021 owns durable work state and wakes; ADR-0020 owns artifact
bytes and references. Conversation history remains the source for exact
quotes, prior actions, and tool outcomes.

## Store and implementation sequence

The store adds `memory_records` with immutable revisions or equivalent
history, idempotency identities for writes, and a bounded agent-and-scope
query.
Run claims persist the memory revision set selected for hydration. Mutation
events use an agent-scoped audit stream or an authorized management view; they
are not copied into unrelated conversation histories.

1. Add versioned records, scoped reads, and management inspection/deletion.
2. Add host-side agent memory tools and idempotent mutation transitions.
3. Add bounded hydration and record exact selected revisions per run.
4. Add optional ranking or summarization only after deterministic retrieval
   and provenance tests pass.

## Verification requirements

Deterministic tests without provider credentials or network prove:

- two agents cannot read or mutate each other's records through normal tools;
- one principal's records cannot enter another principal's or an unattributed
  system run, and conversation-private records do not cross threads;
- workspace memory follows the validated workspace ID, while widening a
  conversation record fails without the required grant;
- a child sees no parent memory by default, including after restart;
- concurrent updates to one record cannot silently overwrite each other;
- retrying one accepted memory action writes one revision;
- a failed run does not undo an acknowledged memory mutation or replay it;
- deletion removes content from future hydration while preserving an audit
  tombstone;
- the recorded revision set reconstructs the memory context seen by a run
  while those revisions are retained, and does not expose purged content;
- untrusted memory text is rendered as attributed data rather than trusted
  instructions; and
- size, count, and context budgets fail closed.

## Consequences and non-goals

An agent can retain useful knowledge across unrelated conversations without
turning its entire lifetime into one prompt. Explicit writes and provenance
make memory inspectable and correctable. They also mean an agent may fail to
remember something unless configured to capture it.

This ADR does not define shared mutable memory between agents, vector-search
infrastructure, automatic personality extraction, credential storage, or
cross-domain knowledge transfer.
