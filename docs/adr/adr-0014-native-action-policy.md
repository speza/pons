# ADR-0014: Native action policy and classifier-backed auto mode

**Status:** Proposed
**Implementation:** Not implemented
**Date:** 2026-09-18
**Related:** ADR-0001, ADR-0004, ADR-0006, ADR-0007, ADR-0021

## Context

pons has execution seams that policy implementations can use, but it does not
currently have a native tool-call-start hook. Tool-specific guards exist
(`fs` path containment and the `shell` command allowlist), and `WrapTool` can
intercept execution, but there is no common way to decide whether an action
may run before any hands execute it.

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

Add a native **tool-call-start hook** to `Core`. The built-in action-policy
plugin registers it through `Core.AddHooks(Hooks{OnToolCallStart: ...})`; policy
rules, classification, and user interaction stay in plugins or the composing
application. The same registration value accepts typed agent, step, tool,
and approval callbacks. Each callback can observe or apply the effects its
event supports. `OnToolCallStart` is the pre-execution decision point.

### 1. Tool-call-start hooks are control-plane checks, not tools

The core owns when hooks run, how their decisions combine, how an action is
held for approval, and how a final denial is represented. It does not know why
an action is dangerous, who the user is, or how a prompt is presented.

The native concepts are intentionally small:

```go
type ActionDisposition string

const (
    DispositionAllow ActionDisposition = "allow"
    DispositionAsk   ActionDisposition = "ask"
    DispositionDeny  ActionDisposition = "deny"
)

type ActionAssessment struct {
    Risk       string  // "safe" or "review"
    Confidence float64
    ProbabilityConfidence bool // returned choice probability, not generated text
    ReasonCode string  // stable, non-prose policy reason
    Classifier string  // e.g. "jev-1.13"
}

type ToolCallStartEvent struct {
    Step      int
    Message   string // current user/task instruction, not the full transcript
    RecentContext []ActionContextItem // bounded, source-labeled history
    Workspace string
    Action    protocol.Action
    Tool      *ToolSpec
    Resources []ToolResource
    Decision  ActionDecision // zero value observes and allows
}

type ApprovalRequest struct {
    ToolCallStartEvent
    Assessment ActionAssessment
}

type ApprovalHandler func(context.Context, ApprovalRequest) (ActionDisposition, error)
type Hooks struct {
    OnToolCallStart func(context.Context, *ToolCallStartEvent) error
    // Other event-specific callbacks may be registered alongside it.
}
```

The exact exported names may change during implementation. The semantics are
the contract:

- a classifier returns an assessment, not a permission grant;
- the action-policy plugin maps the assessment to `Allow`, `Ask`, or `Deny`;
- multiple hooks combine with `Deny` before `Ask` before `Allow`, independent
  of registration order;
- matching hooks run in registration order and collect errors; a tool-call-start
  error requests approval, while approval-hook errors stop execution;
- a hook may replace tool arguments; every start hook reevaluates the changed
  call, and invalid or repeatedly changing arguments are denied;
- `Ask` invokes an application-supplied approval handler or ADR-0021's
  durable pause adapter;
- an approval handler may return only `Allow` or `Deny` for the exact pending
  action; and
- `Deny` is final for that invocation.

The core does not own a terminal, GUI, user identity, or persistence format.
An explicitly installed host-side external plugin may also implement
`on_tool_call_start` through the versioned `hook_provider/v1` protocol. It
receives the policy event but no Core handle or inherited credentials; only
its decision is applied to the pending call.
An interactive CLI supplies an approval handler; an embedding application may
block, display a request elsewhere, or bridge it to an asynchronous UI. A
non-interactive caller may use ADR-0021's durable pause and exact-action
resume. Without a handler or durable pause policy, `Ask` is denied rather
than silently allowed.

### 2. Action policy happens before tool execution

For every non-`finish` action in a plan, the core evaluates actions in plan
order before launching any approved tool handler. This gives interactive
clients deterministic prompts and ensures no sibling action executes while a
user is deciding about an earlier action.

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

### 3. Policy precedence

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

### 4. Classifier input and trust context

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

### 5. Configuration trust

User/global or managed configuration may loosen policy. Project-local
configuration may add restrictions but must not remove user/global hard denies
or broaden action policy. Repository content is not an authority merely
because the agent can read or modify it.

The eventual configuration format should support tool/resource-scoped
`allow`, `ask`, and `deny` rules, with explicit precedence and stable matching
semantics. Natural-language classifier context may supplement these rules but
must not replace hard policy.

### 6. Observability and response

Action policy decisions are observable as loop events and in the step result
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

### 7. Jev and supported-model integration

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
- **An external policy capability:** not added to runtime protocol 1. The
  host-side tool-call-start hook must remain outside the untrusted hands plugin
  boundary.

## Consequences

- The core gains a small control-plane concept and deterministic preflight.
- Existing tools and callers remain unchanged when no action policy is
  composed.
- Jev, supported LLMs, and deterministic policies can share one assessment
  contract.
- Same-step tool execution remains concurrent after action policy completes.
- Interactive applications have an exact-action approval/override path.
- Non-interactive applications must choose an explicit response to `Ask`.
- `fs` and `edit` can enforce workspace paths in-process; `bash` remains
  unrestricted unless its deployment supplies a real sandbox or a future
  restricted executor.
- Native action policy is not an OS security boundary and cannot protect
  against malicious in-process plugins.

## Open implementation questions

- The initial implementation exposes `Core.AddHooks` and
  `Core.SetApprovalHandler`; `plugins/actionpolicy` registers a hook that
  composes ordered policy rules and classifier fallbacks. Multiple trusted
  hooks compose with deny before ask before allow. Approval is a synchronous
  callback.
- Core emits `action_decision`, `approval_request`, `approval_resolved`, and
  `action_denied` events. A denied call does not emit `action_end`; the runtime
  persists it with tool status `denied` and a failed result. CLI rendering and
  a remote pending-token/resume API remain open.
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
  The OpenAI adapter's confidence is generated text, so its safe result
  requires approval unless the host explicitly enables
  `AllowGeneratedConfidence`.
- CLI configuration selects one enabled classifier by registered plugin ID,
  along with its model, key environment variable, timeout, and confidence
  behavior. An interactive CLI approval
  handler and per-action concurrency limits remain open.
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
