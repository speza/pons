package pons

import (
	"context"
	"errors"
	"fmt"

	"github.com/samperrin/pons/protocol"
)

// Each hook receives the state available at its point in a run. Hooks are
// observers except where an event documents a mutable field: OnToolCallStart
// sets a decision and OnToolCallEnd may replace the result passed to the brain.
// A hook error stops the run.
type AgentStartEvent struct {
	Message   string
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
	Result     RunResult
	Err        error
	Reason     AgentEndReason
	StopReason string
}

type AgentTurnStartEvent struct {
	Observation protocol.Observation
}

type AssistantResponseEvent struct {
	Turn     int
	Response AssistantResponse
}

// AgentTurnEndEvent closes every opened turn. Err is set when the turn failed;
// Log then holds only the results that were recorded before the failure.
type AgentTurnEndEvent struct {
	Turn int
	Log  protocol.TurnLog
	Err  error
}

// ToolCallEndEvent reports every recorded tool outcome. Decision is set when
// a start hook denied the call, in which case it never executed.
type ToolCallEndEvent struct {
	Turn     int
	Action   protocol.Action
	Tool     *ToolSpec
	Result   protocol.ToolResult // may be changed before the brain observes it
	Decision *ActionDecision
}

// Hooks are event-specific extension points for trusted plugins.
// OnToolCallStart is the sole pre-execution decision point and may be called
// concurrently for the calls of one turn. OnAgentTurnEnd and OnAgentEnd run
// even when the run fails or its context is canceled.
type Hooks struct {
	OnAgentStart        func(context.Context, *AgentStartEvent) error
	OnAgentTurnStart    func(context.Context, *AgentTurnStartEvent) error
	OnAssistantResponse func(context.Context, *AssistantResponseEvent) error
	OnToolCallStart     func(context.Context, *ToolCallStartEvent) error
	OnToolCallEnd       func(context.Context, *ToolCallEndEvent) error
	OnAgentTurnEnd      func(context.Context, *AgentTurnEndEvent) error
	OnAgentEnd          func(context.Context, *AgentEndEvent) error
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
