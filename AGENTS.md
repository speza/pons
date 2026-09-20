# AGENTS.md

## Project

`pons` is a small Go agent harness. Keep the brain/hands separation intact:
brains plan through `protocol.Action`; hands execute through tool plugins; the
core owns orchestration, not task-specific behavior.

## Checks

The module targets Go 1.25 (`go.mod`). Before submitting changes, run:

```sh
make check       # format check, tests, vet, golangci-lint
make test-race   # concurrency-sensitive test pass
```

Install the local lint tool once if needed:

```sh
make install-tools
```

Optional commit hooks:

```sh
pre-commit install
pre-commit run --all-files
```

Tests do not require API keys. Do not add network- or provider-dependent tests
to the default suite.

## Go conventions

- Run `gofmt` on changed Go files.
- Keep `gofmt` authoritative, but manually expand dense composite literals,
  function calls, and signatures across lines when a one-line form obscures
  their structure. Do not enforce a mechanical line-length limit.
- Prefer standard-library APIs supported by the module's Go version.
- Treat `go vet`, `staticcheck`, and `modernize` diagnostics as actionable;
  use a narrowly documented exclusion when a wire-format or public-contract
  change is intentional.
- The project is pre-compatibility: do not retain legacy APIs, storage paths,
  flags, or wire shapes solely for backward compatibility. Prefer the clean
  current architecture, remove superseded code, and update protocol tests and
  relevant ADRs whenever a contract changes.
- New capabilities belong in plugins and must be registered through `Core`;
  do not put tool-specific execution or policy in the brain/core loop.
- External plugins are an untrusted hands boundary: preserve explicit loading,
  resource limits, cancellation, and credential isolation.

## Repository layout

- `protocol/`: dependency-free brain/hands wire types
- `core.go`: orchestration and plugin registry
- `plugins/`: execution, brains, and external adapters
- `runtime/`: server orchestration, storage contract, and persistence
- `internal/`: shared implementation helpers
- `docs/`: architecture decision records and protocol documentation
- `cmd/`: user-facing binaries

Keep tests close to the package they cover, and update `README.md` when a
user-facing command or flag changes.
