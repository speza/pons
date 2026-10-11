# ADR-0025: Integrations are MCP servers, added through a built-in MCP plugin

**Status:** Proposed
**Implementation:** Not implemented
**Date:** 2026-10-10
**Related:** ADR-0004, ADR-0007, ADR-0014, ADR-0016, ADR-0017, ADR-0018,
[external plugin protocol](../reference/external-plugin-protocol.md),
[persistent agents design](../proposals/persistent-agents/design.md)

## Context

A persistent agent is only as useful as what it can reach. Use cases such as
inbox triage (U11), a daily brief, or acting on GitHub issues need the agent
to read and act on the owner's accounts: email, calendar, GitHub, Home
Assistant, and so on. The persistent agents design leaves open how the owner
connects an app once and grants each agent a subset of it.

pons has two kinds of external plugin, both over `pons.plugin.runtime/1`:

- **Hands plugins** provide tools. They are untrusted, run inside the hands
  sandbox, and receive no credentials.
- **Host hook plugins** are trusted control-plane code that observes and
  gates tool calls but provides no tools.

Neither fits an integration: it needs credentials and network access, so it
cannot sit inside the hands sandbox, and it provides tools, which hooks do
not. pons has no MCP support.

Integrations must be easy to contribute and should not be pons-specific.
The Model Context Protocol (MCP) already solves this: servers exist for most
services, SDKs exist in every mainstream language, and an official registry
describes how to install and configure each server.

### Prior art

Other agents have converged on the same shapes, which this ADR adopts rather
than reinventing:

| Concern | Converged practice | Examples |
| --- | --- | --- |
| Server config | A `mcpServers` map: `command`/`args`/`env` for stdio, `type`/`url`/`headers` for HTTP, `${VAR}` expansion | Claude Code `.mcp.json`, Cursor, VS Code `mcp.json`, Codex `[mcp_servers.<name>]` |
| Distribution | The official MCP Registry's `server.json`: packages (npm, PyPI, OCI, MCPB) or remotes, with required and secret inputs declared | MCP Registry, GitHub MCP Registry |
| Plugins | A plugin bundles MCP servers alongside its other parts; lifecycle follows the plugin | Claude Code plugins (`mcpServers` in `plugin.json`), OpenClaw (accepts Claude and Codex plugin bundles) |
| Tool selection | Per-server include/exclude lists with glob patterns | Codex `enabled_tools`/`disabled_tools`, OpenClaw `toolFilter` |
| Approval | MCP tools go through the client's normal permission policy, with a per-server default such as "ask for writes" | Codex `default_tools_approval_mode`, OpenClaw tool policy, Claude Code permissions |
| Tool names | `mcp__<server>__<tool>`, used in permission rules | Claude Code |
| Secrets | Declared inputs marked secret, stored by the client (keychain or secret store), substituted into env or headers | VS Code `inputs` with `password`, MCPB `user_config` with `sensitive`, OpenClaw secret mechanisms |
| Remote auth | MCP authorization (OAuth), with an explicit `login` command | `claude mcp login`, Codex OAuth |
| Isolation | Optional sandbox for stdio servers with filesystem and network allowlists, or containers | VS Code `sandboxEnabled`, Docker MCP Gateway |
| Limits | Startup and per-tool timeouts; large outputs capped | Codex `startup_timeout_sec`/`tool_timeout_sec`, Claude Code output limits |

## Decision

### 1. MCP support is one built-in plugin

`plugins/mcp` is the MCP client. It is registered through `Core` like every
other plugin, and for each server an agent may use it:

1. starts or connects to the server with a clean environment and only that
   server's configured secrets;
2. lists the server's tools and keeps those the agent is allowed (section 4);
3. registers each as a Core tool named `mcp__<server>__<tool>`, rejecting
   name conflicts as `AddTool` already does;
4. routes the brain's `protocol.Action` to `tools/call` and maps the result
   to a `protocol.ToolResult`.

The brain still plans only through actions and the core still owns
orchestration. MCP tools are one more tool source beside the hands proxy.

Connections are long-lived and shared by runs, and live on the host,
outside the hands sandbox. An `http` server's session is opened lazily and
reopened after it expires or fails. A `stdio` server process starts lazily,
restarts with backoff after a crash, and stops with the pons server. The
existing external host limits apply to every call: deadlines, cancellation,
result size, and, for `stdio`, stderr.

pons supports MCP **tools** only. Resources, prompts, and elicitation are
out of scope. Sampling is never offered, because it would let a third-party
server spend the owner's model credentials.

The client library is chosen when implementation starts: the official MCP Go
SDK, or a minimal client for tools over Streamable HTTP and stdio.

### 2. Servers use the standard `mcpServers` shape

Servers are configured in host config under `mcpServers`, in the same shape
as Claude Code, Cursor, and VS Code, so an existing snippet can be pasted in:

