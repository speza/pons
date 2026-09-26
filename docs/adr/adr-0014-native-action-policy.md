# ADR-0014: Native action policy and classifier-backed auto mode

**Status:** Accepted
**Implementation:** Partial — hooks, action policy, classifier adapters, config, external host hooks, and evals implemented; interactive CLI approval and durable resume remain open
**Date:** 2026-09-18
**Related:** ADR-0001, ADR-0004, ADR-0006, ADR-0007, ADR-0021

## Context

Before this decision, pons had execution seams that policy implementations
could use, but no native tool-call-start hook. Tool-specific guards existed
(`fs` path containment and the `shell` command allowlist), and `WrapTool` could
intercept execution, but there was no common way to decide whether an action
could run before any hands executed it.

This matters for a provider-style auto mode. The desired default is not a
manual prompt for every operation: a classifier should allow routine actions,
route uncertain or risky actions to an approval mechanism, and leave explicit
hard policy violations blocked. Jev is a particularly suitable classifier,
but supported LLMs and deterministic implementations should fit the same
seam.

A classifier is not a security boundary. In particular, shell commands can
execute code or access paths that are difficult to infer from command text.
Actual workspace containment and host isolation remain tool/deployment
responsibilities under ADR-0004.

## Decision

Add typed lifecycle **hooks** to `Core`, modelled on coding-agent hooks such
as Claude Code's, with `OnToolCallStart` as the pre-execution permission
point. The built-in action-policy plugin is an ordinary `OnToolCallStart`
hook; policy rules, classification, and user interaction stay in plugins or
the composing application.

### 1. One hook contract

Every hook takes an `Input` value and returns an `Output` that embeds a common
`HookOutput`, plus at most one event-specific decision:

```go
type HookOutput struct {
    Stop              bool   // end the run once the current step is recorded
    StopReason        string
    SystemMessage     string // shown to the user (EventSystemMessage)
    AdditionalContext string // added to the brain's next observation
}

type Hooks struct {
    OnAgentStart        func(context.Context, AgentStartInput) (AgentStartOutput, error)
    OnAgentTurnStart    func(context.Context, AgentTurnStartInput) (AgentTurnStartOutput, error)
    OnAssistantResponse func(context.Context, AssistantResponseInput) (AssistantResponseOutput, error)
    OnToolCallStart     func(context.Context, ToolCallStartInput) (ToolCallStartOutput, error)
    OnPermissionRequest func(context.Context, PermissionRequestInput) (PermissionRequestOutput, error)
    OnToolCallEnd       func(context.Context, ToolCallEndInput) (ToolCallEndOutput, error)
    OnAgentTurnEnd      func(context.Context, AgentTurnEndInput) (AgentTurnEndOutput, error)
    OnAgentEnd          func(context.Context, AgentEndInput) (AgentEndOutput, error)
}
```

| Hook | Event-specific output |
| --- | --- |
| `OnToolCallStart` | `Permission` (allow, ask, deny; empty means no objection), `Reason`, `Assessment`, `UpdatedInput` |
| `OnPermissionRequest` | `Permission` (allow or deny; empty defers) |
| `OnToolCallEnd` | `Result *protocol.ToolResult` (nil leaves the result unchanged) |
| `OnAgentEnd` | `Continue`, `Reason`: keep a finished run going with the reason as context |
| others | none |

Every Output is its own named type, so a hook can gain a field without a
signature change. There is no matcher: a Go hook checks `Action.Kind` itself,
and external plugins declare a tool filter (section 5).

Inputs and outputs carry snake_case JSON tags and are the external wire
format, so a Go field rename cannot silently change the protocol.

### 2. Permission flow

```text
OnToolCallStart (every hook, per call) → merge strictest: deny > ask > allow
  UpdatedInput → validate object → every start hook runs again on the new call
ask → OnPermissionRequest (every hook, in order; any deny wins)
    → unresolved → SetApprovalHandler → no handler or anything but allow → deny
```

- A classifier returns an assessment, not a permission grant; the
  action-policy plugin maps it to allow or ask.
