# Persistent agents design

**Status:** Draft for review
**Date:** 2026-09-24
**Related:** [implementation plan](plan.md),
[ADR-0016](../../adr/adr-0016-persistent-agents-and-async-messaging.md) through
[ADR-0022](../../adr/adr-0022-persistent-agent-workspaces.md)

This document says what the persistent-agents feature is, who it is for, and
why it has its shape. The [implementation plan](plan.md) says
how and in what order it is built. The ADRs hold the detailed contracts.

## Summary

pons becomes a self-hosted home for a long-lived personal agent. The owner
runs one server and gets an agent they name and shape through conversation:
one they can message from anywhere, which remembers them, keeps its own
files, runs routine checks, reaches out when something matters, and starts
private tasks for focused work such as coding or research. The agent persists
as identity, memory, and workspace, not as an always-running process.

## Problem

Today pons answers one conversation at a time. Between conversations it keeps
only transcripts:

- it does nothing unless someone types into a local client;
- it cannot reach the owner through the apps they already use;
- it forgets the owner between conversations unless they paste context back
  in;
- its files live and die with a conversation or a disposable sandbox; and
- it cannot split off focused work, such as a coding task in another
  repository, without polluting the conversation it came from.

Hosted assistants such as Grok's bot and Meta's Muse point at the experience
people want: always available, proactive, and personal. They also mean giving a
third party your context, credentials, and files, and accepting its choice of
models and tools. pons already has the pieces for a self-hosted version:
durable conversations, a scheduler, sandboxed hands, and checkpointed
workspaces. What is missing is agent identity, ways in and out, memory, and
time.

## Goals

1. **An assistant that is always there.** The owner reaches it from chat,
   web, or CLI, and gets durable replies even when a client disconnects.
2. **Proactive, not only reactive.** Agents act on schedules and external
   events, and stay quiet when there is nothing worth saying.
3. **Remembers, inspectably.** Agents keep memory the owner can read, edit,
   and roll back, and one person's memory never reaches another person.
4. **Keeps its work.** Each agent has files that survive restarts and
   sandbox loss without requiring Git.
5. **Focused work without pollution.** The agent starts private tasks with a
   clean context, their own workspace, and only the tools they need.
6. **The owner decides what it is.** One agent the owner names and shapes
   through conversation, not a set of predefined roles. More agents are
   possible, but never required.
7. **Cheap when idle and honest when things fail.** Idle agents cost nothing,
   and no success, send, or silence is ever inferred by accident.

## Non-goals

- A public or multi-tenant bot platform. One runtime serves one trusted
  domain (ADR-0017); hosted offerings would run isolated cells.
- Autonomous goal pursuit, workflow engines, or agents that create agents.
- Group conversations with several humans in one thread (for now).
- Shared memory between agents.
- Exactly-once external side effects; pons records what it cannot know.
- Replacing the chat apps the owner already uses.
- Setting up credentials, connectors, or integrations through chat.

## Who it is for

The owner of a pons server: a developer or technical user who self-hosts,
possibly shared with a household or small team they trust. They configure
agents, connect channels, and approve risky actions. Everyone else who talks
to an agent is someone the owner has explicitly linked.

## What the owner experiences

The owner names their agent, say "Ada". At 07:00 Ada sends a brief to the
owner's chat app, or sends nothing if the day is clear. Mid-morning the owner
asks Ada from their phone to remind them about a call on Friday; on Friday it
does. Later they ask it to fix a flaky test in a repository. Ada starts a
coding task: a fresh context in a sandbox with that repository, which knows
the owner's preferences but not the morning's chat. The task reports back,
Ada replies with the outcome, and it asks before pushing. Over weeks Ada
learns the owner's preferences and keeps notes and scripts in its own
workspace; the owner can open, correct, or roll back any of it.

## Use cases

These scenarios justify the design. The plan cites them per phase; a contract
no use case needs is a candidate to simplify or defer.

