# Persistent agents implementation plan

**Status:** Proposed; phases 1–3 implemented
**Related:** [design doc](design.md),
[ADR-0016](../../adr/adr-0016-persistent-agents-and-async-messaging.md),
[ADR-0017](../../adr/adr-0017-one-runtime-one-trust-domain.md),
[ADR-0018](../../adr/adr-0018-agent-triggers-and-delivery.md),
[ADR-0019](../../adr/adr-0019-agent-memory.md),
[ADR-0020](../../adr/adr-0020-agent-artifact-transfer.md),
[ADR-0021](../../adr/adr-0021-durable-work-and-waits.md),
[ADR-0022](../../adr/adr-0022-persistent-agent-workspaces.md), and
[ADR-0014](../../adr/adr-0014-native-action-policy.md)

This document turns the design into ordered implementation steps. The
[design doc](design.md) owns goals, use cases (U1–U12), and
principles; the ADRs own contracts; this plan owns sequencing, scope, and
exit criteria. When they disagree, fix the ADR first.

Each phase lists concrete steps against today's code, the tests that prove
it, and a demonstrable exit. Phases 1–8 are specified in detail. Phases 9–12
are outlined and are refined when they become next.

## Ordering

The defining capability, an agent that maintains its own identity and memory
through conversation, comes first and in its simplest form. Reach, time, and
tasks follow. Anything not needed yet moves to the phase that first needs it.

| Phase | Depends on | Size |
| --- | --- | --- |
| 1 Name the agent | — | Small |
| 2 Memory and persona v0 | 1 | Medium |
| 3 Run notices and stop (done) | 1 | Small |
| 4 Principals, a chat connector, and delivery | 2, 3 | Large |
| 5 Schedules and budgets | 4 | Medium |
| 6 Agent workspaces and checkpoints | 1 | Medium |
| 7 Memory v1 | 2, 4, 6 | Large |
| 8 Work items and routines | 5 | Medium |
| 9 Tasks | 3, 6 | Large |
| 10 Artifacts | 9 | Medium |
| 11 Durable approvals | 4 and an implemented ADR-0014 | Medium |
| 12 Additional agents | 1 | Medium |

Phase 6 can proceed at any time, alongside phases 4 and 5.

## Milestones

| Milestone | Phases | Demonstration |
| --- | --- | --- |
| M1 An agent that knows you | 1–2 | On a fresh server, the owner-named agent asks what the owner wants help with, and in a new conversation the next day it remembers the owner's preferences. |
| M2 Reachable and proactive | 3–5 | The owner chats with their agent on Telegram, and a morning brief arrives there every day, across restarts. |
| M3 Long-lived | 6–8 | The agent's files survive sandbox replacement, household members' memory stays private, and routines set up by chat keep running. |
| M4 Tasks | 9–11 | The agent starts a coding task from a phone, receives its result and files, and resumes after a durable approval. |
| Later | 12 | The owner adds a second agent without a restart. |

## Phase 1: Name the agent

**Use cases:** all (foundation). **ADRs:** ADR-0016 sections 1–3, minimally.

Create the one agent automatically, give it a directory the owner edits,
and record which agent revision every conversation and run used. There is no API for creating agents; the server always has exactly
one.

Steps:

1. **Default agent.** On first start, `pons serve` creates the default agent
   (`id: "default"`) as a directory under the state directory, the one place
   the owner manages it: `agents/default/agent.json` (name, provider slot,
   model, max turns; empty fields use server defaults) and
   `agents/default/PERSONA.md` (free-form instructions, used verbatim).
   Existing files are never overwritten, and a malformed `agent.json` stops
   startup. The name is a structured field, never parsed from the persona.
   Host configuration keeps providers, credentials, and sandboxes and has no
   agent block.
2. **Definition and fingerprint.** Add `AgentDefinition` in `runtime/` with
   the non-secret fields a run depends on: ID, name, persona text, provider
   slot ID, model, max turns, and configured plugin paths. `Revision()`
   returns a SHA-256 of its canonical JSON. Credentials never enter the definition.
3. **Prompt identity.** Split `plugins/brain/llm/system_prompt.txt` into the
   harness rules and an identity section. Today it opens with "You are an
   expert coding agent"; the identity becomes the name and persona, falling
   back to a neutral default when both are empty. Keep every harness rule.
