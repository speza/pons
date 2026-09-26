# pons documentation

Start with the [project README](../README.md), then use this index.

| Folder | What it holds | Status |
| --- | --- | --- |
| [`adr/`](adr/) | Architecture decision records, one decision each | Each ADR states its decision and implementation status |
| [`design/`](design/) | How the built parts of pons work | Implemented |
| [`reference/`](reference/) | Wire protocols and contracts for integrators | Implemented |
| [`proposals/`](proposals/) | Features not yet built, one folder per feature | Proposed |
| [`diagrams/`](diagrams/) | Architecture diagrams | Current system |

## Design

- [Runtime](design/runtime.md): server, conversations, storage, and client
  synchronization
- [Runtime event log](adr/adr-0023-runtime-event-log.md): canonical history,
  client replay, and operational index recovery
- [Hands environment](design/hands-environment.md): Seatbelt and E2B
  execution environments
- [Remote workspaces](design/remote-workspaces.md): durable remote
  workspaces and checkpoints
- [Git workspaces](design/git-workspaces.md): authenticated Git workspaces
  on E2B

## Reference

- [External plugin protocol](reference/external-plugin-protocol.md)
- [Plugin evals](reference/plugin-evals.md)

## Proposals

- **Persistent agents:** a long-lived, owner-named agent that remembers its
  owner and starts private tasks. Read the [design](proposals/persistent-agents/design.md)
  for what and why, then the [plan](proposals/persistent-agents/plan.md) for
  how and when.

When a proposal ships, its design moves into `design/` and the relevant ADRs
update their implementation status.
