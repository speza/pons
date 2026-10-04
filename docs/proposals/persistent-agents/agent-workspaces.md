# Agent workspaces spec

**Status:** Implemented (delivery steps 1 and 2)
**Date:** 2026-09-29
**Related:** [plan](plan.md) phase 6,
[ADR-0022](../../adr/adr-0022-persistent-agent-workspaces.md),
[remote workspaces](../../design/remote-workspaces.md),
[hands environment](../../design/hands-environment.md)

This spec covers the first slice of phase 6: an agent gets one workspace of
its own that lasts across conversations. Checkpoint history, restore, setup
scripts, exclusions, and off-host storage follow in later slices (see
[Later slices](#later-slices)).

## Summary

Today a workspace belongs to a conversation. The client picks a host
directory or a Git repository for each new conversation, and on E2B every
conversation starts from a fresh file system. A persistent agent has nowhere
to keep the notes, scripts, and data it builds up over time.

After this change, each agent definition chooses a workspace policy. The
default agent uses `agent`: one workspace, owned by the agent, used by every
conversation it has. Files the agent writes on Monday are there on Tuesday,
in a new conversation, on a replaced E2B sandbox.

## Current behavior

| | Seatbelt | E2B |
| --- | --- | --- |
| Workspace chosen by | client, per conversation (host path under `--workspace-root`) | client, per conversation (host source path or Git repository) |
| `spec.WorkspaceID` | conversation ID | conversation ID |
| Workspace lock | host path | conversation ID |
| Persistence | the host directory itself | `archive/v1` checkpoints, scoped to one conversation |

`spec.WorkspaceID = request.ConversationID` is set in
`cmd/pons/runtime_mode.go`; the lock is chosen in
`runtime.Manager.CreateConversation`.

## Goals

- An agent's files persist across conversations on Seatbelt and E2B.
- An E2B agent workspace needs no host source directory.
- Runs that share a workspace never overlap.
- A run's hands can reach the agent's workspace and memory, and nothing else
  in the agent directory.

## Non-goals

- Checkpoint history, retention, and restore (next slice).
- Setup scripts, setup generations, and checkpoint exclusions.
- Seatbelt checkpoints and off-host (S3) checkpoint storage.
- The `shared(<id>)` policy; it waits for a second agent (plan phase 12).
- `template` seeds.
- Choosing a repository or other workspace per conversation under the `agent`
  policy; that moves to task profiles (plan phase 9).
- Moving a workspace between Seatbelt and E2B.

## Design

### Workspace policy

`agent.json` gains a `workspace` field:

```json
{
  "name": "Ada",
  "provider": "",
  "model": "",
  "max_turns": 0,
  "workspace": "agent"
}
```

| Value | Meaning |
| --- | --- |
| `agent` | One workspace for every conversation the agent owns. |
| `per_conversation` | Current behavior: the client picks a host directory or Git repository per conversation. |
| empty or missing | `agent`. |

Any other value fails startup with an error naming the file. The resolved
policy becomes `AgentDefinition.WorkspacePolicy`, so it is part of the
revision fingerprint.

### Workspace identity

A conversation's workspace is fixed when the conversation is created, and is
recorded on it as `workspace_id`, with `agent_workspace` marking the agent's
own workspace. The runner uses these rather than deriving them from the
conversation ID, the workspace ID's spelling, or the current agent revision,
so changing the policy never moves an existing conversation to a different
workspace.

| Policy | `workspace_id` | Workspace lock |
| --- | --- | --- |
| `agent` | `agent-<agent id>` | `workspace_id` |
| `per_conversation`, Seatbelt | conversation ID | host path (as today) |
| `per_conversation`, E2B or Git | conversation ID | conversation ID (as today) |

`WorkspaceLock` stays the serialization key. Every conversation of an agent
using the `agent` policy shares one lock, so the agent runs one thing at a
time: a scheduled run waits for an interactive one, and vice versa. That is
deliberate: two runs editing the same files at once is worse than one
waiting.

### Conversation creation

Under the `agent` policy, `CreateConversation` takes no workspace choice. It
rejects `workspace`, `git_repository`, `git_revision`, and
`git_all_repositories`, and uses the server's default sandbox as the
environment; a conversation that names a different environment is rejected.
One agent workspace therefore lives in exactly one provider.

Under `per_conversation`, creation behaves as today.

The agent summary returned to clients gains `workspace_policy` so a client
knows whether to show a workspace picker.

### Seatbelt

The agent workspace is a directory the server owns:

```text
<state_dir>/agents/<id>/
  agent.json
  PERSONA.md
  revisions/
  memory/
  workspace/      new: the agent's files
```

`agentdir.Store.Workspace` creates `workspace/` (mode 0700) on first use and
requires it to be a real directory, like `memory/`. Agent IDs `.` and `..` are rejected, so the
directory can never resolve outside `agents/<id>/`. It is not created at
startup: hands can replace it, and that must fail only later runs, not server
startup. The runner sets `spec.WorkspacePath` to it. Seatbelt grants `workspace/` and
`memory/` read-write; `agent.json`, `PERSONA.md`, and `revisions/` stay
outside every grant.

The server chooses this path itself, so it bypasses
`validateConversationWorkspace`. That check now rejects client-selected paths
inside the state directory as well as paths containing it, so a
`per_conversation` client cannot reach `agent.json` or share the agent
workspace under a different lock.

No checkpoints are taken on Seatbelt in this slice; the directory is the
durable copy.

### E2B

E2B keeps its existing placement and per-run checkpoint flow, keyed by
`workspace_id`. Because every conversation of the agent shares
`agent-<id>`, a new conversation reconnects to the retained sandbox or
restores the latest checkpoint onto a new one.

A new `empty` workspace strategy seeds the first placement:

```text
no workspace row for agent-<id>
  -> store an empty archive as base and current checkpoint
  -> create logical workspace row (strategy empty)
  -> place as for archive/v1
```

After seeding, `empty` persists exactly like `archive/v1`. The provider's
current rule that a non-Git workspace needs `spec.WorkspacePath` no longer
applies to `empty`. The memory directory keeps its existing sync; it is not
part of the workspace checkpoint.

### Runner

In `cmd/pons/runtime_mode.go`:

- `spec.WorkspaceID` comes from `request.WorkspaceID`, carried from the
  conversation.
- `spec.WorkspacePath` is the agent workspace directory (Seatbelt), the
  conversation's host path (`per_conversation`), or empty (E2B `empty`).
- `spec.WorkspacePlan` is `empty` for an E2B agent workspace, and otherwise
  as today.
- The run log's `workspace_id` field reports the real workspace ID instead
  of the conversation ID.

### Store

- `conversations` gains `workspace_id` (required, never defaulted) and
  `agent_workspace`.
- The E2B `workspaces.strategy` column accepts `empty`.
- The SQLite schema version is bumped. Older databases are rejected, per the
  project's pre-compatibility rule; the error says to remove the database and
  `workspaces/` but keep `agents/`, which now holds the agent's workspace.

### Clients

- CLI: `client` defaults `-workspace` to the current directory only when the
  server's agent reports `per_conversation`; under `agent` it sends no
  workspace and prints that the current directory is not used; it rejects
  an explicit `-workspace` or Git option itself before creating anything.
- Web: when the agent's policy is `agent`, the new-conversation form drops
  the environment and source pickers and shows "Agent workspace". Under
  `per_conversation` it is unchanged. Until the policy is known, the form
  offers no choice and cannot create; if loading it fails, the form offers a
  retry.
- The conversation header shows the workspace ID rather than a host path for
  agent workspaces.

## Tests

All deterministic, with no provider credentials or network:

- **Policy parsing:** empty and missing `workspace` resolve to `agent` on
  Seatbelt and E2B; `per_conversation` is accepted; any other value fails startup; the policy
  changes the revision.
- **Identity:** two conversations under `agent` share `agent-<id>` and one
  lock; under `per_conversation` they keep today's IDs and locks.
- **Creation:** `agent` rejects workspace, Git options, and a non-default
  environment.
- **Stable workspace:** a conversation created under `per_conversation`
  keeps its workspace after the owner switches to `agent`, and the reverse.
- **Serialization:** a queued run in a second conversation of the same agent
  does not start until the first run finishes.
- **Seatbelt persistence:** a file written in one conversation is readable
  in the next; hands cannot read or write `agent.json`, `PERSONA.md`, or
  `revisions/`. The deny checks need real Seatbelt, so they live in the
  opt-in `PONS_SEATBELT_TEST=1` integration test; the default suite covers
  persistence with in-process hands and checks the run's grants.
- **E2B persistence (fake envd):** the first placement seeds an empty base;
  a file written in one conversation survives a second conversation that is
  placed on a replacement sandbox.
- **Store:** the new schema opens; an older schema is rejected.

## Documentation

- `README.md`: `agent.json` `workspace` field; conversation creation without
  a workspace.
- `docs/design/hands-environment.md` and `docs/design/remote-workspaces.md`:
  agent workspaces and the `empty` strategy.
- ADR-0022: implementation status becomes "Partially implemented" (policy
  and `empty` seed).
- `plan.md`: mark phase 6 steps 1 and 2 (`empty` only) done.

## Delivery

1. **Policy, identity, Seatbelt, clients** (done). Everything except E2B
   seeding.
   On E2B an empty policy resolves to `per_conversation`, so a fresh or
   upgraded E2B server keeps working; an explicit `agent` policy stops the
   server at startup with a clear "not yet supported" error naming the file.
   Support is a provider capability, not a sandbox name: a durable provider
   (one that keeps a remote copy it checkpoints, today only E2B) cannot host
   the agent workspace in place. The server checks it at startup, and the
   runner checks it again, with the run's workspace fields, before starting
   hands.
2. **E2B `empty` seed** (done). Removes that error and makes an empty policy
   resolve to `agent` on E2B too. A durable provider runs the agent workspace
   with no `spec.WorkspacePath` and an `empty` plan; others run in the agent's
   `workspace/` in place.

## Later slices

Tracked separately, in roughly this order:

1. Checkpoint history and restore: a checkpoint table replacing the single
   `checkpoint_ref`, retention, and `pons workspace checkpoints|restore`.
2. Seatbelt checkpoints after clean runs, so restore works locally.
3. Setup scripts, setup generations, and checkpoint exclusions.
4. S3-compatible `CheckpointStore`.
5. `template` seeds and `shared(<id>)`, when a second agent needs them.
