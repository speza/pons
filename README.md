# pons

**pons** (*Latin*: "bridge") — in anatomy, the part of the brainstem that
carries signals between the brain and the body, in both directions: motor
commands flow down, sensory observations flow up. The reflex centers live
there.

That is the whole architecture. pons is a minimal, pluggable agent harness in
Go: a ~400-line core (the bridge), one dependency-free JSON seam
(`protocol/`), and everything else — tools, brains, sessions, policy — is a
plugin you compose. The reasoning **brain** and the executing **hands** are
structurally separated; the pons is what connects them:
actions down, observations up, nothing in between improvises.

Built brain/hands-first: reasoning never touches the machine, execution
never reasons.

## The separation contract

```
┌────────────────────────── brain ──────────────────────────┐
│ LLM loop / reasoning / planning. Sees Observations,       │
│ emits Actions. Owns NO execution, NO direct I/O.          │
│ Link-depends only on: protocol.                           │
└──────────────┬─────────────────────────────────────────────┘
               │ ControlPort (NextActions / Interpret / Close)
               │ ToolPort (Execute)
┌──────────────┴───────────── harness ──────────────────────┐
│ The referee: turn loop, events, plugin registry.          │
│ Contains NO task logic.                                   │
└──────────────┬─────────────────────────────────────────────┘
               │ protocol.Action / protocol.ToolResult
┌──────────────┴────────────── hands ───────────────────────┐
│ Real execution: fs, edit, bash, sessions. Jailed or       │
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
4. **`protocol/` is the seam.** Pure, dependency-free JSON types; swappable
   transport. The same `Action`/`ToolResult` wire types work in-process today
   and across a socket for sandboxed/remote hands later (ADR-0004).

## Packages

| Package | Role |
|---|---|
| `protocol/` | Wire contract: `Action`, `ToolResult`, `Observation`. Core reserves only the `finish` kind |
| `core.go` | The turn loop, the two ports, and the plugin seam. Imports only `protocol` |
| `sessions/` | Transcript data-model contract (`Store` interface, entry tree) — engine-independent |
| `plugins/sessionsjsonl` | **Default transcript engine**: append-only NDJSON, one file per session — the file IS the transcript |
| `plugins/sessionrecorder` | Records turns into any `sessions.Store`; publishes the transcript path via `$PONS_SESSION_FILE` |
| `plugins/fs` | `read_file`, `write_file`, `list_dir` — symlink-resilient workspace jail |
| `plugins/edit` | `edit_file` — SEARCH/REPLACE patch application (exact match, unique, fuzzy fallback, CRLF/BOM aware, unified diff in the result payload) |
| `plugins/bash` | `bash` — unrestricted command execution (per-call timeout, output tail-truncation, full output to temp file). Trust comes from composition |
| `plugins/shell` | `run_command` — first-token allowlist variant, for sandboxed compositions |
| `plugins/brain/llm` | Real LLM brain: one model call per turn, tool schemas auto-discovered from registered plugins; context compaction; resume seeding |
| `plugins/brain/scripted` | Deterministic brain (LLM stand-in for CI) |
| `internal/jail`, `internal/unidiff` | Shared libraries between plugins (code, not registrations) |
| `cmd/pons/` | The CLI: LLM brain, interactive mode, `--resume`, compaction |
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
result, err := core.Run(ctx, message)   // plan → act → reflect → repeat
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
keeping the most recent turns verbatim. The transcript file is never touched
— it keeps everything, and the brain reaches details that fell out of
context by grepping `$PONS_SESSION_FILE` like any file on disk.

## Transcripts

Plain NDJSON files, one per session, no engine required to read them:

```
~/.pons/sessions/<munged-project-path>/<session-id>.jsonl
```

- Self-describing records: session header, entries, branch markers
- Greppable, `tail -f`-able while the agent runs, `jq`-able, diffable
- `--resume` continues one by id (or `latest`); the file is never rewritten —
  branching appends, abandoned branches stay queryable
- `sessions.Store.Search` remains contract-level for future compaction/
  resume tooling; the live agent just uses bash

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

go run ./cmd/pons-demo     # scripted brain

export ANTHROPIC_API_KEY=sk-ant-…
go run ./cmd/pons                       # Anthropic

export OPENAI_API_KEY=sk-…
go run ./cmd/pons -provider openai      # OpenAI or any compatible server
go run ./cmd/pons -provider openai -base-url http://127.0.0.1:11434/v1   # Ollama et al.

go run ./cmd/pons -provider codex -login   # one-time: browser login with
                                        # your ChatGPT account (PKCE flow)
go run ./cmd/pons -provider codex       # then just use it

# interactive: one task per line, conversation continues in-process
go run ./cmd/pons -provider codex -i

# resume: continue the most recent session (brain context hydrated from
# the transcript tree — no goal needed for a status/continuation run)
go run ./cmd/pons -provider codex --resume latest
go run ./cmd/pons -provider codex --resume <session-id> -message "next step…"
```

Useful flags: `--workspace` (jail root; default: current directory),
`--session-dir`, `--max-turns`, `--compact-chars`, `--debug` (raw tool
inputs/outputs and provider turn details).

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
capability plugin needs zero brain changes.

## Architecture decisions

The reasoning behind the big choices is recorded as ADRs in [`docs/`](docs/):

| ADR | Decision | Status |
|---|---|---|
| [ADR-0001](docs/adr-0001-pluggable-minimal-harness.md) | Minimal pluggable harness, brain/hands separation | Accepted |
| [ADR-0002](docs/adr-0002-tree-sessions-sqlite.md) | Tree sessions in SQLite (retired engine) | Retired as engine; model lives in ADR-0005 |
| [ADR-0003](docs/adr-0003-tool-result-contract.md) | ToolResult: status envelope / canonical observation / typed plugin payloads | Accepted |
| [ADR-0004](docs/adr-0004-sandboxed-hands-boundary.md) | Hands as a sandbox boundary; protocol as the deployment seam | Design accepted; transport pending |
| [ADR-0005](docs/adr-0005-transcript-model-vs-engine.md) | Transcript model = contract; **default engine = JSONL** | Accepted |
| [ADR-0006](docs/adr-0006-concurrent-tool-execution.md) | Tool execution concurrent within a turn; record in call order | Accepted |

## Dependencies

Two official LLM SDKs (`anthropic-sdk-go`, `openai-go`) and their transitive
deps. Nothing else: no storage drivers, no cgo, no framework — the transcript
layer is stdlib-only, and `pons` builds as a single static binary.

## Next

- Streaming observations: text deltas and partial tool output on `OnEvent`
- Sandboxed-hands transport: NDJSON-framed `ToolPort` over a socket (ADR-0004)
- Branch summaries + agent-driven branching
- `go install`-able releases (the module is `github.com/samperrin/pons`)
