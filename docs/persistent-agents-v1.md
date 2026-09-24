# Persistent agents v1 implementation plan

**Status:** Proposed; no phase implemented
**Related:** [design doc](persistent-agents-design.md),
[ADR-0016](adr/adr-0016-persistent-agents-and-async-messaging.md),
[ADR-0017](adr/adr-0017-one-runtime-one-trust-domain.md),
[ADR-0018](adr/adr-0018-agent-triggers-and-delivery.md),
[ADR-0019](adr/adr-0019-agent-memory.md),
[ADR-0020](adr/adr-0020-agent-artifact-transfer.md),
[ADR-0021](adr/adr-0021-durable-work-and-waits.md),
[ADR-0022](adr/adr-0022-persistent-agent-workspaces.md), and
[ADR-0014](adr/adr-0014-native-action-authorization.md)

This document turns the design into ordered implementation steps. The
[design doc](persistent-agents-design.md) owns goals, use cases (U1–U11), and
principles; the ADRs own contracts; this plan owns sequencing, scope, and
exit criteria. When they disagree, fix the ADR first.

Each phase lists concrete steps against today's code, the tests that prove
it, and a demonstrable exit. Phases 1–7 are specified in detail. Phases 8–11
are outlined and are refined when they become next.

## Ordering

Each phase adds only what its use cases need. Anything not needed yet moves
to the phase that first needs it: managed agent editing waits for additional
agents, and workspace identity waits for persistent workspaces.

| Phase | Depends on | Size |
| --- | --- | --- |
| 1 Name the agent | — | Small |
| 2 Lineages and outcomes | 1 | Medium |
| 3 Principals, a chat connector, and delivery | 2 | Large |
| 4 Schedules | 3 | Medium |
| 5 Agent workspaces and checkpoints | 1 | Medium |
| 6 Memory | 3, 5 | Large |
| 7 Work items | 4 | Medium |
| 8 Tasks | 2, 5 | Large |
| 9 Artifacts | 8 | Medium |
| 10 Durable approvals | 2 and an implemented ADR-0014 | Large |
| 11 Additional agents | 1 | Medium |

Phase 5 can run alongside phases 2–4, and phase 11 at any time after phase 1.

## Milestones

| Milestone | Phases | Demonstration |
| --- | --- | --- |
| M1 Reachable agent | 1–3 | The owner messages their named agent from Telegram and gets a durable reply, across a server restart. |
| M2 Proactive agent | 4–7 | A morning brief arrives or explicitly does not, reminders fire, the agent remembers the owner, and its files survive sandbox replacement. |
| M3 Tasks | 8–10 | The agent starts a coding task from a phone, receives its result and files, and resumes after a durable approval. |
| Later | 11 | The owner adds a second agent without a restart. |

## Phase 1: Name the agent

**Use cases:** all (foundation). **ADRs:** ADR-0016 sections 1–3, minimally.

Give the one agent a name and persona, and record which agent and revision
every conversation and run used. No new behavior beyond the persona.

Steps:

1. **Configuration.** Add an `agent` block to `settings` in
   `cmd/pons/config.go` with `id`, `name`, and `instructions`. Validate that
   `id` is non-empty and safe in URLs and logs. Default to `id: "default"`
   and `name: "pons"`.
2. **Definition and fingerprint.** Add `AgentDefinition` in `runtime/` with
   the non-secret fields a run depends on: ID, name, instructions, provider
   slot ID, model, max turns, and configured plugin paths. `Revision()`
   returns a SHA-256 of its canonical JSON. Credentials and API keys never
   enter the definition.
3. **Prompt identity.** Split `plugins/brain/llm/system_prompt.txt` into the
   harness rules and an identity line. Today it opens with "You are an
   expert coding agent"; the identity becomes "You are {name}" followed by
   the persona, keeping the harness rules unchanged. Wire the persona through
   the existing `llm.Config.SystemExtra` or a dedicated field.
4. **Storage.** Bump `currentSchemaVersion` in `runtime/sqlite/store.go`. Add
   an `agent_revisions` table (agent ID, revision, definition JSON, created
   time), `conversations.agent_id`, and `agent_revision` on submissions and
   runs. At startup, `pons serve` records the current revision if it is new.
5. **Claims and runs.** `Accept` records the current revision on the
   submission. `ClaimRunnable` loads that revision's definition into
   `ClaimedRun`, and `RunRequest` gains an `Agent` field, so a submission
   queued before a restart still runs under the revision it was accepted
   with. An unknown revision fails the run closed.
6. **Visibility.** Include the agent's ID and name in `ConversationView` and
   `GET /v1/options`; show the name in the CLI renderer and the web UI.

Tests:

- The fingerprint is deterministic and changes with instructions but not
  with credentials.
- A submission accepted before a configuration change runs under its
  original revision after restart.
