# ADR implementation status

**As of:** 2026-09-23, branch `docs/persistent-agents-spec`

An ADR's **Status** records the decision: Accepted, Proposed, or Superseded.
It does not mean the feature is implemented. This inventory records the
shipped local scope separately. Update it when implementation work lands;
leave an ADR Accepted when its decision still governs even if later stages
remain unbuilt.

| ADR | Decision | Implementation | Current scope |
| --- | --- | --- | --- |
| [0001 — minimal harness](adr-0001-pluggable-minimal-harness.md) | Accepted | Implemented | [Finite core](../core.go), [brain/hands protocol](../protocol/) and plugin registry. |
| [0002 — tree sessions](adr-0002-tree-sessions-sqlite.md) | Superseded | Superseded | Replaced by ADR-0010 and ADR-0011; no active session-tree persistence path. |
| [0003 — tool results](adr-0003-tool-result-contract.md) | Accepted | Implemented | [`ToolResult`](../protocol/protocol.go) and core dispatch. |
| [0004 — hands boundary](adr-0004-sandboxed-hands-boundary.md) | Accepted | Implemented boundary | In-process tools remain unsandboxed; isolation is opt-in through [Seatbelt](../environment/seatbelt/) or [E2B](../environment/e2b/). |
| [0005 — transcript model](adr-0005-transcript-model-vs-engine.md) | Superseded | Superseded | Canonical semantic history moved to ADR-0010 and ADR-0011. |
| [0006 — concurrent tools](adr-0006-concurrent-tool-execution.md) | Accepted | Implemented | Tool execution in [Core](../core.go) is concurrent with ordered results. |
| [0007 — external plugins](adr-0007-language-neutral-plugin-runtime.md) | Accepted | Implemented | [`tool_provider/v1`](../plugins/external/) host and SDK. |
| [0008 — runtime layer](adr-0008-runtime-orchestration-layer.md) | Accepted | Implemented local slice | [Runtime](../runtime/) and [native HTTP/SSE](../runtime/httptransport/) exist; other channels are proposed in ADR-0018. |
| [0009 — execution environments](adr-0009-hands-execution-environments.md) | Accepted | Implemented local and E2B providers | [Environment contract](../environment/), [Seatbelt](../environment/seatbelt/), and [E2B](../environment/e2b/); other providers remain future work. |
| [0010 — transactional state](adr-0010-runtime-state-and-client-synchronization.md) | Accepted | Implemented | [SQLite canonical messages, runs, and event outbox](../runtime/sqlite/). |
| [0011 — server-owned storage](adr-0011-server-centred-runtime-storage.md) | Accepted | Implemented | [Injected store contract](../runtime/store.go) and one SQLite authority; legacy session stack removed. |
| [0012 — scalable coordination](adr-0012-scalable-runtime-coordination.md) | Accepted | Partial | [Bounded local scheduler](../runtime/scheduler.go) and SQLite claims are implemented; distributed leases, PostgreSQL, and cross-process delivery are not. |
| [0013 — provider slots](adr-0013-named-provider-slots.md) | Accepted | Implemented | [CLI composition](../cmd/pons/) and [LLM fallback](../plugins/brain/llm/). |
| [0014 — action authorization](adr-0014-native-action-authorization.md) | Proposed | Not implemented | Tool-specific guards exist, but the proposed native pre-execution stage and durable approval path do not. |
| [0015 — E2B stdio](adr-0015-e2b-stdio-transport.md) | Accepted | Implemented | [E2B provider](../environment/e2b/) carries the hands protocol and checkpoints workspaces. |
| [0016 — persistent agents](adr-0016-persistent-agents-and-async-messaging.md) | Proposed | Not implemented | Agent directory, private delegation, lineage state, and multi-agent routing are specified only. |
| [0017 — one trust domain](adr-0017-one-runtime-one-trust-domain.md) | Accepted | Current boundary in effect | [Native server](../runtime/httptransport/) is loopback-only and the runtime has no tenant model; hosted cells are not implemented. |
| [0018 — triggers and delivery](adr-0018-agent-triggers-and-delivery.md) | Proposed | Not implemented | Schedules, connector ingress, silent outcomes, and outbound delivery intents are specified only. |
| [0019 — agent memory](adr-0019-agent-memory.md) | Proposed | Not implemented | Scoped durable memory is specified only. Conversation history is the current durable context. |
| [0020 — artifacts](adr-0020-agent-artifact-transfer.md) | Proposed | Not implemented | Authorized immutable transfer is specified only. E2B workspace checkpoints are a separate implemented resource. |
| [0021 — durable work](adr-0021-durable-work-and-waits.md) | Proposed | Not implemented | Work items, wake conditions, and resumable approvals are specified only. |

The persistent-agent feature therefore builds on implemented local runtime
and environment infrastructure. ADR-0014, ADR-0016, and ADR-0018 through
ADR-0021 are the proposed implementation work. ADR-0017 is the governing
single-domain boundary throughout.
