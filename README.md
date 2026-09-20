# pons

**pons** (*Latin*: "bridge") — in anatomy, the part of the brainstem that
carries signals between the brain and the body, in both directions: motor
commands flow down, sensory observations flow up. The reflex centers live
there.

That is the whole architecture. pons is a minimal, pluggable agent harness in
Go: a small core (the bridge), one dependency-free JSON seam (`protocol/`),
and explicit adapters around it. Tools and brains are plugins; runtime
storage and transports are injected infrastructure. The reasoning **brain** and the executing **hands** are
structurally separated; the pons is what connects them:
actions down, observations up, nothing in between improvises.

Built brain/hands-first: reasoning never touches the machine, execution
never reasons.

## Terminology

**Brain** and **hands** are the architectural metaphor: the brain plans and
interprets, while the hands execute. New public APIs use neutral terms where
possible: control/planning, tool/execution, runtime, execution environment,
message, and provider. We do not extend the anatomy metaphor to channels,
schedulers, or other runtime components.

## The separation contract

```
┌────────────────────────── brain ──────────────────────────┐
│ LLM loop / reasoning / planning. Sees Observations,       │
│ emits Actions. Owns NO execution, NO direct I/O.          │
│ Link-depends only on: protocol.                           │
└──────────────┬─────────────────────────────────────────────┘
               │ ControlPort (Respond / Interpret / Close)
               │ ToolPort (Execute)
┌──────────────┴───────────── harness ──────────────────────┐
│ The referee: turn loop, events, plugin registry.          │
│ Contains NO task logic.                                   │
└──────────────┬─────────────────────────────────────────────┘
               │ protocol.Action / protocol.ToolResult
┌──────────────┴────────────── hands ───────────────────────┐
│ Real execution: fs, edit, bash. Jailed or isolated per  │
│ allow-listed per plugin.                                  │
└───────────────────────────────────────────────────────────┘
```

Key invariants:

1. **The brain never executes.** It can only emit `protocol.Action` values —
   the `brain` plugin cannot import a tool plugin, and vice versa.
2. **The hands never plan.** Tools have no message history, no LLM, no goals.
3. **The harness trusts neither.** Whatever the model declares (including its
   advisory `Danger` self-assessment), enforcement happens at the execution
   boundary — inside the producing tool plugin, or middleware over the port.
4. **`protocol/` is the seam.** Pure, dependency-free JSON types; the same
   `Action`/`ToolResult` contract serves in-process hands and the external
   hands-side tool host (ADR-0004 and ADR-0007).

## Packages

| Package | Role |
|---|---|
| `protocol/` | Wire contract: `Action`, `ToolResult`, `Observation`. Core reserves only the `finish` kind |
| `core.go` | The turn loop, the two ports, and the plugin seam. Imports only `protocol` |
| `runtime/` | Transport-independent durable conversations, transactional `Store` contract, run scheduling, workspace leases, and event subscriptions |
| `runtime/sqlite` | SQLite implementation of `runtime.Store` |
| `runtime/httptransport` | Native HTTP/SSE server and client adapter over the runtime |
| `environment/` | Provider/session seam and macOS Seatbelt hands environment |
| `internal/toolhost` | Composes and serves the built-in tool catalog behind the hands boundary |
| `plugins/fs` | `read_file`, `write_file`, `list_dir` — symlink-resilient workspace jail |
| `plugins/edit` | `edit_file` — SEARCH/REPLACE patch application (exact match, unique, fuzzy fallback, CRLF/BOM aware, unified diff in the result payload) |
| `plugins/bash` | `bash` — unrestricted command execution (per-call timeout, output tail-truncation, full output to temp file). Trust comes from composition |
| `plugins/shell` | `run_command` — first-token allowlist variant, for sandboxed compositions |
| `plugins/external` | Persistent hands-side JSON-RPC/NDJSON host and `tool_provider/v1` adapter; `plugins/external/sdk` serves Go and dependency-free TypeScript plugins |
| `plugins/brain/llm` | Real LLM brain: one model call per turn, tool schemas auto-discovered from registered plugins; context compaction; resume seeding; provider failover chain |
| `plugins/brain/scripted` | Deterministic brain (LLM stand-in for CI) |
| `internal/jail`, `internal/unidiff` | Shared libraries between plugins (code, not registrations) |
| `cmd/pons/` | Bundled runtime+client CLI, long-lived server, and standalone HTTP/SSE client |
| `cmd/pons-hands/` | Standalone built-in tool host served over `tool_provider/v1` |
| `cmd/pons-demo/` | Deterministic scripted demo |

