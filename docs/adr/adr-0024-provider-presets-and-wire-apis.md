# ADR-0024: Provider presets over shared wire APIs

**Status:** Accepted
**Implementation:** Implemented
**Date:** 2026-09-27
**Related:** ADR-0013

## Context

A provider name used to select both a wire format and a host: `openai`
meant Chat Completions at api.openai.com, `openai-responses` meant the
Responses API there, and `codex` meant Responses with a ChatGPT login.
Gateways break that pairing. OpenCode Go serves each model over Chat
Completions, Anthropic Messages, or Responses behind one key; OpenRouter
serves every vendor's models over Chat Completions. Adding each gateway as
its own switch case would duplicate key, header, and endpoint handling per
format.

pi-ai separates the two: an API is one wire implementation, and a provider
is data (base URL, credentials, headers, and the API each model speaks).

## Decision

### 1. Three wire APIs, named as pi-ai names them

`anthropic-messages`, `openai-completions`, and `openai-responses` are the
adapters in `plugins/brain/llm`. They translate the brain's turns and know
nothing about hosts or credentials.

### 2. Providers are presets

A provider is a preset: default base URL, key environment variable,
default model, default API, and optionally extra headers. Presets today
are `anthropic`, `openai`, `codex`, `opencode-go`, and `openrouter`. Any OpenAI- or Anthropic-compatible
server is `openai` or `anthropic` with a `base_url`. `openai-responses` is
no longer a provider; it is `openai` with `api: openai-responses`.

### 3. A generated catalog picks the API per model

`catalog_gen.go` lists, for presets whose models do not share one API, the
API each model speaks. `make catalog` regenerates it from models.dev, the
catalog OpenCode itself routes by, mapping each model's AI SDK package to
a pons API. Only `opencode-go` needs it today. Models missing from the
catalog use the preset's default.

### 4. Any slot may override the API

Provider entries, fallbacks, the flat config, and the `-api` flag accept
`api`. An empty value uses the catalog, then the preset default. `codex`
accepts only `openai-responses`, since its login is for that endpoint.

### 5. pons owns credentials

Adapters are built without the SDKs' environment defaults (Anthropic) or
with their account headers removed (OpenAI). Only the key pons resolved
for the slot is sent, so an `ANTHROPIC_AUTH_TOKEN` or `OPENAI_ORG_ID` in
the environment never reaches a gateway.

### 6. Conversation identity reaches the provider

The runtime passes the conversation ID to the brain as a session ID.
Presets may send it; OpenCode Go requires `x-opencode-session` for routing
and prompt caching.

## Alternatives considered

- **Guess the API from the model family:** rejected. It is wrong whenever
  a family is split (OpenCode serves `qwen3.8-flash` over Anthropic
  Messages but `qwen3.8-max` over Chat Completions), and OpenCode's docs
  and models.dev disagree on some models; the client that works in
  practice follows models.dev.
- **Fetch models.dev at startup:** rejected; startup would need the
  network, and a checked-in catalog is reviewable in diffs.
- **Per-model compatibility flags (reasoning format, token-limit field),
  as pi-ai has:** deferred until pons sends a parameter some host rejects.
  pons sends no reasoning parameters, and the gateways here accept
  `max_completion_tokens`.

## Consequences

- A new gateway is a preset entry, not a new code path; a mixed-API
  gateway also needs a line in `gen_catalog.go`.
- New models reach the catalog only when someone runs `make catalog`;
  until then they use the preset default or an explicit `api`.
- `base_url` is the provider's API root as its docs give it (usually
  ending in `/v1`); the Anthropic adapter drops a trailing `/v1` because
  its SDK adds one.
- `OPENAI_ORG_ID` and `OPENAI_PROJECT_ID` are no longer honoured for
  OpenAI itself.
