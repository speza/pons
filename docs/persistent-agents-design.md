# Persistent agents design

**Status:** Draft for review
**Date:** 2026-09-24
**Related:** [implementation plan](persistent-agents-v1.md),
[ADR-0016](adr/adr-0016-persistent-agents-and-async-messaging.md) through
[ADR-0022](adr/adr-0022-persistent-agent-workspaces.md)

This document says what the persistent-agents feature is, who it is for, and
why it has its shape. The [implementation plan](persistent-agents-v1.md) says
how and in what order it is built. The ADRs hold the detailed contracts.

## Summary

pons becomes a self-hosted home for long-lived agents. The owner runs one
server and gets a **chief of staff**: an assistant they can message from
anywhere, which remembers them, keeps its own files, runs routine checks,
reaches out when something matters, and hands specialist work to other
agents, such as coding agents, that keep their own context. Agents persist
as identities, memory, and workspaces, not as always-running processes.

## Problem

Today pons answers one conversation at a time. Between conversations it keeps
only transcripts:

- it does nothing unless someone types into a local client;
- it cannot reach the owner through the apps they already use;
- it forgets the owner between conversations unless they paste context back
  in;
- its files live and die with a conversation or a disposable sandbox; and
- it has one agent, so an assistant cannot hand a coding task to a coding
  agent without the owner routing it by hand.

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
5. **Hands off work.** The chief of staff delegates to specialist agents,
   which keep their own context and permissions.
6. **Many agents, one model.** The owner runs several agents that differ
   only in configuration, starting from built-in ones.
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

## Who it is for

The owner of a pons server: a developer or technical user who self-hosts,
possibly shared with a household or small team they trust. They configure
agents, connect channels, and approve risky actions. Everyone else who talks
to an agent is someone the owner has explicitly linked.

## What the owner experiences

At 07:00 the chief of staff sends a brief to the owner's chat app, or sends
nothing if the day is clear. Mid-morning the owner asks it from their phone
to remind them about a call on Friday; on Friday it does. Later they ask it
to fix a flaky test in a repository. It hands the task to the coding agent,
which works in its own sandbox and reports back. The chief of staff replies
with the outcome, and the coding agent asks before pushing. Over weeks the
chief of staff learns the owner's preferences and keeps notes and scripts in
its own workspace; the owner can open, correct, or roll back any of it. A
partner in the same household uses the same assistant, and neither sees the
other's memories.

## Use cases

These scenarios justify the design. The plan cites them per phase; a contract
no use case needs is a candidate to simplify or defer.

| ID | Use case |
| --- | --- |
| U1 | **Morning brief.** At 07:00 the assistant summarizes the owner's day to their chat channel, or stays silent when there is nothing worth sending. |
| U2 | **Chat from anywhere.** The owner messages the assistant from phone or web; it remembers preferences across both. |
| U3 | **Reminders.** "Remind me Friday to call X." |
| U4 | **Watch for something.** "Tell me when PR #12 merges; give up after a week." |
| U5 | **Hand off coding from a phone.** The assistant delegates to a coding agent in a sandbox and replies with the result. |
| U6 | **Approve from a phone.** A coding agent wants to push; the owner approves in chat hours later. |
| U7 | **Research report.** A research agent produces a file which comes back to the owner. |
| U8 | **Household.** Two people share one assistant; each person's memory stays private. |
| U9 | **The agent's own computer.** Over months the assistant keeps notes, scripts, and data in its workspace without Git; they survive sandbox loss, and the owner can roll back a bad change. |
| U10 | **Create an agent.** The owner creates a new agent with its own persona, for example a coding agent that reviews a repository every night. |

## Agents

An agent is one kind of object with a persona, tools, workspace, memory,
schedules, and channels. There are no agent types. pons ships built-in
configurations first; owners create their own later.

| | Chief of staff | Coding agent |
| --- | --- | --- |
| Role | Day-to-day help and orchestration | Focused work on one problem or codebase |
| Context | One long-lived workspace and memory | Clean per task, or one workspace per codebase |
| Tools | File, shell, web, memory | File, shell, web, memory, Git |
| Memory | Learns from conversations | Saves what it decides to |
| Delegates | Yes | No, by default |

The real difference between them is context isolation, not capability. An
assistant benefits from accumulating context about the owner's life. A coding
agent working on unrelated problems or codebases is harmed by it. A
persistent coding agent is the same configuration with one workspace per
codebase.

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
4. **Honest outcomes.** Success, silence (`no_update`), failure, and
   "unknown" are explicit states, never inferred from an empty answer.
5. **Private by default.** Delegated agents get only the request, memory is
   per agent and per person, and the model never chooses who it is or where
   a message goes.
