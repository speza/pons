# Remote workspace provisioning design

**Status:** `archive/v1` implemented; `git/v1` deferred
**Date:** 2026-09-21
**Related:** ADR-0009, ADR-0015

## Goal

A remote workspace is an independent durable line of work, not a mounted or
synchronized view of a developer's checkout. The source supplies initial
content. The logical workspace survives replacement or deletion of the VM that
currently hosts it.

```text
source identity     repository or archive used to seed work
workspace identity  independent mutable and durable line of work
placement identity  replaceable VM currently hosting that workspace
```

The runtime conversation ID currently supplies the workspace identity. Several
workspaces may use the same source and immutable base revision without sharing
mutable state.

## Durable resources

Logical state and provider placement have different lifetimes:

```text
workspaces                              execution_environments
──────────                              ──────────────────────
id                  <────────────────── workspace_id
strategy                               provider
source_ref                             environment_id
base_revision                          template
checkpoint_ref                         network_policy
setup_generation                       status / run_id
created_at / updated_at                idle_until / expires_at

one logical workspace ───── 0 or 1 current retained environment
```

Deleting an environment does not delete its workspace. A replacement VM can
restore the latest checkpoint.

`source_ref` and `checkpoint_ref` are non-secret identifiers. They must never
contain embedded credentials.

## Checkpoint storage

Checkpoint bytes live outside SQLite behind `environment.CheckpointStore`:

```go
type CheckpointStore interface {
    PutWorkspaceCheckpoint(context.Context, string, []byte) (string, error)
    WorkspaceCheckpoint(context.Context, string, string, int64) ([]byte, error)
    PruneWorkspaceCheckpoints(context.Context, string, []string) error
}
```

The filesystem implementation in `runtime/checkpoint` stores content-addressed
archives under the runtime state directory. Runtime wiring supplies it separately
from `runtime/sqlite`, which stores metadata and opaque checkpoint references:

```text
<state-dir>/
├── runtime.db
└── workspaces/
    └── <hash-of-workspace-id>/
        └── <sha256>.tar
```

A future implementation may place the same bounded objects in S3 or another
object store. Object-store credentials remain host-side; the host carries the
archive through envd. The immutable base and current checkpoint are retained;
after advancing workspace metadata, the provider prunes the superseded
intermediate checkpoint on a best-effort basis.

The runtime state directory must be outside the source workspace so the initial
seed cannot capture runtime history or recursively include checkpoint objects.

## Current strategy: `archive/v1`

### First placement

```text
resolve canonical source path
  -> archive source once
  -> store archive as base and current checkpoint
  -> create logical workspace row
  -> create E2B VM
  -> upload and extract checkpoint
  -> start fresh pons-hands
```

The source directory is never a checkpoint destination and is never silently
replaced.

### Reused placement

```text
load logical workspace
  -> reconnect retained compatible VM
  -> stop any stale pons-hands process
  -> start fresh pons-hands
```

### Replacement placement

```text
load logical workspace
  -> no retained VM exists
  -> create E2B VM
  -> load latest checkpoint from CheckpointStore
  -> upload and extract checkpoint
  -> start fresh pons-hands
```

### Run completion

```text
stop pons-hands cleanly
  -> reserve sandbox for recovery
  -> create bounded remote archive
  -> download and validate archive
  -> persist content-addressed checkpoint
  -> advance workspaces.checkpoint_ref
  -> mark environment idle
```

Checkpointing occurs after every completed run rather than immediately before
idle deletion. Planned deletion is therefore cheap, and an unplanned VM loss
loses at most work since the last completed checkpoint.

If hands does not stop cleanly, the sandbox is discarded rather than reused.
If hands stops cleanly but checkpoint download, validation, or storage fails,
the prior durable checkpoint remains authoritative and the sandbox is reserved
for manual recovery for one hour. New runs are blocked during that window.
The error reports the sandbox ID and deadline, or warns that retention could
not be fully recorded or extended. There is no automatic checkpoint retry.

