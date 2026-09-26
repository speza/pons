package pons

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/samperrin/pons/protocol"
)

// Hooks are the lifecycle extension points for trusted plugins. Every hook
// receives an Input value and returns an Output that embeds HookOutput, so
// any hook can stop the run, message the user, or add context for the brain.
// Some Outputs add one event-specific decision.
//
// A hook error is reported through EventHookError and discards that hook's
// output; it never stops the run by itself. Hooks that decide fail closed:
// an OnToolCallStart error asks for approval, an OnPermissionRequest error
// denies, and an OnToolCallEnd error withholds the result from the brain.
type Hooks struct {
	OnAgentStart        func(context.Context, AgentStartInput) (AgentStartOutput, error)
	OnAgentTurnStart    func(context.Context, AgentTurnStartInput) (AgentTurnStartOutput, error)
	OnAssistantResponse func(context.Context, AssistantResponseInput) (AssistantResponseOutput, error)
	// OnToolCallStart may run concurrently for the calls of one turn.
	OnToolCallStart     func(context.Context, ToolCallStartInput) (ToolCallStartOutput, error)
	OnPermissionRequest func(context.Context, PermissionRequestInput) (PermissionRequestOutput, error)
	OnToolCallEnd       func(context.Context, ToolCallEndInput) (ToolCallEndOutput, error)
	OnAgentTurnEnd      func(context.Context, AgentTurnEndInput) (AgentTurnEndOutput, error)
	OnAgentEnd          func(context.Context, AgentEndInput) (AgentEndOutput, error)
}

// HookOutput is common to every hook. Stop ends the run once the current
// step is safely recorded; OnAgentEnd ignores it.
type HookOutput struct {
	Stop              bool   `json:"stop,omitempty"`
	StopReason        string `json:"stop_reason,omitempty"`
	SystemMessage     string `json:"system_message,omitempty"`     // shown to the user
	AdditionalContext string `json:"additional_context,omitempty"` // added to the brain's next observation
}

func (o HookOutput) hookOutput() HookOutput { return o }

type AgentStartInput struct {
	Message   string `json:"message"`
	Workspace string `json:"workspace"`
	Platform  string `json:"platform,omitempty"`
}

type AgentStartOutput struct{ HookOutput }

type AgentTurnStartInput struct {
	Turn        int                  `json:"turn"`
	Observation protocol.Observation `json:"observation"`
}

type AgentTurnStartOutput struct{ HookOutput }

type AssistantResponseInput struct {
	Turn     int               `json:"turn"`
	Response AssistantResponse `json:"response"`
}

type AssistantResponseOutput struct{ HookOutput }

// ToolCallStartOutput is a hook's permission decision. An empty Permission
// means no objection. UpdatedInput replaces the call's arguments; every start
// hook then runs again on the changed call before it can proceed.
type ToolCallStartOutput struct {
	HookOutput
	Permission   Permission        `json:"permission,omitempty"`
	Reason       string            `json:"reason,omitempty"`
	Assessment   *ActionAssessment `json:"assessment,omitempty"`
	UpdatedInput json.RawMessage   `json:"updated_input,omitempty"`
}

// ToolCallStartInput describes one exact pending tool call.
type ToolCallStartInput struct {
	Turn          int                 `json:"turn"`
	Message       string              `json:"message"`
	Workspace     string              `json:"workspace"`
	Platform      string              `json:"platform,omitempty"`
	Environment   ActionEnvironment   `json:"environment"`
	Action        protocol.Action     `json:"action"`
	Tool          *ToolSpec           `json:"tool,omitempty"`
	Resources     []ToolResource      `json:"resources,omitempty"`
	ResourceError bool                `json:"resource_error,omitempty"` // projection failed; hooks can still deny
	RecentContext []ActionContextItem `json:"recent_context,omitempty"`
}

// ActionEnvironment is trusted host-supplied execution context. It is
// descriptive input to policy, not proof that a sandbox enforces containment.
type ActionEnvironment struct {
	Provider string `json:"provider,omitempty"`
	Network  string `json:"network,omitempty"`
}

// PermissionRequestInput is one exact call whose start hooks asked for
// approval. Decision is the merged Ask.
type PermissionRequestInput struct {
	ToolCallStartInput
	Decision PermissionDecision `json:"decision"`
}

