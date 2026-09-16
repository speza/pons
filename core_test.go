package pons

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/samperrin/pons/protocol"
)

// fakeBrain is a minimal ControlPort for core tests.
type fakeBrain struct {
	turns [][]protocol.Action // actions per turn; empty plan = finish
	i     int
}

func (b *fakeBrain) NextActions(ctx context.Context, obs protocol.Observation) ([]protocol.Action, error) {
	if b.i >= len(b.turns) {
		return nil, nil
	}
	b.i++
	return b.turns[b.i-1], nil
}

func (b *fakeBrain) Interpret(ctx context.Context, obs protocol.Observation, tr protocol.ToolResult) (protocol.Interpretation, error) {
	return protocol.Interpretation{Continue: tr.OK}, nil
}

func (b *fakeBrain) Close(ctx context.Context) error { return nil }

func setBrain(t *testing.T, c *Core, b ControlPort) {
	t.Helper()
	if err := c.SetBrain(b); err != nil {
		t.Fatal(err)
	}
}

// fakeBrainStop interprets failed results as stop.
type stopBrain struct{ fakeBrain }

func (b *stopBrain) Interpret(ctx context.Context, obs protocol.Observation, tr protocol.ToolResult) (protocol.Interpretation, error) {
	return protocol.Interpretation{Continue: false, StopReason: "failed"}, nil
}

func TestUnknownKindIsObservationNotCrash(t *testing.T) {
	c := New()
	setBrain(t, c, &fakeBrain{turns: [][]protocol.Action{{{Kind: "does_not_exist"}}}})
	if _, err := c.Run(context.Background(), "test goal"); err != nil {
		t.Fatalf("run: %v", err)
	}
}

func TestToolPanicBecomesObservation(t *testing.T) {
	c := New()
	setBrain(t, c, &continueBrain{fakeBrain{turns: [][]protocol.Action{{{ID: "panic", Kind: "panic"}}, {Finish("done")}}}})
	if err := c.AddTool("panic", ToolDef{Handler: func(context.Context, protocol.Action) (protocol.ToolResult, error) {
		panic("boom")
	}}); err != nil {
		t.Fatal(err)
	}
	res, err := c.Run(context.Background(), "goal")
	if err != nil || res.Answer != "done" {
		t.Fatalf("panic handling: result=%+v err=%v", res, err)
	}
	if got := res.History[0].Results[0].Error; got != "tool panic: boom" {
		t.Fatalf("panic observation: %q", got)
	}
}

// alwaysBrain continues regardless of results.
type continueBrain struct{ fakeBrain }

func (b *continueBrain) Interpret(ctx context.Context, obs protocol.Observation, tr protocol.ToolResult) (protocol.Interpretation, error) {
	return protocol.Interpretation{Continue: true}, nil
}

func TestFinishStopsLoop(t *testing.T) {
	c := New()
	setBrain(t, c, &continueBrain{fakeBrain{turns: [][]protocol.Action{
		{{Kind: "does_not_exist"}}, // unknown kind → observation
		{Finish("all done")},
	}}})
	c.AddTool("anything", ToolDef{Handler: func(ctx context.Context, a protocol.Action) (protocol.ToolResult, error) {
		return protocol.ToolResult{ActionID: a.ID, OK: true}, nil
	}})
	if _, err := c.Run(context.Background(), "test goal"); err != nil {
		t.Fatalf("run: %v", err)
	}
}

func TestEmptyPlanErrors(t *testing.T) {
	c := New()
	setBrain(t, c, &fakeBrain{}) // zero turns → NextActions returns nil
	if _, err := c.Run(context.Background(), "test goal"); err == nil {
		t.Fatal("empty plan should error")
	}
}

func TestStopOnFailedResult(t *testing.T) {
	c := New()
	setBrain(t, c, &stopBrain{fakeBrain{turns: [][]protocol.Action{
		{{Kind: "does_not_exist"}},
	}}})
	if _, err := c.Run(context.Background(), "test goal"); err != nil {
		t.Fatalf("run: %v", err)
	}
}

