# ADR-0001: pons — a minimal, plugin-extensible agent harness with hard brain/hands separation

**Status:** Accepted; compile-time-only plugin loading superseded by ADR-0007
**Date:** 2025-09-14
**Context:** Building a lightweight coding-agent harness in Go, informed by pi's
architecture (see `README` discussion). Supersedes the first-pass monolithic
layout (`harness/`, `tools/`, `brain/` packages).

## Problem

Agent harnesses tend to rot in one of two ways:

1. **Control and execution fuse.** Tools become methods on the agent; policy,
   sandboxing, and I/O end up smeared across the reasoning loop. Testing the
   loop requires a live LLM; auditing every side effect requires reading all
   of the code.
2. **The core accretes features.** File tools, shell, sessions, search, UI —
   each addition inflates the core until it is no longer reviewable, and
   removing anything breaks the world.

We want a harness whose core is *trivially* auditable, where every capability
is an explicit, removable addition, and where the reasoning layer ("brain")
is structurally incapable of touching the machine, while the execution layer
("hands") is structurally incapable of reasoning.

## Decision

### 1. The core is only a seam and a loop (~250 lines, two files)

`pons` (the core package) knows exactly three things:

- **`protocol/`** — pure JSON wire types (`Action`, `ToolResult`,
  `Observation`, `TurnLog`). No logic, no imports beyond stdlib. This is the
  brain↔hands contract and the only shared vocabulary.
- **Two ports:**
  - `ControlPort` — what a brain must speak: `NextActions(observation) →
    []Action`, `Interpret(observation, result) → Interpretation`, `Close`.
  - `ToolPort` — what hands must speak: `Execute(action) → ToolResult`.
- **The turn loop:** plan → execute (dispatch by action kind) → interpret →
  repeat, with max-turns and a core-reserved `finish` action.

Everything else is a plugin. A fresh `Core` can do *nothing*: no tools, no
brain, no persistence. Capability surface = exactly the plugin set you
compose. This is also the security posture: you cannot invoke what you did
not install.

### 2. Plugins are pure additions

```go
type Plugin interface { Setup(*Core) error }
```

`Setup` only *registers*; it must not modify or remove existing
registrations. **The contract is enforced by the core:** `AddTool` fails on
a duplicate action kind and `SetBrain` fails if a brain is already
installed — plugin conflicts are composition-time errors, not silent
last-wins. Plugins are ordinary structs with state (a jail root, a DB
handle, an LLM client) — no reflection, no dynamic loading, no service
locator. Composition happens in `main()`:

```go
store, err := sessionsjsonl.New(sessionDir)
if err != nil { return err }
defer store.Close()

core := pons.New()
core.Use(
    fsPlugin, editPlugin, bashPlugin,   // capabilities registered by plugins
    sessionrecorder.New(store),         // persistence + transcript path
    brainscripted.New(steps...),        // the brain (LLM driver later)
)
core.Run(ctx)
```

Registration points, all additive:

| Hook | Used by |
|---|---|
| `AddTool(kind, handler)` | tool plugins (fs, edit, shell, bash) |
| `SetBrain(…)` (via Setup) | brain plugins |
| `WrapTool(func(ToolPort) ToolPort)` | middleware: approval, audit, rate limits |
| `OnTurn(hook)` / `OnTurnError(hook)` | audit / checked persistence plugins |

### 3. Brain/hands separation is structural, not conventional

- The brain can only emit `Action` values; it has no handle to any tool
  implementation. Its `Danger` self-assessment is advisory and untrusted.
- Hands have no goal, no history, no model. They execute one action.
- The core trusts neither: execution is the choke point. Unknown action
  kinds return an *observation* (`OK:false, "no plugin provides…"`) rather
  than a crash, so the brain can adapt to the capability set it actually
  has.

### 4. Capability vocabulary is owned by plugins

The core protocol reserves only `finish`. `read_file` is not a core
concept — the `fs` plugin defines the kind, its JSON args, its guard, and
exports `Action` constructors (`fs.Read(path)`) so brains speak typed,
discoverable vocabulary instead of stringly-typed guesses.

The same rule applies to **result shapes** — `ToolResult` carries only the
universal status envelope and one canonical observation (`Observation()`);
tool-specific result payloads are typed and owned by the plugin that
produces them (see [ADR-0003](adr-0003-tool-result-contract.md)).

### 5. The loop is a packaged API

The turn loop is observable and returns its outcome rather than only errors:

```go
result, err := core.Run(ctx, goal)   // plan → act → reflect → repeat
```

- `RunResult{Answer, Turns, Exhausted, History}` — budget exhaustion is a
  **result**, not an error (pi-style graceful stop).
- `OnEvent(func(pons.Event))` — the loop's event stream (`agent_start`,
  `turn_start`, `action_start`, `action_end`, `turn_end`, `finish`,
  `stopped`, `exhausted`), the seam for future UIs. `OnTurn` observes a
  completed turn; `OnTurnError` is the checked persistence hook (full
  observation + turn log).
