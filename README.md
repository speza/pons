# pons

**pons** (*Latin*: "bridge") is part of the brainstem that carries signals
between the brain and the body in both directions: commands flow down and
observations flow up.

That is the architecture here: the brain sends actions down, and hands return
observations up.

pons is a minimal, pluggable agent harness in Go. A dependency-free
`protocol/` seam connects a reasoning **brain** to executing **hands**:

```
brain  -- protocol.Action -->  core  -- execute -->  hands
       <-- observations -----       <-- results ----
```

The brain plans but never executes. Hands execute but never plan. `Core`
owns orchestration, while tools, brains, storage, transports, and execution
environments are explicit plugins or injected infrastructure.

## Architecture

- The brain emits `protocol.Action` values and has no direct I/O.
- Hands execute actions and receive no conversation history, goals, or LLM.
- Enforcement happens at the execution boundary; model-declared risk is only
  advisory.
- `protocol/` is the shared JSON contract for in-process and external hands.

### Packages

| Package | Role |
| --- | --- |
| `protocol/` | Dependency-free `Action`, `ToolResult`, and `Observation` types |
| `core.go` | Turn loop, ports, and plugin registry |
| `plugins/brain` | LLM and deterministic scripted brains |
| `plugins/fs`, `edit`, `bash`, `shell` | Built-in hands-side tools |
| `plugins/external` | Persistent external tool providers and Go/TypeScript SDKs |
| `runtime/` | Durable SQLite runtime and HTTP/SSE transport |
| `environment/` | Provider/session contracts and durable environment state |
| `environment/seatbelt`, `environment/e2b` | Local and remote execution providers |
| `cmd/` | `pons`, `pons-hands`, and demo binaries |

Adding a tool plugin automatically adds its schema to the LLM brain. The core
does not need tool-specific changes.

## The agent loop

```go
result, err := core.Run(ctx, message) // respond -> act -> reflect -> repeat
```

Tool calls within a turn run concurrently, but results are recorded in the
planned order. A text response, `finish` action, or `MaxTurns` ends a run;
`OnEvent` can stream lifecycle events to a UI or audit consumer. The LLM brain
can compact old context without changing the canonical runtime history.

## Quick start

Tests do not need API keys:

```sh
go test ./...
go run ./cmd/pons-demo
```

Run with a provider:

```sh
export ANTHROPIC_API_KEY=sk-ant-…
go run ./cmd/pons -message "inspect this project"

export OPENAI_API_KEY=sk-…
go run ./cmd/pons -provider openai -message "inspect this project"
go run ./cmd/pons -provider openai \
  -base-url http://127.0.0.1:11434/v1 \
  -message "inspect this project"
```

The supported provider names are `anthropic`, `openai`, `codex`, and
`openai-responses`. Codex uses a ChatGPT subscription:

```sh
go run ./cmd/pons -provider codex -login
go run ./cmd/pons -provider codex -i
```

## Long-lived runtime

Without a subcommand, `pons` runs the server and client in one process, so
interactive output and server logs share a terminal. `serve` runs the server
separately; `client` submits messages and consumes SSE events:

```sh
go run ./cmd/pons serve -provider codex
go run ./cmd/pons client -message "inspect this project"
go run ./cmd/pons client -conversation <id> -message "continue"
```

Runtime state is stored in SQLite at `<state-dir>/runtime.db` (under
`~/.pons/runtime/` by default). The server keeps canonical messages and a
durable event outbox, bounds active runs, serializes each conversation, and
prevents simultaneous runs in one workspace. Clients resume from durable
snapshots and cursors; retries can use `-idempotency-key`.

The v1 server listens on loopback (`127.0.0.1:7337`) and refuses non-loopback
addresses.

## Hands, sandboxes, and external plugins

By default, built-in tools run in-process. On macOS, Seatbelt can place the
hands side in a fresh environment for each run:

```sh
go build -o .build/pons-hands ./cmd/pons-hands
go run ./cmd/pons -provider codex -sandbox seatbelt \
  -hands-command .build/pons-hands \
  -message "inspect this project"
```

E2B can run the same hands protocol in a remote Linux sandbox. Build the
pinned template once (requires `E2B_API_KEY` and Node/npm), then select it:

```sh
make e2b-template
make smoke-e2b
go run ./cmd/pons -provider codex -sandbox e2b \
  -e2b-template pons-hands \
  -message "inspect this project"
```

