# ADR-0019: Agent memory is a scoped file tree owned by one agent

**Status:** Proposed
**Implementation:** Partial — single-owner memory directory granted directly to local runs, with prompt guidance and hydration (plan phase 2); per-run copies, commits, history, scopes, E2B, and extraction not implemented
**Date:** 2026-09-24
**Related:** ADR-0009, ADR-0016, ADR-0017, ADR-0018, ADR-0020, ADR-0021, ADR-0022

## Context

A persistent agent owns many conversations and runs only when work arrives.
Neither one unbounded conversation nor a resident model process is a sound
memory system. A personal assistant needs durable preferences, facts, and
commitments; coding work needs project conventions and decisions.

Agents are already effective with files. A `MEMORY.md` index plus one file per
memory is readable by the model, searchable with ordinary file tools, easy for
the owner to inspect and edit, and needs no bespoke recall API. The risk is
that a plain shared directory has none of the scoping, provenance, or history
a runtime needs: in a household, one person's memories must never appear in
another person's run.

Canonical conversations remain the audit history. Brain context compaction
changes only one provider request and does not create durable memory.
Workspace files, transferable artifacts, and work-item state have different
ownership and lifecycles.

## Decision

### 1. Memory is a per-agent file tree

Each agent with memory enabled owns one memory tree:

```text
memory/
├── MEMORY.md                 agent-wide index
├── <topic>.md                agent-wide memories
└── principals/<principal-id>/
    ├── MEMORY.md             this principal's index
    └── <topic>.md            this principal's memories
```

The recommended convention is one fact or topic per file and one line per file
in the scope's `MEMORY.md`. The runtime does not parse or enforce file
contents beyond the limits in section 4.

Memory is never shared between agents. Two agents serving the same principal
each keep a separate `principals/<id>/` tree. A task run as the same agent
receives only what its ADR-0016 task profile allows: nothing, or the
starting run's scopes read-only. A task run by another agent receives none of
the parent's memory, and a parent receives none of a task's. Information
moves between agents only through a delegation request, a result, or an
authorized artifact under ADR-0020. Any shared or cross-agent memory is a
separate future decision.

Memory specific to one workspace, such as repository conventions, is ordinary
workspace content (for example `AGENTS.md`) and persists under ADR-0022. It is
not part of this tree.

The canonical tree lives in a host-owned memory store outside every workspace,
the runtime database, and the artifact store.

### 2. Scope is what the host mounts

A run sees only the scopes it is entitled to. The host materializes them into
the run's hands environment at fixed paths:

| Run | Agent-wide scope | `principals/<id>/` |
| --- | --- | --- |
| Human or scheduled run with a principal | read-only; read-write if that principal is an agent-wide writer | that principal's only, read-write |
| System run without a principal | read-write only with an agent-wide write grant; otherwise read-only | none |
| Extraction run (section 6) | as its originating lineage | as its originating lineage |
| Task run as the same agent | per its profile: none, or read-only | per its profile: none, or the starting run's principal read-only |
| Task run by another agent | that agent's tree, as a system run without a principal | none; the parent's principal is not inherited |

The principal comes from ADR-0018's verified `source.principal_id`, never from
message content or model arguments. Accounts linked to one principal share
that principal's scope.

The agent definition lists which principals may write agent-wide memory. For a
single-owner assistant this is usually the owner. Otherwise agent-wide memory
is read-only in principal runs, so one person's facts cannot be copied into
memory another person's run will see.

Mounting is provider-specific but must keep unmounted scopes unreadable:

- Seatbelt grants only the run's materialized scope directories.
- E2B uploads the scopes at run start and downloads them at commit.
- Unsandboxed in-process tools cannot enforce this boundary. A composition
  using them may enable memory only for an agent with at most one principal,
  and must report that memory is unscoped.

### 3. Runs work on a copy; the host commits

Every run receives a private copy of its scopes at a recorded base revision.
When hands stop cleanly after a completed or failed run, the host compares the
copy with its base and commits changes for each writable scope atomically:

- If canonical content for any changed file moved since the run's base, the
  commit for that scope is rejected as a conflict. The run's proposed tree is
  retained in history for the administrator, and the next activation receives
  a bounded conflict notice.
- Changes to read-only scopes are discarded and reported.
- A cancelled run, or a run whose hands did not stop cleanly, commits nothing.

A memory commit is independent of the lineage outcome: a failed run may still
commit what it saved, and a rejected commit does not fail the lineage. With
ADR-0016's default of one active run per agent, conflicts are rare.

### 4. Validation and limits

Before commit, the host rejects the scope's change set if it contains
non-regular files, symlinks, paths escaping the scope, non-UTF-8 content, or
content beyond the configured per-file, per-scope, and file-count limits. The
host runs configured secret-pattern checks, but does not claim to detect every
secret. Credentials belong in credential slots, never in memory.

### 5. History, audit, and management

