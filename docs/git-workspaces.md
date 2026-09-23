# Git workspaces on E2B

Configure one Pons server with an E2B template and, for private repositories,
one GitHub App installation. Each client conversation chooses its own repository
and pinned commit. Conversations using the same repository have independent
branches, sandboxes, and checkpoints.

## 1. Build the E2B template

Install Node/npm, set `E2B_API_KEY` in your shell, and build the template once:

```sh
make e2b-template
make smoke-e2b
```

The template contains `pons-hands` and Git. The template build and smoke test
use the shell's `E2B_API_KEY`; the running server can instead read the key from
its config file.

## 2. Create a GitHub App for private repositories

Public repositories do not need an App. For private repositories:

1. [Register a GitHub App](https://docs.github.com/en/apps/creating-github-apps/registering-a-github-app/registering-a-github-app)
   owned by the account or organization that owns the repositories. Give it
   repository **Contents: Read and write** and **Metadata: Read-only**
   permissions. Pons uses an installation token for HTTPS Git access; no user
   authorization or device flow is needed.
2. Install the App on the repositories that Pons may access. GitHub lets you
   choose selected repositories or all repositories for the installation.
   Protect destination branches if the agent has write access.
3. Copy the **App ID** from the App settings page. Copy the **installation ID**
   from the installation settings URL (the number after `/installations/`).
   [Generate a private key](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/managing-private-keys-for-github-apps)
   and store its PEM file on the Pons host.
4. Restrict the key file to its owner: `chmod 600 /absolute/path/to/github-app.pem`.

The App is configured once for the server. Clients choose a repository for each
conversation; they do not repeat App credentials. Pons requests a fresh
installation token for each run. The default token is restricted to that
conversation's primary repository.

## 3. Configure and start the server

Put server settings in `~/.pons/config.json`:

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

Omit `github_app` when you only use public repositories. If you keep
`environment.e2b.api_key` in this file, Pons requires it to be a regular file
readable only by its owner:

```sh
chmod 600 ~/.pons/config.json
go run ./cmd/pons serve --debug
```

Keep the API key and App private key out of repository `.pons.json` files. A
standalone server reads the global config only. Use server flags to override
settings temporarily; repository and revision flags belong to the client.
For Codex, run `go run ./cmd/pons -login` once before starting
the server.

## 4. Create and resume conversations

In another terminal, supply a credential-free HTTPS URL and a full 40-character
commit ID. The commit must exist in the selected repository:

```sh
go run ./cmd/pons client -i \
  -git-repository https://github.com/OWNER/repo-a.git \
  -git-revision FULL_COMMIT_SHA
```

The client prints a conversation ID. Continue the same checkout with:

```sh
go run ./cmd/pons client -i -conversation CONVERSATION_ID
```

Start another client without `-conversation` for an independent checkout,
whether it uses `repo-a` again or a different repository. Git conversations
do not need a host `-workspace`. Pons fetches the pinned commit with its
ancestry and checks it out on `pons/<workspace>/work` without an upstream. The
agent can inspect history, commit, and push; to publish the branch it can run
`git push -u origin HEAD`.

For work that must clone or push **other** repositories in the same App
installation, add `-git-all-repositories` when creating the conversation:

```sh
go run ./cmd/pons client -i \
  -git-repository https://github.com/OWNER/repo-a.git \
  -git-revision FULL_COMMIT_SHA \
  -git-all-repositories
```

This grants hands access to every repository available to that installation.
You can also use `-git-all-repositories` without a primary Git repository and
start from a client-selected host workspace archive. The option requires a
configured GitHub App.

## Storage, logs, and recovery

Pons checkpoints the remote workspace after every run under its runtime state
directory (`~/.pons/runtime/server/` by default). Deleting an idle sandbox
after the default 10-minute timeout does not delete the workspace; the next
run restores its latest checkpoint in a new sandbox. A host workspace used for
an archive conversation is only an initial seed, never a destination for
checkpoint writes. Keep the runtime state directory outside that source path.

`serve --debug` writes structured lifecycle logs for all conversations,
including sandbox creation/reuse, Git provisioning, checkpoints, and idle
cleanup. Server logs include IDs and outcomes, not full responses or tool
results. `client -i --debug` shows progress and tool results for its own
conversation. Run them in separate terminals to keep the prompt readable.

If hands shutdown or checkpointing fails, Pons blocks new runs for that
workspace and retains the sandbox for manual recovery for up to one hour.
The error gives its sandbox ID and deadline. Copy
`/home/user/pons-workspace` through E2B's dashboard or SDK before that
deadline. There is no automatic retry; after expiry, the next run restores
the last successful checkpoint. Do not run cleanup on a sandbox you are
recovering.

To inspect retained Pons sandboxes before manual cleanup, run
`./scripts/cleanup-e2b.sh --dry-run`. `make cleanup-e2b` removes those scoped
to the `pons-hands` template; `make cleanup-e2b-all` targets all running and
paused sandboxes visible to the API key.

The App private key stays on the host. Pons passes a short-lived token to
unrestricted hands for authenticated Git commands, so an agent can inspect or
copy that token. The token normally expires after one hour and is refreshed
at the next run, not during a run. Pons does not place it in the remote URL,
Git config, checkpoint, or sandbox-wide environment, and redacts exact token
values from tool results before host persistence. This does not prevent an
agent from deliberately transforming or persisting its authority.

See the [remote workspace design](remote-workspace-provisioning.md) for the
storage and placement contracts.