```json
{
  "mcpServers": {
    "github": {
      "type": "http",
      "url": "https://api.githubcopilot.com/mcp/",
      "headers": {"Authorization": "Bearer ${secret:github_token}"},
      "disabledTools": ["delete_*"],
      "toolTimeoutSec": 60
    },
    "linear": {
      "type": "http",
      "url": "https://mcp.linear.app/mcp"
    },
    "gmail": {
      "type": "http",
      "url": "https://gmailmcp.googleapis.com/mcp/v1",
      "oauth": {
        "clientId": "1234-abcd.apps.googleusercontent.com",
        "clientSecret": "${secret:google_oauth_client_secret}",
        "scopes": ["https://www.googleapis.com/auth/gmail.readonly"]
      }
    },
    "notes": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-filesystem", "/Users/sam/notes"]
    }
  }
}
```

- `type` is `http` (Streamable HTTP) or `stdio` (the default when `command`
  is set).
- An `http` URL must use HTTPS, except on loopback. `headers` are sent on
  every request to the server.
- An `http` server with no `Authorization` header uses MCP authorization
  (OAuth, section 5). pons registers itself as a client dynamically when the
  server allows it. Servers that require a pre-registered client, such as
  Google Workspace's, take an `oauth` block with `clientId`, `clientSecret`
  (a secret reference), and `scopes`, as Claude Code and VS Code accept.
- `stdio` commands are executed directly, never through a shell.
- `${secret:<name>}` in `headers` or `env` is replaced from the host
  credential store (section 5). Plain `${VAR}` expansion from the pons
  server's environment is not supported: secrets come only from the store,
  and a `stdio` process gets a clean environment.
- `enabledTools` and `disabledTools` take glob patterns and filter the
  server's tools for every agent; `disabledTools` applies after
  `enabledTools`.
- `enabled`, `startupTimeoutSec`, and `toolTimeoutSec` follow Codex's
  meaning.

Project config cannot add or enable MCP servers, as with every other
plugin.

### 3. Plugins can bundle MCP servers

A pons plugin manifest may include an `mcpServers` map in the same shape. Its
servers start and stop with the plugin, and their names are prefixed with
the plugin's, as in Claude Code. A plugin may consist of nothing but MCP
servers, plus any skills or hooks it adds later.

Contributors therefore have three ways in, none of which touches pons's Go
code:

1. **Use a published server:** `pons mcp add` from the MCP Registry, or a
   pasted config snippet.
2. **Publish a new server** to the MCP Registry, in any language.
3. **Ship a plugin** that bundles one or more servers with pons-specific
   defaults, such as tool filters.

### 4. Agents are granted servers and tools

Each agent definition names the servers it may use, with optional tool
filters, in `agent.json`:

```json
{
  "mcpServers": {
    "github": {"enabledTools": ["search_*", "get_*", "list_*"]},
    "linear": {}
  }
}
```

- An agent sees only the servers it names, and only tools that pass both the
  host filters and its own. An agent filter can narrow but never widen.
- The grant is part of the agent's revision.
- A task profile (ADR-0016) names its own subset, never more than its parent
  agent's.
- The tool list a server reports at runtime is untrusted. Tools that appear
  later are filtered by the same rules.

### 5. Secrets stay in the host

MCP secrets live in the host credential store beside provider credentials,
with the same file permissions, and are referenced as `${secret:<name>}`.

- `pons mcp set-secret <name>` reads a secret from the terminal, never from
  an argument, and stores it.
