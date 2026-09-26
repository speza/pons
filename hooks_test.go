package pons

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/samperrin/pons/protocol"
)

// observingBrain records every observation it receives.
type observingBrain struct {
	continueBrain
	seen []protocol.Observation
}

func (b *observingBrain) Respond(ctx context.Context, obs protocol.Observation) (AssistantResponse, error) {
	b.seen = append(b.seen, obs)
	return b.continueBrain.Respond(ctx, obs)
}

func addHooks(t *testing.T, c *Core, h Hooks) {
	t.Helper()
	if err := c.AddHooks(h); err != nil {
		t.Fatal(err)
	}
}

func addPing(t *testing.T, c *Core, called *bool) {
	t.Helper()
	if err := c.AddTool("ping", ToolDef{Handler: func(context.Context, protocol.Action) (protocol.ToolResult, error) {
		if called != nil {
			*called = true
		}
		return protocol.ToolResult{OK: true, Output: "pong"}, nil
	}}); err != nil {
		t.Fatal(err)
	}
}

func eventTexts(c *Core, eventType EventType) *[]string {
	var texts []string
	c.OnEvent(func(event Event) {
		if event.Type == eventType {
			texts = append(texts, event.Text)
		}
	})
	return &texts
}

func TestHooksRunInLifecycleOrder(t *testing.T) {
	c := New()
	setBrain(t, c, &continueBrain{fakeBrain{turns: [][]protocol.Action{{{Kind: "ping"}}, {Finish("done")}}}})
	addPing(t, c, nil)
	var phases []string
	phase := func(name string) { phases = append(phases, name) }
	addHooks(t, c, Hooks{
		OnAgentStart: func(_ context.Context, in AgentStartInput) (AgentStartOutput, error) {
			if in.Message != "task" {
				t.Errorf("message = %q", in.Message)
			}
			phase("agent_start")
			return AgentStartOutput{}, nil
		},
		OnAgentTurnStart: func(context.Context, AgentTurnStartInput) (AgentTurnStartOutput, error) {
			phase("turn_start")
			return AgentTurnStartOutput{}, nil
		},
		OnAssistantResponse: func(context.Context, AssistantResponseInput) (AssistantResponseOutput, error) {
			phase("response")
			return AssistantResponseOutput{}, nil
		},
		OnToolCallStart: func(context.Context, ToolCallStartInput) (ToolCallStartOutput, error) {
			phase("tool_start")
			return ToolCallStartOutput{}, nil
		},
		OnPermissionRequest: func(context.Context, PermissionRequestInput) (PermissionRequestOutput, error) {
			phase("permission")
			return PermissionRequestOutput{}, nil
		},
		OnToolCallEnd: func(_ context.Context, in ToolCallEndInput) (ToolCallEndOutput, error) {
			if !in.Result.OK || in.Decision != nil {
				t.Errorf("tool end input: %+v", in)
			}
			phase("tool_end")
			return ToolCallEndOutput{}, nil
		},
		OnAgentTurnEnd: func(context.Context, AgentTurnEndInput) (AgentTurnEndOutput, error) {
			phase("turn_end")
			return AgentTurnEndOutput{}, nil
		},
		OnAgentEnd: func(_ context.Context, in AgentEndInput) (AgentEndOutput, error) {
			if in.Result.Answer != "done" || in.Reason != AgentEndFinished {
				t.Errorf("agent end input: %+v", in)
			}
			phase("agent_end")
			return AgentEndOutput{}, nil
		},
	})
	if _, err := c.Run(context.Background(), "task"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"agent_start", "turn_start", "response", "tool_start", "tool_end", "turn_end",
		"turn_start", "response", "turn_end", "agent_end",
	}
	if !reflect.DeepEqual(phases, want) {
		t.Fatalf("phases = %v, want %v", phases, want)
	}
}

func TestToolCallEndHooksChainResults(t *testing.T) {
	c := New()
	setBrain(t, c, &continueBrain{fakeBrain{turns: [][]protocol.Action{{{ID: "call", Kind: "ping"}}, {Finish("done")}}}})
	addPing(t, c, nil)
	for _, prefix := range []string{"first: ", "second: "} {
		addHooks(t, c, Hooks{OnToolCallEnd: func(_ context.Context, in ToolCallEndInput) (ToolCallEndOutput, error) {
			result := in.Result
			result.Output = prefix + result.Output
			result.ActionID = "forged"
			return ToolCallEndOutput{Result: &result}, nil
		}})
	}
	result, err := c.Run(context.Background(), "task")
	if err != nil {
		t.Fatal(err)
	}
	if got := result.History[0].Results[0]; got.Output != "second: first: pong" || got.ActionID != "call" {
		t.Fatalf("result = %+v", got)
	}
}