func TestOnTurnAndWraps(t *testing.T) {
	c := New()
	brain := &fakeBrain{turns: [][]protocol.Action{
		{{Kind: "ping", Args: protocol.MustArgsJSON(map[string]string{"x": "1"})}},
		{Finish("done")},
	}}
	setBrain(t, c, brain)

	var seen []string
	c.AddTool("ping", ToolDef{Handler: func(ctx context.Context, a protocol.Action) (protocol.ToolResult, error) {
		x, _ := protocol.StringArg(a.Args, "x")
		return protocol.ToolResult{ActionID: a.ID, OK: true, Output: "pong:" + x}, nil
	}})
	c.WrapTool(func(next ToolPort) ToolPort {
		return wrapFunc(func(ctx context.Context, a protocol.Action) (protocol.ToolResult, error) {
			seen = append(seen, "wrap:"+string(a.Kind))
			return next.Execute(ctx, a)
		})
	})
	var turns []protocol.TurnLog
	c.OnTurn(func(obs protocol.Observation, turn protocol.TurnLog) {
		turns = append(turns, turn)
	})

	if _, err := c.Run(context.Background(), "test goal"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(seen) != 1 || !strings.Contains(seen[0], "ping") {
		t.Fatalf("middleware not invoked: %v", seen)
	}
	if len(turns) != 2 || len(turns[0].Results) != 1 || turns[0].Results[0].Output != "pong:1" {
		t.Fatalf("turn hooks wrong: %+v", turns)
	}
}

func TestSetBrainRejectsNil(t *testing.T) {
	if err := New().SetBrain(nil); err == nil {
		t.Fatal("nil brain must be rejected")
	}
}

func TestTurnErrorHookPropagates(t *testing.T) {
	c := New()
	setBrain(t, c, &fakeBrain{turns: [][]protocol.Action{{Finish("done")}}})
	c.OnTurnError(func(protocol.Observation, protocol.TurnLog) error {
		return errors.New("disk full")
	})
	res, err := c.Run(context.Background(), "goal")
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("hook error = %v", err)
	}
	if res.Turns != 1 || len(res.History) != 1 {
		t.Fatalf("hook failure lost completed turn: %+v", res)
	}
}

func TestAllResultsAreInterpretedBeforeStopping(t *testing.T) {
	c := New()
	brain := &interpretRecordingBrain{}
	setBrain(t, c, brain)
	for _, kind := range []protocol.ActionKind{"a", "b"} {
		if err := c.AddTool(kind, ToolDef{Handler: func(context.Context, protocol.Action) (protocol.ToolResult, error) {
			return protocol.ToolResult{OK: true, ActionID: string(kind)}, nil
		}}); err != nil {
			t.Fatal(err)
		}
	}
	res, err := c.Run(context.Background(), "goal")
	if err != nil {
		t.Fatal(err)
	}
	if len(brain.seen) != 2 || res.Turns != 1 || res.Exhausted {
		t.Fatalf("unexpected run: %+v interpretations=%v", res, brain.seen)
	}
}

type interpretRecordingBrain struct{ seen []string }

func (b *interpretRecordingBrain) NextActions(context.Context, protocol.Observation) ([]protocol.Action, error) {
	if len(b.seen) == 0 {
		return []protocol.Action{{ID: "a", Kind: "a"}, {ID: "b", Kind: "b"}}, nil
	}
	return []protocol.Action{Finish("not reached")}, nil
}
func (b *interpretRecordingBrain) Interpret(_ context.Context, _ protocol.Observation, tr protocol.ToolResult) (protocol.Interpretation, error) {
	b.seen = append(b.seen, tr.ActionID)
	return protocol.Interpretation{Continue: len(b.seen) != 1, StopReason: "first result requested stop"}, nil
}
func (b *interpretRecordingBrain) Close(context.Context) error { return nil }

func TestAdditiveEnforcement(t *testing.T) {
	c := New()
	if err := c.AddTool("x", ToolDef{Handler: func(context.Context, protocol.Action) (protocol.ToolResult, error) { return protocol.ToolResult{}, nil }}); err != nil {
		t.Fatalf("first registration: %v", err)
	}
	if err := c.AddTool("x", ToolDef{Handler: func(context.Context, protocol.Action) (protocol.ToolResult, error) { return protocol.ToolResult{}, nil }}); err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("duplicate kind must error, got: %v", err)
	}
	b := &fakeBrain{}
	if err := c.SetBrain(b); err != nil {
		t.Fatalf("SetBrain: %v", err)
	}
	if err := c.SetBrain(b); err == nil || !strings.Contains(err.Error(), "already set") {
		t.Fatalf("second brain must error, got: %v", err)
	}
}

func TestToolSpecsAreDefensiveCopies(t *testing.T) {
	c := New()
	params := []ToolParam{{Name: "command", Type: "string"}}
	if err := c.AddTool("run", ToolDef{
		Handler: func(context.Context, protocol.Action) (protocol.ToolResult, error) {
			return protocol.ToolResult{}, nil
		},
		Params: params,
	}); err != nil {
		t.Fatal(err)
	}

	params[0].Name = "changed by caller"
	specs := c.ToolSpecs()
	if got := specs[0].Params[0].Name; got != "command" {
		t.Fatalf("registration retained caller's params slice: %q", got)
	}

	specs[0].Params[0].Name = "changed from snapshot"
	if got := c.ToolSpecs()[0].Params[0].Name; got != "command" {
		t.Fatalf("ToolSpecs exposed registry params slice: %q", got)
	}

	eventSpec := c.toolSpec("run")
	eventSpec.Params[0].Name = "changed from event"
	if got := c.ToolSpecs()[0].Params[0].Name; got != "command" {
		t.Fatalf("event metadata exposed registry params slice: %q", got)
	}
}

