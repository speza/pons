# ADR-0002: Tree-based sessions in SQLite (retired engine), searched through the hands layer

**Status:** Retired — the SQLite engine implementation was removed (ADR-0005 amendment: JSONL is the only engine). The data-model decisions survive in `sessions/` + ADR-0005.
**Date:** 2026-09-15
**Context:** The agent needs durable, resumable session history with branching
(pi's JSONL session tree does this via `id`/`parentId` entries). The recurring
concern: if sessions live in SQLite, the agent itself can't `grep` its own
history the way it could grep a JSONL file.

## Decision

Session storage is a **plugin** (`plugins/sessionsqlite`), built on SQLite
(pure-Go driver, no cgo), with this model:

- **Entries are immutable**, addressed by `(session_id, id)`, linked by
  `parent_id`. One mutable **leaf pointer** per session; the live conversation
  is the path root → leaf (walked by a recursive CTE).
- **Branching = moving the leaf** to an earlier entry. Abandoned branches
  remain in the store and stay queryable (`all_branches=true`).
- **Full-text search (FTS5, trigram) is exposed as a hands action**
  (`search_session`), not left to the agent grepping an opaque file. Trigram
  gives grep-like substring semantics; queries < 3 chars fall back to `LIKE`.
- **Export is the escape hatch**: `ExportJSONL` writes the path root→leaf as
  pi-shaped NDJSON for ad-hoc unix tooling and interop.
- **Search is a capability, not a convention**: the agent queries its own
  history through the same policy-checked tool boundary as every other tool —
  no brain ever opens the database.

## Alternatives considered

- **JSONL files (pi's format)** — trivially greppable with unix tools, but tree
  operations (path-to-leaf, ancestry, branching) require full-file scans, and
  structured queries ("all failed commands last session") are impossible.
- **Dual-write (SQLite + JSONL mirror)** — rejected: duplication drifts;
  export-on-demand gives the same greppability without a second source of truth.

## Consequences

**Positive:** real queries over history — branch scoping, kind filtering
(`search_session`'s `kind` argument), recency limits — none of which plain
grep offers; abandoned branches searchable without file-walking; sessions
survive restarts and can later hydrate brain context (resume).

**Composition note:** the store's *location* is a policy of the composing
binary, not the plugin — `sessionsqlite.New(path)` opens whatever it is
given, and the demo binary's durable per-project default
(`~/.pons/sessions/<hash>.db`) is an application convention. Plugins
contribute capabilities; compositions contribute conventions.

**Negative / accepted risks**

- SQLite is opaque to naive text tools — mitigated by the search tools and
  the JSONL export, and by `sqlite3` for humans.
- FTS5 trigram needs ≥3 characters (LIKE fallback covers shorter queries).
- **Regression lesson (recorded because it bit us):** the path-walk CTE must
  join `e.id = w.parent_id` (walk *up*). The inverted join passes naive tests
  whenever the hit is the leaf itself — scoped-search tests must always assert
  a **non-leaf ancestor** hit.

## References

- `plugins/sessionsqlite/` (store + `search_session`/`read_session` actions)
- pi `session-format.md` (the JSONL tree this format mirrors)
- Live proof: the demo brain searches its own recorded history mid-task.