- `Deny` is final for that invocation; approval never sees it.
- `UpdatedInput` replaces the call's arguments. Non-object updates, and calls
  that keep changing after eight passes, are denied. Core records the updated
  call in the durable assistant response and reconciles a stateful brain
  (`AssistantResponseReconciler`) before execution, so the brain's transcript
  always matches what ran.
- `ApprovalHandler` receives the same `PermissionRequestInput`. Without a
  handler or durable pause policy, an unresolved `Ask` is denied.

### 3. Hook errors

A hook error is reported through `EventHookError` and discards that hook's
output; it never stops the run by itself. Hooks that decide fail closed:

- `OnToolCallStart`: the call asks for approval, except that the failed
  hook's own `Deny` still holds;
- `OnPermissionRequest`: the call is denied;
- `OnToolCallEnd`: the result is withheld from the brain.

A hook stops the run deliberately with `Stop`. Checked persistence that must
not fail silently belongs in `Core.OnEventError`, not in hooks.
`OnAgentTurnEnd` and `OnAgentEnd` close every opened turn and run, including
failed and canceled ones, on a context that is not canceled.

### 4. Hooks are trusted user code

Like Claude Code's hooks, pons hooks are trusted: the operator installs them,
and they may approve calls, update input, and change results. The core does
not own a terminal, GUI, user identity, or persistence format. An interactive
CLI supplies an approval handler; an embedding application may block, display
a request elsewhere, or bridge it to an asynchronous UI. A non-interactive
caller may use ADR-0021's durable pause and exact-action resume.

### 5. External hook plugins

An explicitly installed host plugin implements hooks through the versioned
`hook_provider/v1` protocol with the same inputs and outputs in JSON. It may
declare `tools` so the host skips its tool hooks for other tool kinds. Its
effects match an in-process hook's, but the process boundary still applies:
no Core handle or inherited credentials, host deadlines and frame limits, and
inputs without run history and with tool output capped at 16 KiB.

### 6. Action policy happens before tool execution

For every non-`finish` action in a plan, the core decides every action before
launching any approved tool handler. Start hooks for different actions run
concurrently, so classifier latency does not add up across a turn; approvals
are then requested one at a time in plan order. This gives interactive clients
deterministic prompts and ensures no sibling action executes while a user is
deciding about an earlier action.

After preflight:

- allowed actions execute concurrently as required by ADR-0006;
- denied actions do not reach their handlers;
- denied actions receive a `protocol.ToolResult` with `OK:false`, the original
  `ActionID` and `Kind`, and a stable policy-denied error plus safe reason
  code; and
- results, including denials, remain in the original action order.

A denial applies only to that action invocation. The run may continue so the
brain can propose a safer alternative. The core should track repeated denied
attempts and circuit-break a prompt loop rather than repeatedly asking about
an identical or equivalent action.

With no tool-call-start hook installed, current behavior is unchanged.

### 7. Policy precedence

The effective decision follows this order:

```text
hard deny → explicit ask → explicit allow → classifier assessment → ask/allow
```

- **Hard deny** is deterministic and cannot be overridden by the classifier or
  user approval.
- **Explicit ask** always routes the exact action to the approval handler.
- **Explicit allow** bypasses classification for the matching trusted action.
- Otherwise, the configured classifier assesses the action and local policy
  maps its risk and confidence to `Allow` or `Ask`.

Classifier uncertainty, classifier failure, and classifier timeout never
become `Allow`. A configured classifier chain may try Jev first and a
supported-model classifier second; if no classifier can decide, interactive
runs may ask the user and non-interactive runs deny.

The classifier should not normally produce `Deny`. Hard denial belongs to
explicit deterministic policy. This keeps model judgments advisory while
retaining an unoverrideable policy mechanism.

`Allow` applies to the exact pending invocation. It does not create a
persistent grant. Broader/session/project grants are configuration features,
not approval decisions.

### 8. Classifier input and trust context

The initial classifier request includes:

- the current user/task instruction;
- bounded recent user and assistant messages, prior actions, and their results,
  each labeled with its source;