4. **Storage.** At startup, write the resolved definition as a write-once
   snapshot, `agents/default/revisions/<revision>.json`, if it is new.
   Bump `currentSchemaVersion` in `runtime/sqlite/store.go` and add
   `conversations.agent_id` and `agent_revision` on submissions and runs;
   SQLite records only which agent and revision work used, never
   definitions.
5. **Claims and runs.** `Accept` records the current revision on the
   submission. When a run is claimed, the manager resolves that revision's
   snapshot and passes it as a new `RunRequest.Agent`, so a submission
   queued before a persona change still runs under the revision it was
   accepted with. A missing or tampered snapshot fails the run closed.
6. **Visibility.** Include the agent's ID and name in `ConversationView` and
   `GET /v1/options` (read-only); show the name in the CLI renderer and the
   web UI.

Tests:

- The agent directory is created once and never overwritten; malformed
  `agent.json` fails loudly.
- The fingerprint is deterministic and changes with the name and persona
  but not with credentials.
- A submission accepted before a persona change runs under its original
  revision after restart.
- The system prompt uses the persona, or the neutral default, and keeps
  every harness rule.

**Done when:** a fresh server has a default agent, editing its `agent.json`
or `PERSONA.md` and restarting changes how it introduces itself, and every
run records the revision it used.

## Phase 2: Memory and persona v0

**Use cases:** U2, U12. **ADRs:** ADR-0019 (single-principal subset);
ADR-0016 section 2.

Let the agent maintain its own memory and learn how its owner wants it to
work, for one owner. Per-person scopes and a background memory agent come
in phase 7.

Steps:

1. **Memory directory.** Add `agents/<id>/memory/` with `MEMORY.md` and topic
   files, maintained by the agent itself like a small wiki. With one
   principal, the whole tree is agent-wide (ADR-0019 allows unscoped memory
   for a single-principal agent). The owner can read or edit it, but should
   never need to.
2. **Grant.** Grant each run the memory directory itself: a read-write
   Seatbelt grant, or in E2B a copy made at run start whose added, changed,
   or deleted files are applied back when the session closes, so the agent
   uses memory like any other directory. Only the memory directory is granted:
   `PERSONA.md`, `agent.json`, and `revisions/` stay outside every grant.
   Hands always run in a sandbox, so the grant is always enforced: the server
   no longer has in-process hands, and refuses to start without Seatbelt or
   E2B. A local Linux sandbox (such as bubblewrap) is future work.
3. **Prompt.** Add harness guidance for keeping memory: update it when the
   owner states or corrects something worth keeping, one topic per file, one
   line per file in `MEMORY.md`, update rather than duplicate. Hydrate
   `MEMORY.md` as attributed data within a byte budget, never as
   instructions.
4. **No host-side history.** Memory is deliberately plain files with no
   per-run copies, commits, conflict handling, or revision log: users should
   not have to manage memory, and models curate their own memory poorly
   through tools anyway. The agent keeps its file tools so it can apply
   corrections directly; curation moves to a background memory agent in
   phase 7. Concurrent runs may both edit memory; with one agent and one
   active run per conversation, the last write wins.
5. **Owner-written persona.** `PERSONA.md` stays owner-written; the agent is
   never granted it. What the agent learns about how its owner wants it to
   work goes to memory, which it follows as the owner's recorded preferences
   but which cannot override the persona or harness rules.
6. **Onboarding.** When `PERSONA.md` is empty, the prompt tells the agent to
   introduce itself as new, by its name when it has one, ask what the owner
   wants help with, and save what it learns to memory.

Deferred: persona proposals (a `propose_persona` host tool whose exact text
takes effect only when the owner accepts it) need an approval system, which
does not exist yet and will be designed separately. Until then the owner
edits `PERSONA.md` directly, and nothing the agent does changes its standing
instructions.

Tests:

- The agent's memory edits persist across conversations and restarts.
- Under Seatbelt, the agent can write its memory directory but not
  `PERSONA.md`, `agent.json`, or `revisions/`.
- In E2B, the agent sees memory at a sandbox path, and only the files it
  changed are applied back, leaving other runs' changes in place.
- Hydrated `MEMORY.md` is labeled data within its budget.
- An agent with an empty `PERSONA.md` is prompted to introduce itself and
  ask what the owner wants help with; a written persona replaces that
  prompt.