## Even base tools are plugins

`read_file` is not a core concept — the `fs` plugin owns the action kind, its
guard, and its constructors (`fs.Read(path)`). A fresh `Core` can do nothing:
capability surface = exactly the plugin set composed in `main()`. The
purely-additive contract is *enforced*: `AddTool` rejects a duplicate action
kind and `SetBrain` rejects a second brain, so plugin conflicts fail loudly at
startup instead of silently last-wins.

## The agent loop

```go
result, err := core.Run(ctx, message)   // respond → act → reflect → repeat
```

- **`RunResult`** carries the final `Answer` (text-only reply or finish
  reason), `Turns`, `History` (per-turn audit), and `Exhausted` (budget ran
  out — a result, not an error).
- **`OnEvent(func(pons.Event))`** streams the loop live: `agent_start`,
  `turn_start`, `action_start`, `action_end`, `turn_end`, `finish`,
  `stopped`, `exhausted`. UIs attach here; audit observers use `OnTurn`,
  and checked persistence uses `OnTurnError`.
- **Termination** is the brain's call: a text-only LLM reply or a `finish`
  action ends the run; MaxTurns is the backstop. A truncated provider
  response (`max_tokens`/`incomplete`) is an error, never a silent partial
  plan.
- **Tool calls within a turn execute concurrently**; results are recorded in
  call order (never completion order) so transcripts and resumes stay
  deterministic (ADR-0006).

**Context compaction** lives in the LLM brain: when the conversation exceeds
a size estimate (`--compact-chars`, default ~400k chars ≈ 100k tokens;
negative disables), older turns are summarized into a single user message,
keeping the most recent turns verbatim. Canonical runtime messages remain
unchanged in SQLite; compaction only changes the provider context for a run.

## Runtime persistence

The server stores canonical semantic messages, submissions, runs, tool calls,
and its durable client-event outbox in one SQLite database:

```
~/.pons/runtime/<munged-project-path>/runtime.db
```

The runtime reads messages directly for LLM hydration and client snapshots;
it does not rebuild either view by replaying events. SQLite is behind an
injected `runtime.Store` boundary. An inspectable JSONL form can be added as an
export without becoming a second source of truth.

## Development checks

```sh
make check       # gofmt check, tests, vet, and golangci-lint
make test-race   # race-enabled test suite

# optional local commit hooks
pre-commit install
pre-commit run --all-files
```

CI runs the same checks on pushes and pull requests. The lint configuration
keeps `modernize` enabled, including diagnostics such as `slicescontains`;
wire-format-sensitive and intentionally sentence-like error messages are
excluded explicitly in [`.golangci.yml`](.golangci.yml).

## Running

```sh
go test ./...            # everything; no API keys needed
make smoke-runtime       # live Codex + Luna + server/client + Seatbelt smoke test

go run ./cmd/pons-demo     # scripted brain

export ANTHROPIC_API_KEY=sk-ant-…
go run ./cmd/pons -message "inspect this project" # bundled server + client

export OPENAI_API_KEY=sk-…
go run ./cmd/pons -provider openai -message "inspect this project"
go run ./cmd/pons -provider openai -base-url http://127.0.0.1:11434/v1 -message "inspect this project"

go run ./cmd/pons -provider codex -login # one-time: browser login with
                                        # your ChatGPT account (PKCE flow)
go run ./cmd/pons -provider codex -i    # bundled interactive mode

# Long-lived local runtime. The server owns conversations and agent runs;
# clients submit messages and consume SSE events.
go run ./cmd/pons serve -provider codex
go run ./cmd/pons client -message "inspect this project"
go run ./cmd/pons client -conversation <id> -message "continue"

# Build the hands-side host, then create one Seatbelt environment per run.
go build -o .build/pons-hands ./cmd/pons-hands
go run ./cmd/pons -provider codex -sandbox seatbelt \
  -hands-command .build/pons-hands -message "inspect this project"
```

