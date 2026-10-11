# MCP remote servers spec

**Status:** Implemented
**Date:** 2026-10-11
**Related:** [ADR-0025](../../adr/adr-0025-mcp-integrations.md) delivery
step 1 (in review, PR #26),
[ADR-0014](../../adr/adr-0014-native-action-policy.md),
[ADR-0016](../../adr/adr-0016-persistent-agents-and-async-messaging.md)

This spec covers the first slice of ADR-0025: the built-in MCP plugin,
remote `http` servers authenticated with a token from the credential store,
per-agent grants, and approval modes. OAuth, local `stdio` servers, the
Registry, and plugin bundles follow in later slices (see
[Later slices](#later-slices)).

## Summary

Today an agent can only use the tools its hands provide inside the sandbox.
It cannot reach the owner's accounts.

After this change, the owner adds a remote MCP server to host config, stores
its token with `pons mcp set-secret`, and grants the server to the agent in
`agent.json`. The agent then sees the server's tools as
`mcp__<server>__<tool>` and calls them under the server's approval mode. The
token never leaves the host process.

```json
{
  "mcpServers": {
    "github": {
      "type": "http",
      "url": "https://api.githubcopilot.com/mcp/",
      "headers": {"Authorization": "Bearer ${secret:github_token}"},
      "approvalMode": "writes",
      "disabledTools": ["delete_*"]
    }
  }
}
```

## Current behavior

- pons has no MCP support and no named secrets.
- In-process host plugins come from the `plugins` list in
  `~/.pons/config.json`. `hostconfig` builds them once at startup and
  `agentRunner.Run` applies them to each run's `Core`
  (`cmd/pons/runtime_mode.go`).
- `agent.json` holds `name`, `provider`, `model`, `max_turns`, and
  `workspace` (`runtime/agentdir`). `AgentDefinition.Revision` fingerprints
  the resolved definition (`runtime/agent.go`).
- The runtime installs no approval handler. Every unresolved `ask` is denied
  with `approval_required` (`permission.go`), for interactive and queued runs
  alike.
- `actionpolicy.ToolRule` matches exact tool kinds or `"*"`. It has no
  patterns.
- `~/.pons/auth.json` maps auth IDs to Codex credentials, and a store with
  one entry stands in for any ID (`plugins/brain/llm/codexauth.go`). It
  cannot hold other kinds of secret without breaking that rule.

## Goals

- A remote MCP server is configuration, not code.
- An agent sees only the servers it is granted and the tools that pass both
  the host's filters and its own.
- A secret reaches only its own server's HTTP requests.
- MCP calls go through the same action policy as every other tool.
- Everything is testable with no network and no real credentials.

## Non-goals

- OAuth (`pons mcp login`, the `oauth` block). An `oauth` block fails
  startup as not yet supported.
- `stdio` servers. A `command` entry fails startup as not yet supported.
- `pons mcp add`, the MCP Registry, and `mcpServers` in plugin manifests.
- MCP resources, prompts, elicitation, and sampling.
- Interactive approval. This slice only fixes what `ask` does when no one
  can answer.
- Task profiles (ADR-0016), which are not built.
- Reading other agents' `mcpServers` files.
- Deferred tool loading or tool search.

## Client library

ADR-0025 leaves the choice between the official MCP Go SDK and a minimal
client to this spec.

| | Official SDK (`github.com/modelcontextprotocol/go-sdk`) | Minimal client |
| --- | --- | --- |
| This slice | A few lines: `Client`, `StreamableClientTransport`, `ListTools`, `CallTool` | About 500 lines: JSON-RPC, session header, version negotiation, SSE response parsing, pagination, cancellation |
| OAuth (step 2) | `auth` and `oauthex` packages cover resource and server metadata, dynamic and pre-registered clients, and PKCE | All written and maintained here; security-sensitive |
| `stdio` (step 3) | `CommandTransport` takes an `exec.Cmd` pons builds | Reuse the framing in `plugins/external` |
| Spec changes | Tracked upstream | Tracked here |
| Dependencies | Adds the SDK, `google/jsonschema-go`, `yosida95/uritemplate`, `golang.org/x/oauth2`, and `segmentio/encoding` to the build | None |
| Control | Through `ClientOptions` and a custom `http.RoundTripper` | Total |

**Recommendation: the official SDK**, pinned to the latest release (v1.8.0
at the time of writing), behind a small interface inside `plugins/mcp`.

The first slice alone would not justify it. OAuth does: step 2 needs the MCP
authorization flow, and writing that by hand is the largest and riskiest part
of the whole ADR. The project already uses official SDKs for Anthropic and
OpenAI for the same reason.

Everything pons needs to control is reachable:

- `ClientOptions.Capabilities` is set to an empty value, so pons advertises
  no roots, sampling, or elicitation. No sampling or elicitation handler is
  set.
- `StreamableClientTransport.DisableStandaloneSSE` is set. pons needs no
  server-initiated messages, because it lists tools at the start of each
  run.
- `StreamableClientTransport.HTTPClient` carries a pons `RoundTripper` that
  adds the server's headers, refuses redirects, and caps response bodies.
- A lost session surfaces as `ErrSessionMissing`, which pons uses to reopen.

Only `plugins/mcp/client.go` imports the SDK. The rest of the plugin uses
this interface, so a minimal client could replace it later:

```go
type session interface {
	ListTools(ctx context.Context) ([]tool, error)
	CallTool(ctx context.Context, name string, args json.RawMessage) (callResult, error)
	Close() error
}
```

The implementation uses v1.8.0. That version first tries the 2026-07-28
protocol's stateless `server/discover` and falls back to the `initialize`
handshake, so pons works with servers on either.

## Design

### Host configuration

`~/.pons/config.json` gains a top-level `mcpServers` map. Keys inside it use
the standard camelCase shape so a snippet from another agent can be pasted
in.

| Field | Meaning | Default |
| --- | --- | --- |
| `type` | `http` | required in this slice |
| `url` | Streamable HTTP endpoint | required |
| `headers` | Sent on every request; values may contain `${secret:<name>}` | none |
| `enabled` | `false` keeps the entry without connecting | `true` |
| `approvalMode` | `ask`, `writes`, or `allow` | `ask` |
| `enabledTools`, `disabledTools` | Glob patterns over the server's tool names | all tools |
| `startupTimeoutSec` | Deadline to connect and list tools | 10 |
| `toolTimeoutSec` | Deadline for one tool call | 60 |

Validation happens at startup and names the server:

- The name is lowercase letters, digits, `_`, and `-`, at most 32
  characters, starts and ends with a letter or digit, and does not contain
  `__`, so a tool name always splits at the first `__` after the prefix.
- `url` uses `https`, or `http` when the host is `localhost` or a loopback
  IP literal. It has no user info, no fragment, and no `${...}` reference:
  secrets go in `headers`.
- A header value that contains `${` in any form other than
  `${secret:<name>}` is an error. Environment expansion is not supported.
- Patterns use `path.Match` syntax; a malformed pattern is an error.
- Unknown fields are errors, as elsewhere in the config.
- A project `.pons.json` that sets `mcpServers` is rejected, like `plugins`.

`approvalMode` is the name this spec gives ADR-0025's per-server approval
mode; the ADR does not name the key. It is Codex's
`default_tools_approval_mode` in the camelCase of its neighbours.

An `http` server with no `Authorization` header is used without
authentication. If it answers 401, its tools fail with an error saying OAuth
login is not supported yet.

### Secrets

Named secrets live in `~/.pons/secrets.json`, beside `auth.json`: a JSON
object of name to value, mode 0600 in a 0700 directory. A write takes an
advisory lock on `secrets.json.lock` and replaces the file in one step, so a
reader never sees it half written. A file that is not a regular file, or is
readable by anyone else, is refused.

`auth.json` is not reused, because its single-entry fallback treats every
entry as a Codex credential. Step 2 stores OAuth tokens in the same new
file.

```sh
pons mcp set-secret github_token
```

- The value is read from the terminal without echo. When standard input is
  not a terminal, one line is read from it, so a password manager can pipe
  the value in. It is never an argument.
- Names match `[A-Za-z0-9_-]{1,64}`. An empty value is rejected.
- The command prints only the name it stored.

```sh
pons mcp tools github
```

`pons mcp tools <server>` connects to one configured server with its stored
secrets and prints each tool's name, whether it is annotated read-only, and
whether the host filters keep it. It lets the owner check a token and write
filters without starting a conversation. It needs no running pons server and
prints no header or secret. A disabled server is still checked.

Reading without echo uses `golang.org/x/term`, a small new dependency on top
of `golang.org/x/sys`, which pons already has.

The server resolves every enabled server's headers once at startup. A
missing secret stops startup with the server name, the secret name, and the
command to run. Changing a secret takes effect on restart.

A resolved value exists only in the plugin's HTTP transport. To keep it
there:

- Headers are never logged or put in an error.
- The transport refuses redirects, so a header cannot follow one to another
  host.
- Any resolved secret value of eight bytes or more is replaced with
  `[secret]` in tool results, tool descriptions, input schemas, and error
  text before they leave the plugin, including where JSON encoding has
  escaped it. This covers a server that echoes its request.
- Values in the URL's query string are redacted the same way, because
  transport errors quote the URL.

### Agent grants

`agent.json` gains `mcpServers`:

```json
{
  "name": "Ada",
  "mcpServers": {
    "github": {"enabledTools": ["search_*", "get_*", "list_*"]},
    "linear": {}
  }
}
```

- Each key names a server in host config. A name that is not configured
  fails startup with an error naming the file. A configured but disabled
  server is accepted and contributes no tools.
- A grant takes `enabledTools` and `disabledTools`, with the same pattern
  rules. No other field is accepted.
- An agent with no `mcpServers` gets no MCP tools.

A tool is visible when it passes the host filter and the grant filter. Each
filter passes a tool when `enabledTools` is empty or matches it, and
`disabledTools` does not match it. Because the two are intersected, a grant
can narrow but never widen.

The grant becomes `AgentDefinition.MCPServers` (JSON `mcp_servers`, with
`enabled_tools` and `disabled_tools` per grant), so it is part of the revision. The field is omitted when empty, so no grants
and an empty `mcpServers` object have the same revision.

Host server settings (URL, host filters, approval mode) are not in the
revision. Like credentials and host policy, a run uses the server's current
configuration. A run whose recorded grant names a server that is no longer
configured fails before its sandbox starts, with an error naming the server.

### Plugin

`plugins/mcp` has two parts.

`mcp.Manager` is built once at server startup from the validated config and
resolved secrets, and closed at shutdown. It owns one lazily opened session
per server, shared by all runs. Opening is serialized per server.

`Manager.Plugin(ctx, grants, logger)` returns a `pons.Plugin` for one run.
`agentRunner.Run` adds it after the hands proxy, so the MCP plugin registers
through `Core` like every other plugin. Its `Setup`:

1. connects to each granted, enabled server and lists its tools, in
   parallel, each within `startupTimeoutSec`;
2. applies both filters and the limits below;
3. registers each tool with `Core.AddTool` as `mcp__<server>__<tool>`, with
   the server's description and input schema;
4. registers the approval hook (see [Approval](#approval)).

Tools are listed at the start of every run. A tool a server adds later
therefore appears in the next run, through the same filters. A call to a
tool that was not registered gets the core's usual "no plugin provides"
result.

A server that cannot be reached at run start does not fail the run. Its
tools are absent, and an `OnAgentStart` hook tells the owner which server is
unavailable and why, in at most 512 bytes of the server's error. The brain is
told only that the server is unavailable: host context is not labeled as
untrusted the way a tool result is, so text a server chose never goes there.

A name conflict with an already registered tool fails `Setup`, as `AddTool`
does today.

### Limits on what a server reports

The tool list is untrusted input that ends up in the brain's prompt.

| Limit | Value | When exceeded |
| --- | --- | --- |
| Tool name | `[A-Za-z0-9_-]+`, full name at most 64 characters | Tool skipped, logged |
| Description | 2 KiB | Truncated |
| Input schema | 64 KiB; an object schema with no top-level `anyOf`, `oneOf`, `allOf`, `not`, or `enum` | Tool skipped, logged |
| Tools per server, after filters | 128 | Server unavailable for the run, with an error asking for a narrower filter |

The 64-character limit is the strictest tool-name limit among the supported
providers. A schema is normalized to `"type": "object"` with a `properties`
member, because one schema a provider rejects would fail every model request
of the run. A response from a server is capped at four times the result
limit and at least 8 MiB, so a small result limit does not cut off a large
tool list.

### Calls and results

A handler maps `protocol.Action` to `tools/call` and back:

- `Action.Args` is sent unchanged as the tool's arguments.
- The call runs under `toolTimeoutSec` and the run's context. Cancelling
  either cancels the request.
- Text content blocks are joined into `ToolResult.Output`. Other blocks
  become a placeholder line such as `[image content omitted]`. With no text
  content, `structuredContent` is rendered as JSON. A result carries one
  form, never both: servers repeat structured content as text.
- A result with `isError` is `OK: false` with the server's text as `Error`.
  A transport or protocol failure is `OK: false` with a pons-written error.
- `Payload` is `{"server": ..., "tool": ...}`.
- A response over the result limit (`plugin_max_result_bytes`, 1 MiB by
  default) fails the call rather than being truncated, as external plugins
  do.
- A server that answers a call by asking for more input is treated as a
  failed call.

A lost session is replaced, but a call is sent again only when it provably
did not run. A session ending fails every call in flight on it, including
ones the server already accepted, so the SDK's error alone is not proof. The
plugin's HTTP transport records, per call, whether the request was sent and
whether the server answered that request with 404 for its session. The call
is retried once on a new session when its own request was refused that way or
was never sent; otherwise the failure is returned.

Listing tools changes nothing, so any failed listing is retried once on a new
session. That also recovers, at the start of the next run, a session a server
lost without the 404 the specification describes.

### Untrusted results

MCP results are data, never instructions:

- `ToolSpec.Source` is `{PluginName: "mcp", External: true}`. Hooks and
  classifiers already receive the tool's source with every call, and the
  server name is in the tool name.
- In the classifier's recent context, an MCP result keeps the source
  `tool_result` and the kind `mcp__<server>__<tool>`, in a live run and
  after hydration from stored history. It is never a `user` item.
- The brain's system prompt already tells it to treat tool output as
  untrusted data. This slice adds no MCP-specific prompt text.

### Approval

The plugin's `OnToolCallStart` hook answers only for its own tools:

| `approvalMode` | Tool | Hook answer |
| --- | --- | --- |
| `allow` | any | no objection |
| `writes` | annotated `readOnlyHint: true` | no objection |
| `writes` | anything else | `ask`, reason `mcp_write_approval` |
| `ask` | any | `ask`, reason `mcp_approval` |

The core merges this with every other start hook and keeps the strictest
answer. So action policy can tighten a server (deny `mcp__github__delete_*`
under `allow`) but cannot loosen one; to loosen, change the mode.
Annotations come from the server, so `writes` is a convenience for a trusted
server, not a boundary.

Two small changes outside the plugin make this work as ADR-0025 describes:

1. **Patterns in action policy rules.** `actionpolicy.ToolRule` matches
   tool kinds with `path.Match`, so `mcp__github__*` works. `"*"` still
   matches every tool and a name always matches itself, whatever characters
   they contain. Rules, host filters, and grants share one matcher. A
   malformed pattern fails startup.
2. **`approval_unavailable`.** The runtime installs an approval handler that
   reports that no one can answer. The core then denies an unresolved `ask`
   with `approval_unavailable` instead of `approval_required`. This applies
   to every tool, not only MCP tools, and is the seam ADR-0021's durable
   approvals will fill. `OnPermissionRequest` hooks still run first and can
   allow the call.

**Consequence.** The runtime has no approval UI, so today every
run is one with no one to ask. Under the default mode `ask`, every call to a
new server is denied until the owner sets `approvalMode` to `writes` or
`allow`. This spec keeps `ask` as the default, as the ADR says, and has the
server log a warning at startup for each granted server left on `ask`. Defaulting to `writes` until approvals exist was considered and rejected: a
new server should not act without the owner choosing that.

### Runner and startup

- `cmd/pons/config.go`: `settings.MCPServers`; reject it in `.pons.json`.
- `cmd/pons/main.go`: route `pons mcp set-secret|tools` like
  `pons workspace ...`; build
  the `mcp.Manager` from global settings and pass it in `serverOptions`.
- `defaultAgent`: validate the grant against the configured servers and set
  `AgentDefinition.MCPServers`.
- `agentRunner.Run`: check the recorded grant, install the approval handler,
  and add `Manager.Plugin(...)` to the run's plugins.
- `runServerReady`: close the manager on shutdown.
- `pons eval` is unchanged; it does not load MCP servers.

No runtime database change. Tool events already record the tool kind, which
carries the server name.

### Logging

At startup the server logs each configured server's name, URL host,
approval mode, and whether it is granted. With `--debug`, a run logs each
server's tool count and each skipped tool. Arguments, results, headers, and
secret values are never logged.

## Tests

All deterministic, with no network and no real credentials. A fake server in
`plugins/mcp/mcptest` is a hand-written `httptest` handler, independent of
the SDK, that records the requests it receives and can be told to misbehave.

- **Config:** a non-HTTPS, non-loopback URL is rejected; loopback HTTP is
  accepted; `${VAR}`, `stdio`, `oauth`, bad names, and bad patterns are
  rejected; `.pons.json` cannot add or enable a server.
- **Secrets:** `set-secret` stores a piped value with mode 0600 and never
  prints it; a missing secret stops startup naming the command to run; a
  store readable by others is refused.
- **`pons mcp tools`:** lists the fake server's tools with their read-only
  and filter status, and prints no secret; a failed connection exits
  non-zero with the server's error.
- **Grants:** an agent sees only the servers it names and the tools that
  pass both filters; a grant cannot widen a host filter; an unknown server
  fails startup; a grant change changes the revision; a definition with no
  grant keeps its previous revision.
- **Late tools:** a tool the server adds between two runs appears in the
  second run only if it passes the filters.
- **Secret isolation:** the fake server receives the header. The secret
  value appears in no event, log line, tool result, tool description, or
  classifier input, including when the server echoes it back. Hands are
  covered by construction: the plugin has no path to the environment spec. A redirect to
  a second fake server is refused and that server receives nothing.
- **Approval:** each row of the approval table; a deny rule for
  `mcp__<server>__*` wins under `allow`; an unresolved `ask` is denied with
  `approval_unavailable`.
- **Results:** text, non-text, structured, and `isError` results map as
  described; an oversized result fails; an MCP result is a `tool_result`
  item with its server's kind in classifier context.
- **Resilience:** an expired session is reopened and a refused call retried
  once, while a call in flight when the session ends is never sent twice; a
  session lost in a non-standard way is recovered;
  a hung call honors its deadline and cancellation; an unreachable server
  leaves the run working, with a notice.
- **Sampling:** the `initialize` request advertises no sampling, and a
  server that refuses to initialize without it is reported unavailable.
- **Limits:** an invalid or over-long tool name and an oversized schema are
  skipped; too many tools makes the server unavailable.

## Documentation

- `README.md`: a "MCP servers" section covering host config, `pons mcp
  set-secret`, `pons mcp tools`, the `agent.json` grant, approval modes, and what `ask` does
  today; the `mcpServers` field in the agent section; pattern support in
  action policy rules; `approval_unavailable`; the revision now includes the
  grant; the new dependencies.
- ADR-0025: implementation status becomes "Partially implemented (step 1)"
  once it is merged. ADR-0014: note pattern matching in tool rules.
- `docs/README.md`: link this spec (done with this document).

## Delivery

One task, in four steps that each leave `make check` passing:

1. **Config and secrets.** `mcpServers` parsing and validation, the secret
   store, and `pons mcp set-secret`.
2. **Plugin.** The SDK-backed client, `Manager`, tool registration, result
   mapping, limits, the fake server, and `pons mcp tools`.
3. **Grants and approval.** `agent.json` grants, the revision, runner
   wiring, the approval hook, rule patterns, and `approval_unavailable`.
4. **Documentation.**

## Decisions

Settled in review on 2026-10-11:

- **Client library:** the official SDK. Go has no OAuth support in the
  standard library. `golang.org/x/oauth2` covers the token exchange and
  refresh but not MCP's server discovery or client registration, which a
  minimal client would have to add by hand in step 2.
- **Default approval mode:** `ask`, with the startup warning.
- **Config key:** `approvalMode`.
- **Secret store:** a new `~/.pons/secrets.json`. Restructuring `auth.json`
  would change the Codex login and refresh path for no gain.
- **`pons mcp tools <server>`:** included in this slice.

## Later slices

In ADR-0025's order:

1. OAuth: `pons mcp login`, dynamic and pre-registered clients, token
   storage, and refresh.
2. Local `stdio` servers, documented as unsandboxed.
3. `pons mcp add` from the MCP Registry, and `mcpServers` in plugin
   manifests.
4. The per-server `sandbox` block for `stdio` servers.
