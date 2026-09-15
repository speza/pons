# ADR-0002: Tree-based sessions in SQLite (retired)

**Status:** Retired — no SQLite engine or session tools are shipped. The
engine-independent model survives in `sessions/`; the current engine decision
is recorded in ADR-0005.
**Date:** 2026-09-15

## Historical context

The first session design needed durable, resumable history with branching. It
combined three decisions that are now deliberately separate:

1. the transcript data model (immutable entries, parent links, and a leaf);
2. the storage engine (then SQLite with FTS5); and
3. the location (always a convention of the composing binary).

## Retired decision

The prototype used a `plugins/sessionsqlite` Store with this tree model:

- entries were immutable, addressed by `(session_id, id)`, and linked by
  `parent_id`;
- each session had one mutable leaf pointer; the live conversation was the
  path root → leaf; and
- branching moved the leaf while leaving abandoned branches queryable.

It also exposed `search_session`/`read_session` actions and exported the path
to JSONL. Those implementation choices were retired when the transcript became
plain JSONL: the file itself is greppable, and the live composition uses bash
over `$PONS_SESSION_FILE` instead of dedicated session actions.

## Current decision

Use the contract in `sessions/` and the stdlib-only
`plugins/sessionsjsonl` engine. Its append-only NDJSON file is the transcript;
see ADR-0005 for the format, crash behavior, and composition policy. A future
indexed or database-backed Store may be added without changing the contract.

## References

- `sessions/sessions.go` — current data-model and Store contract
- `plugins/sessionsjsonl/` — shipped engine
- ADR-0005 — transcript model versus engine