| ID | Use case |
| --- | --- |
| U1 | **Morning brief.** At 07:00 the agent summarizes the owner's day to their chat channel, or stays silent when there is nothing worth sending. |
| U2 | **Chat from anywhere.** The owner messages the agent from phone or web; it remembers preferences across both. |
| U3 | **Reminders.** "Remind me Friday to call X." |
| U4 | **Watch for something.** "Tell me when PR #12 merges; give up after a week." |
| U5 | **Coding from a phone.** The agent starts a coding task in a sandbox with the chosen repository and replies with the result. |
| U6 | **Approve from a phone.** A coding task wants to push; the owner approves in chat hours later. |
| U7 | **Research report.** A research task produces a file which comes back to the owner. |
| U8 | **Household.** Two people share one agent; each person's memory stays private. |
| U9 | **The agent's own computer.** Over months the agent keeps notes, scripts, and data in its workspace without Git; they survive sandbox loss, and the owner can roll back a bad change. |
| U10 | **A second agent.** The owner creates another agent without a restart, for example for a household member who wants their own. |
| U11 | **Inbox triage.** The agent sorts new email in a low-privilege task and drafts replies; nothing is sent until the owner approves. |
| U12 | **Onboarding by chat.** A new agent, already named by the owner, asks what the owner wants help with; its role and routines take effect once the owner confirms them. |

## Your agent and its tasks

The owner gets one agent and names it in the agent's settings file; the
agent never chooses or changes its own name. It has no predefined role: its
persona, preferences, and routines come from the owner's instructions and
its memory. It keeps one long-lived context, workspace, and memory, which is
what makes it useful day to day.

Work that would pollute that context runs as a **task**: a private
conversation that starts with only the request, runs once, and reports back.
Each task runs under a **task profile** that the owner configures and the
agent chooses from:

| Profile | Tools | Workspace | Memory | Typical use |
| --- | --- | --- | --- | --- |
| `general` (built in) | File, shell, web | Fresh per task | Owner's preferences, read-only | Research, one-off investigations (U7) |
| `coding` (built in) | File, shell, web, Git | A configured repository, fresh or persistent per repository | Owner's preferences, read-only | Coding tasks (U5, U6) |
| Owner-defined, for example `inbox` | Mail read and draft only | Fresh per task | None | Reading untrusted content with few privileges (U11) |

A task cannot start further tasks. The agent picks a profile and, where
allowed, one of the profile's named workspaces; it can never choose an
arbitrary path or grant itself tools. A persistent coding context is a
`coding` task on a per-repository workspace.

Additional agents are possible, for example a separate agent for each
household member, and a profile may run its tasks as another agent. The
default is still one agent: something becomes its own agent only when it
needs a separate identity, persona, or memory that the owner talks to
directly.

## Prior art

Two hosted products launched in 2026 take the same broad shape. The
summaries below reflect their launch announcements as of September 2026.

