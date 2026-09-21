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

Workspace synchronization is copy-in/checkpoint-out. A successful checkpoint
is staged locally and replaces the prior workspace so remote deletions are
preserved. If checkpoint download or validation fails, the original local
workspace remains in place, closing the session reports an error, and the
sandbox is discarded rather than reused.

Sandbox affinity is durable operational state. SQLite stores the workspace
key, provider, sandbox ID, template, workspace strategy/source/revisions/setup
generation, status, run lease, idle deadline, and provider expiry. It never
stores the E2B API key or temporary envd access token. Reconnection obtains a
fresh token from E2B using the host-side secret. The strategy-neutral workspace
fields support the future control-plane provisioning contract described in
`remote-workspace-provisioning.md`.
An idle janitor deletes sandboxes after the configured timeout. Provider TTL
remains a crash-cleanup backstop, and stale active records are removed after
that expiry. A server restart can therefore reuse an idle sandbox without
preserving process memory or an open protocol connection.

External hands plugins are rejected by the first implementation. Their local
manifests and executables are not portable into a remote Linux template without
an explicit artifact mapping contract.

## Consequences

- Local and E2B environments use the same tool host and protocol semantics.
- No sandbox application port is exposed.
- Startup and shutdown transfer the entire workspace and are unsuitable for
  very large repositories; archive size is bounded.
- Abnormal host termination can lose changes made after the last completed
  checkpoint; interrupted tool calls are not retried.
- The template must be built for the sandbox architecture before use.
- Incremental synchronization, multi-manager environment fencing, pause/resume
  policy, and remote external plugins remain future provider features.
