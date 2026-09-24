# Persistent agents v1 implementation plan

**Status:** Proposed; no phase implemented
**Related:** [ADR-0016](adr/adr-0016-persistent-agents-and-async-messaging.md),
[ADR-0017](adr/adr-0017-one-runtime-one-trust-domain.md),
[ADR-0018](adr/adr-0018-agent-triggers-and-delivery.md),
[ADR-0019](adr/adr-0019-agent-memory.md),
[ADR-0020](adr/adr-0020-agent-artifact-transfer.md),
[ADR-0021](adr/adr-0021-durable-work-and-waits.md),
[ADR-0022](adr/adr-0022-persistent-agent-workspaces.md), and
[ADR-0014](adr/adr-0014-native-action-authorization.md)

This document orders the work needed to turn the one-agent runtime into a
self-hosted runtime for persistent agents. The ADRs own the contracts; this
plan owns sequencing, scope, and exit criteria. When the two disagree, fix the
ADR first.

## Goal

One pons server, run by its owner, hosts any number of long-lived agents
that the owner creates and edits at runtime. There are no agent kinds: every
agent is the same object with its own persona, tools, workspace policy,
memory, schedules, and channels. Every activation remains a finite
`Core.Run`; nothing runs while an agent is idle.

Two typical configurations guide the design:

- a **chief of staff**: a personal assistant reachable from chat, which helps
  with day-to-day work, runs scheduled checks, messages the owner
  proactively, and orchestrates other agents; and
- **coding and research agents**, with the same web access and memory, which
  take delegated or scheduled tasks and keep each problem's context separate.

## Built-in agents

pons starts with built-in agent configurations. Owners can later create their
own (phase 11), starting from these as presets. Either way the runtime only
sees the resulting definition values.

| Setting | Chief of staff | Coding agent |
| --- | --- | --- |
| Persona | Owner-written personality and instructions | Owner-written coding instructions |
| Tools | File, shell, web, memory | File, shell, web, memory, Git |
| Workspace (ADR-0022) | `agent`: one long-lived computer | `per_conversation`, or `shared(<repo>)` for one persistent workspace per codebase |
| Memory capture (ADR-0019) | `extract` | `explicit` |
| Can delegate | Yes, to allowed agents | No, by default |

The main difference is context isolation, not capability. A coding agent
keeps codebase knowledge in the workspace (for example `AGENTS.md`) and
general preferences in its agent-wide memory, so switching codebases does not
pollute its context. A persistent coding agent is the same preset with a
`shared(<repo>)` workspace.

The runtime serves one trusted administrative domain under ADR-0017: an owner,
household, or small team. It is not a public or multi-tenant bot platform.
Open public ingress, moderation, anonymous-user isolation, and fleet scale are
out of scope.

## Use cases

These scenarios justify the design. Each phase cites the use cases it serves;
a contract no use case needs is a candidate to simplify or defer.

| ID | Use case | Needs |
| --- | --- | --- |
| U1 | **Morning brief.** At 07:00 the assistant summarizes the owner's day to their chat channel, or stays silent when there is nothing worth sending. | schedules, delivery, memory, `no_update` |
| U2 | **Chat from anywhere.** The owner messages the assistant from phone or web; it remembers preferences across both. | bindings, principal linking, memory extraction |
| U3 | **Reminders.** "Remind me Friday to call X." | work items, `wait_until`, detached-work grant |
| U4 | **Watch for something.** "Tell me when PR #12 merges; give up after a week." | work items, `wait_for` with deadline, event ingress |
| U5 | **Hand off coding from a phone.** The assistant delegates to a coding agent in E2B and replies with the result. | delegation, hands eligibility |
| U6 | **Approve from a phone.** A coding agent wants to push; the owner approves in chat hours later. | durable approvals, ADR-0014 |
| U7 | **Research report.** A research agent produces a file which comes back to the owner. | artifacts, outbound attachments |
| U8 | **Household.** Two people share one assistant; each person's memory stays private. | principals, principal-scoped memory |
| U9 | **The agent's own computer.** Over months the assistant keeps notes, scripts, and data in its workspace without Git; they survive VM loss, and the owner can roll back a bad change. | agent workspaces, checkpoints, retention, restore |
| U10 | **Create an agent.** The owner creates a new agent with its own persona from the web UI or CLI, for example a coding agent that reviews a repository every night, without restarting the server. | managed definitions, revisions, schedules |

## Ordering principle

The assistant path comes before delegation. A persistent assistant needs
identity, channels, delivery, schedules, and memory; it does not need
delegation. Delegation is valuable for coding work but carries the strictest
hands-isolation prerequisites (ADR-0016 section 10). Building the assistant
path first delivers a usable product sooner and exercises the lineage,
outcome, and delivery machinery that delegation reuses.

| Phase | Depends on |
| --- | --- |
| 1 Agent identity | — |
| 2 Lineage and outcomes | 1 |
| 3 Principals and delivery | 2 |
| 4 Schedules | 3 |
| 5 Agent workspaces | 1 |
| 6 Memory | 3, 5 |
| 7 Work items | 4 |
| 8 Delegation | 2 |
| 9 Artifacts | 8 |
| 10 Approvals | 2 and an implemented ADR-0014 |
| 11 Custom agents | 1 |

