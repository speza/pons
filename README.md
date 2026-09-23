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

For a reusable setup, put server defaults in `~/.pons/config.json`:

```json
{
  "provider": "codex"
}
```

Log in once, then run a task:

```sh
go run ./cmd/pons -login
go run ./cmd/pons -message "inspect this project"
```

The supported provider names are `anthropic`, `openai`, `codex`, and
`openai-responses`. Codex uses a ChatGPT subscription. For a temporary
provider override, use flags:

```sh
export ANTHROPIC_API_KEY=sk-ant-…
go run ./cmd/pons -provider anthropic -message "inspect this project"
```

Bundled mode also reads `.pons.json` from its current directory. Explicit
flags override config values. See [configuration](#configuration) for E2B and
GitHub App settings.

## Long-lived runtime

Without a subcommand, `pons` runs the server and client in one process, so
interactive output and server logs share a terminal. `serve` runs the server
separately; `client` submits messages and consumes SSE events:

```sh
# Terminal 1
go run ./cmd/pons serve --debug
```

```sh
# Terminal 2
go run ./cmd/pons client -i -workspace /path/to/project
go run ./cmd/pons client -conversation <id> -message "continue"
```

The client selects a workspace when creating a conversation; the server does
not select a project at startup. `client -workspace` defaults to the client's
current directory. That path must exist on the server host and be inside
`workspace_root` (the server user's home directory by default). Set
`"workspace_root"` in the global config to allow other host paths. Git
conversations select a repository and commit instead; see the
[Git workspace guide](docs/git-workspaces.md).

The server reads `~/.pons/config.json`. A standalone `serve` process does not
read `.pons.json` from its startup directory. The loopback server trusts other
local processes that can connect to it.

Runtime state lives under `~/.pons/runtime/server/` by default. Run one server
per state directory; additional clients connect to that server. The server
listens on loopback (`127.0.0.1:7337`) and refuses non-loopback addresses.
Clients can resume conversations by ID.

### Web UI

The server embeds the built React client. From a fresh checkout, build it and
start the server after setting up a provider as shown in [Quick start](#quick-start):

```sh
make web-install
make web-build
go run ./cmd/pons serve --debug
```

Open `http://127.0.0.1:7337/` in a browser. Choose a sandbox and a source,
then select **New conversation**. With E2B, the Git source takes a
credential-free HTTPS repository URL and either a branch name or a full
40-character commit SHA. Pons uses the branch tip when it first provisions the
workspace, then creates a separate work branch. E2B can also upload a host
directory as the initial workspace. In-process and Seatbelt conversations use
a host directory.

For a host directory, enter its **absolute path on the server host** (for
example, `/Users/you/projects/my-project`). The path must exist inside
`workspace_root`, which defaults to the server user's home directory. Set
`"workspace_root"` in `~/.pons/config.json` if your projects live elsewhere.

The browser uses the same HTTP/SSE runtime API as the CLI. Frontend source is
in [`web/`](web/); run `make web-build` after changing it so the server embeds
the new assets.

When the server is started with `-sandbox seatbelt` or `-sandbox e2b`, the web
client lets each new conversation choose between the in-process tools and the
configured sandbox provider. The choice is stored on the conversation and
applies to its future runs.

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
template once (requires `E2B_API_KEY` and Node/npm), then configure the server
as shown in the [Git workspace guide](docs/git-workspaces.md):

```sh
make e2b-template
make smoke-e2b
```

Run `go run ./cmd/pons serve --debug` after configuring the server.

Each client can create an independent checkout from a repository and full
commit ID:

```sh
go run ./cmd/pons client -i \
  -git-repository https://github.com/OWNER/REPOSITORY.git \
  -git-revision FULL_COMMIT_SHA
```

Use `-git-branch main` instead of `-git-revision` to start from a remote branch.

New conversations from the same repository still get separate checkouts.
The [Git workspace guide](docs/git-workspaces.md) covers GitHub App setup,
cross-repository access, checkpoints, recovery, and cleanup. The initial E2B
backend does not support external plugin manifests.

External tools are enabled explicitly with a manifest; they are never
discovered implicitly. Example providers are in
[`examples/external-echo`](examples/external-echo) and
[`examples/external-echo-ts`](examples/external-echo-ts). See the
[external plugin protocol](docs/external-plugin-protocol.md) and
[hands environment design](docs/hands-environment-v1.md) for details.

## Configuration

Put stable server settings in `~/.pons/config.json`; use flags for a particular
conversation or temporary override. Bundled mode also reads `.pons.json` in
its current directory. A standalone server only reads the global file.
Configuration supports provider/model settings, runtime limits, tool output
limits, named provider slots, and failover. Use repeatable
`-fallback provider[:model]` flags for a temporary failover chain.

The [Git workspace guide](docs/git-workspaces.md) has the complete E2B and
GitHub App config example. Keep credentials in the global file, not in a
repository's `.pons.json`. Client commands select the repository and revision
per conversation.

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
- Support per-run environment overrides in the long-lived runtime.
- Add branching and `go install`-able releases.
