# pons web client

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

Run `make web-build` after editing the frontend. The browser client and runtime
API are still under development.

Sandbox conversations show an environment setup log below the message that
started each run. It opens while the run is active and remains available after
reconnecting or reopening the conversation.