Useful flags: `--workspace` (jail root; default: current directory),
`--state-dir`, `--max-turns`, `--compact-chars`, `--plugin MANIFEST` (explicit
repeatable hands-side external plugin), `--plugin-path PATH` (safe PATH for
interpreted plugin runtimes), `--sandbox seatbelt`, `--hands-command`, and
`--debug` (runtime/provider/model settings, effective sandbox and network
policy, hands startup, raw tool inputs/outputs, and provider turn details).
Tool output caps
are tunable at the composition: `--fs-read-bytes`
(read_file cap; 0 = 256 KiB, negative = unlimited), `--bash-timeout`,
`--bash-max-lines`, `--bash-max-bytes`, and `--plugin-max-result-bytes`
(external tool result cap; 0 = 1 MiB). External children get an empty
environment by default; use `--plugin-path` or an embedding
`external.HostConfig{Path: ...}` for Node/Bun without inheriting credentials.

With `--sandbox seatbelt`, each server-managed run starts a fresh `pons-hands`
tool host under a generated macOS Seatbelt policy and closes it when the run
becomes idle. The workspace persists; network and ambient credentials are
denied unless explicitly configured. Without `--sandbox`, tools run in-process
for development and platforms without the Seatbelt backend.

`pons serve` runs the v1 orchestration server on `127.0.0.1:7337` by default.
It durably accepts messages, serializes each conversation, enforces a global
run limit and an exclusive lease for the workspace, hydrates a fresh brain for
each active run, and streams events over SSE. Runtime state lives in
`<state-dir>/runtime.db`; clients hydrate with
`GET /v1/conversations/{id}` and then subscribe after its durable cursor.
`pons client` is the HTTP/SSE client. When continuing an existing conversation
it renders the canonical snapshot before subscribing after that snapshot's
cursor; use `--server`, `--conversation`, and `--idempotency-key` to control
connection, continuation, and safe retry behavior. The v1 server
refuses non-loopback listen addresses. There is no separate direct execution
or JSONL persistence path. With no subcommand, pons starts a server on an
ephemeral loopback port, uses the same HTTP/SSE client path, and shuts the
server down when the invocation or interactive session ends.

`make smoke-runtime` runs a consistent end-to-end check with
`gpt-5.6-luna`. It builds `pons-hands`, starts the bundled server/client path
with a fresh temporary SQLite database and Seatbelt environment, asks the
agent to verify the module path through `read_file`, and requires the final
answer `SMOKE_OK`. It uses the saved Codex login and is intentionally excluded
from `make check` because it calls a live model. Set `PONS_SMOKE_MODEL` to
override the model when diagnosing provider-specific behavior.

Durable settings live in `~/.pons/config.json` (global defaults) and
`.pons.json` in the workspace (project overrides); explicit flags win over
both. Recognized keys: `provider`, `model`, `base_url`, `state_dir`,
`max_turns`, `compact_chars`, `fs_read_bytes`, `bash_timeout`,
`bash_max_lines`, `bash_max_bytes`, `plugin_max_result_bytes`, `fallbacks`,
and the multi-provider block: `providers` (array of
`{id, provider, model, base_url, api_key}`) plus
`default_provider_id`. Multiple entries of the same provider type are
allowed (e.g. two ChatGPT subscriptions with distinct auth entries).
The default entry is the primary and the remaining entries back it up in
listed order. The flat `provider`/`model`/`base_url` keys and `providers`
are mutually exclusive. Decoding is strict: unknown keys, trailing data, or
wrong types fail startup rather than being ignored. Invocation intent
(`--message`, `-i`, `--debug`, `--login`) stays flag-only.

Provider failover: when the primary provider fails to complete a turn
(outage, rate limit), the brain retries the same conversation on the next
slot — the history is provider-agnostic, so a mid-run switch is a clean
handoff. Configure with `-fallback provider[:model]` (repeatable, replaces
the backup list) or through `providers`/`fallbacks` in a config file;
cancellation is never retried. An explicit `-provider` flag runs ad-hoc
(flags-only credentials) with the declared providers backing it up.

