# ADR-0020: Agents exchange immutable artifacts by authorized reference

**Status:** Proposed; decision summary, detailed contract deferred to its phase
**Implementation:** Not implemented
**Date:** 2026-09-23
**Related:** ADR-0004, ADR-0009, ADR-0016, ADR-0018, ADR-0019, ADR-0022

## Context

ADR-0016 gives a delegated child only a text request. That is inadequate when
a research or coding agent needs to hand back a report, patch, or generated
file. Sending a path is not transfer: agents have different workspaces, and a
path can escape a sandbox or change after it is sent. Sharing a workspace
defeats the recipient's independent workspace and authority.

An artifact is an immutable piece of task data. It differs from a workspace
(mutable execution state under ADR-0022) and memory (agent-owned knowledge
under ADR-0019).

## Decision

- **Publish immutable content.** An authorized agent publishes one regular
  file from its hands environment through a host-side capability. The host
  checks containment, rejects symlinks and special files, enforces size and
  type limits, and stores a content-addressed blob outside every workspace.
  Content never changes under an artifact ID. Publishing is idempotent by
  source run and action ID.
- **References are opaque and granted.** An artifact ID is not an access
  grant. A recipient can read an artifact only through a grant committed with
  a delegation, a delegation result, or an outbound delivery the policy
  permits. Guessing an ID grants nothing.
- **Transfer extends delegation.** `delegate(agent_id, message,
  artifact_refs)` and delegation results may carry references. The host
  applies the same recipient and egress policy as the request, and a rejected
  transfer creates no child.
- **Materialize, never mount.** A recipient asks its environment provider to
  materialize a verified read-only copy at a provider-chosen path. Writable
  work starts from an independent copy.
- **Fail closed.** Missing, corrupted, oversized, or revoked content fails;
  the runtime never falls back to a filesystem path. Deletion stops future
  reads but cannot retract copies already materialized or delivered.
- **Storage.** Blobs use the same content-addressed backends as ADR-0022
  checkpoints, in a separate namespace, with SQLite metadata and grants.
  Artifact bytes never appear in events or audit logs.

## Deferred to implementation

Retention and garbage collection, directory bundles, provider export and
materialization interfaces, attachment projection for outbound channels, and
the detailed verification list are specified when phase 9 of
[`docs/persistent-agents-v1.md`](../persistent-agents-v1.md) begins.

## Consequences and non-goals

Agents can hand over concrete results without sharing mutable workspaces, at
the cost of storage for immutable copies. This ADR does not define
collaborative editing, whole-workspace transfer, URLs as artifacts, or a
shared drive.