func TestDeniedCallReachesToolCallEndOnly(t *testing.T) {
	c := New()
	setBrain(t, c, &fakeBrain{turns: [][]protocol.Action{{{Kind: "ping"}}}})
	called := false
	addPing(t, c, &called)
	addHooks(t, c, Hooks{
		OnToolCallStart: func(context.Context, ToolCallStartInput) (ToolCallStartOutput, error) {
			return ToolCallStartOutput{Permission: PermissionDeny, Reason: "blocked"}, nil
		},
		OnToolCallEnd: func(_ context.Context, in ToolCallEndInput) (ToolCallEndOutput, error) {
			if in.Decision == nil || in.Decision.Permission != PermissionDeny || in.Decision.Reason != "blocked" {
				t.Errorf("denied end input: %+v", in)
			}
			result := in.Result
			result.Error, result.OK = "blocked by hook", true
			return ToolCallEndOutput{Result: &result}, nil
		},
	})
	result, err := c.Run(context.Background(), "task")
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("denied tool ran")
	}
	if got := result.History[0].Results[0]; got.Error != "blocked by hook" || got.OK {
		t.Fatalf("denied result = %+v", got)
	}
}

func TestHookErrorsAreReportedNotFatal(t *testing.T) {
	c := New()
	setBrain(t, c, &fakeBrain{turns: [][]protocol.Action{{Finish("done")}}})
	for _, name := range []string{"first", "second"} {
		addHooks(t, c, Hooks{OnAgentTurnStart: func(context.Context, AgentTurnStartInput) (AgentTurnStartOutput, error) {
			return AgentTurnStartOutput{Stop: true}, errors.New(name + " failed")
		}})
	}
	reported := eventTexts(c, EventHookError)
	result, err := c.Run(context.Background(), "task")
	if err != nil || result.Answer != "done" {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
	if len(*reported) != 2 || !strings.Contains((*reported)[0], "first failed") || !strings.Contains((*reported)[1], "second failed") {
		t.Fatalf("reported = %v", *reported)
	}
}

func TestToolCallEndErrorWithholdsResult(t *testing.T) {
	c := New()
	setBrain(t, c, &continueBrain{fakeBrain{turns: [][]protocol.Action{{{Kind: "ping"}}, {Finish("done")}}}})
	addPing(t, c, nil)
	addHooks(t, c, Hooks{OnToolCallEnd: func(context.Context, ToolCallEndInput) (ToolCallEndOutput, error) {
		return ToolCallEndOutput{}, errors.New("redaction failed")
	}})
	result, err := c.Run(context.Background(), "task")
	if err != nil {
		t.Fatal(err)
	}
	if got := result.History[0].Results[0]; got.OK || got.Output != "" || !strings.Contains(got.Error, "withheld") {
		t.Fatalf("result = %+v", got)
	}
}

func TestStopBeforeBrainCall(t *testing.T) {
	c := New()
	brain := &observingBrain{continueBrain: continueBrain{fakeBrain{turns: [][]protocol.Action{{Finish("done")}}}}}
	setBrain(t, c, brain)
	var end AgentEndInput
	addHooks(t, c, Hooks{
		OnAgentTurnStart: func(context.Context, AgentTurnStartInput) (AgentTurnStartOutput, error) {
			return AgentTurnStartOutput{Stop: true, StopReason: "budget reached"}, nil
		},
		OnAgentEnd: func(_ context.Context, in AgentEndInput) (AgentEndOutput, error) {
			end = in
			return AgentEndOutput{}, nil
		},
	})
	if _, err := c.Run(context.Background(), "task"); err != nil {
		t.Fatal(err)
	}
	if len(brain.seen) != 0 || end.Reason != AgentEndStopped || end.StopReason != "budget reached" {
		t.Fatalf("brain calls = %d, end = %+v", len(brain.seen), end)
	}
}

func TestStopDuringPreflightDeniesTheTurn(t *testing.T) {
	c := New()
	setBrain(t, c, &continueBrain{fakeBrain{turns: [][]protocol.Action{
		{{ID: "a", Kind: "ping"}, {ID: "b", Kind: "ping"}},
		{Finish("done")},
	}}})
	called := false
	addPing(t, c, &called)
	addHooks(t, c, Hooks{OnToolCallStart: func(_ context.Context, in ToolCallStartInput) (ToolCallStartOutput, error) {
		if in.Action.ID == "b" {
			return ToolCallStartOutput{Stop: true, StopReason: "operator stop"}, nil
		}
		return ToolCallStartOutput{}, nil
	}})
	result, err := c.Run(context.Background(), "task")
	if err != nil {
		t.Fatal(err)
	}
	if called || result.Turns != 1 || result.Answer != "" {
		t.Fatalf("called = %v, result = %+v", called, result)
	}
	for _, tr := range result.History[0].Results {
		if tr.OK || !strings.Contains(tr.Error, "run_stopped") {
			t.Fatalf("result = %+v", tr)
		}
	}
}

func TestAdditionalContextAndSystemMessages(t *testing.T) {
	c := New()
	brain := &observingBrain{continueBrain: continueBrain{fakeBrain{turns: [][]protocol.Action{{{Kind: "ping"}}, {Finish("done")}}}}}
	setBrain(t, c, brain)
	addPing(t, c, nil)
	addHooks(t, c, Hooks{
		OnAgentStart: func(context.Context, AgentStartInput) (AgentStartOutput, error) {
			return AgentStartOutput{AdditionalContext: "repo is read-only", SystemMessage: "policy loaded"}, nil
		},
		OnToolCallEnd: func(context.Context, ToolCallEndInput) (ToolCallEndOutput, error) {
			return ToolCallEndOutput{AdditionalContext: "ping hit staging"}, nil
		},
	})
	messages := eventTexts(c, EventSystemMessage)
	if _, err := c.Run(context.Background(), "task"); err != nil {
		t.Fatal(err)
	}
	if len(brain.seen) != 2 ||
		!reflect.DeepEqual(brain.seen[0].Context, []string{"repo is read-only"}) ||
		!reflect.DeepEqual(brain.seen[1].Context, []string{"ping hit staging"}) {
		t.Fatalf("observed context: %+v", brain.seen)
	}
	if !reflect.DeepEqual(*messages, []string{"policy loaded"}) {
		t.Fatalf("system messages = %v", *messages)
	}
}

func TestAgentEndCanContinueFinishedRun(t *testing.T) {
	c := New()
	brain := &observingBrain{continueBrain: continueBrain{fakeBrain{turns: [][]protocol.Action{
		{Finish("first answer")},
		{Finish("checked answer")},
	}}}}
	setBrain(t, c, brain)
	ends := 0
	addHooks(t, c, Hooks{OnAgentEnd: func(_ context.Context, in AgentEndInput) (AgentEndOutput, error) {
		ends++
		if in.Result.Answer == "first answer" {
			return AgentEndOutput{Continue: true, Reason: "run the tests before finishing"}, nil
		}
		return AgentEndOutput{}, nil
	}})
	result, err := c.Run(context.Background(), "task")
	if err != nil {
		t.Fatal(err)
	}
	if result.Answer != "checked answer" || result.Turns != 2 || ends != 2 ||
		!reflect.DeepEqual(brain.seen[1].Context, []string{"run the tests before finishing"}) {
		t.Fatalf("result = %+v, ends = %d, context = %v", result, ends, brain.seen[1].Context)
	}
}

func TestClosingHooksSeeFailureAndCancellation(t *testing.T) {
	c := New()
	setBrain(t, c, &fakeBrain{turns: [][]protocol.Action{{{Kind: "ping"}}}})
	addPing(t, c, nil)
	ctx, cancel := context.WithCancel(context.Background())
	var turnErr, agentErr error
	var reason AgentEndReason
	var closingCtxErrs []error
	addHooks(t, c, Hooks{
		OnToolCallStart: func(context.Context, ToolCallStartInput) (ToolCallStartOutput, error) {
			cancel()
			return ToolCallStartOutput{}, nil
		},
		OnAgentTurnEnd: func(ctx context.Context, in AgentTurnEndInput) (AgentTurnEndOutput, error) {
			turnErr = in.Err
			closingCtxErrs = append(closingCtxErrs, ctx.Err())
			return AgentTurnEndOutput{}, nil
		},
		OnAgentEnd: func(ctx context.Context, in AgentEndInput) (AgentEndOutput, error) {
			agentErr, reason = in.Err, in.Reason
			closingCtxErrs = append(closingCtxErrs, ctx.Err())
			return AgentEndOutput{Continue: true}, nil
		},
	})
	if _, err := c.Run(ctx, "task"); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
	if !errors.Is(turnErr, context.Canceled) || !errors.Is(agentErr, context.Canceled) || reason != AgentEndFailed {
		t.Fatalf("turn err = %v, agent err = %v, reason = %q", turnErr, agentErr, reason)
	}
	if !reflect.DeepEqual(closingCtxErrs, []error{nil, nil}) {
		t.Fatalf("closing hook contexts = %v", closingCtxErrs)
	}
}