Each accepted commit creates an immutable scope revision stored through the
same content-addressed `CheckpointStore` backends as ADR-0022, in a separate
memory namespace. SQLite records the revision ID, parent revision, run ID,
lineage, changed paths, and digests. Each run records the base revisions it
received, so the memory it saw can be reconstructed while those revisions are
retained.

An authorized management surface can list, read, edit, and restore scopes to
an earlier revision. A restore creates a new revision; it never rewrites
history. Purging content removes it from future materialization and, subject
to retention policy, from stored revisions; audit keeps IDs and digests
without claiming to replay erased text. Deleting an agent or principal needs a
defined cascade policy before that operation is exposed.

Model-controlled hands never reach the canonical store, other scopes, or the
management surface.

### 6. Hydration and capture

Each run's context includes the `MEMORY.md` of every mounted scope, bounded by
a configured byte budget, with its mount path. It is rendered as attributed,
lower-trust data, never as a system or administrator instruction. The agent
reads individual memory files with its ordinary file tools. Memory text,
including text the agent wrote itself, cannot promote itself into
instructions; administrator instructions stay in the agent definition.

Each agent definition selects a capture policy:

- `explicit` (default): only the agent's own file edits during ordinary runs
  and administrator edits change memory.
- `extract`: after a root lineage becomes terminal, the runtime queues one
  extraction activation for the same agent, idempotent by originating
  `root_id`. It is a normal finite run with `source.kind = system`, its own
  lineage, the originating lineage's scopes, and only file tools on its memory
  mounts. It cannot deliver, delegate, or reach the workspace. Its commit
  follows sections 3 and 4.

An agent may require review of extraction commits: they are stored as
proposed revisions, are not materialized, and become current only when an
authorized reviewer accepts them. A failed extraction is not retried
automatically and does not affect the originating lineage.

Memory holds compact reusable knowledge. ADR-0021 owns durable task state and
wakes; ADR-0020 owns transferable files; conversation history remains the
source for exact quotes, prior actions, and tool outcomes.

## Store changes

The store adds memory scope revisions, per-run base revisions, commit audit
rows, and review state. Revision content is stored through `CheckpointStore`.

[`docs/proposals/persistent-agents/plan.md`](../proposals/persistent-agents/plan.md) builds this in
two steps: a single-owner version with local mounts in phase 2, then principal
scopes, E2B, and a background memory agent in phase 7.

### Phase 2 simplification

Phase 2 implements sections 1, 2 (one agent-wide scope), and 6's hydration, but
deliberately not sections 3 to 5. The run is granted the canonical
`agents/<id>/memory/` directory itself; there are no per-run copies, commits,
conflict retention, revision log, or management commands. Memory should need
no attention from its owner, and models curate their own memory poorly through
tools, so host-side versioning added machinery without making memory better.
The agent keeps direct file access so it can apply corrections, and curation
moves to a background memory agent (section 6's extraction and consolidation)
in phase 7, which revisits sections 3 to 5 for multi-principal writes.

## Verification requirements

Deterministic tests without provider credentials or network prove:

- two agents never see each other's memory, including for the same principal
  and across delegation in either direction;
- one principal's scope is not materialized into another principal's run or
  an unattributed system run, and linked accounts share one scope;
- agent-wide memory is read-only for principals who are not agent-wide
  writers, and discarded writes are reported;
- concurrent runs that change the same file produce one commit and one
  retained conflict, never a silent overwrite;
- a cancelled or uncleanly stopped run commits nothing, while a failed run's
  clean commit persists;
- symlinks, escaping paths, special files, oversized content, and file-count
  overruns are rejected;
- recorded base revisions reconstruct what a run saw, and a restore creates a
  new revision without changing history;
- `MEMORY.md` is rendered as attributed data within its byte budget;
- an extraction run runs once per lineage, touches only its memory mounts, and
  under review does not change materialized memory until accepted; and
- the unsandboxed composition refuses memory for an agent with more than one
  principal.

## Alternatives

- **SQLite memory records with dedicated tools:** rejected. They offer
  precise per-record checks, but are less inspectable, need a bespoke
  recall/revise API, and ignore how effectively agents already use files.
  Mount-level scoping and commit audit recover the properties that matter.
- **One shared directory per agent:** rejected because it leaks between
  principals.
- **Memory inside the workspace:** rejected because workspaces may be
  per-conversation or shared between agents under ADR-0022, and their
  checkpoints have different retention.
- **Conversation-scoped memory:** dropped; the conversation transcript already
  serves that purpose.

## Consequences and non-goals

Memory is plain files the owner can read and correct, and agents use tools
they already know. Scoping depends on the hands provider enforcing mounts, so
principal-scoped memory needs Seatbelt or E2B. An `explicit` agent may fail to
save something; an `extract` agent costs one extra activation per completed
lineage. Within one principal's scope, nothing stops an agent from writing
poor or misplaced notes; history and restore keep that correctable.

This ADR does not define memory shared between agents, group-conversation
memory, vector search, embeddings, or cross-domain knowledge transfer.
Ranking or indexes may be added later only as rebuildable projections that
cannot widen access.