Credentials live in one auth store: `~/.pons/auth.json` (0600), a JSON
object mapping auth ids to codex OAuth credentials
(`{"codex-personal": {"access": …, "accountId": …, "refresh": …, "expires": …}}`).
A codex provider entry's `id` names its credential directly — `--login -as
<id>` is all the wiring. A store holding exactly one entry stands in for
any id, so single-subscription setups need no naming at all. The legacy
single-credential file shape is read as the `codex` entry and upgraded in
place on the next refresh. Refreshes rewrite the whole store under an
advisory file lock, so concurrent pons processes do not clobber each
other's tokens.

External tool arguments are typed JSON objects, not string maps. Small Go and
TypeScript provider examples live in [`examples/external-echo`](examples/external-echo)
and [`examples/external-echo-ts`](examples/external-echo-ts); build one beside
its manifest, then pass `--plugin ./plugin.json`. For a Node-based plugin, pass
for example `--plugin-path /usr/local/bin:/usr/bin:/bin`. Manifests are never
discovered implicitly.

Tool calls within a turn execute **concurrently**; results are recorded in
call order (never completion order) so transcripts and resumes stay
deterministic — see ADR-0006.

The Codex provider authenticates with your **ChatGPT subscription** using
pons's own OAuth login: `--login` runs a PKCE browser flow (local callback
server on `127.0.0.1:1455`, the same scheme the Codex CLI uses) and stores
credentials at `~/.pons/auth.json` (0600). Pons owns that file: refreshed
tokens are written back, so the subscription stays usable across runs
without re-authenticating. Subscription-backend quirks handled by the
adapter: `max_output_tokens` is rejected (output limits are
server-managed), and `response.completed` carries an empty `output[]` —
items arrive via `response.output_item.done` and are accumulated. `-model`
defaults to `gpt-5.6-terra`; available models come from the backend's
`/models` endpoint (they rotate by client version).

The LLM brain discovers tools automatically: a plugin registered via
`AddTool` becomes a tool schema for the model in the same run — adding a
capability plugin needs zero brain changes. External `tool_provider/v1`
capabilities follow the same path after hands-side discovery.

## Architecture decisions

The reasoning behind the big choices is recorded as ADRs in [`docs/`](docs/):

| ADR | Decision | Status |
|---|---|---|
| [ADR-0001](docs/adr-0001-pluggable-minimal-harness.md) | Minimal pluggable harness, brain/hands separation | Accepted |
| [ADR-0002](docs/adr-0002-tree-sessions-sqlite.md) | Earlier finite-harness JSONL session storage | Superseded by ADR-0010/0011 |
| [ADR-0003](docs/adr-0003-tool-result-contract.md) | ToolResult: status envelope / canonical observation / typed plugin payloads | Accepted |
| [ADR-0004](docs/adr-0004-sandboxed-hands-boundary.md) | Hands are the execution boundary; isolation is deployment policy | Accepted |
| [ADR-0005](docs/adr-0005-transcript-model-vs-engine.md) | Earlier finite-harness transcript/storage separation | Superseded by ADR-0010 |
| [ADR-0006](docs/adr-0006-concurrent-tool-execution.md) | Tool execution concurrent within a turn; record in call order | Accepted |
| [ADR-0007](docs/adr-0007-language-neutral-plugin-runtime.md) | Language-neutral persistent external tool plugins | Accepted |
| [ADR-0008](docs/adr-0008-runtime-orchestration-layer.md) | Long-lived orchestration runtime above the core; built-in chat/message path | Accepted |
| [ADR-0009](docs/adr-0009-hands-execution-environments.md) | Provider-backed hands execution environments; Seatbelt first | Accepted |
| [ADR-0010](docs/adr-0010-runtime-state-and-client-synchronization.md) | Transactional runtime messages/state, client snapshots, durable event outbox, transient token deltas | Accepted |
| [ADR-0011](docs/adr-0011-server-centred-runtime-storage.md) | Server-centred execution; injected runtime store; legacy session stack removed | Accepted |

The concrete first-runtime shape and client synchronization contract are
described in [`docs/runtime-v1.md`](docs/runtime-v1.md), and the hands
environment shape is described in
[`docs/hands-environment-v1.md`](docs/hands-environment-v1.md).

## Dependencies

The two official LLM SDKs (`anthropic-sdk-go`, `openai-go`), a pure-Go SQLite
driver, and their transitive dependencies. The server uses the standard
library HTTP stack and builds without cgo.

## Next

- Streaming observations: text deltas and partial tool output on `OnEvent`
- Linux and remote execution-environment providers behind the environment seam
- Per-run execution-environment lifecycle in the long-lived runtime
- Branch summaries + agent-driven branching
- `go install`-able releases (the module is `github.com/samperrin/pons`)