// PermissionRequestOutput resolves an Ask. An empty Permission defers to the
// other permission hooks and then the approval handler; any Deny wins.
type PermissionRequestOutput struct {
	HookOutput
	Permission Permission `json:"permission,omitempty"`
}

// ToolCallEndInput reports every recorded outcome. Decision is set when the
// call was denied, in which case it never executed.
type ToolCallEndInput struct {
	Turn     int                 `json:"turn"`
	Action   protocol.Action     `json:"action"`
	Tool     *ToolSpec           `json:"tool,omitempty"`
	Result   protocol.ToolResult `json:"result"`
	Decision *PermissionDecision `json:"decision,omitempty"`
}

// ToolCallEndOutput may replace the result the brain observes; nil leaves it
// unchanged. A denied call's result stays unsuccessful.
type ToolCallEndOutput struct {
	HookOutput
	Result *protocol.ToolResult `json:"result,omitempty"`
}

// AgentTurnEndInput closes every opened turn. Err is set when the turn
// failed; Log then holds only what was recorded before the failure.
type AgentTurnEndInput struct {
	Turn int              `json:"turn"`
	Log  protocol.TurnLog `json:"log"`
	Err  error            `json:"-"`
}

type AgentTurnEndOutput struct{ HookOutput }

type AgentEndReason string

const (
	AgentEndFinished  AgentEndReason = "finished"
	AgentEndStopped   AgentEndReason = "stopped"
	AgentEndExhausted AgentEndReason = "exhausted"
	AgentEndFailed    AgentEndReason = "failed"
)

// AgentEndInput closes every started run, including failed and canceled
// ones, on a context that is not canceled.
type AgentEndInput struct {
	Result     RunResult      `json:"result"`
	Err        error          `json:"-"`
	Reason     AgentEndReason `json:"reason"`
	StopReason string         `json:"stop_reason,omitempty"`
}

// AgentEndOutput can keep a finished run going: Continue gives the brain
// another turn with Reason as added context. It is ignored for any other
// end reason and when a hook asks to stop.
type AgentEndOutput struct {
	HookOutput
	Continue bool   `json:"continue,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// AddHooks registers a plugin's hooks. Hooks of the same kind run in
// registration order.
func (c *Core) AddHooks(h Hooks) error {
	c.hooks = append(c.hooks, h)
	return nil
}

// hookEffects accumulates the common output of the hooks in one step.
type hookEffects struct {
	stop       bool
	stopReason string
	context    []string
	messages   []string
}

func (fx *hookEffects) add(out HookOutput) {
	if out.Stop && !fx.stop {
		fx.stop, fx.stopReason = true, out.StopReason
	}
	if out.AdditionalContext != "" {
		fx.context = append(fx.context, out.AdditionalContext)
	}
	if out.SystemMessage != "" {
		fx.messages = append(fx.messages, out.SystemMessage)
	}
}

type hookResult interface{ hookOutput() HookOutput }

// runHooks calls one kind of hook in registration order. Each successful
// output is folded into fx and passed to use; errors are returned for
// reporting and the failed hook's output is discarded.
func runHooks[In any, Out hookResult](
	ctx context.Context,
	hooks []Hooks,
	name string,
	selectHook func(Hooks) func(context.Context, In) (Out, error),
	in In,
	fx *hookEffects,
	use func(Out),
) []error {
	var errs []error
	for i, registered := range hooks {
		hook := selectHook(registered)
		if hook == nil {
			continue
		}
		out, err := hook(ctx, in)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s hook %d: %w", name, i+1, err))
			continue
		}
		fx.add(out.hookOutput())
		if use != nil {
			use(out)
		}
	}
	return errs
}

// reportHooks publishes hook errors and user messages. It returns an error
// only when a checked event sink fails.
func (c *Core) reportHooks(turn int, errs []error, fx *hookEffects) error {
	for _, err := range errs {
		c.logf("[turn %d] %v", turn, err)
		if emitErr := c.emit(Event{Type: EventHookError, Turn: turn, Text: err.Error()}); emitErr != nil {
			return fmt.Errorf("event hook_error: %w", emitErr)
		}
	}
	for _, message := range fx.messages {
		if err := c.emit(Event{Type: EventSystemMessage, Turn: turn, Text: message}); err != nil {
			return fmt.Errorf("event system_message: %w", err)
		}
	}
	fx.messages = nil
	return nil
}
