# Remote workspace provisioning design

**Status:** Proposed; `archive/v1` implemented, `git/v1` deferred
**Date:** 2026-09-21
**Related:** ADR-0009, ADR-0015

## Goal

Support isolated remote workspaces without making the host checkout and the
sandbox filesystem appear to be one mounted directory. Workspace behavior is
selected explicitly by a versioned strategy:

- `archive/v1` transfers a bounded local snapshot into a retained sandbox and
  checkpoints the resulting tree back to the host;
- `git/v1` will give the agent a repository-backed worktree inside the sandbox,
  including ordinary credentialed Git commands through its bash tool.

Local and Seatbelt providers continue to operate directly on the local
workspace. This document concerns remote environments.

## Identities

Remote execution has three distinct identities:

```text
source identity     repository or archive from which work starts
workspace identity  one independent mutable line of agent work
placement identity  the replaceable VM currently hosting that workspace
```

Several VMs may clone the same repository and immutable base revision. They do
not share mutable state unless they intentionally push to the same Git ref. A
retained VM is a performance optimization; durable Git state must survive its
loss.

The workspace identity should normally follow the conversation or another
explicit task/workspace ID. It must not be inferred from a local checkout path
for `git/v1`.

## Current strategy: `archive/v1`

The E2B provider currently:

1. archives the configured local workspace with a size bound;
2. uploads and extracts it when creating a sandbox;
3. retains the remote filesystem across runs;
4. starts a fresh `pons-hands` protocol process for each run; and
5. downloads a validated checkpoint after the run.

This mode is useful for smoke tests, experiments, non-Git directories, and
proving remote lifecycle and transport behavior. It is not the final workflow
for independently publishable engineering work.

The current checkpoint replaces the local workspace, preserving remote
changes and deletions. Concurrent local editing during an archive-backed run is
unsupported because it can diverge from the retained remote tree. A later
archive revision should add digest-based conflict detection or artifact-only
export before being presented as a collaborative synchronization mechanism.

## Future strategy: `git/v1`

### Workspace plan

The future domain contract should carry a non-secret plan resembling:

```go
type WorkspacePlan struct {
    ID                 string // stable independent workspace identity
    Strategy           string // archive/v1 or git/v1
    SourceRef          string // opaque trusted source configuration reference
    BaseRevision       string // immutable initial commit
    CheckpointRevision string // latest known durable remote commit
    SetupGeneration    int
}
```

The exact type should be added when `git/v1` is implemented rather than exposed
speculatively in the current provider API. Existing durable state already
reserves the strategy, source, base revision, checkpoint revision, and setup
generation fields. `environment.State.Key` can hold the workspace identity.

`SourceRef` is an identifier resolved through trusted configuration. It must
not be a clone URL containing credentials.

### Setup lifecycle

For a new Git workspace:

```text
resolve trusted source configuration
  -> create VM
  -> acquire repository-scoped credential capability
  -> clone inside VM
  -> checkout checkpoint revision, or immutable base revision
  -> configure workspace branch/ref
  -> start pons-hands
```

For a replacement VM:

```text
create VM
  -> clone the same source
  -> checkout latest durable checkpoint revision
  -> continue the workspace
```

The VM therefore does not become the only durable copy of useful work.

### Agent Git capability

Git is part of the agent's normal development capability. Through the existing
bash tool, hands may run operations including:

```text
git status
git diff
git log
git fetch
git checkout / switch
git add
git commit
git rebase
git push
```

`git push` is not translated into a special pons tool. It remains an ordinary
bash action so the agent can use standard repository workflows.

Authenticated Git means the sandbox intentionally possesses repository write
authority. A credential usable by ordinary Git under unrestricted bash cannot
also be considered secret from that bash process. Security therefore comes
from capability scope, lifetime, repository policy, isolation, and audit—not
from claiming the token is invisible to hands.

### Credential model

Never pass developer credentials, organization-wide tokens, or `E2B_API_KEY`
to hands. The preferred first implementation uses a short-lived GitHub App
installation token with:

- access to one configured repository;
- metadata read and contents read/write only;
- no administration or organization-wide authority;
- no workflow authority unless separately justified;
- a short expiration; and
- a dedicated agent branch/ref namespace.

E2B can transport explicit process environment values securely into its
isolated VM, but values supplied to `pons-hands` are readable by the agent. A
credential helper may avoid accidental persistence in URLs and Git config; it
does not make the credential inaccessible to arbitrary bash.

