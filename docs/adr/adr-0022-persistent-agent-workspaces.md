# ADR-0022: Persistent agents keep durable workspaces without Git

**Status:** Proposed
**Implementation:** Not implemented; builds on implemented `archive/v1` checkpoints
**Date:** 2026-09-24
**Related:** ADR-0009, ADR-0015, ADR-0016, ADR-0017, ADR-0019, ADR-0020

## Context

A persistent agent accumulates files: notes, scripts, downloaded data, small
tools it wrote, and configuration. Many of these agents have no repository, so
Git cannot be the durability mechanism. The files must survive VM loss,
replacement, and months of use, and the owner must be able to roll back a bad
change.

Most of the mechanism exists. The `archive/v1` strategy in
[`remote-workspaces.md`](../design/remote-workspaces.md)
checkpoints an E2B workspace to a content-addressed `CheckpointStore` after
every completed run and restores the latest checkpoint onto a replacement VM.
It is shaped for short coding tasks:

- the workspace ID is the conversation ID, so every conversation starts from
  a fresh file system;
- a workspace must be seeded from a source path on the server;
- only a filesystem `CheckpointStore` exists;
- retention keeps only the base and latest checkpoint;
- there is no restore to an earlier checkpoint; and
- local Seatbelt workspaces have no checkpoints.

## Decision

### 1. Workspace identity is agent policy

Each agent definition selects a workspace policy, resolved to ADR-0016's
validated `workspace_id`:

- `per_conversation`: a new workspace per root conversation (current
  behavior). Suitable for isolated coding tasks.
- `agent`: one workspace for every conversation the agent owns, the agent's
  own computer. Suitable for a persistent assistant.
- `shared(<id>)`: an administrator-named workspace which several agent
  definitions may use under ADR-0016's workspace validation.

Workspace exclusion under ADR-0016 serializes runs that share a workspace.
A task uses the workspace policy and named workspaces of its ADR-0016 task
profile, such as a fresh workspace or a configured repository, and never the
parent conversation's workspace.

### 2. Seeding does not require a source

A new logical workspace is seeded by one of:

- `empty`: an empty directory;
- `template(<id>)`: an administrator-configured archive, stored once in the
  checkpoint store and referenced by digest;
- `source(<path>)`: the existing `archive/v1` source-path seed; or
- `git(<repository>)`: the existing `git/v1` strategy.

The seed is recorded as the workspace's base. After seeding, all four
strategies persist through the same checkpoint mechanism; Git remains an
optional publishing capability, not the recovery path.

### 3. Capabilities are reproducible; state is checkpointed

Installed tools and packages are environment configuration, not state. An
agent's workspace policy may name:

- a provider template or image;
- a setup script run on first placement and whenever its setup generation
  changes; and
- checkpoint exclusions for rebuildable paths such as caches, virtual
  environments, and `node_modules`.

Setup outputs outside the workspace are never checkpointed. Excluded paths are
rebuilt by setup or by the agent. This keeps checkpoints small and makes a
replacement VM converge to the same capabilities without archiving them.

### 4. Every provider can checkpoint

E2B keeps its implemented per-run checkpoint. Seatbelt gains an optional
checkpoint after each run whose hands stopped cleanly. Its workspace is
already a persistent host directory, so the checkpoint is for history and
off-host backup, not recovery of the live copy.

The existing failure rules apply to all providers: a failed checkpoint leaves
the prior one authoritative, and an unclean hands stop skips checkpointing.
Checkpoints follow the same archive size, entry-count, symlink, and path
limits.

### 5. Checkpoint storage may be off-host

`CheckpointStore` gains an S3-compatible object-store implementation beside
the filesystem one. Object-store credentials stay in host composition and
never reach brains or hands. Objects remain content-addressed and verified by
digest and size on read. ADR-0019 memory revisions and ADR-0020 artifact
blobs may use the same backend in separate namespaces.

Off-host storage protects workspace and memory content. Backing up the
runtime database is an operator responsibility and is documented separately;
pons does not claim a full-server backup.

### 6. Retention and restore

Each workspace has a retention policy, for example the latest checkpoint,
the last N checkpoints, and one per day for D days. The base is retained while
any retained checkpoint depends on it. Pruning is best-effort and never
removes a checkpoint referenced by workspace metadata or an active
restoration.

An authorized management surface lists checkpoints with time, run, size, and
digest, and restores a workspace to a chosen checkpoint. Restore requires no
active run in that workspace, creates a new current checkpoint with the old
content, discards any retained environment, and lets the next placement
extract it. History is never rewritten. Model-controlled hands cannot list,
prune, or restore checkpoints.

### 7. Visibility

Workspace views show policy, seed, current checkpoint, checkpoint size
against its limit, and the latest checkpoint failure. The owner is warned
before a workspace reaches its limit rather than discovering it through a
failed checkpoint.

## Store changes

`workspaces` gains policy, seed kind and reference, setup generation,
exclusions, and retention policy. A checkpoint table replaces the single
current reference with retained history.

[`docs/proposals/persistent-agents/plan.md`](../proposals/persistent-agents/plan.md) places this work
in phase 6.

## Verification requirements

Deterministic tests without provider credentials or network prove:

- an `agent` workspace persists across conversations while a
  `per_conversation` workspace does not;
- `empty` and `template` seeds need no source path, and a template is stored
  once;
- excluded paths are absent from checkpoints and setup reruns when its
  generation changes;
- retention keeps the configured set, never prunes a referenced checkpoint,
  and keeps the base while needed;
- restore requires an idle workspace, creates a new checkpoint, and does not
  alter older ones;
- a Seatbelt checkpoint failure leaves the prior checkpoint authoritative and
  the live directory untouched; and
- an object-store fake verifies digest and size on read and never exposes its
  credentials to hands.

## Consequences

Persistent agents get a durable computer without Git, with history, rollback,
and optional off-host storage, by extending the existing checkpoint model
instead of adding a second persistence system. Whole-workspace archives after
every run cost time and storage as workspaces grow; exclusions and retention
bound that, and incremental checkpoints can come later behind the same
interface.

This ADR does not define incremental or block-level snapshots, live
synchronization with the owner's machine, VM memory snapshots, or backup of
the runtime database.
