# ADR-0013: Named provider slots with a single auth store

**Status:** Accepted
**Date:** 2026-09-17
**Related:** ADR-0001

## Context

The brain originally configured one provider (a flat
provider/model/base_url plus one credential file). Supporting several
subscriptions of the same kind (two ChatGPT accounts, an Anthropic key
alongside) requires a way to name credentials and order providers without
spreading credential paths through configuration.

Provider outages and rate limits are normal operating conditions for a
turn-based harness; a run should survive them when another configured
provider can serve the same conversation.

## Decision

### 1. The provider chain is a list of named slots

A config file may declare `providers` (a list of
`{id, provider, model, base_url, api_key}` entries) and
`default_provider_id`. The default entry is the primary slot; the
remaining entries back it up in listed order. Entries of the same provider
type are allowed. The flat `provider`/`model`/`base_url` keys and the
`providers` list are mutually exclusive, and several providers without a
`default_provider_id` fail composition — ambiguity must be resolved
loudly, not silently.

### 2. Failover retries the turn on the next slot

The brain holds the conversation as provider-agnostic turns, so a
mid-run provider switch replays the same history on the next provider.
Any `Complete` error moves to the next slot; caller cancellation and a
provider-reported cancellation abort the chain. All slots failing returns
the last error. Fallback is per turn, not per token: a provider that dies
mid-stream fails that turn and the next turn moves on.

### 3. One auth store, entries keyed by id

Codex OAuth credentials live in `~/.pons/auth.json` (0600), a JSON object
mapping auth ids to credentials. A provider entry's `id` names its
credential; `--login -as <id>` writes one, and a bare `--login` writes the
singular `codex` entry. A store with exactly one entry stands in for any
id, so single-subscription setups need no naming. The legacy
single-credential file shape reads as the `codex` entry and upgrades in
place on the next refresh. Saves rewrite the whole store under an advisory
file lock so concurrent pons processes do not clobber each other's tokens.

### 4. Precedence: flags > project config > global config > defaults

Durable settings live in `~/.pons/config.json` (global defaults). Bundled mode
also reads `.pons.json` in its current directory (project overrides, per key).
A standalone server loads only global settings because each conversation may
select a different workspace. Explicit flags win over file settings. Decoding
is strict — unknown keys, trailing data, and wrong types fail startup.

## Alternatives considered

- **Per-credential auth files referenced by path:** rejected — the id
  would live in the config and the credential in a file, keeping two
  names in sync for no capability gain; the store file is pons-wide
  policy, not per-provider configuration.
- **Single credential per store with per-entry paths:** superseded by
  Decision 3; sharing one credential across differently-named entries has
  no real use (same sub, same rate limits).
- **Retry the primary before failing over:** rejected; the SDK-level
  retries are deliberately zero and failover already re-serves the same
  conversation, so a primary retry adds latency without a different
  failure mode.

## Consequences

- Credential wiring is `--login -as <id>` plus an entry with the matching
  id; nothing else.
- A provider switch mid-run is invisible to the brain except through the
  log line naming the slot that took over.
- The auth store is a coordination point: its lock serializes refreshes
  across processes, and its shape is the only credential schema.

## References

- `plugins/brain/llm/llm.go` — `Fallback`, `failoverClient`, `providerClient`
- `plugins/brain/llm/codexauth.go` — the auth store, resolution, locking
- `cmd/pons/config.go`, `cmd/pons/provider.go` — config schema and slot resolution
- `README.md` — user-facing flags, config keys, and login flow
