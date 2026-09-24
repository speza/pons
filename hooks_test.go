package pons

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/samperrin/pons/protocol"
)

func TestTypedHooksRunStepAndToolLifecycle(t *testing.T) {
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
		OnAgentStepStart: func(_ context.Context, e *AgentStepStartEvent) error {
			if e.Observation.Step == 0 {
				t.Error("missing step")
			}
			appendPhase("step_start")
			return nil
		},
		OnAssistantResponse: func(context.Context, *AssistantResponseEvent) error {
			appendPhase("response")
			return nil
		},
		OnAgentStepEnd: func(context.Context, *AgentStepEndEvent) error {
			appendPhase("step_end")
			return nil
		},
		OnToolCallStart: func(_ context.Context, e *ToolCallStartEvent) error {
			appendPhase("tool_start")
			return nil
		},
		OnToolCallError: func(_ context.Context, e *ToolCallErrorEvent) error {
			if e.Err == nil || e.Result.OK {
				t.Errorf("tool error event: %+v", e)
			}
			appendPhase("tool_error")
			return nil
		},
		OnToolCallEnd: func(context.Context, *ToolCallEndEvent) error {
			appendPhase("tool_end")
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Run(context.Background(), "task"); err != nil {
		t.Fatal(err)
	}
	want := []string{"agent_start", "step_start", "response", "tool_start", "tool_error", "tool_end", "step_end", "step_start", "response", "step_end", "agent_end"}
	if !reflect.DeepEqual(phases, want) {
		t.Fatalf("phases = %v, want %v", phases, want)
	}
}

func TestTypedHooksCanChangeInputAndResult(t *testing.T) {
	c := New()
	setBrain(t, c, &continueBrain{fakeBrain{turns: [][]protocol.Action{{{Kind: "ping"}}, {Finish("done")}}}})
	if err := c.AddTool("ping", ToolDef{Handler: func(_ context.Context, a protocol.Action) (protocol.ToolResult, error) {
		value, err := protocol.StringArg(a.Args, "value")
		if err != nil {
			return protocol.ToolResult{}, err
		}
		return protocol.ToolResult{OK: true, Output: value}, nil
	}}); err != nil {
		t.Fatal(err)
	}
	if err := c.AddHooks(Hooks{
		OnAgentStart: func(_ context.Context, e *AgentStartEvent) error { e.Message = "changed task"; return nil },
		OnAgentStepStart: func(_ context.Context, e *AgentStepStartEvent) error {
			if e.Observation.Message != "changed task" {
				t.Errorf("observation = %q", e.Observation.Message)
			}
			return nil
		},
		OnToolCallStart: func(_ context.Context, e *ToolCallStartEvent) error {
			if len(e.Action.Args) == 0 {
				e.Decision.UpdatedArgs = protocol.MustArgsJSON(map[string]string{"value": "changed"})
			}
			return nil
		},
		OnToolCallEnd: func(_ context.Context, e *ToolCallEndEvent) error {
			e.Result.Output = "hooked: " + e.Result.Output
			return nil
		},
		OnAgentEnd: func(_ context.Context, e *AgentEndEvent) error {
			e.Result.Answer = "final answer"
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	var persistedArgs string
	c.OnEvent(func(event Event) {
		if event.Type == EventAssistantResponse {
			persistedArgs, _ = protocol.StringArg(event.Actions[0].Args, "value")
		}
	})
	result, err := c.Run(context.Background(), "task")
	if err != nil {
		t.Fatal(err)
	}
	if got := result.History[0].Results[0].Output; got != "hooked: changed" {
		t.Fatalf("output = %q", got)
	}
	if persistedArgs != "changed" {
		t.Fatalf("assistant event arguments = %q", persistedArgs)
	}
	if result.Answer != "final answer" {
		t.Fatalf("answer = %q", result.Answer)
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
		OnToolCallDenied: func(_ context.Context, e *ToolCallDeniedEvent) error {
			e.Result.Error = "blocked by hook"
			phases = append(phases, "denied")
			return nil
		},
		OnToolCallError: func(context.Context, *ToolCallErrorEvent) error { phases = append(phases, "error"); return nil },
		OnToolCallEnd:   func(context.Context, *ToolCallEndEvent) error { phases = append(phases, "end"); return nil },
	}); err != nil {
		t.Fatal(err)
	}
	result, err := c.Run(context.Background(), "task")
	if err != nil {
		t.Fatal(err)
	}
	if called || !reflect.DeepEqual(phases, []string{"start", "denied"}) {
		t.Fatalf("called = %v, phases = %v", called, phases)
	}
	if got := result.History[0].Results[0].Error; got != "blocked by hook" {
		t.Fatalf("error = %q", got)
	}
}

func TestTypedHooksCanStopRun(t *testing.T) {
	c := New()
	setBrain(t, c, &fakeBrain{turns: [][]protocol.Action{{{Kind: "ping"}}}})
	if err := c.AddHooks(Hooks{
		OnAgentStepStart: func(context.Context, *AgentStepStartEvent) error { return errors.New("blocked") },
	}); err != nil {
		t.Fatal(err)
	}
	_, err := c.Run(context.Background(), "task")
	if err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("error = %v", err)
	}
}

func TestToolHookErrorsStillReportAllFinishedCalls(t *testing.T) {
	for _, phase := range []string{"end", "error", "denied"} {
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
			case "error":
				hooks.OnToolCallError = func(context.Context, *ToolCallErrorEvent) error { return hookErr }
			case "denied":
				hooks.OnToolCallStart = func(_ context.Context, event *ToolCallStartEvent) error {
					if event.Action.ID == "first" {
						event.Decision.Action = DispositionDeny
					}
					return nil
				}
				hooks.OnToolCallDenied = func(context.Context, *ToolCallDeniedEvent) error { return hookErr }
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

func TestToolCallUpdateIsRecheckedBeforeExecution(t *testing.T) {
	c := New()
	setBrain(t, c, &fakeBrain{turns: [][]protocol.Action{{{Kind: "ping", Args: protocol.MustArgsJSON(map[string]string{"value": "original"})}}}})
	called := false
	if err := c.AddTool("ping", ToolDef{Handler: func(context.Context, protocol.Action) (protocol.ToolResult, error) {
		called = true
		return protocol.ToolResult{OK: true}, nil
	}}); err != nil {
		t.Fatal(err)
	}
	var checked []string
	if err := c.AddHooks(Hooks{OnToolCallStart: func(_ context.Context, e *ToolCallStartEvent) error {
		value, err := protocol.StringArg(e.Action.Args, "value")
		if err != nil {
			return err
		}
		if value == "original" {
			e.Decision.UpdatedArgs = protocol.MustArgsJSON(map[string]string{"value": "changed"})
		}
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	if err := c.AddHooks(Hooks{OnToolCallStart: func(_ context.Context, e *ToolCallStartEvent) error {
		value, err := protocol.StringArg(e.Action.Args, "value")
		if err != nil {
			return err
		}
		checked = append(checked, value)
		if value == "changed" {
			e.Decision.Action = DispositionDeny
		}
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	result, err := c.Run(context.Background(), "task")
	if err != nil {
		t.Fatal(err)
	}
	if called || !reflect.DeepEqual(checked, []string{"changed"}) || result.History[0].Results[0].OK {
		t.Fatalf("called = %v, checked = %v, result = %+v", called, checked, result.History[0].Results[0])
	}
}

func TestApprovalHooksCanResolveAskAndDenyWins(t *testing.T) {
	c := New()
	setBrain(t, c, &fakeBrain{turns: [][]protocol.Action{{{Kind: "ping"}}}})
	called := false
	if err := c.AddTool("ping", ToolDef{Handler: func(context.Context, protocol.Action) (protocol.ToolResult, error) {
		called = true
		return protocol.ToolResult{OK: true}, nil
	}}); err != nil {
		t.Fatal(err)
	}
	if err := c.AddHooks(Hooks{
		OnToolCallStart: func(_ context.Context, e *ToolCallStartEvent) error {
			e.Decision.Action = DispositionAsk
			return nil
		},
		OnApprovalRequest: func(_ context.Context, e *ApprovalRequestEvent) error {
			allow := DispositionAllow
			e.Decision = &allow
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.AddHooks(Hooks{OnApprovalRequest: func(_ context.Context, e *ApprovalRequestEvent) error {
		deny := DispositionDeny
		e.Decision = &deny
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	var resolved ActionDisposition
	if err := c.AddHooks(Hooks{OnApprovalResolved: func(_ context.Context, e *ApprovalResolvedEvent) error {
		resolved = e.Decision
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	result, err := c.Run(context.Background(), "task")
	if err != nil {
		t.Fatal(err)
	}
	if called || resolved != DispositionDeny || result.History[0].Results[0].OK {
		t.Fatalf("called = %v, resolved = %q, result = %+v", called, resolved, result.History[0].Results[0])
	}
}

func TestStepEndHookFailureRunsOnce(t *testing.T) {
	c := New()
	setBrain(t, c, &fakeBrain{turns: [][]protocol.Action{{Finish("done")}}})
	called := 0
	if err := c.AddHooks(Hooks{
		OnAgentStepEnd: func(context.Context, *AgentStepEndEvent) error {
			called++
			return errors.New("step end failed")
		},
	}); err != nil {
		t.Fatal(err)
	}
	_, err := c.Run(context.Background(), "task")
	if err == nil || !strings.Contains(err.Error(), "step end failed") || called != 1 {
		t.Fatalf("error = %v, calls = %d", err, called)
	}
}

func TestErrorHookFailurePreservesBothErrors(t *testing.T) {
	c := New()
	setBrain(t, c, &fakeBrain{turns: [][]protocol.Action{{Finish("done")}}})
	original := errors.New("step blocked")
	hookFailure := errors.New("error hook failed")
	if err := c.AddHooks(Hooks{
		OnAgentStepStart: func(context.Context, *AgentStepStartEvent) error { return original },
		OnAgentStepError: func(context.Context, *AgentStepErrorEvent) error { return hookFailure },
	}); err != nil {
		t.Fatal(err)
	}
	_, err := c.Run(context.Background(), "task")
	if !errors.Is(err, original) || !errors.Is(err, hookFailure) {
		t.Fatalf("error = %v", err)
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
		if err := c.AddHooks(Hooks{OnAgentStepStart: func(context.Context, *AgentStepStartEvent) error {
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

func TestApprovalHooksCollectErrorsBeforeExecution(t *testing.T) {
	for _, phase := range []string{"request", "resolved"} {
		t.Run(phase, func(t *testing.T) {
			c := New()
			setBrain(t, c, &fakeBrain{turns: [][]protocol.Action{{{Kind: "ping"}}}})
			calledTool := false
			if err := c.AddTool("ping", ToolDef{Handler: func(context.Context, protocol.Action) (protocol.ToolResult, error) {
				calledTool = true
				return protocol.ToolResult{OK: true}, nil
			}}); err != nil {
				t.Fatal(err)
			}
			if err := c.AddHooks(Hooks{OnToolCallStart: func(_ context.Context, e *ToolCallStartEvent) error {
				e.Decision.Action = DispositionAsk
				return nil
			}}); err != nil {
				t.Fatal(err)
			}
			if err := c.SetApprovalHandler(func(context.Context, ApprovalRequest) (ActionDisposition, error) {
				return DispositionAllow, nil
			}); err != nil {
				t.Fatal(err)
			}
			first := errors.New("first approval hook failed")
			second := errors.New("second approval hook failed")
			var called []string
			for _, item := range []struct {
				name string
				err  error
			}{{"first", first}, {"second", second}} {
				h := Hooks{}
				if phase == "request" {
					h.OnApprovalRequest = func(context.Context, *ApprovalRequestEvent) error {
						called = append(called, item.name)
						return item.err
					}
				} else {
					h.OnApprovalResolved = func(context.Context, *ApprovalResolvedEvent) error {
						called = append(called, item.name)
						return item.err
					}
				}
				if err := c.AddHooks(h); err != nil {
					t.Fatal(err)
				}
			}
			_, err := c.Run(context.Background(), "task")
			if !errors.Is(err, first) || !errors.Is(err, second) || calledTool ||
				!reflect.DeepEqual(called, []string{"first", "second"}) {
				t.Fatalf("error = %v, called = %v, tool ran = %v", err, called, calledTool)
			}
		})
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
		e.Decision.UpdatedArgs = protocol.MustArgsJSON(map[string]string{"value": "unsafe"})
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
