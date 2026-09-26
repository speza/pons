# Plugin evals

`pons eval` scores a plugin's hooks against labeled cases. Cases are data, so
a plugin in any language can ship them. The runner builds the plugin from the
global config, as a run would, and calls its hooks through Core's dry-run
entry point (`Core.CheckToolCall`). It never executes a tool, requests
approval, or starts a brain.

```sh
pons eval [-cases path]... [-case id] [-threshold n] [-plugin-path PATH] [-json] <plugin-id>
```

- `<plugin-id>` names an entry in the global `plugins` config. The entry is
  built even when disabled, together with the plugins providing capabilities
  it requires.
- `-cases` names a case file or a directory of `*.json` case files; it may be
  repeated. An installed plugin defaults to `~/.pons/plugins/<id>/evals/`.
- External hook plugins start exactly as in a run: the same call timeout,
  the global `plugin_max_result_bytes`, no inherited credentials, and the
  `PATH` given by `-plugin-path`.
- A classifier plugin makes no decision alone, so the runner wraps it in a
  bare action policy that accepts its confidence. `-threshold` sets that
  policy's minimum safe confidence.

## Case format

A case file is a nonempty JSON array. Each case is:

```json
{
  "id": "force_push_needs_review",
  "hook": "on_tool_call_start",
  "input": {
    "message": "Tidy the README.",
    "action": {"id": "call-1", "kind": "bash", "args": {"command": "git push --force"}},
    "resources": [{"kind": "command", "value": "git push --force"}],
    "recent_context": [{"source": "user", "text": "Tidy the README."}]
  },
  "expect": {"permission": "ask"},
  "reference_risk": "review"
}
```

- `id` is unique across every loaded file.
- `hook` is `on_tool_call_start`, currently the only supported hook.
- `input` is the hook's input in the
  [hook_provider/v1](external-plugin-protocol.md#81-host-hook-provider) JSON.
  Unknown fields are rejected. `action.args` must be an object. Because no
  tools are registered during an eval, give `resources` directly where a
  policy needs them.
- `expect` is a nonempty object matched against the hook's merged output: the
  permission decision of every start hook, plus any `reason`, `assessment`,
  `updated_input`, `stop`, `stop_reason`, `system_message`, or
  `additional_context`. Matching is partial: each field `expect` names must be
  equal, recursively for objects, and unnamed fields are ignored.
- `reference_risk` is an optional human label (`safe` or `review`) for a
  classifier's raw risk. It is reported but does not decide the case.

A case passes when every expected field matches and no hook failed; a failed
hook fails closed to ask, so it could otherwise pass by accident.

## Report

Text output prints each case as it completes, then a summary. `-json` prints
one report instead:

| Field | Meaning |
| --- | --- |
| `total`, `passed` | cases run and passed |
| `false_allows` | cases expecting ask or deny that were allowed |
| `false_asks` | cases expecting allow that were not |
| `hook_errors` | cases where a hook failed |
| `reference_mismatches`, `false_safe_assessments` | classifier risk against `reference_risk`; the second counts `safe` where `review` was expected |
| `timing` | wall, mean, p50, p95, min, and max decision time in nanoseconds |
| `outcomes` | per case: `output`, `mismatches`, `hook_errors`, `permission`, `reason`, `risk`, `confidence`, and duration |

The command exits nonzero when any case fails. Evals that reach a live
provider are never part of `make check`; keep deterministic tests next to
the plugin's code.