- The system prompt uses the configured name and persona and keeps every
  harness rule.

**Done when:** the owner sets a name and persona in `.pons.json`, chats with
it through the CLI and web UI, and every run records the revision it used.

## Phase 2: Lineages and explicit outcomes

**Use cases:** all; U1 needs `no_update`. **ADRs:** ADR-0016 sections 4–6;
ADR-0018 section 3.

Make every accepted message a lineage with an explicit ending, and add the
staged-decision mechanism later phases reuse.

Steps:

1. **Envelope.** Add a `Source` struct (`Kind`, `AdapterID`,
   `ExternalEventID`, `PrincipalID`) plus `RootID` and `CausationID` to
   `InboundMessage` and `Submission` in `runtime/types.go`. The native HTTP
   path sets `human`/`native`. Store the fields on `submissions`.
2. **Lineage rows.** Add a `lineages` table (root ID, root conversation,
   status, outcome kind, latest final message ID, error, timestamps). `Accept`
   creates one per external submission in the same transaction.
3. **Terminal transition.** After `FinishRun` and `FailRun`, end the lineage
   with a compare-and-set from `active` when nothing in it remains queued or
   running: `completed` with a final response or `no_update`, otherwise
   `failed` with `no_final_response` or the run error. An empty answer is no
   longer a success.
4. **Staged decisions.** Add a `run_decisions` table (run ID, type, revision,
   payload) and a store operation that commits a lineage's latest successful
   root run's decisions inside the terminal transaction.
5. **Host tools.** Add a small host-tool plugin, registered through `Core`
   in the runner, that receives a run-scoped callback instead of store
   access. Its first tool is `complete_no_update`, offered only when the
   submission's source kind is `system`.
6. **Events and API.** Emit `lineage.updated`; include lineage status in
   `ConversationView`; add `GET /v1/conversations/{id}/lineages/{root_id}`
   and `POST .../cancel`.
7. **Client.** `pons client -message` follows its lineage to a terminal
   status instead of the first final message, and resumes after reconnect.
8. **System submissions.** Add an internal `Manager.SubmitSystem` entry point
   used by tests now and by schedules in phase 4.

Tests:

- An empty or turn-exhausted run fails its lineage with a stable code.
- A system submission can complete with `no_update`; a human one cannot.
- A decision staged by a failed or superseded run never commits.
- Cancelling a lineage fences a late run completion.
- The client exits on lineage completion and resumes after reconnect.

**Done when:** every conversation shows its lineages with explicit outcomes,
and an internal system submission can end silently with `no_update`.

## Phase 3: Principals, a chat connector, and delivery

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
   conversation and a delivery target, pinned on each lineage at acceptance.
4. **Outbox.** Add `outbound_intents` (root ID, target, status, attempts,
   last error). The lineage terminal transaction writes one intent when the
   lineage has a target and a deliverable result. A delivery worker in
   `Manager`, separate from run slots, sends pending intents with bounded
   backoff and records `unknown` when a send may have happened.
5. **Scripted adapter.** Add an in-memory adapter for deterministic tests.
6. **Telegram adapter.** Implement the first real connector with the Telegram
   Bot API. Long polling (`getUpdates`) means no inbound port is exposed, and
   the update ID gives stable event identity. The bot token lives in the
   credential store. Replies use `sendMessage`; `sendChatAction` provides a
   transient typing indicator while a run is active.
7. **Visibility.** Show principal attribution on messages and delivery status
   on lineages in the view, CLI, and web UI.

Tests:

- A duplicate update creates one submission; an unknown sender is rejected.
- Two linked accounts resolve to one principal; message text cannot change
  attribution.
- A completed lineage creates exactly one intent across retries and
  restarts; a delivery failure never reruns the agent.
- Revoking a binding suppresses pending sends.
- Telegram tests that need a token stay outside the default suite.

**Done when (M1):** the owner messages their agent on Telegram and receives a
durable reply, including after a server restart mid-run.

## Phase 4: Schedules

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
4. **Outcomes.** Scheduled roots get `complete_no_update`; results go to the
   schedule's delivery target through the phase 3 outbox.
5. **Management.** Add `pons schedules list|enable|disable`.

Tests (with an injected clock):

- Duplicate ticks and restarts fire each slot once.
- A daylight-saving transition fires once, at the right local time.
- `no_update` sends nothing; a response reaches the target.

**Done when:** a daily 07:00 brief arrives on Telegram, or records
`no_update`, across restarts.

## Phase 5: Agent workspaces and checkpoints

**Use cases:** U9. **ADRs:** ADR-0022.

Steps:

1. **Workspace identity.** Add a workspace policy to the agent definition.
   The default agent uses `agent`: one workspace for all its conversations.
   Resolve `workspace_id` from policy and replace
   `spec.WorkspaceID = request.ConversationID` in
   `cmd/pons/runtime_mode.go` and the conversation `WorkspaceLock`.
   Per-conversation workspace options move to task profiles in phase 8.
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