Phase 5 can start as soon as phase 1 lands, and phase 8 after phase 2, so the
assistant and delegation tracks can proceed in parallel.

## Milestones

| Milestone | Phases | Demonstration |
| --- | --- | --- |
| M1 Reachable assistant | 1–3 | The owner messages a non-default agent from a chat channel and receives a durable reply. |
| M2 Proactive assistant | 4–7 | A scheduled morning brief reads the owner's memory files and arrives on the owner's channel, or records `no_update`; the agent's workspace survives VM replacement. |
| M3 Delegating agents | 8–10 | A coordinator delegates a coding task, receives an artifact, and resumes after a durable approval. |
| Later: custom agents | 11 | The owner creates their own agent from a built-in preset without a restart. |

## Phase 1: Agent identity and composition

**Use cases:** all.

**ADRs:** ADR-0016 sections 1–3; ADR-0022 section 1.

- Store agent definitions in SQLite with immutable revisions, seeded from
  configuration on first start. Each definition has an ID, persona, provider
  slot, tools, workspace policy, memory capture policy, and limits; there is
  no agent kind.
- Seed the built-in chief-of-staff and coding-agent definitions, with the
  chief of staff as the default. Configuration may adjust their persona,
  provider slot, and tools.
- Record the revision on submissions and runs; runs keep the revision they
  started with. Fail closed on an unresolvable revision.
- Add a read-only agent list to the API and web UI.
- Persist `agent_id`, `workspace_id`, and parent delegation columns on
  conversations. Carry agent and workspace identity through claims.
- Replace the use of conversation ID as `environment.Spec.WorkspaceID` with
  validated workspace IDs.
- Compose brain, tools, environment, and credential grants per agent in
  `cmd/pons`. `Core` does not change.
- Accept an optional `agent_id` on conversation creation; expose ownership in
  views and the web UI.

**Done when:** both built-in agents serve conversations through the same
`Core` with different tools and personas, a configuration change creates a new
revision without moving in-flight work, and workspace aliases are rejected.

## Phase 2: Submission envelope, lineage, and explicit outcomes

**Use cases:** all; `no_update` for U1.

**ADRs:** ADR-0016 sections 4–6; ADR-0018 section 3.

- Add the canonical source envelope (ADR-0016 section 4) to submissions.
  Native HTTP ingress sets `source.kind = human`, `adapter_id = native`.
- Add durable lineage rows keyed by `root_id`, with `active` compare-and-set
  terminal transitions and outcome kind.
- Add the staged terminal decision mechanism (ADR-0016 section 6) and the
  first decision, `complete_no_update`, for system roots.
- Fail a lineage whose run stops without an explicit outcome with
  `no_final_response`.
- Make `pons client -message` follow a lineage to its terminal status.
- Add lineage status to snapshots and a `lineage.updated` event.

**Done when:** an empty or exhausted run cannot complete a lineage, a system
root can record `no_update`, and a client reconnect resumes following the same
lineage.

## Phase 3: Principals, bindings, and delivery

**Use cases:** U1, U2, U8.

**ADRs:** ADR-0018 sections 1, 4, 5, and 6.

- Add the principal registry: administrator-declared principals with linked
  channel accounts (ADR-0018 section 6).
- Add connector bindings from `(adapter_id, external_account_id,
  external_thread_id)` to an agent and conversation, with an optional delivery
  target.
- Add outbound intents written in the lineage terminal transaction, a delivery
  worker outside agent worker slots, and the retry/unknown state machine.
- Ship a scripted connector and delivery adapter for deterministic tests.
- Ship one real chat connector behind an authenticated adapter. The native
  loopback server is not exposed.
- Emit transient presence signals (for example "typing") from run start to
  terminal; they are best-effort and never durable.

**Done when:** a scripted connector message from a linked account reaches the
bound agent, its reply produces one outbound intent across retries and
restarts, and a revoked binding suppresses pending sends.

## Phase 4: Schedules

**Use cases:** U1.

**ADRs:** ADR-0018 section 2.

- Add administrator-owned schedule definitions with time zone, recurrence,
  target conversation policy, optional principal, and delivery target.
- Persist `next_fire_at` and accepted slots; accept one slot atomically with
  its submission; catch up at most one missed slot.
- Add management operations to list, enable, and disable schedules.

**Done when:** duplicate ticks create one submission, restart catches up one
slot across a DST change, and a scheduled run's response reaches the
schedule's delivery target.

## Phase 5: Agent workspaces and checkpoints

**Use cases:** U9; also U1 and U5 wherever agents keep files.

**ADRs:** ADR-0022.

- Resolve workspace policy per agent: `per_conversation`, `agent`, or
  `shared(<id>)`. (Policy resolution itself lands in phase 1.)
- Add `empty` and `template` seeds alongside the existing source and Git
  seeds.
- Add setup scripts with a setup generation, and checkpoint exclusions for
  rebuildable paths.