- `pons mcp login <server>` runs MCP authorization (OAuth) for an `http`
  server, with the browser redirect returning to the pons server's loopback
  address (`http://localhost:<port>/mcp/oauth/callback`, which the owner
  adds to a pre-registered client's allowed redirects). pons stores the tokens and refreshes them before they expire. A
  refresh that fails marks the server as needing login; its tools fail with
  an error that says so, rather than disappearing.
- `pons mcp add` reads a Registry `server.json`, prompts for each input
  marked `isSecret`, stores it, and writes the server config with
  `${secret:...}` references.

A secret reaches only its own server's process or HTTP requests. It never
reaches the brain, hands, events, logs, the classifier, or any other plugin.

### 6. MCP calls go through action policy

MCP tools are ordinary actions to ADR-0014's action policy and permission
hooks. Rules match on the tool name, so `mcp__github__*` or
`mcp__gmail__send_*` work as patterns.

- Each server has a default approval mode, as in Codex: `ask` (the default),
  `writes` (allow tools annotated `readOnlyHint`, ask for the rest), or
  `allow`. Annotations come from the untrusted server, so `writes` is a
  convenience for trusted servers, not a security boundary.
- In a run with no one to ask, such as a scheduled run, `ask` denies with
  `approval_unavailable` until ADR-0021's durable approvals exist.
- MCP results are untrusted data, labeled with their server as the source.
  An email body or issue comment cannot express user intent, grant a tool, or
  change policy.

### 7. Isolation follows VS Code's model, later

Adding an MCP server means trusting it with its secrets. A remote `http`
server runs elsewhere and needs no local isolation, which is one reason
remote servers are delivered first. In this ADR a `stdio` server runs on the
host as the pons user, with a clean environment and only its secrets, and
documentation says so.

A follow-up adds an optional per-server `sandbox` block in VS Code's shape,
with filesystem write paths and allowed network domains, enforced by
Seatbelt on macOS and the Linux sandbox provider elsewhere.

## Configuration and store changes

- Host config: `mcpServers`.
- Plugin manifest: optional `mcpServers`.
- `agent.json`: `mcpServers` grants with optional tool filters.
- Host credential store: named secrets and OAuth tokens.
- No runtime database changes. Tool events already record calls and
  results; MCP calls carry the server name.

## Verification requirements

Deterministic tests, with a fake MCP server and no network or real
credentials, prove:

- an agent sees only the servers it names and tools that pass both filters,
  and a grant change changes the revision;
- a tool reported after startup is filtered by the same rules;
- secrets reach only their own server, in its headers or environment, never
  hands, events, logs, or the classifier;
- a non-HTTPS, non-loopback `http` URL is rejected;
- `ask` and `writes` behave as described, and `ask` denies with
  `approval_unavailable` when no one can answer;
- an expired `http` session is reopened, a crashed `stdio` server restarts
  with backoff, and a hung call honors its deadline and cancellation;
- OAuth tokens are refreshed before expiry, and a failed refresh makes the
  server's tools fail with a login-required error;
- a server that requires sampling fails to initialize;
- project config cannot add or enable a server;
- MCP results are labeled untrusted in classifier context;
- a Registry `server.json` with a secret input produces a config with a
  `${secret:...}` reference and no secret value.

## Alternatives considered

- **A pons-specific integration manifest and scopes.** An earlier draft of
  this ADR added an `integration` placement, a custom manifest, and named
  scopes. Rejected: the `mcpServers` shape, Registry `server.json`, and tool
  filters already cover it, and anything pons-specific is harder to
  contribute to.
- **MCP servers inside the hands sandbox.** Rejected: credentials would sit
  inside the untrusted boundary, and the sandbox denies network by default.
- **Built-in Go integrations in core.** Rejected: core would accumulate
  service-specific code, which AGENTS.md forbids.
- **A separate MCP gateway process,** as Docker's. Rejected for now: the
  plugin already runs outside the sandbox and owns secrets. A gateway may be
  worth it when servers run in containers.

## Consequences

pons uses the existing MCP ecosystem and its conventions, so servers,
snippets, and Registry entries written for other agents work unchanged. An
integration is configuration, not code.

The cost is trust in third-party servers, with local `stdio` servers running
as the pons user until the sandbox follow-up lands, and prompt size: a large server can add many
tools. Tool filters bound this; deferred tool loading or tool search can come
later behind the same plugin.

## Delivery

Remote servers come first: many services now host official MCP servers,
they need no local process or sandbox, and they suit an always-on agent.

1. **Remote servers with token auth:** `plugins/mcp`, `http` servers in host
   `mcpServers`, `headers` with `${secret:...}` and `pons mcp set-secret`,
   `agent.json` grants, approval modes, and a test suite against an
   in-process fake server.
2. **OAuth:** `pons mcp login`, dynamic and pre-registered clients, token
   storage, and refresh. Google Workspace's Gmail, Calendar, and Drive
   servers are the target.
3. **Local servers:** `stdio` servers, documented as unsandboxed.
4. **Registry and plugins:** `pons mcp add` from the MCP Registry, and
   `mcpServers` in plugin manifests.
5. **Isolation:** the per-server `sandbox` block for `stdio` servers.

## Open questions

- Should pons read Claude Code's or Cursor's existing `mcpServers` files, or
  only its own config?
- Does a second agent with different credentials for the same service need
  its own server entry (for example `github-work`)? That works today without
  new design; confirm it is enough.

## References

- [MCP Registry](https://modelcontextprotocol.io/registry/about) and its
  `server.json` format
- [Claude Code MCP](https://code.claude.com/docs/en/mcp)
- [Codex MCP](https://learn.chatgpt.com/docs/extend/mcp?surface=cli)
- [VS Code MCP servers](https://code.visualstudio.com/docs/copilot/customization/mcp-servers)
- [MCP Bundles (MCPB)](https://github.com/modelcontextprotocol/mcpb)
- [OpenClaw MCP](https://docs.openclaw.ai/tools/mcp) and
  [plugins](https://docs.openclaw.ai/plugins)
- [Google Workspace MCP servers](https://developers.google.com/workspace/guides/configure-mcp-servers)
- [Docker MCP Gateway](https://docs.docker.com/ai/mcp-catalog-and-toolkit/mcp-gateway/)