type wrapFunc func(ctx context.Context, a protocol.Action) (protocol.ToolResult, error)

func (f wrapFunc) Execute(ctx context.Context, a protocol.Action) (protocol.ToolResult, error) {
	return f(ctx, a)
}

func TestRunResultAndEvents(t *testing.T) {
	c := New()
	setBrain(t, c, &continueBrain{fakeBrain{turns: [][]protocol.Action{
		{{Kind: "ping", Args: protocol.MustArgsJSON(map[string]string{"x": "1"})}},
		{Finish("the answer")},
	}}})
	c.AddTool("ping", ToolDef{Handler: func(ctx context.Context, a protocol.Action) (protocol.ToolResult, error) {
		return protocol.ToolResult{ActionID: a.ID, OK: true, Output: "pong"}, nil
	}})

	var types []EventType
	c.OnEvent(func(e Event) { types = append(types, e.Type) })

	res, err := c.Run(context.Background(), "do the thing")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Answer != "the answer" || res.Turns != 2 || res.Exhausted {
		t.Fatalf("result: %+v", res)
	}
	if len(res.History) != 2 || len(res.History[0].Results) != 1 || res.History[0].Results[0].Output != "pong" {
		t.Fatalf("history: %+v", res.History)
	}
	want := []EventType{
		EventAgentStart, EventTurnStart,
		EventActionStart, EventActionEnd, EventTurnEnd,
		EventTurnStart, EventTurnEnd, EventFinish, // finish turn is recorded too
	}
	if len(types) != len(want) {
		t.Fatalf("events: %v", types)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("event %d = %s, want %s", i, types[i], want[i])
		}
	}
}

func TestExhaustedIsResultNotError(t *testing.T) {
	c := New()
	c.MaxTurns = 2
	// A brain that keeps calling a tool forever, never finishing.
	setBrain(t, c, endlessBrain{})
	c.AddTool("ping", ToolDef{Handler: func(ctx context.Context, a protocol.Action) (protocol.ToolResult, error) {
		return protocol.ToolResult{ActionID: a.ID, OK: true}, nil
	}})
	res, err := c.Run(context.Background(), "forever")
	if err != nil {
		t.Fatalf("exhausted budget should not be an error: %v", err)
	}
	if !res.Exhausted || res.Turns != 2 || res.Answer != "" {
		t.Fatalf("result: %+v", res)
	}
}

// endlessBrain plans one tool action every turn, never finishing.
type endlessBrain struct{}

func (endlessBrain) NextActions(ctx context.Context, obs protocol.Observation) ([]protocol.Action, error) {
	return []protocol.Action{{ID: "p", Kind: "ping"}}, nil
}
func (endlessBrain) Interpret(ctx context.Context, obs protocol.Observation, tr protocol.ToolResult) (protocol.Interpretation, error) {
	return protocol.Interpretation{Continue: true}, nil
}
func (endlessBrain) Close(ctx context.Context) error { return nil }

func TestConcurrentExecutionOrderedResults(t *testing.T) {
	// call order: [blocker, instant] — the blocker waits for a signal that
	// only the *second* call emits. Sequential execution would deadlock
	// (the blocker never releases); concurrency lets the instant call run
	// while the blocker waits. Results must still be recorded in call order.
	c := New()
	setBrain(t, c, &continueBrain{fakeBrain{turns: [][]protocol.Action{
		{
			{ID: "first", Kind: "blocker"},
			{ID: "second", Kind: "instant"},
		},
		{Finish("done")},
	}}})
	sig := make(chan struct{})
	c.AddTool("blocker", ToolDef{Handler: func(ctx context.Context, a protocol.Action) (protocol.ToolResult, error) {
		select {
		case <-sig:
			return protocol.ToolResult{ActionID: a.ID, OK: true, Output: "blocked-ok"}, nil
		case <-time.After(2 * time.Second):
			return protocol.ToolResult{ActionID: a.ID, OK: false, Error: "sequential execution detected"}, nil
		}
	}})
	c.AddTool("instant", ToolDef{Handler: func(ctx context.Context, a protocol.Action) (protocol.ToolResult, error) {
		close(sig)
		return protocol.ToolResult{ActionID: a.ID, OK: true, Output: "instant-ok"}, nil
	}})

	res, err := c.Run(context.Background(), "concurrent + ordered")
	if err != nil {
		t.Fatal(err)
	}
	first, second := res.History[0].Results[0], res.History[0].Results[1]
	if first.Output != "blocked-ok" || second.Output != "instant-ok" {
		t.Fatalf("results not in call order: %+v / %+v", first, second)
	}
}
