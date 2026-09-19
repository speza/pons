# ADR-0011: The server owns runtime persistence behind an injected store

**Status:** Accepted
**Date:** 2026-09-19
**Related:** ADR-0001, ADR-0002, ADR-0005, ADR-0008, ADR-0010

## Context

pons originally had a finite CLI harness that recorded completed core turns
through `sessionrecorder` into a `sessions.Store`. The shipped implementation
wrote one JSONL file per session. The later server runtime persisted canonical
messages, operational state, and client events transactionally in SQLite.

Keeping both paths created two application architectures and two persistence
models. A completed-turn observer also cannot atomically persist message
acceptance, run claims, pre-execution tool intent, tool outcomes, and client
outbox events.

pons has not made a compatibility commitment yet, so retaining the obsolete
path would add ambiguity without protecting an established public contract.

## Decision

### 1. The application is server-centred

Only the server composition executes managed agent runs. `pons serve` exposes
that lifecycle as a long-lived process; the default CLI bundles the same server
on an ephemeral loopback port and acts as its HTTP/SSE client. There is no
direct one-shot execution path.

### 2. SQLite is the only shipped authoritative store

The server stores canonical semantic messages, submissions, runs, tool calls,
and durable client events in one SQLite database. It does not dual-write a
session tree or JSONL transcript.

The runtime manager consumes the domain-level `runtime.Store` interface.
SQLite lives in the child adapter package `runtime/sqlite`, which imports the
runtime contract; the runtime package does not import its implementation. The
composing process imports both packages, constructs the concrete store, and
closes it. Store operations
represent atomic runtime transitions rather than exposing SQL, allowing a
future PostgreSQL implementation without changes to the manager, transports,
or runner.

The storage backend is an infrastructure adapter, not a `pons.Plugin`.
`pons.Plugin` remains the finite brain/hands capability seam. Database
lifecycle and transactions do not participate in that registry.

### 3. Remove the legacy session stack

The `sessions` contract, `plugins/sessionsjsonl`, `plugins/sessionrecorder`,
resume conversion helpers, direct CLI flags, and their tests are removed.
The deterministic core demo remains finite but has no persistence plugin.

If inspectable JSONL is useful, it will be an explicit export of canonical
runtime messages. Export is not an authoritative backend and is never used for
runtime recovery, LLM hydration, or client hydration.

### 4. Compatibility is not yet a design constraint

Until the project deliberately declares a compatibility policy, superseded
APIs, flags, storage formats, and wire shapes should be removed rather than
carried as legacy paths. Contract changes still require tests and an ADR or an
update to the governing ADR.

## Consequences

- Every application run is managed, durable, observable, and reconnectable
  through the same runtime path.
- There is one authoritative persistence model and no cross-store
  reconciliation problem.
- Storage implementations are swappable through `runtime.Store`, while SQLite
  remains the only shipped implementation for now.
- Agents cannot inspect a live JSONL transcript directly. A query capability
  or export can be designed against canonical runtime messages when needed.
- ADR-0002 and ADR-0005 remain as historical records but are superseded for
  the application architecture.

## References

- `runtime/store.go` — backend-independent runtime store contract
- `runtime/sqlite/store.go` — SQLite implementation
- `runtime/manager.go` — storage consumer and run coordinator
- `cmd/pons/runtime_mode.go` — server composition and HTTP/SSE client
