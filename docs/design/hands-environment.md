# Hands execution environment v1 design

**Status:** Local Seatbelt and remote E2B per-run providers implemented
**Related:** [ADR-0009](../adr/adr-0009-hands-execution-environments.md),
[ADR-0007](../adr/adr-0007-language-neutral-plugin-runtime.md)

This document describes the local Seatbelt backend and the initial E2B remote
backend. E2B uses the standard library HTTP client rather than an SDK, and its
credential-dependent checks remain outside the default test suite.

## Common shape

The host owns the brain, `Core`, provider credentials, policy, and the
conversation. An environment provider owns the hands placement:

```text
pons host
  brain + Core + policy
          |
   environment provider
          |
     hands endpoint
          |
  pons-hands + tools + workspace
```

The provider-facing abstraction is lifecycle and connectivity, not a
container API:

```text
Start(spec) -> HandsSession
HandsSession.Catalog() -> tool descriptions
HandsSession.Execute(action) -> ToolResult
HandsSession.Close()
```

The host still normalizes action IDs and tool kinds, applies call deadlines
and result limits, and decides what a failed environment means. The hands
session receives no brain history or user conversation.

## Tool host

`pons-hands` is a separate hands-side executable. It composes the built-in
hands capabilities (`fs`, `edit`, `bash`) and explicitly configured external
hands providers. It performs the same capability discovery and action/result
exchange as the current external tool provider host.

A tool host is one environment-level process, not one process per tool.
This keeps a shared workspace and tool catalog while allowing the whole hands
side to be placed behind one OS or remote boundary.

The local transport is the existing `tool_provider/v1` JSON-RPC/NDJSON stdio
connection:

```text
host external adapter
       ⇅ stdin/stdout JSON-RPC/NDJSON
sandbox-launched pons-hands
```

A remote adapter should preserve the same semantic catalog and action/result
contract. If the remote transport cannot carry stdio directly, it needs a
small authenticated relay or a host-side transport adapter; provider-specific
command APIs should not become a second tool vocabulary.

## Environment specification

The first policy object should remain small and explicit:

```text
workspace path or workspace seed
explicit read-only paths needed by configured hands providers
network: disabled | enabled by explicit policy
environment variables: explicit allowlist
credentials: explicit references, never inherited
resource limits: cpu, memory, process count, disk, call timeout
hands command/template
lifetime policy
```

Defaults are restrictive:

- only the selected workspace is writable;
- required system/runtime files are readable but unrelated host paths are not;
- network is disabled;
- the parent environment and credentials are not inherited; and
- the host owns deadlines, cancellation, output limits, and teardown.

A provider may offer stronger or weaker guarantees, but it must report enough
metadata for the host and UI to identify the backend and policy in use.
`Metadata.WorkspacePath` is the hands-side working directory, not the host seed
path, and `Metadata.Platform` is the execution `GOOS/GOARCH`. Brain observations
and hydrated prompts use those values. The E2B template/build targets Linux amd64.

## Local Seatbelt backend

The first backend is macOS Seatbelt. It launches `pons-hands` under a
provider-generated profile and connects its stdio to the existing external
adapter.

The profile grants access to the explicit workspace, required system runtime
files, temporary scratch storage, and the manifest, executable, argument
files, and PATH directories of explicitly configured external hands providers.
Those provider paths are read-only. It denies network, the host home directory,
ambient credentials, and unrelated paths. The profile must also
allow the process creation and executable lookup needed by the configured
hands; otherwise `bash` would be unusable even though the workspace policy
was correct.

The local environment can be created for an active agent burst and torn down
when the conversation becomes idle. Workspace changes remain on disk. Seatbelt
is a baseline boundary and should not be described as a universal guarantee
against a kernel-level escape.

## E2B backend

E2B is a useful model for the remote provider because its sandbox is a
provisioned Linux environment rather than a local container abstraction. Its
current SDK surface includes:

- create a sandbox from a named template;
- set a lifetime timeout, metadata, environment variables, and network access;
- run foreground or background commands with working directories and output
  callbacks;
- read and write files in the sandbox;
- connect to an existing sandbox by ID, including paused sandboxes;
- pause, resume, set a timeout, inspect, and kill; and
- expose a sandbox port through an HTTP/WebSocket host address.

The E2B adapter does this:

