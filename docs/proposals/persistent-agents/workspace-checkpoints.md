# Workspace checkpoint history spec

**Status:** Implemented for E2B; Seatbelt checkpoints dropped (2026-10-10)
**Date:** 2026-10-04
**Related:** [plan](plan.md) phase 6 step 4,
[ADR-0022](../../adr/adr-0022-persistent-agent-workspaces.md) sections 4
and 6, [agent workspaces spec](agent-workspaces.md),
[remote workspaces](../../design/remote-workspaces.md)

This spec covers the second slice of phase 6: a workspace keeps a history of
checkpoints, old ones are pruned by a retention policy, and the owner can
restore an earlier one. It applies to checkpointed (E2B) workspaces only;
see [Seatbelt](#seatbelt).

## Summary

The first slice gave each agent one workspace that lasts across
conversations. Nothing protects it from a bad change: if the agent deletes or
mangles its files, the next checkpoint overwrites the last good one, and the
store prunes everything except the base and the latest archive.

After this slice, every completed run that changes the workspace adds a
checkpoint to its history. A retention policy keeps recent and daily checkpoints. The owner can
list them and restore one, which makes it the workspace's newest checkpoint
without rewriting history. Agent-controlled hands can do none of this.

## Current behavior

- `workspaces.checkpoint_ref` holds the single current checkpoint.
  `base_revision` holds the base for `archive/v1` and `empty`, and a Git
  object ID for `git/v1`.
- After each completed E2B run, `e2bSession.persistCheckpoint` stores a new
  archive, advances `checkpoint_ref`, and prunes every archive except the base
  and the new current one.
- Archives are content-addressed files under
  `<state_dir>/workspaces/<workspace id>/`.
- Seatbelt workspaces have no checkpoints.
- There is no way to list checkpoints or restore one.

## Goals

- Every checkpoint a workspace takes is recorded with its time, run, and size.
- Retention bounds the history without ever deleting a checkpoint that is
  still referenced.
- The owner can list checkpoints and restore one through the server.
- Restore never rewrites history and never runs alongside a run in that
  workspace.

## Non-goals

- Configurable retention. This slice uses fixed defaults.
- A web UI for checkpoints. The CLI and HTTP API come first.
- Restoring single files, diffs between checkpoints, or exporting a
  checkpoint to a host directory.
- Size warnings before a workspace reaches its limit (ADR-0022 section 7).
- Setup scripts, exclusions, off-host storage, incremental checkpoints.
- Checkpoints for any Seatbelt workspace (see [Seatbelt](#seatbelt)).

## Design

### Checkpoint history

A new table replaces `workspaces.checkpoint_ref`:

```sql
CREATE TABLE workspace_checkpoints (
  workspace_id TEXT NOT NULL REFERENCES workspaces(id),
  seq INTEGER NOT NULL CHECK (seq > 0),
  ref TEXT NOT NULL CHECK (ref <> ''),
  kind TEXT NOT NULL CHECK (kind IN ('base', 'run', 'restore')),
  run_id TEXT NOT NULL DEFAULT '',
  restored_from INTEGER,
  size_bytes INTEGER NOT NULL CHECK (size_bytes >= 0),
  created_at INTEGER NOT NULL,
  PRIMARY KEY (workspace_id, seq)
);
```

- `seq` increases by one per workspace. The current checkpoint is the row
  with the highest `seq`.
- `base` is the first archive of a workspace: the seed of an `archive/v1` or
  `empty` workspace, or the initial checkout of a `git/v1` workspace.
- `run` is the checkpoint after a completed run, with its `run_id`. A run
  whose archive equals the current checkpoint's `ref` appends nothing, so
  runs that change no files cannot push earlier checkpoints out of
  retention. A `restore` is always appended.
- `restore` is written by a restore. Its `ref` is the restored checkpoint's
  `ref`, and `restored_from` is that checkpoint's `seq`.

Because archives are content-addressed, a restore needs no copy: the new row
points at the existing archive. The same `ref` can appear in several rows.

`workspaces.checkpoint_ref` is removed. `environment.WorkspaceState` drops
`CheckpointRef`, and the state store gains:

```go
AppendWorkspaceCheckpoint(ctx, WorkspaceCheckpoint) (WorkspaceCheckpoint, error) // assigns seq
WorkspaceCheckpoints(ctx, workspaceID string) ([]WorkspaceCheckpoint, error)     // ascending seq
CurrentWorkspaceCheckpoint(ctx, workspaceID string) (WorkspaceCheckpoint, error)
DeleteWorkspaceCheckpoints(ctx, workspaceID string, seqs []int64) error
```

`AppendWorkspaceCheckpoint` requires the workspace row, so a seed saves the
workspace and then appends its base. A workspace row with no checkpoints, left
by an interrupted seed, gets its base from the stored seed archive that
`base_revision` names, without reading the source again; only if that archive
is gone is it seeded again. A `git/v1` workspace is checked out again.
Each append also sets the workspace's `updated_at` to the checkpoint's time.
`CheckpointStore.PutWorkspaceCheckpoint` also returns the archive size, which
becomes `size_bytes`.

The SQLite schema changes in place. Its version restarts at 1, and any
database written under another version, including every earlier one up to
12, is rejected per the pre-compatibility rule.

### Retention

After a checkpoint is appended, the workspace's history is pruned. Fixed
defaults:

| Keep | Rule |
| --- | --- |
| Current | the highest `seq` |
| Recent | the 10 highest `seq` values |
| Daily | the newest checkpoint of each UTC day, for the last 14 days |
| Base | the `base` row, always |
| Restore sources | the `restored_from` row of every kept `restore` row |

The archives those rows keep are also bounded to 1 GiB per workspace, or four
times the current checkpoint's size if that is larger. Over budget, the oldest
rows other than the current checkpoint and the base are dropped until the
distinct archives fit; rows sharing an archive count it once. Every checkpoint
is a full archive, so without this a workspace near its size limit could keep
about 25 full copies.

Rows outside that set are deleted in one transaction. Then archive files are
pruned: any file under the workspace's checkpoint directory whose `ref` no
longer appears in any row is removed. File pruning is best-effort and
reports failures through the existing `onError` hook; an orphaned file is
harmless and is removed on a later prune.

A row is never deleted while a restore of it is in progress (see
[Restore](#restore)), and a `ref` is never removed while any row references
it.

### Listing

`GET /v1/workspaces/{id}/checkpoints` returns the history, newest first:

```json
{
  "workspace_id": "agent-default",
  "checkpoints": [
    {"seq": 42, "kind": "run", "run_id": "...", "size_bytes": 81920, "created_at": "...", "current": true},
    {"seq": 41, "kind": "restore", "restored_from": 37, "size_bytes": 79100, "created_at": "..."}
  ]
}
```

`ref` is not exposed; `seq` is the handle. An unknown workspace (no
`workspaces` row, including every Seatbelt workspace) returns
`404`.

### Restore

`POST /v1/workspaces/{id}/restore` with `{"seq": 37}`:

1. **Reserve.** The Manager reserves the workspace's lock so the scheduler
   claims no run for it. The request fails with `409 Conflict` if a run in
   that workspace is active or claimed. Queued submissions stay queued and
   run after the restore.
2. **Validate.** The checkpoint exists. The provider then checks, cheapest
   first and under its workspace lock: the checkpoint fits its workspace size
   limit, no session is live, the workspace has no unexpired environment in
   `recovery` or `active`, and the archive verifies by digest and size. The
   provider owns these checks because it owns placement. A recovery sandbox holds edits that were never checkpointed, so
   restore refuses with `409` until it expires or the owner resolves it. An
   unexpired `active` record without a claimed run is left by a crash and
   blocks new runs the same way, so it also refuses.
3. **Apply.** The provider applies the restore (below).
4. **Record.** A `restore` row is appended, making the old content current.
5. **Release** the reservation.

A failure at any step before 4 leaves the history and the current checkpoint
unchanged. Once step 3 succeeds, step 4 runs even if the request is canceled.
The Manager's `Close` cancels a restore in progress and waits for it, so
shutdown neither hangs on a stalled provider call nor closes the store
between applying and recording a restore. Workspace IDs that are empty or
contain `/`, `\`, or NUL are rejected as unknown before anything is reserved.

The reservation must also be honored by `ClaimRunnable`. It is a
`workspace_reservations` row keyed by workspace ID, which the claim query
excludes through the conversation's `workspace_id`. Reserving fails if a
submission of a conversation with that workspace ID is running or the row
already exists. `RecoverRunning` deletes every reservation at startup, so a
crash leaves none behind. Releasing the reservation wakes the scheduler.

The response is `200` with the new `restore` entry. A malformed body, trailing
data, or a non-positive `seq` is `400`, an unknown workspace or `seq` is
`404`, a server shutting down is `503`, and a corrupt archive is `500` with
the verification error. A failure to release the reservation does not change
the outcome: the release is retried once, then reported in the server log,
and startup clears the reservation.

`runtime.Manager` owns the reservation and calls a `runtime.WorkspaceRestorer`
for steps 2 to 4. `runtime/checkpoint.History` implements it, and listing,
over the state store, the checkpoint store, and the provider's
`environment.WorkspaceRestorer`; it is configured only when the provider
implements one.

Providers implement an optional interface:

```go
type WorkspaceRestorer interface {
	RestoreWorkspace(ctx context.Context, workspaceID string, checkpoint WorkspaceCheckpoint) error
}
```

- **E2B:** delete the retained idle environment, if any, and its state. The
  next placement creates a new sandbox and extracts the current checkpoint,
  which after step 4 is the restored one. Nothing is uploaded during restore.
  It returns `environment.ErrWorkspaceInUse` (mapped to `409`) if a session
  of the workspace is live or its environment is unexpired and not idle, and
  refuses a checkpoint larger than the current `MaxWorkspaceBytes`, which the
  next placement could not load.
A server whose provider does not implement `WorkspaceRestorer` returns
`501 Not Implemented`.

### CLI

```text
pons workspace checkpoints [-server URL] [-workspace-id ID]
pons workspace restore     [-server URL] [-workspace-id ID] SEQ
```

`-workspace-id` defaults to the server's agent workspace. Both are thin
clients of the HTTP API; they never open the state directory directly.

### Seatbelt

Seatbelt workspaces are not checkpointed. A Seatbelt agent workspace is a
plain host directory, so it already persists across conversations and
restarts; checkpoints would only add undo. As with agent memory, pons keeps
no host-side history of it, and the owner's own backups (such as Time
Machine) cover it. A Seatbelt server returns `501` from restore and `404`
from listing.

An earlier draft added Seatbelt checkpoints as a second delivery part; it was
dropped on 2026-10-10 as not worth the per-run archive cost.

### Who can restore

Only the owner, through the loopback HTTP API and the CLI. Hands never
receive the checkpoint store, state store, or server address, and no tool
exposes listing or restore.

## Tests

All deterministic, with no provider credentials or network.

- **History:** each completed fake-envd run appends a `run` row with its run
  ID and size; the first placement of an `empty` workspace appends a `base`
  row; a run that changes nothing appends no row.
- **Retention:** with a fake clock, retention keeps the current, the 10
  newest, the newest per day for 14 days, and the base; older rows are
  deleted; an archive shared by a retained row survives.
- **Restore records:** restoring `seq` N appends a `restore` row with N's ref
  and `restored_from = N`, and leaves earlier rows untouched.
- **Restore applies (E2B):** after restore, the next run's sandbox contains
  the restored files, not the latest ones.
- **Exclusion:** restore returns 409 while a run is active; a submission
  queued during restore runs after it and sees the restored files.
- **Recovery:** restore returns 409 while the workspace has a recovery
  environment.
- **Failure:** a corrupt archive fails restore and leaves the current
  checkpoint unchanged.
- **Crash:** a reservation left by a crash is cleared at startup.
- **HTTP and CLI:** list returns newest first with `current` set; restore of
  an unknown `seq` returns 404.

## Documentation

- `README.md`: `pons workspace checkpoints|restore`.
- `docs/design/remote-workspaces.md`: history, retention, restore; remove
  "base and latest only".
- ADR-0022: update implementation status.
- `plan.md`: mark phase 6 step 4 done.

## Delivery

One part: history, retention, restore (E2B), HTTP and CLI. Delivered in
PR #24.

## Open questions

- Should retention be configurable in host config? Deferred until the fixed
  defaults prove wrong.
- Should restore also offer "restore as of a time"? The CLI can add it later
  on top of `seq`.
