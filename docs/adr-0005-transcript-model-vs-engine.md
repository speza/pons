# ADR-0005: The transcript model is a contract; the engine and location are not

**Status:** Accepted — amended 2026-09-15: default engine is JSONL (see amended decision)
**Date:** 2026-09-15
**Supersedes (in part):** ADR-0002 — which bundled three decisions into one
plugin. Related: ADR-0001 (plugin philosophy), ADR-0003 (ToolResult wire shape).

## Context

How transcripts/conversations are stored is a fundamental decision. The
reference points disagree by design: pi and Claude Code use append-only
**JSONL** (greppable with unix tools, tail-able while running); OpenAI Codex
uses **SQLite**. ADR-0002 chose SQLite and — critically — placed the *data
model* (the entry tree, kinds, leaf pointer, search semantics) inside the
`sessionsqlite` plugin. That bundled three separable decisions:

1. **What a transcript IS** — the data model.
2. **HOW it is stored** — the engine.
3. **WHERE it lives** — the location (already moved to the composing binary).

Only (1) is fundamental. If (2) and (3) are separate, the model can't be
held hostage by the engine.

## Decision

**The transcript data model is a standalone contract package** (`sessions/`),
same pattern as `protocol/` for the brain↔hands seam:

- Pure, JSON-serializable types: `Entry` (immutable node: id, parentId, kind,
  text, payload), `Session`, entry kinds.
- The `Store` interface: `Append` (child + leaf move), `PathToLeaf`,
  `Children`, `Branch`, `GetEntry`, `Leaf`, `Search`, session create/get,
  `Close`. Implementations must preserve the tree semantics.
- `WriteJSONL` — the pi-shaped NDJSON interchange serializer lives in the
  contract, not the engine: interchange is engine-independent by definition.

**The engine is a plugin implementation**, not the model. `sessionsqlite`
(SQLite + FTS5, WAL, recursive-CTE tree walks) is the shipped implementation
and now asserts `sessions.Store` compile-time. A future
`plugins/sessionsjsonl` (append-only NDJSON + sidecar index, or no index) or
a pi-format reader would implement the same contract.

**The location is composition policy** (see ADR-0002's composition note):
`~/.pons/sessions/<hash>.db` is a `pons` binary convention, not a plugin
or model rule.

**Shipped default engine: JSONL** (amended after benchmarking; SQLite remains
an alternate plugin). The measured trade — 1,000 realistic entries:

| Operation | SQLite (FTS5, modernc) | JSONL (naive scan) | JSONL advantage |
|---|---|---|---|
| append ×1000 | 319ms | 23ms | **14×** |
| search ×50 | 310ms | 43ms | **7×** — a naive full scan beats indexed FTS |
| path-to-leaf ×20 | 61ms (CTE) | 15ms (load-all) | **4×** |
| dependency tree | modernc.org: 259MB module cache, multi-MB binary | stdlib only | distribution |
| status | **removed** (was alternate) | shipped default | |
| grep/tail the transcript | no (ADR-0002's finding) | native | the headline property |

The reason a *naive scan* beats a *full-text index*: our no-cgo constraint
forces the pure-Go SQLite driver, which is slow enough that linear scans win
at agent-session scale (hundreds–thousands of entries, per-session files).
The SQLite implementation has since been **removed entirely** (along with
the `modernc.org/sqlite` dependency tree — 259MB module cache, multi-MB
binary): it had no consumer (both binaries compose the JSONL engine; the
agent-facing tools are engine-agnostic), and our two worst session bugs
were SQL bugs. If indexed queries at scale are ever a real need, the
`sessions.Store` contract makes a SQLite plugin a straight re-add —
ADR-0002 records its design. The JSONL engine's `Search` is scan-based
over per-session files, which covers the contract at transcript scale.

The JSONL engine's file format is self-describing NDJSON
(`{"type":"session"|"entry"|"leaf", …}`) — the file IS the transcript:
grep-able with unix tools, `tail -f`-able while the agent runs, and
crash-tolerant (a truncated trailing line is skipped on load). Leaf
semantics: the last leaf-relevant record wins (an earlier branch marker
must not override entries appended after it — caught by the
non-leaf-ancestor scoped-search test).

## Consequences

**Positive:** the fundamental decision (the model) is now a dependency-free
contract with documented semantics; engines are swappable without touching
brains, tools, or the loop; the interface is small enough (~9 methods) that a
JSONL backend is a plausible afternoon, not a redesign.

**Negative / accepted risks**

- JSONL search is a linear scan — fine at session scale (per-session files
  bound it); re-select SQLite for indexed queries at scale.
- One writer per session file; concurrent writers to the *same* session are
  unsupported (matches the loop's sequential design).
- `Search`'s shape (substring + scope, no regex) is frozen into the contract;
  regex needs remain on the export path by design.

## References

- `sessions/sessions.go` (contract), `plugins/sessionsqlite/` (engine)
- pi `session-format.md`, Claude Code transcripts (the JSONL lineage)
- ADR-0002 (storage engine details, composition note)


## Amendment (same day): the agent-facing session tools are gone — bash is the interface

Once the transcript became a plain NDJSON file, `search_session` /
`read_session` were solutions to a problem that no longer existed. They
existed to compensate for SQLite's opacity (ADR-0002's rationale); with
JSONL the compensation layer was legacy.

**Decision:** the session capability set is now minimal — one plugin
(`sessionrecorder`) that *records* turns into any `sessions.Store` and
publishes the transcript path as `PONS_SESSION_FILE` (the pi `PI_*` env-var
pattern). There are no session search/read tools:

- the live brain has its own conversation in context — session search only
  matters after compaction/resume, where grep over the file is the tool;
- bash is strictly more expressive than the removed tools (regex, context
  lines, jq);
- the LLM's system prompt states the contract; the shell inherits the env
  var, so the agent introspects its own history with plain unix tools.

This also retires ADR-0002's "search through the hands layer" rationale: it
was a property of the SQLite engine, not of sessions themselves. The
`sessions.Store.Search` method stays in the contract (it powers
compaction/resume tooling), but the live agent surface no longer carries it.

### Amendment (same day): context compaction is a brain-side operation

Compaction lives in the LLM brain (`llm.Config.CompactChars`/`CompactKeep`):
when the estimated conversation exceeds the budget, older turns are
summarized by the model into one user message and the recent tail is kept
verbatim. Two consequences follow from the layering:

- **The transcript file is never modified by compaction** — it keeps every
  turn, so `$PONS_SESSION_FILE` remains the complete, greppable record the
  agent reaches through bash; that is precisely the fallback the
  post-compaction context points at (the summary message says so).
- The engine is irrelevant to compaction — both the JSONL and SQLite stores
  keep the full tree; compaction only rewrites what the *brain* replays.

Live-verified: with `--compact-chars 4000`, a 6-turn codex session compacted
twice mid-run and still completed correctly, while the JSONL file retained
every raw entry.
