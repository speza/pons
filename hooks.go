package pons

import (
	"context"
	"errors"
	"fmt"

	"github.com/samperrin/pons/protocol"
)

// Each hook receives the state available at its particular point in a run.
// Hooks may change fields that their event documents as mutable or return an
// error to stop the run. A hook that only reads its event is an observer.
type AgentStartEvent struct {
	Message   string // may be changed before the brain observes it
	Workspace string
	Platform  string
}

type AgentEndReason string

const (
	AgentEndFinished  AgentEndReason = "finished"
	AgentEndStopped   AgentEndReason = "stopped"
	AgentEndExhausted AgentEndReason = "exhausted"
	AgentEndFailed    AgentEndReason = "failed"
)

type AgentEndEvent struct {
	Result     RunResult // may be changed before Run returns
	Err        error     // final error snapshot
	Reason     AgentEndReason
	StopReason string
}

type AgentErrorEvent struct {
	Step int
	Err  error // may be replaced with a non-nil error
}

type AgentStepStartEvent struct {
	Observation protocol.Observation // message and history may be changed
}

type AssistantResponseEvent struct {
	Step     int
	Response AssistantResponse
}

type AgentStepEndEvent struct {
	Step int
	Run  RunResult // completed step snapshot
}

type AgentStepErrorEvent struct {
	Step int
	Err  error // may be replaced with a non-nil error
}

type ToolCallEndEvent struct {
	Step   int
	Action protocol.Action
	Tool   *ToolSpec
	Result protocol.ToolResult
}

type ToolCallErrorEvent struct {
	Step   int
	Action protocol.Action
	Tool   *ToolSpec
	Result protocol.ToolResult
	Err    error // original execution error; change Result to recover
}

type ToolCallDeniedEvent struct {
	Step     int
	Action   protocol.Action
	Tool     *ToolSpec
	Result   protocol.ToolResult
	Decision ActionDecision
}

type ApprovalRequestEvent struct {
	Request  ApprovalRequest    // exact pending call; read only
	Decision *ActionDisposition // nil defers to the application approval handler
}

type ApprovalResolvedEvent struct {
	Request  ApprovalRequest   // exact pending call; read only
	Decision ActionDisposition // may be changed from allow to deny
}

// Hooks are event-specific extension points for trusted plugins. Start hooks
// run before the corresponding work and may reject it; tool end hooks may
// change the result passed to the brain, and agent end may change the returned
// result. OnToolCallStart is the sole pre-execution tool decision point.
// Denied calls never execute and do not emit OnToolCallEnd or OnToolCallError.
type Hooks struct {
	OnAgentStart        func(context.Context, *AgentStartEvent) error
	OnAgentEnd          func(context.Context, *AgentEndEvent) error
	OnAgentError        func(context.Context, *AgentErrorEvent) error
	OnAgentStepStart    func(context.Context, *AgentStepStartEvent) error
	OnAssistantResponse func(context.Context, *AssistantResponseEvent) error
	OnAgentStepEnd      func(context.Context, *AgentStepEndEvent) error
	OnAgentStepError    func(context.Context, *AgentStepErrorEvent) error
	OnToolCallStart     func(context.Context, *ToolCallStartEvent) error
	OnToolCallEnd       func(context.Context, *ToolCallEndEvent) error
	OnToolCallError     func(context.Context, *ToolCallErrorEvent) error
	OnToolCallDenied    func(context.Context, *ToolCallDeniedEvent) error
	OnApprovalRequest   func(context.Context, *ApprovalRequestEvent) error
	OnApprovalResolved  func(context.Context, *ApprovalResolvedEvent) error
}

// AddHooks registers a plugin's hooks in composition order. Every matching
// callback runs; callback errors are joined in that same order.
func (c *Core) AddHooks(h Hooks) error {
	c.hooks = append(c.hooks, h)
	return nil
}

func dispatchHook[T any](ctx context.Context, hooks []Hooks, selectHook func(Hooks) func(context.Context, *T) error, event *T) error {
	var errs []error
	for i, registered := range hooks {
		if hook := selectHook(registered); hook != nil {
			if err := hook(ctx, event); err != nil {
				errs = append(errs, fmt.Errorf("hook %d: %w", i+1, err))
			}
		}
	}
	return errors.Join(errs...)
}
