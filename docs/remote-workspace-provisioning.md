# Remote workspace provisioning design

**Status:** `archive/v1`, `git/v1`, and bring-your-own `github_app/v1`
implemented; App Manifest onboarding deferred
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
    PutWorkspaceCheckpoint(context.Context, string, io.Reader, int64) (string, error)
    WorkspaceCheckpoint(context.Context, string, string, int64) (io.ReadCloser, error)
    PruneWorkspaceCheckpoints(context.Context, string, []string) error
}
```

Both operations enforce an archive byte limit. Writes stream into a private
temporary file while hashing, then sync and atomically publish it before metadata
advances. Reads verify size and digest before returning a rewound reader that the
caller must close. Seeding and envd downloads stage bounded temporary files;
validation and uploads use readers, without buffering whole archives in memory.
Validation still extracts into a disposable directory and enforces expanded-size
and entry-count limits. This trades temporary disk space and extra I/O for bounded
payload memory; path metadata still scales with the bounded entry count.

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

The client selects the archive source path when creating the conversation. It
must exist on the server host within the server's allowed workspace root. The
runtime state directory must be outside that source workspace so the initial
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

## Git strategy: `git/v1`

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

The implemented initial slice accepts a credential-free HTTPS repository URL
and a full 40-character SHA-1 commit ID. GitHub repositories use SHA-1 object
IDs today; SHA-256 repositories are outside this initial contract because their
object format must be selected when the local repository is initialized. On
first placement the provider:

```text
create E2B VM
  -> initialize an empty Git repository in the workspace
  -> fetch only the configured immutable commit
  -> check it out on an independent pons/<workspace>/work branch
  -> persist a bounded host-owned archive checkpoint
  -> record the logical workspace
  -> start fresh pons-hands
```

Replacement placement still restores the latest archive checkpoint, preserving
the repository metadata together with dirty and unpushed state. Repository URLs
containing user information, query parameters, or fragments are rejected so
credentials cannot enter workspace metadata or checkpoints. The CLI repository
URL is the initial non-secret `source_ref`; a named repository configuration
registry can replace that direct identity when authenticated repositories are
added.

The workspace strategy and source provider are separate axes. `git/v1` remains
provider-neutral: `environment/gitworkspace` owns plan validation, branch
identity, transient Git process credentials, and result redaction, while an
environment adapter such as E2B owns command execution and checkpoint
transport. A future named repository configuration identifies its source
provider explicitly, for example:

```json
{
  "id": "main-repo",
  "provider": "github",
  "owner": "speza",
  "repository": "pons",
  "auth": "github_app/v1"
}
```

`source_ref` then stores `main-repo`, not the resolved clone URL. Other source
providers can define their own configuration and authentication kinds without
changing `git/v1`, workspace persistence, or checkpoint recovery.

## Agent Git authority

Git remains available through the existing bash tool. Hands may run ordinary
commands including `git commit`, `fetch`, `rebase`, and authenticated `push`.
The credential is therefore an intentional agent capability, not a value that
can be hidden from unrestricted bash.

Never pass developer credentials or `E2B_API_KEY`
to hands. The first authenticated source integration is specifically
`github_app/v1`, which supplies a short-lived GitHub App installation token
with:

- access to the conversation's primary repository by default, or every
  repository granted to the installation when explicitly requested;
- metadata read and contents read/write only;
- no administration authority;
- no workflow authority unless separately justified;
- short expiration; and
- protected destination branches.

Local pons installations do not depend on a centrally owned Pons GitHub App.
The implemented first slice accepts a bring-your-own App ID, installation ID,
and host-side private-key path. The owner chooses selected or all repositories
when installing the App.
GitHub's App Manifest flow can later create an app owned by the user or their
organization and place its returned ID and private key into the local credential
store, improving interactive setup without changing runtime authentication.

For each run, the local host signs a GitHub App JWT and requests a one-hour
installation token with Contents write and Metadata read permissions. The
token is restricted to the conversation's primary repository by default.
An explicit cross-repository option requests access to every repository in the
installation, allowing the agent to clone more repositories during its run.
Each conversation stores its own primary source and pinned commit, so one server
can place independent workspaces for different repositories. Only the
installation token crosses into the microVM, through
transient Git process configuration. The App private key never leaves the
host. The repository may use an SSH remote in the developer's checkout; pons
uses HTTPS in the remote workspace and does not forward SSH keys or an SSH agent.

The installation token is delegated authority available to unrestricted hands;
an agent can inspect, transform, or write any credential it is able to use.
Pons itself does not place the token in URLs, files, durable state, checkpoints,
or sandbox-wide configuration. Exact token and Authorization-header values in
tool results are redacted before host persistence to reduce accidental leakage,
but redaction is not a security boundary against deliberate encoding. Tokens
are not refreshed within a run: authenticated Git operations stop working when
the token expires, normally after one hour, and the next run mints a new token.

A stronger later design can exchange E2B workload identity through a trusted
credential broker. Credential helpers reduce accidental persistence but do not
hide authority from hands.

Pons-managed credential plumbing must not place credentials or credential
references in SQLite, repository URLs, workspace archives, logs, or templates.
Exact delegated token values are redacted from tool results and transcripts as
an accidental-leak safeguard, subject to the unrestricted-hands limitation
above.

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

`git/v1` uses an explicit non-secret workspace plan rather than overloading
`WorkspacePath` with repository configuration.

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
4. Add a named repository configuration registry with an explicit source
   provider.
5. Add user-owned App Manifest setup for the implemented `github_app/v1`
   credentials.
6. Refine publication policy for dirty and unpushed Git state.
7. Add branch-policy, credential-leakage, expiry, and conflicting-push tests.

Live provider and Git tests remain opt-in and credential-dependent.