**Done when (M1):** on a fresh server the owner-named agent introduces itself
and asks what the owner wants help with, and the next day, in a new
conversation, it follows the preferences the owner gave it and remembers what
the owner told it.

## Phase 3: Run notices and stop

**Status:** Done in [#17](https://github.com/speza/pons/pull/17).
**ADRs:** ADR-0023.

The chat is the only channel today, so every run must end in something the
owner can see there. A failed run appends its durable `run.failed` event and a
stopped run a new `run.stopped` event; both project a notice into the
conversation ("An error occurred while generating a response. Try again." or
"Stopped. The response was not finished."), with the technical error kept as
detail. `POST /v1/conversations/{id}/stop` and a Stop button cancel the active
run gracefully: still-requested tools are marked interrupted and the stop is
recorded in one transaction, while an answer that finishes first is kept.

This replaced the planned lineages. Grouping several runs into one request,
an explicit silent `no_update` outcome, and staged decisions (ADR-0016
sections 5–6, ADR-0018 section 3) are deferred: with the real LLM brain a run
ends in prose or an error, and the owner reads every outcome in the chat. They
are revisited only if delivery, schedules, or tasks show a concrete need.

## Phase 4: Principals, a chat connector, and delivery

**Use cases:** U2, U8; enables U1. **ADRs:** ADR-0018 sections 1 and 4–6.

Let the owner reach the agent from a chat app, and deliver replies durably.

Steps:

1. **Principals.** Add a `principals` block to configuration: each principal
   has an ID, display name, and linked accounts (`adapter_id`,
   `external_account_id`). Store them in `principals` and
   `principal_accounts` tables at startup. The native client acts as a
   configured owner principal.
2. **Connector contract.** Add a `runtime/connector` package defining an
   adapter: an ID, a `Run(ctx, Ingress)` loop that submits verified events,
   and `Send(ctx, Intent)` for delivery. Ingress resolves the sender to a
   principal (unknown senders are denied), finds or creates the binding for
   `(adapter, account, thread)`, and accepts the message with
   `external_event_id` idempotency.
3. **Bindings.** Add a `bindings` table mapping an external thread to a
   conversation and a delivery target, pinned on each accepted input.
4. **Outbox.** Add `outbound_intents` (run ID, target, status, attempts,
   last error). The transaction that records a run's final answer, error
   notice, or stop notice writes one intent when its input has a target. A delivery worker in
   `Manager`, separate from run slots, sends pending intents with bounded
   backoff and records `unknown` when a send may have happened.
5. **Scripted adapter.** Add an in-memory adapter for deterministic tests.
6. **Telegram adapter.** Implement the first real connector with the Telegram
   Bot API. Long polling (`getUpdates`) means no inbound port is exposed, and
   the update ID gives stable event identity. The bot token lives in the
   credential store. Replies use `sendMessage`; `sendChatAction` provides a
   transient typing indicator while a run is active.
7. **Confirmations in chat.** Deliver approval-system confirmations as Telegram
   inline buttons; only the owner's verified account can accept them.
8. **Conversation windowing.** A Telegram chat is one conversation that never
   ends. Hydrate a bounded window: recent messages in full and a summary of
   older ones, with memory carrying what matters. Stop loading the whole
   history for every run.
9. **Visibility.** Show principal attribution on messages and delivery status
   on runs in the view, CLI, and web UI.

Tests:

- A duplicate update creates one submission; an unknown sender is rejected.
- Two linked accounts resolve to one principal; message text cannot change
  attribution.
- A finished run creates exactly one intent across retries and
  restarts; a delivery failure never reruns the agent.
- Revoking a binding suppresses pending sends.
- Hydration stays within its window for a conversation of any length.
- Telegram tests that need a token stay outside the default suite.

**Done when:** the owner messages their agent on Telegram and receives a
durable reply, including after a server restart mid-run, and, once the
approval system exists, confirms a persona change with a button.

## Phase 5: Schedules and budgets

**Use cases:** U1. **ADRs:** ADR-0018 section 2.

Steps:

1. **Definitions.** Add a `schedules` configuration block: ID, recurrence,
   time zone, target conversation policy (fixed conversation or new per
   firing), optional principal, delivery target, payload text, and enabled
   flag. Use a small recurrence format (daily or weekly at local times) with
   `time.LoadLocation`, rather than adding a cron dependency.
2. **Slots.** Add a `schedule_slots` table. A scheduler loop in `Manager`
   reconciles due slots on a timer, accepting each slot atomically with a
   system submission keyed by `(schedule_id, nominal_fire_at)`.
3. **Catch-up.** After downtime, fire at most the latest missed slot and
   record how many were skipped. Skip nonexistent local times and fire
   repeated ones once.
4. **Outcomes.** A scheduled run's answer or error notice goes to the
   schedule's delivery target through the phase 4 outbox. Whether a check may
   end silently is an open question, not a planned feature.
5. **Budgets.** Add a per-agent daily token budget; runs that would exceed
   it fail with a stable code and notify the owner once.
6. **Management.** Add `pons schedules list|enable|disable`. Configured
   schedules are the power-user path; phase 8 lets the owner set up
   routines by chat.

Tests (with an injected clock):

- Duplicate ticks and restarts fire each slot once.
- A daylight-saving transition fires once, at the right local time.
- A scheduled run's answer or error notice reaches the target.

**Done when (M2):** a daily 07:00 brief arrives on Telegram across restarts,
and stops at the budget.

## Phase 6: Agent workspaces and checkpoints

**Use cases:** U9. **ADRs:** ADR-0022.

Steps:

1. **Workspace identity.** Add a workspace policy to the agent definition.
   The default agent uses `agent`: one workspace for all its conversations.
   Resolve `workspace_id` from policy and replace
   `spec.WorkspaceID = request.ConversationID` in
   `cmd/pons/runtime_mode.go` and the conversation `WorkspaceLock`.
   Per-conversation workspace options move to task profiles in phase 9.
2. **Seeds.** Add `empty` and `template` seeds to `environment` alongside
   `archive/v1` and `git/v1`.
3. **Setup and exclusions.** Add a setup script with a setup generation, and
   checkpoint exclusions for rebuildable paths.
4. **History, retention, restore.** Replace the single `checkpoint_ref` with
   a checkpoint history table; apply the retention policy; add
   `pons workspace checkpoints|restore`.
5. **Seatbelt checkpoints.** Optionally checkpoint the local workspace
   directory after each clean run.
6. **Off-host storage.** Add an S3-compatible `CheckpointStore`. Decide
   between a small SigV4 implementation and a dependency when this step
   starts.

Tests:

- The agent workspace persists across conversations and E2B replacement.
- Excluded paths are not archived; setup reruns on a generation change.
- Retention never prunes a referenced checkpoint; restore creates a new one.

**Done when:** files the agent creates survive a new conversation and a
replaced sandbox, and the owner can restore yesterday's workspace.

## Phase 7: Memory v1

**Use cases:** U2, U8. **ADRs:** ADR-0019.

Extend phase 2's memory to several people, E2B, and long-term upkeep.

Steps:

1. **Principal scopes.** Split the tree into agent-wide files and
   `principals/<id>/` folders, and mount only the scopes a run is entitled
   to. Agent-wide memory is read-only unless the principal is an agent-wide
   writer.
2. **Concurrent writers.** Decide whether several people's runs writing
   memory at once need per-scope copies and commits, which phase 2 left out.
3. **E2B scopes.** Copy only the entitled scopes into the sandbox, using
   phase 2's sync.
4. **Background memory agent.** On idle conversations, a memory-only
   activation extracts what is worth keeping, merges duplicates, prunes
   stale entries, and keeps `MEMORY.md` within its budget, so memory stays
   useful without the owner or the conversational agent curating it.

Tests:

- One principal's memory never reaches another's run; linked accounts
  share it.
- The background memory agent keeps the index within budget without losing referenced
  files.

**Done when:** two household members use the agent on Telegram, each is
remembered, and neither sees the other's memory.

## Phase 8: Work items and routines

**Use cases:** U3, U4. **ADRs:** ADR-0021 sections 1 and 3.

Steps:

1. **Items.** Add `work_items` with owner, principal, delivery target,
   summary, status, wake condition, and limits, plus client creation and
   `pons work list|cancel`.
2. **Agent tools.** Add `create_work` behind a detached-work grant and
   `decide_work` (`complete`, `wait_until`, `wait_for`), recorded when the
   run finishes. Revisit ADR-0021's staged-decision design before building
   this phase, since it depends on the deferred lineages.
3. **Wakes.** Timer wakes through the phase 5 scheduler, configured event
   keys through phase 4 ingress, and one `deadline_expired` wake when a
   `wait_for` deadline passes.
4. **Exclusion.** At most one active run per item; bounded pending wakes.
5. **Routines by chat.** "Every morning at 7, send me a brief" becomes a
   recurring work item the agent proposes through an approval-system confirmation,
   running on the phase 5 engine.

Tests:

- A reminder fires once at its time and survives restarts.
- A matching event wins over a deadline, and the deadline wake is suppressed.
- A cancelled item cannot be reopened by a late wake.

**Done when (M3):** "remind me Friday at 10:00 to call Alex" arrives on
Telegram on Friday at 10:00, and a morning brief set up by chat keeps
running.

## Phase 9: Tasks (outline)

**Use cases:** U5; U7 and U11 build on it. **ADRs:** ADR-0016 sections 7–12.

- Check at startup that the configured hands are eligible for tasks, and
  refuse to enable tasks otherwise.
- Add task profiles and named workspaces (such as configured repositories)
  to the agent definition, with built-in `general` and `coding` profiles.
- Add the `start_task` host tool, private child conversations, the one-run
  lifecycle, result routing, cancellation, and per-conversation limits.
  Decide then whether a request needs to group the parent's runs with its
  tasks (ADR-0016 section 5).
- Mount memory read-only into tasks as their profile allows.
- Show tasks in the conversation view and web UI.

**Done when:** a coding task started from Telegram runs in a configured
repository, reads but cannot change the owner's memory, and returns its
result across a restart.

## Phase 10: Artifacts (outline)

**Use cases:** U7. **ADRs:** ADR-0020, whose detailed contract is written
first.

- Add a content-addressed blob store and publish/materialize host tools.
- Extend `start_task` and task results with artifact grants.
- Attach artifacts to Telegram delivery within size limits.

**Done when:** a research task's file reaches the owner on Telegram, and a
changed source file never alters it.

## Phase 11: Durable approvals (outline)

**Use cases:** U6, U11. **ADRs:** ADR-0021 section 2, written in detail
first; requires ADR-0014's preflight authorization stage.

- Persist pending approvals and paused turns, releasing workers and
  workspaces.
- Extend the approval system's confirmations to exact tool calls, delivered to
  Telegram.
- Resume the exact persisted action after revalidation.

**Done when:** a coding task's push waits for approval on Telegram, survives
a restart, and runs exactly once after approval.

## Phase 12: Additional agents (outline)

**Use cases:** U10. **ADRs:** ADR-0016 section 2.

- Each agent is another directory under `agents/`, managed as files.
- Add `pons agents list|create|edit|disable` and a web UI view.
- Compose the brain, tools, and environment per agent, and route
  conversations and bindings to a chosen agent.

**Done when:** the owner creates a second agent for a household member,
without a restart, reachable from that person's Telegram account.

## Cross-cutting requirements

- Each phase bumps the SQLite schema version as needed. Old databases may
  require recreation under the pre-compatibility policy.
- New host capabilities are host-side tools registered through `Core`; none
  run inside hands.
- Each phase's tests are deterministic and need no provider credentials or
  network. Real connectors keep credential-dependent tests outside the
  default suite.
- Conversational by default, confirmed for authority: anything that changes
  the agent's identity or permissions is proposed in chat and takes effect
  only through a confirmation. Credentials, connectors, and integrations are
  configured outside chat.
- Configuration seeds; it does not replace conversation. Each phase starts
  with settings in `.pons.json` where needed and adds CLI management only
  where the owner needs to change things while the server runs.
- Update `README.md` when commands, flags, or configuration change, and set
  each ADR's implementation line as phases land.

## Out of scope for v1

- Public or multi-tenant ingress, moderation, and per-user abuse controls.
- Group conversations with several humans in one thread.
- Automatic cross-channel identity inference; linking is owner-declared.
- Multimodal message parts; artifacts cover files.
- Memory shared between agents.
- Streaming partial responses to external channels.
- Agents created by models, and distributed workers.

## Open questions

- Telegram is proposed as the first connector; confirm before phase 4.
- Should the web UI grow management views for schedules, memory, and work
  items, or should the CLI cover them first?
- Integrations (connecting apps such as email with scoped access) are needed
  for U11 and richer briefs, and need an ADR before those phases.