6. **Files over bespoke APIs.** Memory and workspaces are plain files the
   agent already knows how to use and the owner can inspect.
7. **Trusted host, untrusted model.** The host decides identity, scope,
   delivery, and policy. Model output and anything hands can reach are
   treated as untrusted.
8. **Self-hosted, one trust domain.** pons does not pretend to isolate
   mutually untrusted tenants.

## Key decisions

Each decision is summarized here; its ADR holds the contract.

- **One agent shape with managed definitions** (ADR-0016). Agents are
  versioned definitions in the runtime, so editing one never changes work
  already in flight. *Why:* supports built-in and custom agents without
  types or restarts.
- **Lineages and explicit outcomes** (ADR-0016). Every external message
  starts a lineage that ends as completed, failed, or cancelled, with
  `no_update` as an explicit silent success. *Why:* a scheduled check that
  finds nothing must be distinguishable from a crash.
- **One-level delegation into private conversations** (ADR-0016). The chief
  of staff can hand a task to another agent, which cannot delegate further.
  *Why:* covers U5 while avoiding the hardest multi-level lifecycle rules.
- **Triggers and delivery around the same queue** (ADR-0018). Chat,
  schedules, and events enter through one path; replies leave through a
  durable outbox to a destination the owner configured. *Why:* one runner,
  no model-chosen destinations, and no reruns when a send fails.
- **Owner-linked identities** (ADR-0018). The owner declares which chat
  accounts belong to which person. *Why:* memory follows a person across
  apps without guessing identity.
- **File-based, mount-scoped memory** (ADR-0019). Each agent has a
  `MEMORY.md` tree with a folder per person; a run sees only the folders it
  is entitled to, and the host saves and versions changes. *Why:* inspectable
  and familiar to agents, while keeping household members private.
- **Durable workspaces without Git** (ADR-0022). Workspaces are per task,
  per agent, or per codebase, checkpointed with history, restore, and
  optional off-host storage. *Why:* agents keep their work, and context
  isolation is a workspace choice.
- **Work items for ongoing intent** (ADR-0021). Reminders and watches are
  stored with their wake conditions, not held by a running agent. *Why:*
  weeks-long intent must survive restarts and cost nothing while waiting.
- **Artifacts and durable approvals** (ADR-0020, ADR-0021). Summarized now,
  specified when built. *Why:* needed for U6 and U7, but far enough away
  that details would go stale.
- **One trust domain per runtime** (ADR-0017). *Why:* honest security
  claims, and a simple local runtime.

## Alternatives considered

- **Always-running agent processes.** Rejected: idle cost, fragile across
  restarts, and state hidden in memory rather than durable storage.
- **One ever-growing conversation per agent as its memory.** Rejected:
  unbounded context, no scoping between people, and hard to correct.
- **Memory as database records with bespoke tools.** Rejected in favor of
  files: less inspectable and ignores how well agents already use files.
- **Separate agent types for assistants and coding agents.** Rejected: they
  share almost everything; the differences are settings.
- **Replies sent by a model-chosen "send message" tool.** Rejected for
  ordinary replies: delivery must be durable and destinations owner-set.
- **Multi-tenant hosted runtime.** Rejected for pons itself; hosting would
  use isolated per-domain cells.
- **Git as the only way to keep files.** Rejected: many agents have no
  repository.

## Risks and trade-offs

- **Serialized agents.** A chief of staff with one workspace handles one run
  at a time, so a slow task can delay a quick reply. Mitigation: hand heavy
  work to other agents.
- **Isolation depends on sandboxing.** Scoped memory and delegation need
  Seatbelt or E2B. The unsandboxed development setup cannot offer them.
- **Memory quality.** Automatic extraction may save noise; history, review,
  and restore keep it correctable.
- **Storage growth.** Whole-workspace checkpoints grow with use; exclusions
  and retention bound them.
- **Specification ahead of code.** Later phases are summaries on purpose and
  will change once earlier phases are built.

## Success measures

- M1: the owner messages the chief of staff from a real chat app and gets a
  durable reply across a server restart.
- M2: a morning brief arrives, or explicitly does not, every day for two
  weeks without duplicates or silent failures, using the owner's memory.
- M3: a coding task delegated from a phone returns a result without leaking
  the assistant's context.
- Throughout: nothing runs while idle, and the owner can inspect and correct
  every memory and workspace change.

## Open questions

- Which chat app is the first connector?
- What should the owner's management surface be first: web UI or CLI?
- How much should automatic memory extraction save before review becomes
  necessary?
- Do agents need token or cost budgets before M2?
- Is serialized handling acceptable for the chief of staff, or does it need
  a second, conversation-scoped lane for quick replies?
