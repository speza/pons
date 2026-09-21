# Remote workspace provisioning design

**Status:** Proposed
**Date:** 2026-09-21
**Related:** ADR-0009, ADR-0015

## Goal

Support host-controlled workspace setup—initially Git-backed workspaces—without
turning repository acquisition, revision selection, credentials, or checkpoint
publication into agent tools. The brain may edit and run code in the materialized
working tree, but it does not choose a repository, fetch arbitrary refs, perform
a checkout, or receive source-control credentials.

This is a future capability. The current E2B provider implements only
`local_archive/v1`.

## Trust boundary

Provisioning is control-plane work owned by the runtime and environment
provider. It happens before `pons-hands` starts and after it stops. It is not a
`protocol.Action`, is not included in the model tool catalog, and cannot be
requested by model output.

A setup implementation must keep two areas distinct:

- **control workspace:** repository metadata, credentials, fetch cache, and
  baseline state, inaccessible to hands;
- **hands workspace:** a materialized tree writable by `pons-hands`, without
  source-control credentials and, when policy requires it, without `.git`.

Merely hiding a `git_checkout` tool is insufficient while unrestricted bash can
access the same repository metadata. A hardened Git strategy must either do Git
work on the host and upload a credential-free tree, or use a privileged setup
identity and filesystem permissions that prevent the hands identity from
reading the control workspace. Network policy for setup is separate from hands
network policy and is disabled again before hands starts.

## Proposed contracts

```go
type WorkspacePlan struct {
    Strategy       string // local_archive/v1, git/v1
    SourceRef      string // opaque deployment configuration reference
    Revision       string // immutable commit SHA after resolution
    Destination    string
    SetupGeneration int
}

type WorkspaceProvisioner interface {
    Prepare(context.Context, Placement, WorkspacePlan) (PreparedWorkspace, error)
    Checkpoint(context.Context, Placement, PreparedWorkspace) (Checkpoint, error)
    Destroy(context.Context, Placement, PreparedWorkspace) error
}
```

`Placement` is a provider control-plane handle, not a tool port. `Prepare` runs
before the tool host is reachable. `Checkpoint` runs after the tool host has
shut down. Provider adapters may implement placement operations using envd,
volume APIs, host-side archive transfer, or another authenticated control
channel.

The environment provider remains responsible for sandbox lifecycle. The
provisioner owns only workspace materialization and checkpoint policy.

## Durable state

`execution_environments` reserves strategy-neutral columns:

- `workspace_strategy`: versioned behavior name, currently `local_archive`;
- `workspace_source_ref`: opaque non-secret configuration identifier;
- `workspace_revision`: immutable prepared baseline, such as a commit SHA;
- `checkpoint_revision`: latest durable checkpoint/digest;
- `setup_generation`: operator-controlled compatibility generation.

These values participate in reuse compatibility. A provider must discard or
re-provision an idle environment when its strategy, source, requested revision,
network policy, template, or setup generation is incompatible.

The table must not contain clone URLs with embedded credentials, private keys,
OAuth tokens, provider access tokens, or deploy tokens. `workspace_source_ref`
resolves through trusted deployment configuration or a secret broker.

## Git v1 lifecycle

### Configuration

The operator supplies a source definition outside the transcript:

```text
id: product-api
repository: https://github.com/example/product-api.git
allowed_refs: refs/heads/main
credential_ref: scm/product-api-read
submodules: deny | allowlisted
lfs: disabled | enabled
```

The runtime validates the source against deployment policy. The requested ref
is resolved to an immutable commit SHA before the environment becomes active.
Moving branch names are never the durable workspace revision.

### Prepare

For a new environment:

1. Resolve `workspace_source_ref` through trusted configuration.
2. Obtain a short-lived, read-only credential from the secret broker.
3. Fetch the allowlisted repository and exact revision through the control
   plane.
4. Verify repository identity and resolved commit.
5. Materialize a clean tree into the hands workspace without credentials.
6. Exclude `.git`, or place repository metadata in a control directory that the
   hands user cannot access.
7. Apply configured submodule/LFS policy; both default to disabled.
8. Remove setup credentials and disable setup network access.
9. Start `pons-hands` only after all checks succeed.

Host-side clone plus archive upload is the simplest secure implementation.
Cloning inside E2B is acceptable only after E2B has distinct setup and hands
identities with enforceable filesystem permissions and temporary network
policy.

### Checkpoint

Git v1 does not push or commit automatically. After a run it should:

1. stop `pons-hands`;
2. compare the hands tree against the immutable baseline;
3. produce a bounded, validated checkpoint (archive plus manifest, or patch plus
   untracked-file bundle);
4. persist its digest in `checkpoint_revision`;
5. update the local workspace or durable artifact store according to deployment
   policy; and
6. mark the environment idle only after the checkpoint is durable.

Publishing a branch, commit, or pull request is a separate authorized
control-plane operation. It is never an implicit consequence of an agent run.

### Resume

On reuse, the runtime checks provider, template, network policy, strategy,
source reference, baseline revision, checkpoint revision, and setup generation.
It then reconnects the sandbox, verifies the expected control metadata, starts
a fresh tool host, and records the new run lease. A mismatch causes explicit
re-provisioning; it must not silently merge two workspace histories.

## Local changes and conflicts

A retained remote workspace is authoritative between runs, while the local
workspace is a durable checkpoint mirror. Future implementations must detect
local changes made after the last checkpoint. They may:

- reject acquisition with a clear conflict;
- discard and re-provision the remote environment from the local source; or
- run an explicit host-controlled reconciliation policy.

They must not overwrite unrecognized local changes silently. The reserved
revision fields provide the durable comparison points needed for this check.

## Failure policy

- Setup failure prevents hands startup and records no usable environment.
- Checkpoint failure leaves the prior durable checkpoint intact and discards
  the sandbox rather than reusing uncertain state.
- Provider expiry falls back to the last durable checkpoint; no tool action is
  retried automatically.
- An interrupted setup/checkpoint is recovered by lifecycle state, never by
  asking the model what probably happened.

## Implementation stages

1. Add workspace plan types and a `local_archive/v1` provisioner around the
   existing archive code.
2. Add source configuration resolution with opaque references and validation.
3. Implement host-side `git/v1` clone at an exact SHA, excluding `.git` from
   hands.
4. Add manifest/digest checkpoints and local-change conflict detection.
5. Add an explicit, separately authorized publication workflow if needed.
6. Only then consider provider-side clone caches, volumes, or privileged setup
   identities for performance.

## Tests

- Unit-test source/ref allowlists and immutable revision resolution.
- Verify credentials never enter SQLite, archives, process environments, logs,
  transcripts, or tool results.
- Verify hands cannot read control repository metadata.
- Verify setup networking is disabled before tool execution.
- Test submodule, symlink, archive traversal, LFS, size, and file-count limits.
- Test checkpoint failure, provider expiry, server restart, and incompatible
  setup-generation re-provisioning.
- Keep live provider/Git tests opt-in and credential-dependent.
