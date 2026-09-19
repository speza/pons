# ADR-0009: Hands execute inside provider-backed environments

**Status:** Accepted
**Date:** 2026-09-18
**Related:** ADR-0004, ADR-0007, ADR-0008

## Context

ADR-0004 establishes that hands are the execution boundary and that an
in-process path jail or command allowlist is not an operating-system security
boundary. The current external tool runtime provides a persistent child
process and a language-neutral tool protocol, but it does not choose or
supervise an isolated execution environment.

The always-on runtime makes that deployment seam more important. The hands may
run commands, modify a workspace, load external plugins, and eventually use
networked services. The environment must reduce both:

- accidental damage from model-generated actions; and
- damage from untrusted or compromised hands/plugin code.

A container-specific abstraction is too narrow. A local macOS Seatbelt
process, Linux bubblewrap process, remote execution service such as E2B or
Modal, and a future VM may all provide the hands environment through different
launch and transport mechanisms.

## Decision

### 1. The deployment seam is an execution environment

pons introduces an execution-environment concept above the hands protocol.
The core remains unaware of the environment implementation:

```text
pons host: brain + Core + policy
              |
       environment provider
              |
       tool host endpoint
              |
       fs / edit / bash / external hands
```

The abstraction is intentionally not named after OCI, containers, Seatbelt, or
any one remote service. An environment provider owns provisioning, policy,
connectivity, health, and teardown. The host continues to own action
correlation and execution deadlines.

### 2. One complete tool host runs inside an environment

The environment contains one `pons-hands` tool host with the built-in hands
capabilities and any explicitly composed external hands providers. The host
registers the tool host's discovered catalog as proxy tools in `Core`.

Individual tools are not independently sandboxed in v1. A single environment
provides the workspace and process context shared by the hands in one active
agent burst. The brain and its conversation remain outside the environment.

### 3. `tool_provider/v1` remains the communication contract

The environment boundary does not introduce a second action/result protocol.
The host sends `protocol.Action` values and receives `protocol.ToolResult`
values using the existing `tool_provider/v1` JSON-RPC/NDJSON contract from
ADR-0007.

The first local transport is the hands process's stdin/stdout. A remote
provider must either relay this stdio protocol through an authenticated bridge
or use a future protocol revision that explicitly adds a remote transport; it
must not silently reinterpret runtime protocol 1 as an arbitrary network wire.
Provider-specific transport and lifecycle code stays in the environment
adapter; the brain/hands data contract does not change.

The host remains responsible for:

- action deadlines and cancellation;
- frame, stderr, pending-call, and result limits;
- correlation identity and result namespace normalization;
- health and shutdown; and
- treating the environment as failed when its endpoint becomes unusable.

### 4. Seatbelt is the first local backend

The first environment backend targets macOS Seatbelt. It launches the complete
tool host under a generated policy profile and connects its stdio to the
existing external host adapter.

The default profile grants:

- read/write access only to the explicitly selected workspace;
- read-only access to required system binaries and libraries;
- a temporary scratch area;
- process execution needed by the tool host and configured commands;
- no network access;
- no host home-directory access;
- no inherited credentials or ambient environment; and
- host-enforced time and resource limits where available.

Extra paths, network access, secrets, and mounts require explicit deployment
policy. Activating an external provider explicitly adds its manifest,
executable, existing path arguments, and configured PATH directories as
read-only policy inputs; it does not grant general home-directory access.
Seatbelt is a baseline for both accidental and hostile hands code; it
is not a claim that every local OS configuration provides VM-strength
isolation.

Linux bubblewrap is a later local backend. Remote environment providers such
as E2B, Modal, and exe.dev are later adapters behind the same environment
concept. OCI is not the foundational API, although a provider may use a
container internally.

### 5. The workspace is explicit and persistent

A local environment receives the real workspace path explicitly. The policy
allows the tool host to operate on that path and does not infer access
from the child working directory.

Workspace changes persist after an environment is torn down. The environment
itself is disposable; it does not imply rollback or snapshot semantics. A
remote provider owns its workspace synchronization strategy and must expose
an equivalent workspace contract before it can be used as a backend.

### 6. Credentials and network access are opt-in

The tool host receives an empty environment by default. Credentials are
not copied into the environment implicitly. A future host-side broker may
provide scoped credentials for a specific action or provider.

Network access is disabled by default. Enabling it is an environment policy,
not a model/tool decision, and must be visible in composition and audit data.

### 7. Environment lifetime follows active execution

The host starts an environment when an active agent burst needs hands, keeps it
alive while tool calls and steering are in progress, and tears it down when
the burst becomes idle or the run is cancelled. The workspace remains the
source of truth after teardown.

A remote provider may choose to retain a warm environment for latency, but
that is an adapter optimization. Core semantics do not depend on environment
residency.

## Alternatives considered

- **Make OCI the common abstraction:** not selected. OCI describes one
  implementation family and does not model native or remote environments
  cleanly.
- **Sandbox each tool independently:** not selected for v1. It complicates
  shared workspace state, discovery, and tool-call latency.
- **Rely on in-process policy or a subprocess alone:** not selected as a
  security boundary; both share too much trust with the host.
- **Create a new remote action/result protocol:** not selected. The existing
  language-neutral hands protocol already carries the required contract.
- **Run the brain inside the environment:** not selected. Provider credentials,
  conversation state, and control policy remain on the host.

## Consequences

- Local and remote hands can share one host-side tool catalog and action/result
  contract.
- The first implementation is platform-specific, but the platform-specific
  code is isolated in an environment provider.
- Seatbelt policy authoring and command availability need careful testing;
  allowing `bash` means the profile must support the intended process and
  filesystem behavior without exposing unrelated host paths.
- A sandbox limits effects, not prompt injection or the meaning of tool
  output. Brains and UIs must continue to treat observations as untrusted
  data.
- Stronger isolation, remote workspaces, warm environments, and credential
  brokers remain provider-specific future work.

## References

- `docs/hands-environment-v1.md` — first local backend and provider contract
- `docs/adr-0004-sandboxed-hands-boundary.md` — hands boundary and isolation
  policy
- `docs/adr-0007-language-neutral-plugin-runtime.md` — external hands wire
  protocol
- `plugins/external/` — current persistent tool-provider host
- `environment/` — provider/session contract and Seatbelt implementation
- `internal/toolhost/` — built-in tool catalog served by `pons-hands`
- `protocol/` — action and result types
