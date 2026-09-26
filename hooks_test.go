package pons

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/samperrin/pons/protocol"
)

func TestTypedHooksRunTurnAndToolLifecycle(t *testing.T) {
	c := New()
	setBrain(t, c, &continueBrain{fakeBrain{turns: [][]protocol.Action{
		{{Kind: "fail"}},
		{Finish("done")},
	}}})
	if err := c.AddTool("fail", ToolDef{Handler: func(context.Context, protocol.Action) (protocol.ToolResult, error) {
		return protocol.ToolResult{OK: false, Error: "expected failure"}, nil
	}}); err != nil {
		t.Fatal(err)
	}
	var phases []string
	appendPhase := func(name string) { phases = append(phases, name) }
	if err := c.AddHooks(Hooks{
		OnAgentStart: func(_ context.Context, e *AgentStartEvent) error {
			if e.Message != "task" {
				t.Errorf("message = %q", e.Message)
			}
			appendPhase("agent_start")
			return nil
		},
		OnAgentEnd: func(_ context.Context, e *AgentEndEvent) error {
			if e.Result.Answer != "done" {
				t.Errorf("answer = %q", e.Result.Answer)
			}
			appendPhase("agent_end")
			return nil
		},
		OnAgentTurnStart: func(_ context.Context, e *AgentTurnStartEvent) error {
			if e.Observation.Turn == 0 {
				t.Error("missing turn")
			}
			appendPhase("turn_start")
			return nil
		},
		OnAssistantResponse: func(context.Context, *AssistantResponseEvent) error {
			appendPhase("response")
			return nil
		},
		OnAgentTurnEnd: func(context.Context, *AgentTurnEndEvent) error {
			appendPhase("turn_end")
			return nil
		},
		OnToolCallStart: func(_ context.Context, e *ToolCallStartEvent) error {
			appendPhase("tool_start")
			return nil
		},
		OnToolCallEnd: func(_ context.Context, e *ToolCallEndEvent) error {
			if e.Result.OK || e.Decision != nil {
				t.Errorf("tool end event: %+v", e)
			}
			appendPhase("tool_end")
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Run(context.Background(), "task"); err != nil {
		t.Fatal(err)
	}
	want := []string{"agent_start", "turn_start", "response", "tool_start", "tool_end", "turn_end", "turn_start", "response", "turn_end", "agent_end"}
	if !reflect.DeepEqual(phases, want) {
		t.Fatalf("phases = %v, want %v", phases, want)
	}
}

func TestToolCallEndHookCanChangeResult(t *testing.T) {
	c := New()
	setBrain(t, c, &continueBrain{fakeBrain{turns: [][]protocol.Action{{{Kind: "ping"}}, {Finish("done")}}}})
	if err := c.AddTool("ping", ToolDef{Handler: func(context.Context, protocol.Action) (protocol.ToolResult, error) {
		return protocol.ToolResult{OK: true, Output: "pong"}, nil
	}}); err != nil {
		t.Fatal(err)
	}
	if err := c.AddHooks(Hooks{OnToolCallEnd: func(_ context.Context, e *ToolCallEndEvent) error {
		e.Result.Output = "hooked: " + e.Result.Output
		e.Result.ActionID = "forged"
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	result, err := c.Run(context.Background(), "task")
	if err != nil {
		t.Fatal(err)
	}
	if got := result.History[0].Results[0]; got.Output != "hooked: pong" || got.ActionID == "forged" {
		t.Fatalf("result = %+v", got)
	}
}

func TestTypedHooksDeniedCallNeverStartsExecution(t *testing.T) {
	c := New()
	setBrain(t, c, &fakeBrain{turns: [][]protocol.Action{{{Kind: "ping"}}}})
	called := false
	if err := c.AddTool("ping", ToolDef{Handler: func(context.Context, protocol.Action) (protocol.ToolResult, error) {
		called = true
		return protocol.ToolResult{OK: true}, nil
	}}); err != nil {
		t.Fatal(err)
	}
	var phases []string
	if err := c.AddHooks(Hooks{
		OnToolCallStart: func(_ context.Context, e *ToolCallStartEvent) error {
			phases = append(phases, "start")
			e.Decision.Action = DispositionDeny
			return nil
		},
		OnToolCallEnd: func(_ context.Context, e *ToolCallEndEvent) error {
			if e.Decision == nil || e.Decision.Action != DispositionDeny {
				t.Errorf("denied end event: %+v", e)
			}
			e.Result.Error = "blocked by hook"
			e.Result.OK = true
			phases = append(phases, "end")
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	result, err := c.Run(context.Background(), "task")
	if err != nil {
		t.Fatal(err)
	}
	if called || !reflect.DeepEqual(phases, []string{"start", "end"}) {
		t.Fatalf("called = %v, phases = %v", called, phases)
	}
	if got := result.History[0].Results[0]; got.Error != "blocked by hook" || got.OK {
		t.Fatalf("denied result = %+v", got)
	}
}

func TestTypedHooksCanStopRun(t *testing.T) {
	c := New()
	setBrain(t, c, &fakeBrain{turns: [][]protocol.Action{{{Kind: "ping"}}}})
	if err := c.AddHooks(Hooks{
		OnAgentTurnStart: func(context.Context, *AgentTurnStartEvent) error { return errors.New("blocked") },
	}); err != nil {
		t.Fatal(err)
	}
	_, err := c.Run(context.Background(), "task")
	if err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("error = %v", err)
	}
}

func TestToolHookErrorsStillReportAllFinishedCalls(t *testing.T) {
	for _, phase := range []string{"end", "denied"} {
		t.Run(phase, func(t *testing.T) {
			c := New()
			setBrain(t, c, &fakeBrain{turns: [][]protocol.Action{{
				{ID: "first", Kind: "ping"},
				{ID: "second", Kind: "ping"},
			}}})
			if err := c.AddTool("ping", ToolDef{Handler: func(_ context.Context, action protocol.Action) (protocol.ToolResult, error) {
				return protocol.ToolResult{OK: action.ID == "second"}, nil
			}}); err != nil {
				t.Fatal(err)
			}
			hookErr := errors.New("post-execution hook failed")
			hooks := Hooks{}
			switch phase {
			case "end":
				hooks.OnToolCallEnd = func(_ context.Context, event *ToolCallEndEvent) error {
					if event.Action.ID == "first" {
						return hookErr
					}
					return nil
				}
			case "denied":
				hooks.OnToolCallStart = func(_ context.Context, event *ToolCallStartEvent) error {
					if event.Action.ID == "first" {
						event.Decision.Action = DispositionDeny
					}
					return nil
				}
				hooks.OnToolCallEnd = func(_ context.Context, event *ToolCallEndEvent) error {
					if event.Decision != nil {
						return hookErr
					}
					return nil
				}
			}
			if err := c.AddHooks(hooks); err != nil {
				t.Fatal(err)
			}
			var reported []string
			c.OnEvent(func(event Event) {
				if event.Type == EventActionEnd || event.Type == EventActionDenied {
					reported = append(reported, event.Action.ID)
				}
			})
			_, err := c.Run(context.Background(), "task")
			if !errors.Is(err, hookErr) {
				t.Fatalf("run error = %v, want hook error", err)
			}
			if !reflect.DeepEqual(reported, []string{"first", "second"}) {
				t.Fatalf("reported calls = %v", reported)
			}
		})
	}
}

func TestTurnEndHookFailureRunsOnce(t *testing.T) {
	c := New()
	setBrain(t, c, &fakeBrain{turns: [][]protocol.Action{{Finish("done")}}})
	called := 0
	if err := c.AddHooks(Hooks{
		OnAgentTurnEnd: func(context.Context, *AgentTurnEndEvent) error {
			called++
			return errors.New("turn end failed")
		},
	}); err != nil {
		t.Fatal(err)
	}
	_, err := c.Run(context.Background(), "task")
	if err == nil || !strings.Contains(err.Error(), "turn end failed") || called != 1 {
		t.Fatalf("error = %v, calls = %d", err, called)
	}
}

func TestClosingHooksSeeFailureAndPreserveBothErrors(t *testing.T) {
	c := New()
	setBrain(t, c, &fakeBrain{turns: [][]protocol.Action{{Finish("done")}}})
	original := errors.New("turn blocked")
	hookFailure := errors.New("turn end hook failed")
	var turnErr, agentErr error
	var reason AgentEndReason
	if err := c.AddHooks(Hooks{
		OnAgentTurnStart: func(context.Context, *AgentTurnStartEvent) error { return original },
		OnAgentTurnEnd: func(_ context.Context, e *AgentTurnEndEvent) error {
			turnErr = e.Err
			return hookFailure
		},
		OnAgentEnd: func(_ context.Context, e *AgentEndEvent) error {
			agentErr, reason = e.Err, e.Reason
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	_, err := c.Run(context.Background(), "task")
	if !errors.Is(err, original) || !errors.Is(err, hookFailure) {
		t.Fatalf("error = %v", err)
	}
	if !errors.Is(turnErr, original) || !errors.Is(agentErr, hookFailure) || reason != AgentEndFailed {
		t.Fatalf("turn err = %v, agent err = %v, reason = %q", turnErr, agentErr, reason)
	}
}

func TestClosingHooksRunAfterCancellation(t *testing.T) {
	c := New()
	setBrain(t, c, &fakeBrain{turns: [][]protocol.Action{{{Kind: "ping"}}}})
	ctx, cancel := context.WithCancel(context.Background())
	if err := c.AddTool("ping", ToolDef{Handler: func(context.Context, protocol.Action) (protocol.ToolResult, error) {
		return protocol.ToolResult{OK: true}, nil
	}}); err != nil {
		t.Fatal(err)
	}
	var closingCtxErrs []error
	if err := c.AddHooks(Hooks{
		OnToolCallStart: func(context.Context, *ToolCallStartEvent) error {
			cancel()
			return nil
		},
		OnAgentTurnEnd: func(ctx context.Context, _ *AgentTurnEndEvent) error {
			closingCtxErrs = append(closingCtxErrs, ctx.Err())
			return nil
		},
		OnAgentEnd: func(ctx context.Context, _ *AgentEndEvent) error {
			closingCtxErrs = append(closingCtxErrs, ctx.Err())
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Run(ctx, "task"); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
	if !reflect.DeepEqual(closingCtxErrs, []error{nil, nil}) {
		t.Fatalf("closing hook contexts = %v", closingCtxErrs)
	}
}

func TestHooksCollectErrorsInRegistrationOrder(t *testing.T) {
	c := New()
	setBrain(t, c, &fakeBrain{turns: [][]protocol.Action{{Finish("done")}}})
	first := errors.New("first hook failed")
	second := errors.New("second hook failed")
	var called []string
	for _, item := range []struct {
		name string
		err  error
	}{{"first", first}, {"second", second}} {
		if err := c.AddHooks(Hooks{OnAgentTurnStart: func(context.Context, *AgentTurnStartEvent) error {
			called = append(called, item.name)
			return item.err
		}}); err != nil {
			t.Fatal(err)
		}
	}
	_, err := c.Run(context.Background(), "task")
	if !errors.Is(err, first) || !errors.Is(err, second) ||
		!reflect.DeepEqual(called, []string{"first", "second"}) ||
		strings.Index(err.Error(), first.Error()) > strings.Index(err.Error(), second.Error()) {
		t.Fatalf("error = %v, called = %v", err, called)
	}
}

func TestToolStartDenyStillCallsLaterHooks(t *testing.T) {
	c := New()
	setBrain(t, c, &fakeBrain{turns: [][]protocol.Action{{{Kind: "ping"}}}})
	if err := c.AddTool("ping", ToolDef{Handler: func(context.Context, protocol.Action) (protocol.ToolResult, error) {
		t.Fatal("denied tool ran")
		return protocol.ToolResult{}, nil
	}}); err != nil {
		t.Fatal(err)
	}
	var called []string
	if err := c.AddHooks(Hooks{OnToolCallStart: func(_ context.Context, e *ToolCallStartEvent) error {
		called = append(called, "deny")
		e.Decision.Action = DispositionDeny
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	if err := c.AddHooks(Hooks{OnToolCallStart: func(_ context.Context, e *ToolCallStartEvent) error {
		called = append(called, "error")
		e.Decision.Action = DispositionAllow
		return errors.New("private failure")
	}}); err != nil {
		t.Fatal(err)
	}
	result, err := c.Run(context.Background(), "task")
	if err != nil || !reflect.DeepEqual(called, []string{"deny", "error"}) ||
		result.History[0].Results[0].OK || strings.Contains(result.History[0].Results[0].Error, "private failure") {
		t.Fatalf("error = %v, called = %v, result = %+v", err, called, result.History[0].Results[0])
	}
}