A stronger later design can use E2B workload identity:

```text
sandbox workload identity
  -> trusted credential broker
  -> short-lived repository capability
```

A non-exportable SSH agent or policy-enforcing Git proxy could further prevent
private-key extraction while still allowing `git push`, but the agent can
exercise whatever scoped authority that capability represents.

Credentials and credential references must not appear in:

- SQLite environment state;
- repository URLs or `.git/config`;
- workspace files or archives;
- transcripts, logs, tool results, or checkpoint metadata; or
- templates and long-lived sandbox environment variables.

### Branch and repository policy

Each independent workspace should default to a distinct branch or checkpoint
ref, for example:

```text
refs/heads/pons/<workspace-id>/work
```

Multiple VMs may use the same source and base commit while pushing separate
refs. Protected branches should reject direct pushes and force-pushes, require
normal checks, and require pull requests where appropriate. Tags and unrelated
refs should be denied by repository policy where the hosting provider supports
it.

The agent may intentionally share a branch with another workspace, but normal
Git non-fast-forward or force-with-lease behavior must surface conflicts rather
than silently replacing another workspace's result.

### Completion and durability

The runtime must not assume that every successful agent run pushed its work.
At session close, a Git provider should inspect and report at least:

```text
current branch/ref
current HEAD
latest known remote checkpoint revision
dirty working tree
unpushed commits
```

If the agent pushed successfully, `checkpoint_revision` records the resulting
remote commit. Dirty or unpushed work remains dependent on the retained VM and
must be reported as non-durable. A future policy may require a durable push
before a run is considered complete, but automatic commit/push is not implied
by the first Git contract.

Publishing a normal branch or pull request can be performed by the agent using
its scoped Git capability and, if granted, a separate pull-request capability.
Repository branch protection remains the final authority.

## Durable state

`execution_environments` stores only non-secret placement and compatibility
metadata:

- `environment_key`: stable workspace identity;
- `workspace_strategy`: `archive/v1` or future `git/v1`;
- `workspace_source_ref`: opaque trusted source ID;
- `workspace_revision`: immutable base revision;
- `checkpoint_revision`: latest durable archive digest or Git commit;
- `setup_generation`: operator-controlled compatibility generation;
- provider, VM ID, template, network policy, lifecycle, and expiry fields.

A provider must discard or re-provision an idle environment when its strategy,
source, requested revision, network policy, template, or setup generation is
incompatible. It must not silently merge independent workspace histories.

## Network policy

Archive hands retain the existing default-deny network policy. A Git-capable
agent needs egress to its configured repository host while cloning, fetching,
or pushing. The eventual policy should distinguish setup access and hands
access and should prefer repository-host allowlists over unrestricted internet
access when the execution platform supports them.

## Failure policy

- Setup failure prevents hands startup and records no reusable environment.
- Provider expiry falls back to the latest durable Git checkpoint; no tool
  action is retried automatically.
- Dirty or unpushed Git work is explicitly non-durable if the VM is lost.
- A rejected push is returned as a normal bash/tool result for the agent to
  resolve.
- Credential acquisition failure does not fall back to a broader credential.
- An interrupted archive checkpoint leaves the previous local tree intact and
  discards uncertain reusable state.

## Interface impact

No new public Go interface is required for `git/v1` in the current PR:

- `environment.Provider` and `HandsSession` already cover placement and tool
  execution;
- `StateStore` already persists strategy-neutral workspace metadata;
- `environment.State.Key` can become the remote workspace ID; and
- the current `Spec.Workspace` remains the local path required by
  `archive/v1` and Seatbelt.

When Git support is implemented, add an explicit workspace plan or source
configuration to `environment.Spec` rather than overloading `Workspace` or
passing secrets through it. Credential acquisition belongs in provider
configuration or an injected credential broker, not serialized domain state.

## Implementation stages

1. Ship and clearly label the current `archive/v1` strategy.
2. Add explicit workspace identity and source-plan types with no credentials.
3. Resolve trusted repository configuration and immutable base revisions.
4. Implement in-VM clone and credential-free Git operations.
5. Add short-lived, repository-scoped write credentials for hands bash.
6. Persist and verify remote checkpoint revisions across VM replacement.
7. Add branch-policy, credential-leakage, expiry, and conflicting-push tests.
8. Add workload-identity credential exchange or a non-exportable Git capability
   if stronger isolation is required.

Live provider and Git tests remain opt-in and credential-dependent.
