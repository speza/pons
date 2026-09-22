# ADR-0015: E2B carries the hands protocol through envd process streams

**Status:** Accepted
**Date:** 2026-09-21
**Related:** ADR-0007, ADR-0009

## Context

ADR-0009 requires a remote environment to preserve `tool_provider/v1` rather
than translating each pons tool into provider-specific filesystem and command
operations. The E2B design sketch considered either an authenticated custom
port bridge or a direct provider adapter.

E2B already exposes authenticated process start, stdin, stdout, stderr, and
signal operations through its sandbox-local envd service. Adding a separately
reachable pons HTTP or WebSocket service would create another network protocol,
a second authentication secret, and a public sandbox port.

A remote workspace also cannot be treated as the local directory. The first
provider needs an explicit transfer and checkpoint policy.

## Decision

The E2B environment provider uses the platform API to create, reconnect,
refresh, and destroy a sandbox. It uses authenticated envd APIs to:

1. upload a bounded tar archive of the local workspace;
2. start the template's `pons-hands` process;
3. carry the existing NDJSON `tool_provider/v1` bytes through that process's
   stdin and stdout;
4. keep stderr separate and bounded by the existing external host;
5. send cancellation signals to the process; and
6. archive and download the remote workspace after each run.

The provider adapts the remote byte streams to `external.Host`; initialization,
catalog validation, action correlation, cancellation, frame limits, result
normalization, and shutdown therefore remain the existing protocol behavior.
There is no E2B-specific tool catalog.

The first run for a workspace creates a secure sandbox from an explicitly
named template. Later runs reconnect to that sandbox and start a fresh
`pons-hands` process against its retained filesystem. The template contains a
Linux `pons-hands` binary but no model or E2B credentials. `E2B_API_KEY`
remains in the host process. Sandbox internet access is disabled unless the
operator enables the existing sandbox network policy.

The local source workspace is a one-time seed, not a synchronization target.
After each run, the provider downloads and validates a bounded checkpoint,
stores it under the runtime state directory, and advances the logical
workspace's content-addressed checkpoint reference. It never replaces the
source checkout. The state directory must be outside that source; the provider
retains the immutable base and latest checkpoint and prunes superseded
intermediate archives after advancing durable metadata. If checkpoint
download, validation, or persistence fails,
closing the session reports an error and the sandbox is reserved for manual
recovery rather than reused or immediately deleted.

Logical workspace state and sandbox affinity are separate durable resources.
SQLite stores strategy, source, base revision, checkpoint reference, and setup
generation in `workspaces`; `execution_environments` stores only the current
provider placement, active run ID, idle deadline, and provider expiry. Archive
bytes remain outside SQLite and can move from the local checkpoint store to an
object-store implementation later. Neither store contains the E2B API key or
temporary envd access token. Reconnection obtains a fresh token from E2B using
the host-side secret.
While a session is active, a heartbeat refreshes both the E2B timeout and the
durable provider-expiry deadline. An idle janitor deletes sandboxes after the
configured timeout. Provider TTL remains a crash-cleanup backstop, and stale
active records are removed after that expiry. A server restart can therefore
reuse an idle sandbox without preserving process memory or an open protocol
connection.

Only idle placements are reconnected. Unexpired active or recovery placements
block new runs, even after a host restart or a template/network-policy change.
An active record might remain when a recovery reservation could not be written,
so it must also be protected until its recorded expiry. Expired placements are
destroyed and replaced from the last durable checkpoint. Interrupted tool calls
are never replayed automatically.
Checkpointing begins only after the current hands process has stopped cleanly;
a lost envd stream is not evidence of process exit. Failed shutdown discards
the sandbox and leaves the original local workspace unchanged.

After confirmed hands shutdown, the provider stops heartbeats and records a
`recovery` reservation before checkpointing. The reservation has no run ID or
idle deadline and expires one hour after shutdown. E2B's TTL is set slightly
longer as a cleanup backstop. Only a successful checkpoint and idle transition
release the reservation. Failures retain the original deadline, report the
sandbox ID for manual file recovery, and do not automatically retry a tool or
checkpoint. The janitor deletes recovery placements at their deadline. If the
database or timeout update fails, retention is best-effort and the error warns
operators to recover immediately; the sandbox is not explicitly deleted.

Run cancellation cancels startup and tool calls, but an established transport
has a session-owned lifetime so normal shutdown can checkpoint completed edits.
Heartbeat requests have bounded deadlines; failures are reported but do not
prevent a fresh checkpoint attempt during shutdown. Janitor sweeps have bounded
deadlines and skip busy lifecycle transitions rather than blocking shutdown.
Placement records are removed only after sandbox deletion succeeds (or the
sandbox is already absent), so failed deletions remain retryable.
Control-command stdout and stderr are each capped at 1 MiB.

Seed and checkpoint archives share the same symlink policy. Host validation
uses root-confined filesystem operations and rejects duplicate normalized
paths. Checkpoint files and their containing directory hierarchy are synced
before a reference is published to SQLite.

These guarantees concern the hands protocol process. Background services in a
retained sandbox are not promised to survive replacement or to be included in
a transactionally consistent filesystem snapshot.

External hands plugins are rejected by the first implementation. Their local
manifests and executables are not portable into a remote Linux template without
an explicit artifact mapping contract.

## Consequences

- Local and E2B environments use the same tool host and protocol semantics.
- No sandbox application port is exposed.
- Startup and shutdown transfer the entire workspace and are unsuitable for
  very large repositories; archive size is bounded and the source checkout is
  never overwritten.
- Abnormal host termination can lose changes made after the last completed
  checkpoint; interrupted tool calls are not retried.
- The template must be built for the sandbox architecture before use.
- Incremental synchronization, multi-manager environment fencing, pause/resume
  policy, and remote external plugins remain future provider features.
