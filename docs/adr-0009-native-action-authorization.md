# ADR-0009: Native action authorization and classifier-backed auto mode

**Status:** Proposed
**Date:** 2026-09-18
**Related:** ADR-0001, ADR-0004, ADR-0006, ADR-0007

## Context

pons has execution seams that policy implementations can use, but it does not
currently have a native authorization stage. Tool-specific guards exist
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

Add a native **pre-execution authorization stage** to `Core`, while keeping
policy rules, classification, and user interaction in plugins or the
composing application.

### 1. Authorization is a control-plane stage, not a tool

The core owns when authorization runs, how an action is held for approval, and
how a final denial is represented. It does not know why an action is
dangerous, who the user is, or how a prompt is presented.

The native concepts are intentionally small:

```go
type AuthorizationAction string

const (
    AuthorizationAllow AuthorizationAction = "allow"
    AuthorizationAsk   AuthorizationAction = "ask"
    AuthorizationDeny  AuthorizationAction = "deny"
)

type ActionAssessment struct {
    Risk       string  // classifier-defined category, e.g. "safe" or "dangerous"
    Confidence float64
    ReasonCode string  // stable, non-prose policy reason
    Classifier string  // e.g. "jev-1.13"
}

type AuthorizationRequest struct {
    Turn      int
    Message   string // current user/task instruction, not the full transcript
    Workspace string
    Action    protocol.Action
    Tool      *ToolSpec
    Resources []ToolResource
}

type ApprovalRequest struct {
    AuthorizationRequest
    Assessment ActionAssessment
}

type ApprovalHandler func(context.Context, ApprovalRequest) (AuthorizationAction, error)
```

The exact exported names may change during implementation. The semantics are
the contract:

- a classifier returns an assessment, not a permission grant;
- local policy maps the assessment to `Allow`, `Ask`, or `Deny`;
- `Ask` invokes an application-supplied approval handler;
- an approval handler may return only `Allow` or `Deny` for the exact pending
  action; and
- `Deny` is final for that invocation.

The core does not own a terminal, GUI, user identity, or persistence format.
An interactive CLI supplies an approval handler; an embedding application may
block, display a request elsewhere, or bridge it to an asynchronous UI. A
non-interactive caller must provide an explicit policy for `Ask`; absent one,
the action is denied rather than silently allowed.

### 2. Authorization happens before tool execution

For every non-`finish` action in a plan, the core authorizes actions in plan
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

With no authorization policy installed, current behavior is unchanged.

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
- the action kind and typed arguments;
- the registered tool description and schema;
- tool-owned policy resources, such as a path, command, URL, or target; and
- the workspace and trusted-environment configuration.

It does not include hidden model reasoning or the full transcript by default.
Prior tool output may be useful evidence in a future bounded context, but
having been observed by the agent does not automatically make a destination,
file, or instruction trusted. Tool output remains untrusted data.

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
or broaden authorization. Repository content is not an authority merely
because the agent can read or modify it.

The eventual configuration format should support tool/resource-scoped
`allow`, `ask`, and `deny` rules, with explicit precedence and stable matching
semantics. Natural-language classifier context may supplement these rules but
must not replace hard policy.

### 6. Observability and response

Authorization decisions are observable as loop events and in the turn result
history. The event API should distinguish an action denied before execution
from one that reached `EventActionStart`.

A denied tool result should be model-facing but stable, for example:

```text
action was not allowed by the authorization policy (reason: external_directory)
```

Raw classifier prose is not included by default; it may leak policy details or
become another prompt-injection channel. UI/audit integrations may retain a
bounded classifier explanation separately.

A denied result is an unsuccessful observation, not a Go execution error. The
brain may adapt and retry with a materially different, safer action. Repeated
identical denials should be bounded by a circuit breaker.

### 7. Jev and supported-model integration

A future classifier plugin may implement the classifier and policy pieces
without becoming a model-callable tool:

1. apply deterministic local policy;
2. build a bounded structured state from the request;
3. ask Jev or a supported-model classifier for an assessment;
4. map risk and confidence to `Allow` or `Ask`;
5. invoke the approval handler for `Ask`; and
6. return the final decision to the core.

The classifier is intended to replace routine manual approval in auto mode,
not to replace user control or OS isolation. Commands, arguments, and task
content sent to a hosted classifier are an explicit data-egress boundary.

`WrapTool` remains useful for post-execution result classification, redaction,
and middleware that needs to observe the actual result. The native
authorization stage is specifically for preflight decisions and approval.

## Alternatives considered

- **Only `WrapTool`:** rejected for interactive approval because authorization
  would occur inside concurrently launched tool calls and the core could not
  provide deterministic preflight semantics.
- **An approval prompt in `Core`:** rejected because the core should not own a
  terminal, GUI, user identity, or persistence format. It owns the callback
  seam, not presentation.
- **A full permissions/RBAC system:** deferred; per-action authorization does
  not require an identity model.
- **Putting the decision in the brain:** rejected. Model output, including
  `Action.Danger`, is advisory and cannot authorize its own execution.
- **A classifier-only security boundary:** rejected. Jev and other models can
  misclassify shell semantics, prompt injection, and data exfiltration.
- **An external policy capability:** not added to runtime protocol 1. The
  host-side authorization seam must remain outside the untrusted hands plugin
  boundary.

## Consequences

- The core gains a small control-plane concept and deterministic preflight.
- Existing tools and callers remain unchanged when no authorization policy is
  composed.
- Jev, supported LLMs, and deterministic policies can share one assessment
  contract.
- Same-turn tool execution remains concurrent after authorization completes.
- Interactive applications have an exact-action approval/override path.
- Non-interactive applications must choose an explicit response to `Ask`.
- `fs` and `edit` can enforce workspace paths in-process; `bash` remains
  unrestricted unless its deployment supplies a real sandbox or a future
  restricted executor.
- Native authorization is not an OS security boundary and cannot protect
  against malicious in-process plugins.

## Open implementation questions

- Final exported names and whether classifier/policy composition lives in a
  generic policy plugin or directly on `Core`.
- Exact event type and CLI rendering for `Ask`, `Allow`, and `Deny`.
- Whether the first approval handler is synchronous only, or whether the core
  needs a pending-token/resume API for remote UIs.
- The denial-loop circuit-breaker threshold and equivalence algorithm.
- The default classifier model/provider configuration and per-action latency
  or concurrency limits.