- the action kind and typed arguments;
- the registered tool description and schema;
- tool-owned policy resources, such as a path, command, URL, or target; and
- the workspace and trusted-environment configuration.

It does not include hidden model reasoning or the full transcript by default.
The recent context is needed to interpret follow-ups such as “push it” after a
specific proposed action was discussed. It is bounded by item count and text
size. Prior tool output is evidence, but having been observed by the agent
does not make a destination, file, or instruction trusted. Only messages with
user provenance can express user intent; tool output remains untrusted data.
The classifier evaluates each newly proposed action against the follow-up and
recent context. An unclear or changed action must be asked about again; the
follow-up is not a persistent grant.

Tools may expose a policy-facing resource projection rather than requiring the
core to parse arbitrary argument objects:

```text
read_file / write_file / edit_file → path
bash                         → command
web_fetch                    → URL
```

Generic tools fall back to their typed arguments. A resource projection helps
configuration express boundaries such as “ask outside the workspace,” but it
is not a substitute for OS-level containment and cannot make arbitrary shell
syntax safe.

### 9. Configuration trust

User/global or managed configuration may loosen policy. Project-local
configuration may add restrictions but must not remove user/global hard denies
or broaden action policy. Repository content is not an authority merely
because the agent can read or modify it.

The eventual configuration format should support tool/resource-scoped
`allow`, `ask`, and `deny` rules, with explicit precedence and stable matching
semantics. Natural-language classifier context may supplement these rules but
must not replace hard policy.

### 10. Observability and response

Action policy decisions are observable as loop events and in the turn result
history. The event API should distinguish an action denied before execution
from one that reached `EventActionStart`.

A denied tool result should be model-facing but stable, for example:

```text
tool call was not allowed (reason: external_directory)
```

Raw classifier prose is not included by default; it may leak policy details or
become another prompt-injection channel. UI/audit integrations may retain a
bounded classifier explanation separately.

A denied result is an unsuccessful observation, not a Go execution error. The
brain may adapt and retry with a materially different, safer action. Repeated
identical denials should be bounded by a circuit breaker.

### 11. Jev and supported-model integration

The action-policy plugin uses a classifier without making it a model-callable
tool:

1. apply deterministic local policy;
2. build a bounded structured state from the request;
3. ask Jev or a supported-model classifier for an assessment;
4. map risk and confidence to `Allow` or `Ask`;
5. return a hook decision; and
6. let the core invoke the approval handler for `Ask`.

The classifier is intended to replace routine manual approval in auto mode,
not to replace user control or OS isolation. Commands, arguments, and task
content sent to a hosted classifier are an explicit data-egress boundary.

`WrapTool` remains useful for post-execution result classification, redaction,
and middleware that needs to observe the actual result. The native
tool-call-start hook is for preflight decisions and approval.

## Alternatives considered

- **Only `WrapTool`:** rejected for interactive approval because checks
  would occur inside concurrently launched tool calls and the core could not
  provide deterministic preflight semantics.
- **An approval prompt in `Core`:** rejected because the core should not own a
  terminal, GUI, user identity, or persistence format. It owns the callback
  seam, not presentation.
- **A full permissions/RBAC system:** deferred; per-action action policy does
  not require an identity model.
- **Putting the decision in the brain:** rejected. Model output, including
  `Action.Danger`, is advisory and cannot approve its own execution.
- **A classifier-only security boundary:** rejected. Jev and other models can
  misclassify shell semantics, prompt injection, and data exfiltration.
- **A separate permission-policy seam beside observer-only hooks:**
  considered. It separates deciding from observing, but departs from the hook
  model users know from coding agents and splits one lifecycle across two
  mechanisms. A single hook contract with a shared output envelope keeps the
  hooks consistent while `OnToolCallStart` and `OnPermissionRequest` carry the
  permission decisions.
- **Hooks that mutate their event in place:** replaced by returned Outputs, so
  a hook's possible effects are visible in its signature.