- Truncated provider responses (`max_tokens` / `incomplete`) are surfaced as
  errors, never silently treated as complete plans.

### 6. Sessions are plugins (see ADR-0005)

The transcript model is the engine-independent contract in `sessions/`:

- entries are immutable, addressed by `(session_id, id)`, linked by `parent_id`;
- one mutable **leaf pointer** per session; the live conversation is the path
  root → leaf; branching moves the pointer and abandoned branches stay
  queryable;
- `sessionsjsonl` is the shipped append-only NDJSON engine;
- `WriteJSONL` writes the path as pi-shaped NDJSON, while the live agent
  inspects the transcript through ordinary `bash`/`grep` over `$PONS_SESSION_FILE`.

There are no dedicated session search/read actions. `Store.Search` remains in
the contract for storage consumers and future engines; the composition decides
where the transcript lives, and the session recorder publishes its path.

## Consequences

**Positive**

- The loop is testable with zero LLM calls (`brainscripted`); core tests are
  hermetic.
- Every capability is opt-in and removable; the audit surface for "what can
  this agent do" is `main()`.
- Hands can later move to a subprocess/container without redesign: the
  contract is already JSON types over a port.
- Session trees get real queries (path, branching, and scoping) in the store
  contract while the shipped transcript remains grep-able NDJSON.

**Negative / accepted risks**

- Compile-time plugin composition was initially the only loading model. This
  limitation was later superseded by ADR-0007's language-neutral external
  plugin runtime; trusted in-process Go composition remains supported.
- A first-token shell allowlist is policy, not security (e.g. `ls; rm -rf /`
  passes). Real isolation must come from the OS/container boundary around the
  hands process — deliberately out of scope for the core, same conclusion pi
  documents.
- JSONL search is a linear scan and the transcript files are private; a
  future indexed engine can implement the same `sessions.Store` contract.

## Decision log

| ADR | Decision | Status |
|---|---|---|
| [ADR-0002](adr-0002-tree-sessions-sqlite.md) | Earlier SQLite tree-session design | Retired |
| [ADR-0003](adr-0003-tool-result-contract.md) | ToolResult: status / canonical observation / typed payloads | Accepted |
| [ADR-0004](adr-0004-sandboxed-hands-boundary.md) | Hands as a sandbox boundary; protocol as deployment seam | Design accepted; transport pending |
| [ADR-0005](adr-0005-transcript-model-vs-engine.md) | Transcript model = contract; default engine = JSONL (SQLite retired) | Accepted |
| [ADR-0006](adr-0006-concurrent-tool-execution.md) | Within-turn tool execution concurrent; record in call order | Accepted |

## Non-goals (for now)

- Streaming partial tool results over the port (designed in; not wired).
- Streaming LLM responses (the brain completes per turn; deltas are a UI concern).
- Automatic LLM-provider retries/cost tracking; transient provider errors
  surface to the caller.
- Remote/subprocess hands transport (same protocol, next ADR).
- Compaction/branch summaries as first-class entries (schema leaves room:
  `KindNote`).

## Addendum (LLM brain, same day)

The LLM brain shipped as `plugins/brain/llm` without changing the core
beyond one addition: `ToolDef`/`ToolSpec` — tools now register a schema so
LLM-driven brains can discover the capability vocabulary (`ToolSpecs()`).
Loop semantics: one model call per turn; `tool_use` blocks become Actions;
a text-only response is the finish signal; `Interpret` queues results as
`tool_result` blocks (no second model call). Wire formats are pinned by
httptest tests; conversation blocks are a sealed `Block` interface inside
the plugin (the brain↔hands protocol stays stringly-typed by design).

### Provider adapters now use the official Go SDKs

Wire handling moved to `github.com/anthropics/anthropic-sdk-go` (Messages)
and `github.com/openai/openai-go` (Chat Completions + Responses/SSE). Our
adapters are thin Block-model translators; `Raw{Item}` is an opaque
provider-native block used to replay Responses reasoning items between
turns. The only hand-rolled network code left is Codex subscription auth
(pons-owned OAuth login + refresh, written back to ~/.pons/auth.json) —
auth plumbing, not
wire format.

Subscription-backend quirks discovered live (documented for posterity):
- `max_output_tokens` is rejected by the ChatGPT codex endpoint
- `response.completed` carries an empty `output[]`; items must be
  accumulated from `response.output_item.done` events
- input items must carry an explicit `"type":"message"` (openai-go's
  EasyInputMessage elides it)
- current subscription models (rotate — query `/models`): `gpt-6-astra`,
  `gpt-5.6-sol`, `gpt-5.6-terra`, `gpt-5.6-luna`, `gpt-5.5`

## References

- `core.go` — loop, ports, plugin seam
- `protocol/protocol.go` — wire contract
- `plugins/fs`, `plugins/shell`, `plugins/sessionsjsonl`, `plugins/brain/scripted`
- pi docs: `security.md` (no built-in sandbox, by design), `containerization.md`
  (Gondolin: route tool execution into a micro-VM), `development.md`
  (experimental remote harness), `session-format.md` (JSONL session tree)
