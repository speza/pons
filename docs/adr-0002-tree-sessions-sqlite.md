# ADR-0002: Session trees use append-only JSONL (SQLite is not selected)

**Status:** Accepted
**Date:** 2026-09-15
**Related:** ADR-0001, ADR-0005

## Context

A session must preserve the conversation that is active while retaining
branches that the user abandoned. A database is not required for that model,
and a database-backed implementation would make ordinary transcript
inspection less accessible.

The first prototype considered SQLite and dedicated session actions. The
current composition instead uses the tree contract in `sessions/` and the
append-only engine in `plugins/sessionsjsonl`.

## Decision

### 1. A session is an immutable entry tree with one leaf

Entries are addressed by `(session_id, id)` and contain a `parent_id`. An
append creates an immutable child and moves the session's leaf to that entry.
`Branch` moves the leaf to an existing entry; entries on abandoned branches
remain queryable. `PathToLeaf` returns the active conversation from root to
leaf.

These semantics are the `sessions.Store` contract. They are independent of
how a store persists bytes, and are shared by recording and resume code.

### 2. The shipped engine is one private NDJSON file per session

`plugins/sessionsjsonl` stores a session header, entry records, and append-only
leaf records in `<session-id>.jsonl`. The last leaf-relevant record determines
the leaf. A record is newline terminated; a truncated final line is ignored on
load, so an interrupted append does not rewrite earlier history.

The directory is created with mode `0700`, and transcript files with mode
`0600`. `sessionsjsonl` implements all of `sessions.Store`, including
substring search scoped either to the active path or to all branches.

SQLite is not a dependency and no SQLite session engine is shipped.

### 3. Transcript inspection uses the file, not session actions

`sessionrecorder` records completed turns but does not register
`search_session` or `read_session` actions. The CLI publishes the current
transcript path as `PONS_SESSION_FILE`; the agent and the user inspect it with
ordinary tools such as `bash`, `grep`, `tail`, or `jq`.

`Store.Search` remains a storage-level operation for resume and other host
consumers. It is not part of the live hands capability set.

### 4. The composing binary chooses the location

The store receives its directory from the application. The CLI's default is
`~/.pons/sessions/<project-name>/`, where `<project-name>` is derived from the
absolute project path; `--session-dir` overrides it. The session engine does
not impose this location.

## Consequences

- The transcript is readable while the agent is running and remains a
  complete, append-only record for resume and compaction.
- Branching requires a leaf marker rather than rewriting the file.
- Search is a linear scan of one session file, which keeps the implementation
  dependency-free and bounds the scan to a single session.
- A store instance serializes access to its files; separate store instances
  sharing a directory are not coordinated.
- An indexed store would be a different implementation of the same contract,
  not a change to the session model.

## References

- `sessions/sessions.go` — tree model and `Store`
- `plugins/sessionsjsonl/` — append-only JSONL engine
- `plugins/sessionrecorder/` — turn recording and `PONS_SESSION_FILE`