`archive/v1` is suitable for smoke tests, experiments, non-Git sources, and
isolated remote work. An explicit export operation may later materialize a
checkpoint into a chosen destination; automatic source synchronization is not
part of the strategy.

## Future strategy: `git/v1`

A Git workspace uses the same logical workspace and placement model:

```text
strategy        git/v1
source_ref      trusted repository configuration ID
base_revision   immutable initial commit
checkpoint_ref  latest durable recovery state
```

Several independent workspaces may clone the same repository and base commit.
Each should normally publish through a distinct branch or checkpoint ref, for
example:

```text
refs/heads/pons/<workspace-id>/work
```

Git publication and workspace recovery are related but distinct:

```text
Agent Git capability
  commit / fetch / rebase / push
  publishes useful work

Host checkpoint capability
  preserves dirty or unpushed state
  allows recovery after VM loss
```

A first Git implementation may use a pushed commit as its durable checkpoint
when the tree is clean. Dirty or unpushed state must either be captured by a
host-owned blob checkpoint or explicitly reported as non-durable.

## Agent Git authority

Git remains available through the existing bash tool. Hands may run ordinary
commands including `git commit`, `fetch`, `rebase`, and authenticated `push`.
The credential is therefore an intentional agent capability, not a value that
can be hidden from unrestricted bash.

Never pass developer credentials, organization-wide tokens, or `E2B_API_KEY`
to hands. The preferred initial credential is a short-lived GitHub App
installation token with:

- access to one configured repository;
- metadata read and contents read/write only;
- no administration or organization-wide authority;
- no workflow authority unless separately justified;
- short expiration; and
- protected destination branches.

A stronger later design can exchange E2B workload identity through a trusted
credential broker. Credential helpers reduce accidental persistence but do not
hide authority from hands.

Credentials and credential references must not appear in SQLite, repository
URLs, workspace archives, transcripts, logs, tool results, or templates.

## Provider contract

The common execution contract remains provider-neutral:

```go
type Provider interface {
    Start(context.Context, Spec) (HandsSession, error)
}

type Spec struct {
    WorkspaceID   string // logical identity
    WorkspacePath string // local source for local/archive providers
    RunID         string
    // command, environment, network policy, protocol limits...
}
```

Provider-specific template, image, region, compute, and credential-broker
configuration stays on the provider. Modal, Kubernetes, and E2B can share the
same workspace metadata and checkpoint contracts while implementing placement
differently.

When `git/v1` is implemented, add an explicit non-secret workspace plan rather
than overloading `WorkspacePath` with repository configuration.

## Network and credentials

Archive hands retain default-deny networking. Git-capable hands need egress to
the configured repository host. Prefer separate setup and hands policies and
repository-host allowlists when the execution platform supports them.

E2B API keys, envd access tokens, object-store credentials, and other provider
control-plane credentials remain host-only.

## Failure policy

- Source seeding or setup failure starts no hands session.
- A failed checkpoint leaves the prior checkpoint authoritative.
- Environment deletion leaves logical workspace state intact.
- Provider expiry restores from the latest durable checkpoint; no tool action
  is retried automatically.
- A rejected Git push is a normal bash result for the agent to resolve.
- Credential acquisition never falls back to a broader credential.
- Independent workspace histories are never silently merged.

## Next stages

1. Keep `archive/v1` checkpoints bounded, validated, and content-addressed.
2. Add explicit checkpoint export and logical workspace deletion policy.
3. Add an S3-compatible `CheckpointStore` when multi-host durability is needed.
4. Add non-secret Git workspace plans and in-VM clone.
5. Add short-lived repository-scoped Git credentials.
6. Define durable handling for dirty and unpushed Git state.
7. Add branch-policy, credential-leakage, expiry, and conflicting-push tests.

Live provider and Git tests remain opt-in and credential-dependent.
