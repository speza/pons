# ADR-0005: The transcript model is a contract; engine and location are composition choices

**Status:** Accepted
**Date:** 2026-09-15
**Related:** ADR-0001, ADR-0002, ADR-0003

## Context

A transcript has two separate concerns. The data model defines what a
conversation and its branches mean. The storage engine and filesystem
location define how a composing application keeps that model.

pons uses the same separation for sessions that it uses for the brain/hands
protocol. The session contract must not require a particular storage engine,
provider, or CLI directory; JSONL is the selected implementation rather than
the model itself.

## Decision

### 1. `sessions/` is the engine-independent transcript contract

The package contains pure, JSON-serializable types:

- `Entry`, an immutable node with an id, parent id, kind, text, and payload;
- `Session`, a tree with one mutable leaf pointer; and
- `Store`, the operations for creating sessions, appending entries, walking
  the active path, branching, reading children, searching, and closing.

The active conversation is the path from the root to the leaf. Branching moves
the leaf to an existing entry; it does not delete abandoned descendants.
Implementations must preserve these semantics.

`WriteJSONL` serializes entries as engine-independent NDJSON. It belongs in the
contract package because export is defined in terms of the model rather than a
particular storage engine.

### 2. JSONL is the shipped storage engine

`plugins/sessionsjsonl` is the default and only shipped `sessions.Store`
engine. It uses one append-only, self-describing NDJSON file per session. A
file contains a session record, entry records, and optional leaf records. The
last leaf-relevant record wins, and a truncated trailing line is ignored on
load.

The engine performs substring search by scanning the session file. It limits
search to the active root-to-leaf path unless the caller requests all
branches. Files and their directory use private permissions. SQLite is not a
runtime dependency and is not part of the current composition.

### 3. Location belongs to the composing application

The store receives its directory from its caller. The CLI selects a durable
per-project directory under `~/.pons/sessions/` and allows `--session-dir` to
override it. The session contract and engine do not require that location.

### 4. Recording, resume, and compaction use different layers

`sessionrecorder` records completed turns into any `sessions.Store` and
publishes the configured transcript path as `PONS_SESSION_FILE`. It does not
add session search or read actions; the live agent can inspect the file with
bash and standard text tools.

Resume hydrates the active path into the LLM brain. Context compaction is also
a brain operation: it rewrites only the brain's in-memory provider context.
The transcript remains complete and unchanged, so it remains the source for
inspection and resume.

## Consequences

- The model, engine, and location can be reasoned about independently.
- JSONL is directly inspectable with `grep`, `tail`, `jq`, and other standard
  tools while the agent is running.
- The default implementation stays dependency-free and scans only one
  session file for search.
- A `Store` instance serializes its own access; separate instances sharing a
  directory are not coordinated.
- Search is intentionally substring-based. Regex use belongs to transcript
  export or ordinary file tools rather than the store contract.

## References

- `sessions/sessions.go` — transcript model and `Store`
- `plugins/sessionsjsonl/` — JSONL engine
- `plugins/sessionrecorder/` — recording and transcript publication
- `docs/adr-0002-tree-sessions-sqlite.md` — concrete tree/storage choice