- [Grok Bot](https://x.ai/news/designing-grok-bot) (xAI) gives each account
  many named bots. Memory and routines belong to each bot; tools and skills
  live at the account level. Bots run on cloud computers, trigger from
  schedules and events, save demonstrated workflows as rerunnable skills, and
  can message each other, share group chats, and hand off tasks.
- [Muse](https://about.fb.com/news/2026/09/introducing-muse-personal-ai-agent/)
  (Meta) is one agent per person on a dedicated VM holding the agent and its
  data. A separate Sentinel agent approves every outbound action. Muse works
  after the app closes, asks before sensitive actions, keeps an audit trail,
  and lets people grant each app read or send access separately.

pons adopts the persistent identity, the agent's own computer, per-agent
memory, schedules and events, background work, and approvals. It differs by
being self-hosted with owner-chosen models, by pairing one agent (like Muse)
with private tasks instead of many peer bots, and by keeping household
members' memory apart. The open questions below cover the ideas
still to adopt: account-level integrations, skills, and checks on every
outbound action.

## Design principles

These explain the decisions below and should settle future disputes.

1. **Agents are configuration.** One execution loop serves every agent. New
   behavior comes from settings and tools, not new agent types.
2. **Persistent identity, finite runs.** An agent persists as data. Every
   activation is a bounded run that starts, finishes, and releases its
   resources, so idle agents are free and restarts are safe.
3. **Durable before acting.** Work is recorded before it runs, and replies
   are recorded before they are sent. A crash never silently loses accepted
   work or sends a message twice without saying so.
4. **Honest outcomes.** Every run ends in an answer, an error notice, or a
   stop notice recorded in the log, never a silent guess.
5. **Private by default.** Tasks get only the request, memory is per agent
   and per person, and the model never chooses who it is, which workspace it
   uses, or where a message goes.
6. **Files over bespoke APIs.** Memory and workspaces are plain files the
   agent already knows how to use and the owner can inspect.
7. **Trusted host, untrusted model.** The host decides identity, scope,
   delivery, and policy. Model output and anything hands can reach are
   treated as untrusted.
8. **Self-hosted, one trust domain.** pons does not pretend to isolate
   mutually untrusted tenants.
9. **Conversational by default, confirmed for authority.** The owner shapes
   the agent by talking to it, but anything that changes its identity or
   permissions takes effect only through an explicit confirmation of the
   exact change. Learned memory never becomes instruction on its own.

## Key decisions

Each ADR holds the contract for its decision.

| Decision | ADR | Why |
| --- | --- | --- |
| One named agent, versioned; settings and persona in a host-owned agent directory | 0016 | The owner shapes one agent instead of designing roles; edits never change work in flight |
| Every run ends in an answer, error notice, or stop notice in the log | 0023 | The owner always sees what happened, live or after a reload |
| Private one-level tasks under owner-configured profiles | 0016 | Context isolation for coding and research without predefined agents |
| Chat, schedules, and events share one intake; replies leave through a durable outbox | 0018 | One runner, owner-set destinations, no reruns when a send fails |
| Owner-linked identities across chat apps | 0018 | Memory follows a person without guessing identity |
| File-based memory, scoped by what each run mounts | 0019 | Inspectable and familiar to agents, while keeping people apart |
| Durable workspaces without Git | 0022 | The agent keeps its work across sandboxes and months |
| Work items for reminders and watches | 0021 | Weeks-long intent survives restarts and costs nothing while waiting |
| Artifacts and durable approvals, summarized for now | 0020, 0021 | Needed for later use cases; details would go stale |
| One trust domain per runtime | 0017 | Honest security claims and a simple local runtime |

## Alternatives considered

- **Always-running agent processes.** Rejected: idle cost, fragile across
  restarts, and state hidden in memory rather than durable storage.
- **One ever-growing conversation per agent as its memory.** Rejected:
  unbounded context, no scoping between people, and hard to correct.
- **Memory as database records with bespoke tools.** Rejected in favor of
  files: less inspectable and ignores how well agents already use files.
- **Predefined specialist agents** (a coding agent, a research agent).
  Rejected as the default: they make the owner design roles up front, while
  task profiles give the same isolation inside one agent.
- **Many peer agents that message each other** (as in Grok Bot). Deferred:
  more flexible, but harder to keep private and to reason about.
- **Replies sent by a model-chosen "send message" tool.** Rejected for
  ordinary replies: delivery must be durable and destinations owner-set.
- **Multi-tenant hosted runtime.** Rejected for pons itself; hosting would
  use isolated per-domain cells.
- **Git as the only way to keep files.** Rejected: many agents have no
  repository.

## Risks and trade-offs

- **Serialized agent.** The agent's own conversations share one workspace
  and run one at a time. Mitigation: heavy work runs as tasks in their own
  workspaces, in parallel.
- **Task trust depends on profiles.** An `inbox` task reading hostile email
  is only as safe as its profile's tools, approvals, and outbound checks.
- **Isolation depends on sandboxing.** Scoped memory and tasks need
  Seatbelt or E2B. The unsandboxed development setup cannot offer them.
- **Memory quality.** Automatic extraction may save noise; history, review,
  and restore keep it correctable.
- **Storage growth.** Whole-workspace checkpoints grow with use; exclusions
  and retention bound them.
- **Specification ahead of code.** Later phases are summaries on purpose and
  will change once earlier phases are built.

## Success measures

The [plan's milestones](plan.md#milestones) define each
demonstration. Beyond them:

- a daily brief runs for two weeks with no duplicates and no silent failures;
- nothing runs while the agent is idle; and
- the owner can inspect and roll back every memory, persona, and workspace
  change.

## Open questions

- Telegram is proposed as the first chat connector; confirm before the plan
  reaches it.
- How much should automatic memory extraction save before review becomes
  necessary?
- Is serialized handling acceptable for the agent's own conversations, or
  does it need a second lane for quick replies?
- **Households.** Should each household member get their own agent, as with
  Muse, or share one agent with per-person memory? The current design
  supports both.
- **Integrations.** How does the owner connect apps such as email, GitHub, or
  Home Assistant once, and grant each agent a scoped subset (for example read
  but not send)? This likely needs its own ADR before U11.
- **Skills.** Should an agent save a repeatable workflow as a file in its
  workspace or memory and rerun it on a schedule, as Grok Bot does?
- **Outbound checks.** Should ADR-0014's classifier check every outbound
  action (side-effecting tool calls, delivery, tasks), like Muse's
  Sentinel, rather than only individual tool calls?
- **Talking to a task.** Should the owner be able to follow up inside a
  running or finished task, rather than only through the agent?
- **Silent outcomes and request grouping.** Should a scheduled check be able
  to end with nothing to send, and should several runs (for example a parent
  and its tasks) count as one request? Deferred from phase 3 (ADR-0016
  sections 5–6); revisit when delivery, schedules, or tasks need it.