To create a new remote workspace from a public Git repository instead of
archiving the local workspace, supply its credential-free HTTPS URL and a full
immutable commit ID:

```sh
go run ./cmd/pons -provider codex -sandbox e2b \
  -git-repository https://github.com/OWNER/REPOSITORY.git \
  -git-revision COMMIT_SHA \
  -message "inspect this project"
```

Git provisioning enables sandbox networking and checks out the commit on an
independent `pons/<workspace>/work` branch before hands starts. Subsequent Git
commands use the existing bash tool.

For private GitHub repositories, create a deployment-owned GitHub App with
Metadata read and Contents read/write permissions, install it on the repositories
the agent may use, and provide its App ID, installation ID, and private key.
The key must be readable only by its owner:

```sh
chmod 600 /secure/pons-github-app.pem
go run ./cmd/pons -provider codex -sandbox e2b \
  -git-repository https://github.com/OWNER/REPOSITORY.git \
  -git-revision COMMIT_SHA \
  -github-app-id APP_ID \
  -github-app-installation-id INSTALLATION_ID \
  -github-app-private-key /secure/pons-github-app.pem \
  -message "inspect this project"
```

One server can create separate Git workspaces for separate conversations.
Choose each conversation's primary repository and pinned commit in the client:

```sh
go run ./cmd/pons serve -provider codex -sandbox e2b \
  -github-app-id APP_ID \
  -github-app-installation-id INSTALLATION_ID \
  -github-app-private-key /secure/pons-github-app.pem

go run ./cmd/pons client \
  -git-repository https://github.com/OWNER/repo-a.git -git-revision REPO_A_SHA \
  -message "Work in repo-a"
go run ./cmd/pons client \
  -git-repository https://github.com/OWNER/repo-b.git -git-revision REPO_B_SHA \
  -message "Work in repo-b"
go run ./cmd/pons client \
  -git-repository https://github.com/OWNER/repo-a.git -git-revision REPO_A_SHA \
  -message "Work independently in repo-a"
```

Each client command without `-conversation` creates an independent workspace.
Git sessions clone inside E2B; the host `-workspace` is not uploaded for them.
It defaults to the directory where the server starts and is used for local
configuration and as the seed only for archive sessions without a primary repo.
By default, Pons mints a one-hour token restricted to that conversation's
primary repository. For a cross-repository task, pass `-git-all-repositories`
when creating the conversation. This grants the agent access to every
repository available to the installation, including repositories it clones
during a run. A conversation without a primary repository can also use this
option and start from the archive seed.

The App private key stays on the host; the token is deliberately delegated
to unrestricted hands through its process
environment, so the agent can use, inspect, or copy it. Pons does not itself
write the token to the remote URL, Git configuration, workspace, checkpoint,
or sandbox-wide environment, and exact token values in tool results are
redacted before host persistence as an accidental-leak safeguard. This cannot
prevent an agent from transforming or deliberately persisting its authority.
The token is not refreshed during a run, so authenticated Git operations fail
after it expires (normally one hour); the next run receives a new token. This
`github_app/v1` authentication is separate from generic `git/v1` provisioning.
Pons does not depend on a centrally controlled shared App or forward SSH
credentials.

To remove retained pons sandboxes manually, run `make cleanup-e2b`; use
`./scripts/cleanup-e2b.sh --dry-run` to inspect matches first. The default is
scoped to the `pons-hands` template; `make cleanup-e2b-all` explicitly targets
all running and paused sandboxes visible to the API key.