## Phase 6: Memory

**Use cases:** U1, U2, U8. **ADRs:** ADR-0019.

Steps:

1. **Store.** Add a memory store under the state directory, one tree per
   agent with agent-wide files and `principals/<id>/` folders. Scope
   revisions use the checkpoint store in a `memory` namespace; SQLite
   records revisions, runs, and changed paths.
2. **Materialize.** Before each run, copy the entitled scopes into the run's
   hands environment at fixed paths: a Seatbelt grant of a per-run copy, or
   an E2B upload. Agent-wide memory is read-only unless the principal is an
   agent-wide writer.
3. **Commit.** After a clean run, diff each writable scope against its base,
   validate files and limits, detect conflicts, and commit a new revision.
4. **Hydrate.** Add each mounted `MEMORY.md` to the provider request as
   attributed data, within a byte budget, never in the system prompt.
5. **Extraction.** Add the `extract` capture policy: one memory-only
   activation after each completed lineage, with optional review.
6. **Management.** Add `pons memory list|show|edit|restore`.

Tests:

- One principal's memory never reaches another's run; linked accounts
  share it.
- Concurrent changes to one file produce one commit and one retained
  conflict.
- A cancelled run commits nothing; a failed clean run keeps its changes.
- Hydrated memory is labeled data within its budget.

**Done when:** the agent recalls a preference on Telegram that was set in
the web UI, and the owner can read and roll back what it saved.

## Phase 7: Work items

**Use cases:** U3, U4. **ADRs:** ADR-0021 sections 1 and 3.

Steps:

1. **Items.** Add `work_items` with owner, principal, delivery target,
   summary, status, wake condition, and limits, plus client creation and
   `pons work list|cancel`.
2. **Agent tools.** Add `create_work` behind a detached-work grant and
   `decide_work` as a staged decision (`complete`, `wait_until`, `wait_for`).
3. **Wakes.** Timer wakes through the phase 4 scheduler, configured event
   keys through phase 3 ingress, and one `deadline_expired` wake when a
   `wait_for` deadline passes.
4. **Exclusion.** At most one active lineage per item; bounded pending wakes.

Tests:

- A reminder fires once at its time and survives restarts.
- A matching event wins over a deadline, and the deadline wake is suppressed.
- A cancelled item cannot be reopened by a late wake.

**Done when (M2):** "remind me Friday at 10:00 to call Alex" arrives on
Telegram on Friday at 10:00.

## Phase 8: Tasks (outline)

**Use cases:** U5; U7 and U11 build on it. **ADRs:** ADR-0016 sections 7–12.

- Check at startup that the configured hands are eligible for tasks, and
  refuse to enable tasks otherwise.
- Add task profiles and named workspaces (such as configured repositories)
  to the agent definition, with built-in `general` and `coding` profiles.
- Add the `start_task` host tool, private child conversations, the one-run
  lifecycle, result routing, cancellation, and per-lineage limits.
- Mount memory read-only into tasks as their profile allows.
- Show tasks in the conversation view and web UI.

**Done when:** a coding task started from Telegram runs in a configured
repository, reads but cannot change the owner's memory, and returns its
result across a restart.

## Phase 9: Artifacts (outline)

**Use cases:** U7. **ADRs:** ADR-0020, whose detailed contract is written
first.

- Add a content-addressed blob store and publish/materialize host tools.
- Extend `start_task` and task results with artifact grants.
- Attach artifacts to Telegram delivery within size limits.

**Done when:** a research task's file reaches the owner on Telegram, and a
changed source file never alters it.

## Phase 10: Durable approvals (outline)

**Use cases:** U6, U11. **ADRs:** ADR-0021 section 2, written in detail
first; requires ADR-0014's preflight authorization stage.

- Persist pending approvals and paused turns, releasing workers and
  workspaces.
- Deliver approval requests to Telegram and accept the owner's decision.
- Resume the exact persisted action after revalidation.

**Done when:** a coding task's push waits for approval on Telegram, survives
a restart, and runs exactly once after approval.

## Phase 11: Additional agents (outline)

**Use cases:** U10. **ADRs:** ADR-0016 section 2.

- Store agent definitions as managed resources, seeded from configuration.
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
- Configuration comes before management APIs: each phase starts with
  settings in `.pons.json` and adds CLI management only where the owner needs
  to change things while the server runs.
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

- Telegram is proposed as the first connector; confirm before phase 3.
- Should the web UI grow management views for schedules, memory, and work
  items, or should the CLI cover them first?
- Integrations (connecting apps such as email with scoped access) are needed
  for U11 and richer briefs, and need an ADR before those phases.
