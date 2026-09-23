# pons web testing UI

This UI exists to exercise and inspect the HTTP/SSE runtime during web
development and testing. It is not intended for use outside that workflow.
The Go server embeds the built frontend. From the repository root, run:

```sh
make web-install
make web-build
go run ./cmd/pons serve --debug
```

Open `http://127.0.0.1:7337/` and choose a sandbox and source. E2B Git
conversations use a credential-free HTTPS repository URL and either a branch
name or full 40-character commit SHA. Pons creates a separate work branch from
the branch tip or selected commit. E2B can also
upload a host directory. In-process and Seatbelt conversations require an
absolute host path inside `workspace_root` (the server user's home directory by
default). Configure a brain provider first; see the [root
README](../README.md#quick-start).

Run `make web-build` after editing the frontend. The browser UI and runtime API
are still under development.

Sandbox conversations show an environment setup log below the message when E2B
is first provisioned or a workspace is restored into a replacement sandbox.
Warm runs that reconnect to an existing sandbox skip the setup log. Saved setup
logs reopen expanded after a refresh, keep their full height as the conversation
grows, and can be collapsed manually.
The conversation header links back to the first setup log when later messages
have moved it out of view.
Failed runs show their saved error beneath the message that started the run,
including after a refresh.