- **External hook plugins in the untrusted hands boundary:** rejected. Host
  hooks run outside the hands sandbox and are installed by the operator as
  trusted code; hands plugins remain untrusted.

## Consequences

- The core gains a small control-plane concept and deterministic preflight.
- Existing tools and callers remain unchanged when no action policy is
  composed.
- Jev, supported LLMs, and deterministic policies can share one assessment
  contract.
- Same-turn tool execution remains concurrent after action policy completes.
- Interactive applications have an exact-action approval/override path.
- Non-interactive applications must choose an explicit response to `Ask`.
- `fs` and `edit` can enforce workspace paths in-process; `bash` remains
  unrestricted unless its deployment supplies a real sandbox or a future
  restricted executor.
- Native action policy is not an OS security boundary and cannot protect
  against malicious in-process plugins.

## Implementation notes and remaining work

- The implementation exposes `Core.AddHooks` with the eight hooks above and
  `Core.SetApprovalHandler`; `plugins/actionpolicy` registers an
  `OnToolCallStart` hook that composes ordered policy rules and classifier
  fallbacks. Approval is a synchronous callback. `HookOutput.AdditionalContext`
  reaches the brain as `protocol.Observation.Context`; the LLM brain adds it
  to the next user turn and does not persist it across runs.
- Core emits one `action_decision` event per call with the final decision
  after approval, and `action_denied` instead of `action_end` for a denied
  call. The runtime persists a denial with tool status `denied` and a failed
  result, and index rebuild accepts that status. CLI rendering and a remote
  pending-token/resume API remain open.
- Three denials of the same action kind and JSON arguments within one run
  suppress further approval requests for that action. A materially different
  argument set is assessed afresh.
- Optional TypeSafe and OpenAI classifier adapters are available through
  `plugins/actionpolicy`; trusted host code or global `~/.pons/config.json`
  configures them. The OpenAI
  adapter defaults to `gpt-6-luna` and accepts a compatible model through
  `RemoteConfig.Model`. The TypeSafe adapter defaults to `jev-latest` and also
  accepts a model name through `RemoteConfig.Model`. Jev's choice confidence
  is a returned probability.
  The OpenAI and Codex adapters' confidence is a model-generated estimate
  rather than a calibrated probability, so their `safe` results request
  approval unless the policy sets `AllowGeneratedConfidence`
  (`allow_generated_confidence`). A safe returned-probability result at or
  above the threshold allows the call; review and low-confidence results
  request approval.
- `pons eval <plugin-id>` evaluates a configured plugin's hooks against
  labeled cases through `Core.CheckToolCall`, a dry run of the start hooks
  without approval or execution. Cases are data in the hook_provider/v1 JSON
  (`docs/reference/plugin-evals.md`), so external plugins can ship evals in
  their `evals/` directory. When every classifier fails, `action_policy`
  still asks but reports the outage as a hook error, which fails an eval case
  and logs a warning in a run.
- Configured rules match tool kinds (`"*"` for all) with deny, ask, or allow,
  applied before classifiers. A policy may be rules-only; a call that no rule
  settles and no classifier assesses requests approval.
- CLI configuration selects one enabled classifier by registered plugin ID,
  along with its model, key environment variable, timeout, and confidence
  behavior. An interactive CLI approval
  handler and per-action concurrency limits remain open.
- Subscription-backed classifier provider slots resolve from global host
  configuration and explicit CLI flags, so project settings cannot redirect
  policy requests or credentials.
- The global `plugins` list gives each plugin a registered `id`, a checked
  semver config/API `version`, `enabled`, and plugin-owned `config`. Duplicate
  IDs and unsupported versions fail startup. Enabled plugins declare provided
  and required capabilities; the CLI preserves list order except where a
  dependency must come first, then applies trusted host plugins through `Core.Use`.
  Classifier plugins register a named classifier capability, which the action
  policy plugin resolves during `Setup`. External hands manifests use the same
  config registry but retain the separate, resource-limited process boundary.
- The exported resume API for ADR-0021's persisted pending plan remains open;
  directly attended approval currently uses a synchronous callback.
