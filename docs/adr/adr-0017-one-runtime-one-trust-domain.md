# ADR-0017: One runtime serves one trusted administrative domain

**Status:** Accepted
**Implementation:** Current single-domain boundary in effect; hosted cells not implemented
**Date:** 2026-09-22
**Related:** ADR-0001, ADR-0004, ADR-0008, ADR-0009, ADR-0011, ADR-0012, ADR-0016

## Context

pons is a local-first agent harness. One runtime manager owns canonical state,
configuration, workspaces, environments, plugins, and credential references in
one SQLite store. Its native server is loopback-only and has no authenticated
principal model.

Persistent multi-agent work does not imply SaaS multi-tenancy. Several agents
and collaborators can share one administrator and policy authority. Hosting
mutually untrusted customers requires authentication and object authorization,
isolated secrets and execution, tenant-scoped storage and scheduling, quotas,
metering, retention, deletion, encryption, and operational audit. Adding a
`tenant_id` alone supplies none of those properties.

Brain/hands separation limits planner execution and supports hands isolation.
It does not isolate host-side brains, the runtime, canonical state, trusted
plugins, or configuration between tenants.

## Decision

### 1. One runtime is one trust domain

A runtime instance represents one **trusted administrative domain**. Its
agents, conversations, workspaces, credentials, channels, and host plugins are
governed by one policy authority.

A domain may include several trusted humans and agents. Mutually untrusted
users or organizations must not share a runtime instance, logical store
namespace, host plugin process, or credential environment.

pons therefore does not add `TenantID` to `Core`, `protocol.Action`, runtime
types, store methods, event cursors, or tool contracts. Store and configuration
are implicitly scoped to their runtime.

### 2. Least privilege remains intra-domain

Under ADR-0016, agent definitions can differ in tools, recipient allowlists,
workspaces, network access, provider slots, and credential grants.
Deterministic policy then limits confused-deputy behavior and accidental data
movement.

These are least-privilege controls inside one domain, not hostile-tenant
isolation. In-process host plugins are trusted, and isolating hands does not
isolate host-side brains or canonical state.

Several authenticated people may eventually collaborate inside one domain.
Principal permissions can govern who may message an agent, approve an action,
read a conversation, or cancel work. Cross-domain sharing, federation, and
public marketplaces remain separate designs.

### 3. Hosted products use isolated runtime cells

A future hosted control plane routes each domain to an isolated runtime cell:

```text
authenticated request -> control plane -> runtime cell A
                                      \-> runtime cell B
```

The control plane owns account identity, authentication, domain routing,
provisioning and deletion, fleet placement, quotas and billing, secret and key
delivery, backups, export, retention, regional policy, and cross-domain audit.
Each cell continues to use ordinary single-domain pons contracts.

The simplest cell is one process and database file per domain. Denser hosting
may use shared infrastructure with separate processes, VMs, database schemas,
roles, or databases, provided isolation is enforced below pons's runtime
contract.

A shared PostgreSQL server does not itself create safe shared tenancy. One
logical namespace and coordination authority per cell remains preferred. A
future shared-store proposal must explicitly cover row authorization,
namespace-qualified uniqueness, tenant-aware leases and claims, fair
scheduling, cache isolation, encryption, retention, and cross-tenant tests.

### 4. Preserve future-safe seams

The current architecture provides these seams without implementing tenancy:

- public IDs are opaque, not derived from usernames or paths;
- stores and environments are injected;
- `Core` has no process-global conversation or credential state;
- transport authentication stays outside the finite core;
- scheduler and event correctness are independent of UI and channel.

ADR-0016 requires an injected agent directory, explicit credential grants,
and agent ownership metadata for conversations. These are implementation
requirements for multi-agent work, not properties of the current runtime.
Runtime-level export and deletion are future hosted-cell operations. The
existing seams make isolated cells practical without adding unused tenant
vocabulary to local APIs.

### 5. Security claims fail closed

The native server remains loopback-only until an authenticated transport
decision is accepted. Documentation describes pons as local-first or
single-domain and does not recommend direct exposure to untrusted networks.

Serving mutually untrusted customers requires approved authentication and
isolation above separate cells. A tenant field, reverse proxy, hands sandbox,
or PostgreSQL migration alone is insufficient.

## Alternatives

- **Add `tenant_id` everywhere:** rejected because it adds permanent contract
  complexity while falsely implying authentication and isolation.
- **Make the primary runtime a shared SaaS server:** rejected because it turns
  a small harness into an identity, billing, compliance, and fleet platform
  before the agent model is validated.
- **Allow exactly one human per runtime:** rejected because trusted teams,
  households, and channels can share one policy authority.
- **Ignore hosting entirely:** rejected because the current opaque IDs and
  injected infrastructure, together with the planned explicit grants and
  runtime-level export/deletion, give isolated cells a clear path.

## Consequences

The finite core and local runtime stay small, brain/hands retains its intended
meaning, and multi-agent work avoids unrelated SaaS concepts. Hosted products
can rely on stronger process, VM, and storage boundaries rather than only
application predicates, and pons does not claim security it has not built.

Hosted deployments initially provision more runtime and store units.
Cross-domain administration belongs to the control plane. Large fleets may
eventually need denser cells or a deliberately multi-tenant store, and
collaboration over non-loopback channels still needs a principal model.

## References

- `core.go` — finite brain/hands loop
- `runtime/`, `runtime/sqlite/` — single-domain runtime and local store
- ADR-0004 — hands isolation
- ADR-0008, ADR-0011, ADR-0012 — runtime persistence and coordination
- ADR-0016 — persistent agents and asynchronous delegation