The E2B API key remains on the host, internet access is disabled unless
`-sandbox-network` is set, and the local workspace is used only to seed a new
logical workspace. Bounded checkpoints are stored under the runtime state
directory after every run; the source checkout is never replaced. Workspace
metadata and sandbox placement are persisted separately in SQLite, so a
workspace survives sandbox deletion and can be restored in a new VM. The state
directory must remain outside the source workspace; pons rejects unsafe nested
configuration. The immutable base and latest checkpoint are retained while
superseded intermediate checkpoints are pruned for archive workspaces; Git
workspaces retain only the latest full checkpoint because their immutable base
is a Git object ID, not a checkpoint reference. Idle sandboxes are deleted after
`-sandbox-idle-timeout` (10 minutes by default).
With `--debug`, the server writes JSON logs for sandbox creation or reuse, Git
provisioning steps, checkpoint and recovery transitions, and idle cleanup.
Run logs include conversation and run IDs; E2B lifecycle logs include workspace
and sandbox IDs. Server logs contain run outcomes and answer lengths, not answer
text or tool results. Lifecycle logging does not include Git credentials or
authenticated command output. The interactive client receives progress for
its own run and can show full tool results with `--debug`. Server debug logs
include maintenance for all workspaces, including when the server and
interactive client run together. To keep server logs and the interactive
prompt in separate terminals, run `serve --debug` in one terminal and
`client -i` in another.
If hands stops cleanly but checkpointing fails, the sandbox is quarantined for
manual recovery for one hour, not deleted immediately. The error includes its
ID and deadline. New runs for that workspace are blocked during that window;
use E2B's dashboard or SDK to copy `/home/user/pons-workspace` to a safe location
before the deadline. Do not run the cleanup scripts on a sandbox being recovered.
There is no automatic checkpoint retry or early release command yet. After
expiry, cleanup removes the sandbox and future runs restore the last successful
checkpoint. If metadata or provider timeout updates also fail, the error warns
that the recovery window could be shorter. Unexpired active placements after
a crash are likewise protected until their recorded expiry.
The initial E2B backend does not support external plugin manifests.

External tools are enabled explicitly with a manifest; they are never
discovered implicitly. Example providers are in
[`examples/external-echo`](examples/external-echo) and
[`examples/external-echo-ts`](examples/external-echo-ts). See the
[external plugin protocol](docs/external-plugin-protocol.md) and
[hands environment design](docs/hands-environment-v1.md) for details.

Useful flags include `-workspace`, `-state-dir`, `-max-turns`,
`-compact-chars`, repeatable `-plugin`, `-plugin-path`, `-sandbox`,
`-hands-command`, `-e2b-template`, `-e2b-hands-path`,
`-git-repository`, `-git-revision`, `-git-all-repositories`, `-github-app-id`, `-github-app-installation-id`,
`-github-app-private-key`, `-sandbox-idle-timeout`, `-fallback`, and `-debug`.

## Configuration

Defaults can be stored in `~/.pons/config.json`; `.pons.json` in the workspace
overrides them, and explicit flags win. Configuration supports provider/model
settings, runtime limits, tool output limits, named provider slots, and
failover. Use repeatable `-fallback provider[:model]` flags for a temporary
failover chain.

For a reusable E2B server with one GitHub App installation, put the following
JSON in `~/.pons/config.json` (substitute your values and an absolute key path):

```json
{
  "provider": "codex",
  "environment": {
    "sandbox": "e2b",
    "e2b": {
      "template": "pons-hands",
      "api_key": "YOUR_E2B_API_KEY"
    },
    "github_app": {
      "app_id": 123456,
      "installation_id": 789012,
      "private_key": "/absolute/path/to/github-app.pem"
    }
  }
}
```

Keep the API key in this global file, not in a repository's `.pons.json`, and
run `chmod 600 ~/.pons/config.json` and `chmod 600` on the PEM. Pons rejects a
config containing `environment.e2b.api_key` if group or other users can read
it. The E2B template build and smoke test still use `E2B_API_KEY` in the shell;
this is a one-time setup step. Once the template exists, `go run ./cmd/pons serve` uses
the configured E2B and GitHub App settings. Client commands still select the
repository and revision per conversation.

Codex credentials are stored with restrictive permissions in
`~/.pons/auth.json`. Multiple named credentials and provider slots are
supported.

## Development

```sh
make check       # format, tests, vet, and golangci-lint
make test-race   # race-enabled tests
make test-integration
```

Install the local lint tool once if needed with `make install-tools`.
`make smoke-runtime` is a separate live-provider and Seatbelt check; it is not
part of the default suite.

Architecture decisions and protocol details live in [`docs/`](docs/),
including the [runtime design](docs/runtime-v1.md) and the
[ADR index](docs/).

## Dependencies

The project uses the official Anthropic and OpenAI Go SDKs, a pure-Go SQLite
driver, and the standard-library HTTP stack.

## Roadmap

- Stream text and partial tool-output observations through `OnEvent`.
- Add a local Linux execution-environment provider and incremental remote workspace sync.
- Support per-run environments in the long-lived runtime.
- Add branching and `go install`-able releases.
