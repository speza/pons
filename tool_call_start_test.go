package pons

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/samperrin/pons/protocol"
)

func TestActionPolicyPreflightAndOrderedResults(t *testing.T) {
	c := New()
	c.Workspace = "/workspace"
	c.Platform = "linux/amd64"
	c.ActionEnvironment = ActionEnvironment{Provider: "sandbox", Network: "disabled"}
	setBrain(t, c, &continueBrain{fakeBrain{turns: [][]protocol.Action{
		{
			{ID: "first", Kind: "run", Args: protocol.MustArgsJSON(map[string]string{"command": "one"})},
			{ID: "second", Kind: "run", Args: protocol.MustArgsJSON(map[string]string{"command": "two"})},
			{ID: "third", Kind: "run", Args: protocol.MustArgsJSON(map[string]string{"command": "three"})},
		},
		{Finish("done")},
	}}})
	var sequence []string
	if err := c.AddTool("run", ToolDef{
		Handler: func(_ context.Context, a protocol.Action) (protocol.ToolResult, error) {
			if len(sequence) != 4 {
				t.Errorf("handler started before preflight completed: %v", sequence)
			}
			return protocol.ToolResult{OK: true, Output: a.ID}, nil
		},
		Resources: StringArgResource("command", "command"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.AddHooks(Hooks{OnToolCallStart: func(_ context.Context, req *ToolCallStartEvent) error {
		sequence = append(sequence, "policy:"+req.Action.ID)
		if req.Message != "goal" || req.Workspace != "/workspace" || req.Platform != "linux/amd64" ||
			req.Environment.Provider != "sandbox" || req.Environment.Network != "disabled" || req.Tool == nil ||
			len(req.Resources) != 1 || req.Resources[0].Kind != "command" {
			t.Errorf("incomplete action policy request: %+v", req)
		}
		if req.Action.ID == "second" {
			req.Decision = ActionDecision{Action: DispositionAsk, ReasonCode: "needs_review"}
			return nil
		}
		req.Decision.Action = DispositionAllow
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	if err := c.SetApprovalHandler(func(_ context.Context, req ApprovalRequest) (ActionDisposition, error) {
		sequence = append(sequence, "approve:"+req.Action.ID)
		return DispositionDeny, nil
	}); err != nil {
		t.Fatal(err)
	}
	var started, ended, denied, resultEvents []string
	c.OnEvent(func(e Event) {
		switch e.Type {
		case EventActionStart:
			started = append(started, e.Action.ID)
		case EventActionEnd:
			ended = append(ended, e.Action.ID)
			resultEvents = append(resultEvents, e.Action.ID)
		case EventActionDenied:
			denied = append(denied, e.Action.ID)
			resultEvents = append(resultEvents, e.Action.ID)
		}
	})
	res, err := c.Run(context.Background(), "goal")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sequence[:4], []string{"policy:first", "policy:second", "approve:second", "policy:third"}) {
		t.Fatalf("preflight order: %v", sequence)
	}
	if !reflect.DeepEqual(started, []string{"first", "third"}) ||
		!reflect.DeepEqual(ended, []string{"first", "third"}) ||
		!reflect.DeepEqual(denied, []string{"second"}) ||
		!reflect.DeepEqual(resultEvents, []string{"first", "second", "third"}) {
		t.Fatalf("events: started=%v ended=%v denied=%v results=%v", started, ended, denied, resultEvents)
	}
	results := res.History[0].Results
	if len(results) != 3 || results[0].Output != "first" || results[2].Output != "third" ||
		results[1].OK || results[1].ActionID != "second" || results[1].Kind != "run" ||
		!strings.Contains(results[1].Error, "needs_review") {
		t.Fatalf("results: %+v", results)
	}
}

func TestAllowedDecisionReasonIsNotDenial(t *testing.T) {
	for _, tt := range []struct{ input, want string }{
		{"", ""},
		{"private key: abc", "reason_unavailable"},
	} {
		c := New()
		setBrain(t, c, &continueBrain{fakeBrain{turns: [][]protocol.Action{
			{{ID: "a", Kind: "run"}}, {Finish("done")},
		}}})
		if err := c.AddTool("run", ToolDef{Handler: func(context.Context, protocol.Action) (protocol.ToolResult, error) {
			return protocol.ToolResult{OK: true}, nil
		}}); err != nil {
			t.Fatal(err)
		}
		if err := c.AddHooks(Hooks{OnToolCallStart: func(_ context.Context, req *ToolCallStartEvent) error {
			req.Decision = ActionDecision{Action: DispositionAllow, ReasonCode: tt.input}
			return nil
		}}); err != nil {
			t.Fatal(err)
		}
		c.OnEvent(func(e Event) {
			if e.Type == EventActionDecision && e.Decision.ReasonCode != tt.want {
				t.Errorf("allowed decision has reason %q, want %q", e.Decision.ReasonCode, tt.want)
			}
		})
		if _, err := c.Run(context.Background(), "goal"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestActionPolicyReceivesRecentConversationAndActions(t *testing.T) {
	c := New()
	c.RecentActionContext = []ActionContextItem{
		{Source: ContextAssistant, Text: "May I push commit abc to repo one?"},
		{Source: ContextUser, Text: "push it"},
	}
	setBrain(t, c, &continueBrain{fakeBrain{turns: [][]protocol.Action{
		{{ID: "first", Kind: "run"}},
		{{ID: "second", Kind: "run"}},
		{Finish("done")},
	}}})
	if err := c.AddTool("run", ToolDef{Handler: func(_ context.Context, a protocol.Action) (protocol.ToolResult, error) {
		return protocol.ToolResult{OK: true, Output: a.ID + " result"}, nil
	}}); err != nil {
		t.Fatal(err)
	}
	var seen [][]ActionContextItem
	if err := c.AddHooks(Hooks{OnToolCallStart: func(_ context.Context, req *ToolCallStartEvent) error {
		seen = append(seen, req.RecentContext)
		req.Decision.Action = DispositionAllow
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Run(context.Background(), "push it"); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || len(seen[0]) != 2 ||
		seen[0][0].Source != ContextAssistant || seen[0][1].Source != ContextUser {
		t.Fatalf("prior messages: %+v", seen)
	}
	if len(seen[1]) != 4 || seen[1][2].Source != ContextAction || seen[1][2].ActionID != "first" ||
		seen[1][3].Source != ContextToolResult || seen[1][3].Text != "first result" {
		t.Fatalf("same-run action history: %+v", seen[1])
	}
}

func TestActionPolicyContextIsBounded(t *testing.T) {
	items := make([]ActionContextItem, 30)
	for i := range items {
		items[i] = ActionContextItem{Source: ContextUser, Text: strings.Repeat("é", 2000)}
	}
	got := boundedActionContext(items)
	if len(got) != maxActionContextItems || len(got[0].Text) > maxActionContextText ||
		!utf8.ValidString(got[0].Text) || len(items[0].Text) != 4000 {
		t.Fatalf("context bounds: items=%d bytes=%d", len(got), len(got[0].Text))
	}
}

func TestProjectionFailureCannotBypassHardDeny(t *testing.T) {
	c := New()
	setBrain(t, c, &continueBrain{fakeBrain{turns: [][]protocol.Action{
		{{ID: "a", Kind: "run", Args: protocol.MustArgsJSON(map[string]int{"command": 42})}},
		{Finish("done")},
	}}})
	if err := c.AddTool("run", ToolDef{
		Handler: func(context.Context, protocol.Action) (protocol.ToolResult, error) {
			t.Fatal("hard-denied tool ran")
			return protocol.ToolResult{}, nil
		},
		Resources: StringArgResource("command", "command"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.AddHooks(Hooks{OnToolCallStart: func(_ context.Context, req *ToolCallStartEvent) error {
		if !req.ResourceError {
			t.Error("policy did not see projection failure")
		}
		req.Decision = ActionDecision{Action: DispositionDeny, ReasonCode: "hard_rule"}
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	if err := c.SetApprovalHandler(func(context.Context, ApprovalRequest) (ActionDisposition, error) {
		t.Fatal("hard deny reached approval")
		return DispositionAllow, nil
	}); err != nil {
		t.Fatal(err)
	}
	res, err := c.Run(context.Background(), "goal")
	if err != nil {
		t.Fatal(err)
	}
	if got := res.History[0].Results[0].Error; !strings.Contains(got, "hard_rule") {
		t.Fatalf("denial reason: %s", got)
	}
}

func TestActionPolicyFailuresFailClosed(t *testing.T) {
	tests := []struct {
		name       string
		policy     func(context.Context, *ToolCallStartEvent) error
		approver   ApprovalHandler
		wantReason string
	}{
		{"ask without approver", func(_ context.Context, event *ToolCallStartEvent) error {
			event.Decision.Action = DispositionAsk
			return nil
		}, nil, "approval_required"},
		{"classifier error", func(context.Context, *ToolCallStartEvent) error {
			return errors.New("secret classifier failure")
		}, nil, "approval_required"},
		{"invalid decision", func(_ context.Context, event *ToolCallStartEvent) error {
			event.Decision.Action = "yes"
			return nil
		}, nil, "approval_required"},
		{"invalid approval", func(_ context.Context, event *ToolCallStartEvent) error {
			event.Decision.Action = DispositionAsk
			return nil
		}, func(context.Context, ApprovalRequest) (ActionDisposition, error) {
			return DispositionAsk, nil
		}, "invalid_approval"},
		{"unsafe reason", func(_ context.Context, event *ToolCallStartEvent) error {
			event.Decision = ActionDecision{Action: DispositionDeny, ReasonCode: "private key: abc"}
			return nil
		}, nil, "tool_call_denied"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := New()
			setBrain(t, c, &continueBrain{fakeBrain{turns: [][]protocol.Action{
				{{ID: "a", Kind: "run"}}, {Finish("done")},
			}}})
			called := false
			if err := c.AddTool("run", ToolDef{Handler: func(context.Context, protocol.Action) (protocol.ToolResult, error) {
				called = true
				return protocol.ToolResult{OK: true}, nil
			}}); err != nil {
				t.Fatal(err)
			}
			if err := c.AddHooks(Hooks{OnToolCallStart: tt.policy}); err != nil {
				t.Fatal(err)
			}
			if tt.approver != nil {
				if err := c.SetApprovalHandler(tt.approver); err != nil {
					t.Fatal(err)
				}
			}
			res, err := c.Run(context.Background(), "goal")
			if err != nil {
				t.Fatal(err)
			}
			if called || !strings.Contains(res.History[0].Results[0].Error, tt.wantReason) ||
				strings.Contains(res.History[0].Results[0].Error, "secret") {
				t.Fatalf("failure was not contained: called=%v result=%+v", called, res.History[0].Results[0])
			}
		})
	}
}

func TestToolCallStartHooksCombineWithoutLastWins(t *testing.T) {
	tests := []struct {
		name      string
		decisions []ActionDecision
		wantRun   bool
		wantError string
	}{
		{"ask beats later allow", []ActionDecision{{Action: DispositionAsk}, {Action: DispositionAllow}}, false, "approval_required"},
		{"deny beats earlier allow", []ActionDecision{{Action: DispositionAllow}, {Action: DispositionDeny, ReasonCode: "hard_rule"}}, false, "hard_rule"},
		{"allow when all allow", []ActionDecision{{Action: DispositionAllow}, {Action: DispositionAllow}}, true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := New()
			setBrain(t, c, &continueBrain{fakeBrain{turns: [][]protocol.Action{
				{{ID: "a", Kind: "run"}}, {Finish("done")},
			}}})
			ran := false
			if err := c.AddTool("run", ToolDef{Handler: func(context.Context, protocol.Action) (protocol.ToolResult, error) {
				ran = true
				return protocol.ToolResult{OK: true}, nil
			}}); err != nil {
				t.Fatal(err)
			}
			for _, decision := range tt.decisions {
				if err := c.AddHooks(Hooks{OnToolCallStart: func(_ context.Context, event *ToolCallStartEvent) error {
					event.Decision = decision
					return nil
				}}); err != nil {
					t.Fatal(err)
				}
			}
			result, err := c.Run(context.Background(), "goal")
			if err != nil {
				t.Fatal(err)
			}
			if ran != tt.wantRun || !strings.Contains(result.History[0].Results[0].Error, tt.wantError) {
				t.Fatalf("ran=%t result=%+v", ran, result.History[0].Results[0])
			}
		})
	}
}

func TestToolCallStartHooksCannotChangeExecutedArguments(t *testing.T) {
	c := New()
	setBrain(t, c, &continueBrain{fakeBrain{turns: [][]protocol.Action{
		{{ID: "a", Kind: "run", Args: json.RawMessage(`{"command":"safe"}`)}},
		{Finish("done")},
	}}})
	if err := c.AddTool("run", ToolDef{Handler: func(_ context.Context, action protocol.Action) (protocol.ToolResult, error) {
		if string(action.Args) != `{"command":"safe"}` {
			t.Errorf("executed mutated arguments: %s", action.Args)
		}
		return protocol.ToolResult{OK: true}, nil
	}}); err != nil {
		t.Fatal(err)
	}
	if err := c.AddHooks(Hooks{OnToolCallStart: func(_ context.Context, req *ToolCallStartEvent) error {
		copy(req.Action.Args, []byte(`{"command":"evil"}`))
		req.Decision.Action = DispositionAllow
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	if err := c.AddHooks(Hooks{OnToolCallStart: func(_ context.Context, req *ToolCallStartEvent) error {
		if string(req.Action.Args) != `{"command":"safe"}` {
			t.Errorf("later hook saw mutated arguments: %s", req.Action.Args)
		}
		req.Decision.Action = DispositionAllow
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Run(context.Background(), "goal"); err != nil {
		t.Fatal(err)
	}
}

func TestRepeatedDenialSkipsApproval(t *testing.T) {
	c := New()
	c.MaxSteps = 5
	setBrain(t, c, endlessBrain{})
	if err := c.AddTool("ping", ToolDef{Handler: func(context.Context, protocol.Action) (protocol.ToolResult, error) {
		t.Fatal("denied tool ran")
		return protocol.ToolResult{}, nil
	}}); err != nil {
		t.Fatal(err)
	}
	policyChecks, approvals := 0, 0
	if err := c.AddHooks(Hooks{OnToolCallStart: func(_ context.Context, event *ToolCallStartEvent) error {
		policyChecks++
		event.Decision.Action = DispositionAsk
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	if err := c.SetApprovalHandler(func(context.Context, ApprovalRequest) (ActionDisposition, error) {
		approvals++
		return DispositionDeny, nil
	}); err != nil {
		t.Fatal(err)
	}
	res, err := c.Run(context.Background(), "goal")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Exhausted || policyChecks != repeatedDenialLimit || approvals != repeatedDenialLimit {
		t.Fatalf("denial limit: result=%+v policy checks=%d approvals=%d", res, policyChecks, approvals)
	}
	if got := res.History[3].Results[0].Error; !strings.Contains(got, "repeated_denial") {
		t.Fatalf("circuit breaker result: %s", got)
	}
}
