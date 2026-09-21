# pons

**pons** (*Latin*: "bridge") is part of the brainstem that carries signals
between the brain and the body in both directions: commands flow down and
observations flow up.

That is the architecture here: the brain sends actions down, and hands return
observations up.

pons is a minimal, pluggable agent harness in Go. A dependency-free
`protocol/` seam connects a reasoning **brain** to executing **hands**:

```
brain  -- protocol.Action -->  core  -- execute -->  hands
       <-- observations -----       <-- results ----
```

The brain plans but never executes. Hands execute but never plan. `Core`
owns orchestration, while tools, brains, storage, transports, and execution
environments are explicit plugins or injected infrastructure.

## Architecture

- The brain emits `protocol.Action` values and has no direct I/O.
- Hands execute actions and receive no conversation history, goals, or LLM.
- Enforcement happens at the execution boundary; model-declared risk is only
  advisory.
- `protocol/` is the shared JSON contract for in-process and external hands.

### Packages

| Package | Role |
| --- | --- |
| `protocol/` | Dependency-free `Action`, `ToolResult`, and `Observation` types |
| `core.go` | Turn loop, ports, and plugin registry |
| `plugins/brain` | LLM and deterministic scripted brains |
| `plugins/fs`, `edit`, `bash`, `shell` | Built-in hands-side tools |
| `plugins/external` | Persistent external tool providers and Go/TypeScript SDKs |
| `runtime/` | Durable SQLite runtime and HTTP/SSE transport |
| `environment/` | Provider/session contracts and durable environment state |
| `environment/seatbelt`, `environment/e2b` | Local and remote execution providers |
| `cmd/` | `pons`, `pons-hands`, and demo binaries |

Adding a tool plugin automatically adds its schema to the LLM brain. The core
does not need tool-specific changes.

## The agent loop

```go
result, err := core.Run(ctx, message) // respond -> act -> reflect -> repeat
```

Tool calls within a turn run concurrently, but results are recorded in the
planned order. A text response, `finish` action, or `MaxTurns` ends a run;
`OnEvent` can stream lifecycle events to a UI or audit consumer. The LLM brain
can compact old context without changing the canonical runtime history.

## Quick start

Tests do not need API keys:

```sh
go test ./...
go run ./cmd/pons-demo
```

Run with a provider:

```sh
export ANTHROPIC_API_KEY=sk-ant-…
go run ./cmd/pons -message "inspect this project"

export OPENAI_API_KEY=sk-…
go run ./cmd/pons -provider openai -message "inspect this project"
go run ./cmd/pons -provider openai \
  -base-url http://127.0.0.1:11434/v1 \
  -message "inspect this project"
```

The supported provider names are `anthropic`, `openai`, `codex`, and
`openai-responses`. Codex uses a ChatGPT subscription:

```sh
go run ./cmd/pons -provider codex -login
go run ./cmd/pons -provider codex -i
```

## Long-lived runtime

`serve` owns conversations and agent runs; `client` submits messages and
consumes SSE events:

```sh
go run ./cmd/pons serve -provider codex
go run ./cmd/pons client -message "inspect this project"
go run ./cmd/pons client -conversation <id> -message "continue"
```

Runtime state is stored in SQLite at `<state-dir>/runtime.db` (under
`~/.pons/runtime/` by default). The server keeps canonical messages and a
durable event outbox, bounds active runs, serializes each conversation, and
prevents simultaneous runs in one workspace. Clients resume from durable
snapshots and cursors; retries can use `-idempotency-key`.

The v1 server listens on loopback (`127.0.0.1:7337`) and refuses non-loopback
addresses.

## Hands, sandboxes, and external plugins

By default, built-in tools run in-process. On macOS, Seatbelt can place the
hands side in a fresh environment for each run:

```sh
go build -o .build/pons-hands ./cmd/pons-hands
go run ./cmd/pons -provider codex -sandbox seatbelt \
  -hands-command .build/pons-hands \
  -message "inspect this project"
```

E2B can run the same hands protocol in a remote Linux sandbox. Build the
pinned template once (requires `E2B_API_KEY` and Node/npm), then select it:

```sh
make e2b-template
make smoke-e2b
go run ./cmd/pons -provider codex -sandbox e2b \
  -e2b-template pons-hands \
  -message "inspect this project"
```

To remove retained pons sandboxes manually, run `make cleanup-e2b`; use
`./scripts/cleanup-e2b.sh --dry-run` to inspect matches first. The default is
scoped to the `pons-hands` template; `make cleanup-e2b-all` explicitly targets
all running and paused sandboxes visible to the API key.

The E2B API key remains on the host, internet access is disabled unless
`-sandbox-network` is set, and the local workspace is used only to seed a new
logical workspace. Bounded checkpoints are stored under the runtime state
directory after every run; the source checkout is never replaced. Workspace
metadata and sandbox placement are persisted separately in SQLite, so a
workspace survives sandbox deletion and can be restored in a new VM. Idle
sandboxes are deleted after `-sandbox-idle-timeout` (10 minutes by default).
The initial E2B backend does not support external plugin manifests.

External tools are enabled explicitly with a manifest; they are never
discovered implicitly. Example providers are in
[`examples/external-echo`](examples/external-echo) and
[`examples/external-echo-ts`](examples/external-echo-ts). See the
[external plugin protocol](docs/external-plugin-protocol.md) and
[hands environment design](docs/hands-environment-v1.md) for details.

Useful flags include `-workspace`, `-state-dir`, `-max-turns`,
`-compact-chars`, repeatable `-plugin`, `-plugin-path`, `-sandbox`,
`-hands-command`, `-e2b-template`, `-e2b-hands-path`,
`-sandbox-idle-timeout`, `-fallback`, and `-debug`.

## Configuration

Defaults can be stored in `~/.pons/config.json`; `.pons.json` in the workspace
overrides them, and explicit flags win. Configuration supports provider/model
settings, runtime limits, tool output limits, named provider slots, and
failover. Use repeatable `-fallback provider[:model]` flags for a temporary
failover chain.

Codex credentials are stored with restrictive permissions in
`~/.pons/auth.json`. Multiple named credentials and provider slots are
supported.

## Development

```sh
make check       # format, tests, vet, and golangci-lint
make test-race   # race-enabled tests
make test-integration
```

Install the local lint tool once if needed with `make install-tools`.
`make smoke-runtime` is a separate live-provider and Seatbelt check; it is not
part of the default suite.

Architecture decisions and protocol details live in [`docs/`](docs/),
including the [runtime design](docs/runtime-v1.md) and the
[ADR index](docs/).

## Dependencies

The project uses the official Anthropic and OpenAI Go SDKs, a pure-Go SQLite
driver, and the standard-library HTTP stack.

## Roadmap

- Stream text and partial tool-output observations through `OnEvent`.
- Add a local Linux execution-environment provider and incremental remote workspace sync.
- Support per-run environments in the long-lived runtime.
- Add branching and `go install`-able releases.