- Add checkpoint history, retention policies, and restore through the
  management surface.
- Add optional Seatbelt checkpoints.
- Add an S3-compatible `CheckpointStore`.

**Done when:** an `agent` workspace persists across conversations and VM
replacement without Git, retention keeps the configured checkpoints, and a
restore creates a new checkpoint without rewriting history.

## Phase 6: Memory

**Use cases:** U1, U2, U8.

**ADRs:** ADR-0019.

- Add the per-agent memory store: an agent-wide tree and one tree per
  principal, each with a `MEMORY.md` index. No memory is shared between
  agents.
- Materialize each run's entitled scopes into its hands environment, with
  agent-wide memory read-only unless the principal is an agent-wide writer.
- Commit a run's changes per scope at the end of the run, with validation,
  limits, and conflict detection; record revisions and changed paths.
- Hydrate the mounted `MEMORY.md` indexes as attributed data within a byte
  budget.
- Add the `extract` capture policy with optional review.
- Add management read, edit, and restore.

**Done when:** the assistant recalls a principal's memory across channels and
conversations, another principal's run and another agent never see it, and a
conflicting commit is retained rather than overwriting.

## Phase 7: Work items and waits

**Use cases:** U3, U4.

**ADRs:** ADR-0021 sections 1 and 3.

- Add work items with owner, task, summary, delivery target, wake conditions,
  and one-active-lineage exclusion.
- Allow schedules and bindings to target work items.
- Add the `decide_work` staged decision, timer waits, configured event waits,
  and deadline-expiry wakes.
- Add management create, inspect, and cancel operations.

**Done when:** one work item spans several lineages without a resident worker,
a deadline expiry wakes it once, and a cancelled item cannot be reopened by a
late wake.

## Phase 8: Delegation

**Use cases:** U5.

**ADRs:** ADR-0016 sections 7–12.

- Add the composition eligibility check for delegation (ADR-0016 section 10)
  and reject ineligible hands at startup.
- Add the host `delegate(agent_id, message)` tool for root conversations
  only, with private child conversations, directed allowlists, and the
  recipient catalog.
- Add the one-run delegation lifecycle, transactional result routing,
  cancellation, and per-lineage limits.
- Add delegation and lineage cancellation routes, and show a conversation's
  delegations in the web UI.

**Done when:** ADR-0016's verification list passes, including the black-box
two-agent scenario across restart.

## Phase 9: Artifacts

**Use cases:** U7.

**ADRs:** ADR-0020.

- Add a local content-addressed blob store with SQLite metadata.
- Add provider export and read-only materialization for Seatbelt and E2B.
- Extend `delegate` and result routing with artifact grants.
- Allow outbound intents to carry artifact references where the adapter can
  enforce size and recipient policy.

**Done when:** a research agent's published file reaches the owner through
a delegation result without the owner's agent gaining access to any other
artifact, and a changed source file never alters a published artifact.

Write ADR-0020's detailed contract before starting this phase.

## Phase 10: Durable approvals

**Use cases:** U6.

**ADRs:** ADR-0021 section 2; requires ADR-0014 to be accepted and its
preflight authorization stage implemented first.

- Persist pending approvals and paused turns; release worker and workspace.
- Resume the exact persisted action after revalidation.
- Add an approval surface reachable from the owner's channel through phase 3's
  delivery and ingress, bound to one approval ID.

Write ADR-0021 section 2's detailed contract before starting this phase.

**Done when:** a paused approval survives restart, resumes exactly one action,
and an expired approval yields one denied result.

## Phase 11: Custom agents

**Use cases:** U10.

**ADRs:** ADR-0016 section 2.

- Add administrator create, edit, disable, and list through the API, CLI,
  and web UI, starting from a built-in agent as a preset.
- Validate that a definition only references existing provider and
  credential slots, allowed tools, and allowed recipients.

**Done when:** an owner-created agent serves a conversation and a schedule
without a restart, and an edit leaves in-flight work on its original
revision.

## Cross-cutting requirements

- Each phase bumps the SQLite schema version as needed. Old databases may
  require recreation under the pre-compatibility policy.
- New host capabilities are host-side tools registered through `Core`; none
  run inside hands.
- Each phase's tests are deterministic and need no provider credentials or
  network. Real connectors keep credential-dependent tests outside the
  default suite.
- Update `README.md` when commands, flags, or configuration change, and set
  each ADR's implementation line as phases land.

## Out of scope for v1

- Public or multi-tenant ingress, moderation, and per-user abuse controls.
- Group conversations with several humans in one thread; principal-scoped
  memory assumes one verified human per conversation.
- Automatic cross-channel identity inference; linking is
  administrator-declared.
- Multimodal message parts; artifacts cover files.
- Memory shared between agents.
- Streaming partial responses to external channels.
- Agents created by models, and distributed workers.

## Open questions

- Which chat connector is first?
- Should the web UI grow management views for schedules, memory, and work
  items, or should a CLI cover them first?
- Does the assistant need a small built-in delivery target for native clients
  (for example, the web UI as a notification inbox) before a chat connector
  exists?