```text
1. Build a provider policy from EnvironmentSpec.
2. Reconnect the workspace's sandbox, or create one from a pinned pons-hands template.
3. Request allow_internet_access=false unless policy explicitly enables it.
4. Pass only explicit environment values; keep the E2B API key on the host.
5. Upload the latest durable checkpoint, seed a new archive workspace once, or
   clone the configured immutable Git revision in the VM.
6. Start pons-hands through authenticated envd process APIs.
7. Adapt envd stdin/stdout to the existing host hands session.
8. Execute protocol actions and return ToolResults.
9. Validate and persist a bounded checkpoint outside the source checkout.
10. Mark the sandbox idle; later runs may reuse it or restore a replacement.
```

The template contains the tool host; envd supplies the process transport. The host should not
turn every `bash` call into a new E2B command request: that would duplicate
command, filesystem, schema, cancellation, and result semantics outside
pons. A persistent hands endpoint keeps those semantics in one place.

### E2B transport options

Two transports were considered:

1. **Port bridge:** `pons-hands` listens on a sandbox-local HTTP/WebSocket
   port; the host uses the provider's exposed host address. The bridge carries
   JSON-RPC messages and authenticates the connection.
2. **Provider adapter:** the host maps `protocol.Action` directly to E2B's
   command/files APIs and registers a fixed catalog of proxy tools.

The implementation uses a third, simpler option: E2B envd exposes the remote
process's stdin and stdout directly. Those byte streams carry the unchanged
stdio protocol, so no application port is exposed and no E2B-specific tool
implementation is needed. See ADR-0015.

ADR-0007 currently specifies stdio as the wire transport. A remote bridge can
keep stdio inside the sandbox and use a host-side relay, or a future protocol
revision can explicitly add an authenticated streaming transport. That choice
must be recorded before shipping a remote provider; this design does not
silently change runtime protocol 1.

### E2B workspace model

A local path cannot be mounted directly into a remote E2B sandbox. The local
path is therefore only the source for a new logical workspace. The initial
provider uploads that bounded seed once and persists a full validated
checkpoint after every run without replacing the source checkout.

SQLite stores logical workspace metadata separately from replaceable sandbox
placement. Checkpoint archives live under the runtime state directory and are
addressed by digest; a future checkpoint-store implementation may use S3 or
another object store. A later process reconnects to the retained sandbox or
creates a replacement from the latest checkpoint using its host-side API key
and a fresh envd access token. Transfer size is bounded, and incremental
checkpointing is left for later.

If the E2B sandbox expires, the host treats the environment as failed. It does
not automatically repeat a tool action. A later agent run may create a new
sandbox from the last durable workspace checkpoint and decide what to do.

## Lifecycle and failure

Environment state is operational state, not conversation history. The runtime
records provider identity and environment handle, but never puts provider
credentials into the transcript or model observation.

For an active tool batch:

```text
environment starts
  -> hands catalog is validated
  -> assistant tool calls are persisted
  -> tools execute
  -> results are persisted in call order
  -> environment closes or remains warm by policy
```

A provider crash, expiry, timeout, or malformed response becomes an
unsuccessful hands-boundary result when the host can safely continue. The
runtime does not retry the action. An agent may issue a new action after seeing
the result.

## Testing plan

- Unit-test environment policy construction without launching a sandbox.
- Use a fake hands endpoint for core and runtime tests.
- Add macOS Seatbelt integration tests behind an explicit build/run condition.
- Keep live E2B tests opt-in and credential-dependent; fake API and archive
  tests remain in `go test ./...`.
- Test that no parent environment credentials or network policy accidentally
  enter the hands process.
- Test provider expiry and endpoint failure as ordinary hands-boundary
  failures.

## References

- [E2B Sandbox SDK reference](https://e2b.dev/docs/sdk-reference/js-sdk/v2.6.2/sandbox)
- [E2B Python Sandbox reference](https://e2b.dev/docs/sdk-reference/python-sdk/v2.5.0/sandbox_sync)
- [E2B template reference](https://e2b.dev/docs/sdk-reference/cli/v1.0.9/template)
- [Remote workspace provisioning design](remote-workspaces.md)
- [ADR-0004](../adr/adr-0004-sandboxed-hands-boundary.md)
- [ADR-0007](../adr/adr-0007-language-neutral-plugin-runtime.md)
- `plugins/external/` — current external hands host
