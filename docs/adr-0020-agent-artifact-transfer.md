# ADR-0020: Agents exchange immutable artifacts by authorized reference

**Status:** Proposed
**Date:** 2026-09-23
**Related:** ADR-0004, ADR-0009, ADR-0010, ADR-0015, ADR-0016, ADR-0018, ADR-0019

## Context

ADR-0016 deliberately gives a delegated child only a text request and its
own context. That is safe for many tasks but inadequate when a coding agent
needs to hand over a patch, test report, or generated file. Sending a path is
not transfer: parent and child may have different workspaces, and a path can
escape a sandbox or change after the request. Copying a whole workspace also
defeats the recipient's independent workspace and authority policy.

An artifact is an immutable piece of task data. A workspace is a mutable
execution resource; memory is compact agent-owned knowledge. They must not be
treated as interchangeable storage.

## Decision

### 1. Publish immutable content

An authorized agent can publish a regular file from its current hands
environment through a host-side capability. The environment provider exports
the bytes through its trusted boundary; the host checks the resolved source
path against the configured export roots, rejects symlink escapes and special
files, enforces size and type limits, computes a digest, and stores an
immutable blob. A model-supplied path is a request to inspect, not a grant.
Export opens and reads the same verified file handle, or uses a provider
operation with equivalent containment, so a path swap cannot bypass the
check.
Directory bundles and whole-workspace transfer require a later format and
are not silently supported by recursive copy.

Each artifact reference is opaque and resolves to metadata:

```text
artifact_id, digest, size, media_type, display_name
owner_agent_id, source_conversation_id, source_run_id
root_id, created_at, retention_state
```

The blob store lives outside execution workspaces. Metadata commits only
after the blob is durable; an interrupted publish may leave an unreferenced
blob for garbage collection but never a valid reference to missing bytes.
Publishing is idempotent by source run and action ID. Content does not change
under an existing ID, even if the source file later changes.

The model-facing operations are conceptually
`publish_artifact(path, display_name)` and `materialize_artifact(artifact_id)`.
The host derives ownership and grants; neither operation accepts an agent ID,
blob-store path, or destination workspace path from the model. Authorized
clients may read artifact metadata and bytes through a separate scoped API.

### 2. Transfer is an explicit policy decision

The first extension to ADR-0016's `delegate(agent_id, message)` is
`delegate(agent_id, message, artifact_refs)`, with an optional list of
references. The host validates that the sender may
read each reference and that recipient, size, media type, and egress policy
permit transfer. The decision and exact artifact IDs are committed with child
creation. A rejected transfer creates no child. The child receives only the
authorized references and their bounded metadata, not the sender's path or
workspace. It can ask its environment provider to materialize a read-only
copy at a provider-chosen safe path; writable work starts from an independent
copy. Materialization verifies digest and containment again.

A child can publish its own output. Its terminal result may include authorized
references alongside bounded text; routing to the parent checks the same
access policy and records the grant atomically. No agent gains access to every
artifact owned by another agent merely because the two can delegate. A root
response may offer references to a human or channel only when its delivery
policy permits them under ADR-0018.

The audit log records publisher, recipient grant, lineage, policy decision,
digest, and size. Content reads are attributable. Artifact bytes and private
paths do not appear in ordinary events, tool observations, or egress audit
hashes. A reference in untrusted text is not itself an access grant.

### 3. Retention and failure

Artifact availability survives process and sandbox restart while an owning
lineage or configured retention period requires it. Cancellation stops new
grants but does not retroactively erase an artifact already delivered to a
recipient. Explicit deletion revokes future reads and materialization,
subject to retention and audit policy; it cannot remove copies already
written into another workspace or external channel.

If publication fails, the tool reports failure and emits no valid reference.
If materialization fails, the child receives a bounded error and may ask for
another artifact or fail. Missing, corrupted, oversized, or revoked content
fails closed; the runtime never falls back to an ambient filesystem path.

The first artifact store may be a local content-addressed directory plus
SQLite metadata. A remote blob backend must preserve the same immutable
reference, durability, and grant semantics. Workspace checkpoints from E2B
remain environment state, not artifacts.

## Store and implementation sequence

The store adds artifact metadata, source-action idempotency, and scoped grants.
Blob writes precede metadata publication; a bounded orphan sweep cleans
unreferenced blobs. Delegation and terminal-result transactions include grants
with the corresponding submission. The provider contract gains explicit
export and materialize operations, with local and remote implementations.

1. Add immutable file publication and verified reads with a local blob store.
2. Add provider export/materialization and path-containment tests.
3. Extend delegation and result routing with transactional grants.
4. Add outbound channel attachment projection only for adapters which can
   enforce the configured recipient and size policy.

## Verification requirements

Deterministic tests without provider credentials or external network prove:

- a changed source file does not mutate a published artifact;
- symlinks, special files, traversal, and paths outside export roots fail;
- a child cannot read a parent artifact without a committed grant, including
  by guessing its opaque ID;
- transfer failure creates no child and a repeated accepted action creates no
  duplicate grant or blob metadata;
- local and remote provider fakes materialize verified bytes only inside the
  recipient environment;
- corruption and missing blobs fail closed after restart;
- cancellation and revocation prevent new reads without claiming to retract
  copies already materialized; and
- artifact content cannot leak through event payloads or routine audit logs.

## Consequences and non-goals

Coding and research agents can hand over concrete results without sharing
mutable workspaces. Immutable copies cost storage and require retention and
garbage collection. Deliberate transfer can be slower than a shared path, but
it preserves the private-child and workspace boundaries.

This ADR does not define collaborative file editing, whole-workspace cloning,
automatic attachment of every output, arbitrary URLs as artifacts, or a
general-purpose shared drive.
