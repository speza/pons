package pons

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
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
	var mu sync.Mutex
	var sequence []string
	record := func(entry string) {
		mu.Lock()
		defer mu.Unlock()
		sequence = append(sequence, entry)
	}
	if err := c.AddTool("run", ToolDef{
		Handler: func(_ context.Context, a protocol.Action) (protocol.ToolResult, error) {
			mu.Lock()
			defer mu.Unlock()
			if len(sequence) != 4 {
				t.Errorf("handler started before preflight completed: %v", sequence)
			}
			return protocol.ToolResult{OK: true, Output: a.ID}, nil
		},
		Resources: StringArgResource("command", "command"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.AddHooks(Hooks{OnToolCallStart: func(_ context.Context, req ToolCallStartInput) (ToolCallStartOutput, error) {
		record("policy:" + req.Action.ID)
		if req.Message != "goal" || req.Workspace != "/workspace" || req.Platform != "linux/amd64" ||
			req.Environment.Provider != "sandbox" || req.Environment.Network != "disabled" || req.Tool == nil ||
			len(req.Resources) != 1 || req.Resources[0].Kind != "command" {
			t.Errorf("incomplete action policy request: %+v", req)
		}
		if req.Action.ID == "second" {
			return ToolCallStartOutput{Permission: PermissionAsk, Reason: "needs_review"}, nil
		}
		return ToolCallStartOutput{Permission: PermissionAllow}, nil
	}}); err != nil {
		t.Fatal(err)
	}
	if err := c.SetApprovalHandler(func(_ context.Context, req PermissionRequestInput) (Permission, error) {
		record("approve:" + req.Action.ID)
		return PermissionDeny, nil
	}); err != nil {
		t.Fatal(err)
	}
	var decided, started, ended, denied, resultEvents []string
	c.OnEvent(func(e Event) {
		switch e.Type {
		case EventActionDecision:
			decided = append(decided, e.Action.ID+":"+string(e.Decision.Permission))
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
	// Start hooks run concurrently; approval follows once every call is decided.
	policies := slices.Clone(sequence[:3])
	slices.Sort(policies)
	if !reflect.DeepEqual(policies, []string{"policy:first", "policy:second", "policy:third"}) || sequence[3] != "approve:second" {
		t.Fatalf("preflight order: %v", sequence)
	}
	if !reflect.DeepEqual(decided, []string{"first:allow", "second:deny", "third:allow"}) ||
		!reflect.DeepEqual(started, []string{"first", "third"}) ||
		!reflect.DeepEqual(ended, []string{"first", "third"}) ||
		!reflect.DeepEqual(denied, []string{"second"}) ||
		!reflect.DeepEqual(resultEvents, []string{"first", "second", "third"}) {
		t.Fatalf("events: decided=%v started=%v ended=%v denied=%v results=%v", decided, started, ended, denied, resultEvents)
	}
	results := res.History[0].Results
	if len(results) != 3 || results[0].Output != "first" || results[2].Output != "third" ||
		results[1].OK || results[1].ActionID != "second" || results[1].Kind != "run" ||
		!strings.Contains(results[1].Error, "approval_denied") {
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
		if err := c.AddHooks(Hooks{OnToolCallStart: func(_ context.Context, req ToolCallStartInput) (ToolCallStartOutput, error) {
			return ToolCallStartOutput{Permission: PermissionAllow, Reason: tt.input}, nil
		}}); err != nil {
			t.Fatal(err)
		}
		c.OnEvent(func(e Event) {
			if e.Type == EventActionDecision && e.Decision.Reason != tt.want {
				t.Errorf("allowed decision has reason %q, want %q", e.Decision.Reason, tt.want)
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
	if err := c.AddHooks(Hooks{OnToolCallStart: func(_ context.Context, req ToolCallStartInput) (ToolCallStartOutput, error) {
		seen = append(seen, req.RecentContext)
		return ToolCallStartOutput{Permission: PermissionAllow}, nil
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
	if err := c.AddHooks(Hooks{OnToolCallStart: func(_ context.Context, req ToolCallStartInput) (ToolCallStartOutput, error) {
		if !req.ResourceError {
			t.Error("policy did not see projection failure")
		}
		return ToolCallStartOutput{Permission: PermissionDeny, Reason: "hard_rule"}, nil
	}}); err != nil {
		t.Fatal(err)
	}
	if err := c.SetApprovalHandler(func(context.Context, PermissionRequestInput) (Permission, error) {
		t.Fatal("hard deny reached approval")
		return PermissionAllow, nil
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
		policy     func(context.Context, ToolCallStartInput) (ToolCallStartOutput, error)
		approver   ApprovalHandler
		wantReason string
	}{
		{"ask without approver", func(context.Context, ToolCallStartInput) (ToolCallStartOutput, error) {
			return ToolCallStartOutput{Permission: PermissionAsk}, nil
		}, nil, "approval_required"},
		{"classifier error", func(context.Context, ToolCallStartInput) (ToolCallStartOutput, error) {
			return ToolCallStartOutput{}, errors.New("secret classifier failure")
		}, nil, "approval_required"},
		{"invalid decision", func(context.Context, ToolCallStartInput) (ToolCallStartOutput, error) {
			return ToolCallStartOutput{Permission: "yes"}, nil
		}, nil, "approval_required"},
		{"invalid approval", func(context.Context, ToolCallStartInput) (ToolCallStartOutput, error) {
			return ToolCallStartOutput{Permission: PermissionAsk}, nil
		}, func(context.Context, PermissionRequestInput) (Permission, error) {
			return PermissionAsk, nil
		}, "invalid_approval"},
		{"unsafe reason", func(context.Context, ToolCallStartInput) (ToolCallStartOutput, error) {
			return ToolCallStartOutput{Permission: PermissionDeny, Reason: "private key: abc"}, nil
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
		decisions []PermissionDecision
		wantRun   bool
		wantError string
	}{
		{"ask beats later allow", []PermissionDecision{{Permission: PermissionAsk}, {Permission: PermissionAllow}}, false, "approval_required"},
		{"deny beats earlier allow", []PermissionDecision{{Permission: PermissionAllow}, {Permission: PermissionDeny, Reason: "hard_rule"}}, false, "hard_rule"},
		{"allow when all allow", []PermissionDecision{{Permission: PermissionAllow}, {Permission: PermissionAllow}}, true, ""},
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
				if err := c.AddHooks(Hooks{OnToolCallStart: func(context.Context, ToolCallStartInput) (ToolCallStartOutput, error) {
					return ToolCallStartOutput{Permission: decision.Permission, Reason: decision.Reason}, nil
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
	if err := c.AddHooks(Hooks{OnToolCallStart: func(_ context.Context, req ToolCallStartInput) (ToolCallStartOutput, error) {
		copy(req.Action.Args, []byte(`{"command":"evil"}`))
		return ToolCallStartOutput{Permission: PermissionAllow}, nil
	}}); err != nil {
		t.Fatal(err)
	}
	if err := c.AddHooks(Hooks{OnToolCallStart: func(_ context.Context, req ToolCallStartInput) (ToolCallStartOutput, error) {
		if string(req.Action.Args) != `{"command":"safe"}` {
			t.Errorf("later hook saw mutated arguments: %s", req.Action.Args)
		}
		return ToolCallStartOutput{Permission: PermissionAllow}, nil
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Run(context.Background(), "goal"); err != nil {
		t.Fatal(err)
	}
}

func TestRepeatedDenialSkipsApproval(t *testing.T) {
	c := New()
	c.MaxTurns = 5
	setBrain(t, c, endlessBrain{})
	if err := c.AddTool("ping", ToolDef{Handler: func(context.Context, protocol.Action) (protocol.ToolResult, error) {
		t.Fatal("denied tool ran")
		return protocol.ToolResult{}, nil
	}}); err != nil {
		t.Fatal(err)
	}
	policyChecks, approvals := 0, 0
	if err := c.AddHooks(Hooks{OnToolCallStart: func(_ context.Context, event ToolCallStartInput) (ToolCallStartOutput, error) {
		policyChecks++
		return ToolCallStartOutput{Permission: PermissionAsk}, nil
	}}); err != nil {
		t.Fatal(err)
	}
	if err := c.SetApprovalHandler(func(context.Context, PermissionRequestInput) (Permission, error) {
		approvals++
		return PermissionDeny, nil
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

func TestPreflightDecidesCallsConcurrently(t *testing.T) {
	c := New()
	setBrain(t, c, &continueBrain{fakeBrain{turns: [][]protocol.Action{
		{{ID: "a", Kind: "run"}, {ID: "b", Kind: "run"}},
		{Finish("done")},
	}}})
	if err := c.AddTool("run", ToolDef{Handler: func(context.Context, protocol.Action) (protocol.ToolResult, error) {
		return protocol.ToolResult{OK: true}, nil
	}}); err != nil {
		t.Fatal(err)
	}
	var arrived sync.WaitGroup
	arrived.Add(2)
	if err := c.AddHooks(Hooks{OnToolCallStart: func(context.Context, ToolCallStartInput) (ToolCallStartOutput, error) {
		arrived.Done()
		done := make(chan struct{})
		go func() { arrived.Wait(); close(done) }()
		select {
		case <-done:
			return ToolCallStartOutput{}, nil
		case <-time.After(5 * time.Second):
			return ToolCallStartOutput{}, errors.New("start hooks ran one at a time")
		}
	}}); err != nil {
		t.Fatal(err)
	}
	result, err := c.Run(context.Background(), "goal")
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range result.History[0].Results {
		if !tr.OK {
			t.Fatalf("result = %+v", tr)
		}
	}
}

func TestHookErrorCannotWeakenDeny(t *testing.T) {
	c := New()
	setBrain(t, c, &continueBrain{fakeBrain{turns: [][]protocol.Action{
		{{ID: "a", Kind: "run"}}, {Finish("done")},
	}}})
	if err := c.AddTool("run", ToolDef{Handler: func(context.Context, protocol.Action) (protocol.ToolResult, error) {
		t.Fatal("denied tool ran")
		return protocol.ToolResult{}, nil
	}}); err != nil {
		t.Fatal(err)
	}
	if err := c.AddHooks(Hooks{OnToolCallStart: func(_ context.Context, event ToolCallStartInput) (ToolCallStartOutput, error) {
		return ToolCallStartOutput{Permission: PermissionDeny, Reason: "hard_rule"}, errors.New("audit write failed")
	}}); err != nil {
		t.Fatal(err)
	}
	if err := c.SetApprovalHandler(func(context.Context, PermissionRequestInput) (Permission, error) {
		t.Fatal("deny reached approval")
		return PermissionAllow, nil
	}); err != nil {
		t.Fatal(err)
	}
	result, err := c.Run(context.Background(), "goal")
	if err != nil {
		t.Fatal(err)
	}
	if got := result.History[0].Results[0].Error; !strings.Contains(got, "hard_rule") {
		t.Fatalf("denial reason: %s", got)
	}
}

func TestUpdatedInputIsRecheckedAndRecorded(t *testing.T) {
	c := New()
	setBrain(t, c, &continueBrain{fakeBrain{turns: [][]protocol.Action{
		{{ID: "a", Kind: "run", Args: protocol.MustArgsJSON(map[string]string{"value": "original"})}},
		{Finish("done")},
	}}})
	var executed string
	if err := c.AddTool("run", ToolDef{Handler: func(_ context.Context, a protocol.Action) (protocol.ToolResult, error) {
		executed, _ = protocol.StringArg(a.Args, "value")
		return protocol.ToolResult{OK: true}, nil
	}}); err != nil {
		t.Fatal(err)
	}
	var checked []string
	for _, hook := range []func(string) ToolCallStartOutput{
		func(value string) ToolCallStartOutput {
			if value == "original" {
				return ToolCallStartOutput{UpdatedInput: protocol.MustArgsJSON(map[string]string{"value": "changed"})}
			}
			return ToolCallStartOutput{}
		},
		func(value string) ToolCallStartOutput {
			checked = append(checked, value)
			return ToolCallStartOutput{}
		},
	} {
		if err := c.AddHooks(Hooks{OnToolCallStart: func(_ context.Context, in ToolCallStartInput) (ToolCallStartOutput, error) {
			value, err := protocol.StringArg(in.Action.Args, "value")
			return hook(value), err
		}}); err != nil {
			t.Fatal(err)
		}
	}
	var recorded string
	c.OnEvent(func(e Event) {
		if e.Type == EventAssistantResponse {
			recorded, _ = protocol.StringArg(e.Actions[0].Args, "value")
		}
	})
	result, err := c.Run(context.Background(), "goal")
	if err != nil {
		t.Fatal(err)
	}
	if executed != "changed" || recorded != "changed" || !reflect.DeepEqual(checked, []string{"changed"}) {
		t.Fatalf("executed=%q recorded=%q checked=%v", executed, recorded, checked)
	}
	if got, _ := protocol.StringArg(result.History[0].Actions[0].Args, "value"); got != "changed" {
		t.Fatalf("history args = %q", got)
	}
}

func TestInvalidOrEndlessUpdatesAreDenied(t *testing.T) {
	for name, tt := range map[string]struct {
		update func(int) json.RawMessage
		reason string
	}{
		"non-object": {func(int) json.RawMessage { return json.RawMessage(`[]`) }, "invalid_tool_call_update"},
		"endless": {func(n int) json.RawMessage {
			return protocol.MustArgsJSON(map[string]int{"n": n + 1})
		}, "tool_call_update_limit"},
	} {
		t.Run(name, func(t *testing.T) {
			c := New()
			setBrain(t, c, &continueBrain{fakeBrain{turns: [][]protocol.Action{
				{{ID: "a", Kind: "run", Args: json.RawMessage(`{"n":0}`)}}, {Finish("done")},
			}}})
			if err := c.AddTool("run", ToolDef{Handler: func(context.Context, protocol.Action) (protocol.ToolResult, error) {
				t.Fatal("updated call ran")
				return protocol.ToolResult{}, nil
			}}); err != nil {
				t.Fatal(err)
			}
			if err := c.AddHooks(Hooks{OnToolCallStart: func(_ context.Context, in ToolCallStartInput) (ToolCallStartOutput, error) {
				var args struct{ N int }
				_ = json.Unmarshal(in.Action.Args, &args)
				return ToolCallStartOutput{UpdatedInput: tt.update(args.N)}, nil
			}}); err != nil {
				t.Fatal(err)
			}
			result, err := c.Run(context.Background(), "goal")
			if err != nil {
				t.Fatal(err)
			}
			if got := result.History[0].Results[0]; got.OK || !strings.Contains(got.Error, tt.reason) {
				t.Fatalf("result = %+v, want %s", got, tt.reason)
			}
		})
	}
}

func TestPermissionRequestHooksResolveAsk(t *testing.T) {
	tests := []struct {
		name     string
		answers  []Permission
		hookErr  bool
		approver Permission
		wantRun  bool
		reason   string
	}{
		{"hook allows", []Permission{PermissionAllow}, false, "", true, ""},
		{"deny beats allow", []Permission{PermissionAllow, PermissionDeny}, false, "", false, "approval_denied"},
		{"hook error denies", []Permission{PermissionAllow}, true, "", false, "permission_hook_failed"},
		{"unresolved goes to approver", []Permission{""}, false, PermissionAllow, true, ""},
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
			if err := c.AddHooks(Hooks{OnToolCallStart: func(context.Context, ToolCallStartInput) (ToolCallStartOutput, error) {
				return ToolCallStartOutput{Permission: PermissionAsk, Reason: "needs_review"}, nil
			}}); err != nil {
				t.Fatal(err)
			}
			for _, answer := range tt.answers {
				if err := c.AddHooks(Hooks{OnPermissionRequest: func(_ context.Context, in PermissionRequestInput) (PermissionRequestOutput, error) {
					if in.Decision.Permission != PermissionAsk || in.Decision.Reason != "needs_review" || in.Action.ID != "a" {
						t.Errorf("permission request: %+v", in)
					}
					if tt.hookErr {
						return PermissionRequestOutput{}, errors.New("prompt failed")
					}
					return PermissionRequestOutput{Permission: answer}, nil
				}}); err != nil {
					t.Fatal(err)
				}
			}
			if tt.approver != "" {
				if err := c.SetApprovalHandler(func(_ context.Context, in PermissionRequestInput) (Permission, error) {
					if in.Decision.Reason != "needs_review" {
						t.Errorf("approval request: %+v", in)
					}
					return tt.approver, nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			result, err := c.Run(context.Background(), "goal")
			if err != nil {
				t.Fatal(err)
			}
			if ran != tt.wantRun || !strings.Contains(result.History[0].Results[0].Error, tt.reason) {
				t.Fatalf("ran=%v result=%+v", ran, result.History[0].Results[0])
			}
		})
	}
}

func TestCheckToolCallIsADryRun(t *testing.T) {
	c := New()
	executed := false
	if err := c.AddTool("run", ToolDef{Handler: func(context.Context, protocol.Action) (protocol.ToolResult, error) {
		executed = true
		return protocol.ToolResult{OK: true}, nil
	}}); err != nil {
		t.Fatal(err)
	}
	if err := c.SetApprovalHandler(func(context.Context, PermissionRequestInput) (Permission, error) {
		t.Fatal("dry run reached approval")
		return PermissionAllow, nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, hook := range []func(ToolCallStartInput) (ToolCallStartOutput, error){
		func(in ToolCallStartInput) (ToolCallStartOutput, error) {
			if len(in.Resources) != 1 || in.Resources[0].Value != "git push" {
				t.Errorf("caller resources lost: %+v", in.Resources)
			}
			return ToolCallStartOutput{Permission: PermissionAsk, Reason: "shell_review", AdditionalContext: "first"}, nil
		},
		func(ToolCallStartInput) (ToolCallStartOutput, error) {
			return ToolCallStartOutput{AdditionalContext: "second"}, errors.New("audit failed")
		},
	} {
		if err := c.AddHooks(Hooks{OnToolCallStart: func(_ context.Context, in ToolCallStartInput) (ToolCallStartOutput, error) {
			return hook(in)
		}}); err != nil {
			t.Fatal(err)
		}
	}
	var events []EventType
	c.OnEvent(func(e Event) { events = append(events, e.Type) })

	check, err := c.CheckToolCall(context.Background(), ToolCallStartInput{
		Action:    protocol.Action{ID: "a", Kind: "run", Args: json.RawMessage(`{}`)},
		Resources: []ToolResource{{Kind: "command", Value: "git push"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if check.Decision.Permission != PermissionAsk || check.Decision.Reason != "shell_review" ||
		check.Output.AdditionalContext != "first" || len(check.HookErrors) != 1 {
		t.Fatalf("check = %+v", check)
	}
	if executed || len(events) != 0 {
		t.Fatalf("executed=%v events=%v", executed, events)
	}
}

func TestApprovalSeesProjectionAfterCallsAreRecorded(t *testing.T) {
	c := New()
	setBrain(t, c, &continueBrain{fakeBrain{turns: [][]protocol.Action{
		{{ID: "a", Kind: "run", Args: protocol.MustArgsJSON(map[string]string{"command": "git push"})}},
		{Finish("done")},
	}}})
	if err := c.AddTool("run", ToolDef{
		Handler: func(context.Context, protocol.Action) (protocol.ToolResult, error) {
			return protocol.ToolResult{OK: true}, nil
		},
		Resources: StringArgResource("command", "command"),
	}); err != nil {
		t.Fatal(err)
	}
	var sequence []string
	c.OnEvent(func(e Event) {
		if e.Type == EventAssistantResponse {
			sequence = append(sequence, "recorded")
		}
	})
	addHooks(t, c, Hooks{
		OnToolCallStart: func(context.Context, ToolCallStartInput) (ToolCallStartOutput, error) {
			return ToolCallStartOutput{Permission: PermissionAsk}, nil
		},
		OnPermissionRequest: func(_ context.Context, in PermissionRequestInput) (PermissionRequestOutput, error) {
			if len(in.Resources) != 1 || in.Resources[0].Value != "git push" {
				t.Errorf("permission hook resources: %+v", in.Resources)
			}
			return PermissionRequestOutput{}, nil
		},
	})
	if err := c.SetApprovalHandler(func(_ context.Context, in PermissionRequestInput) (Permission, error) {
		sequence = append(sequence, "approval")
		if len(in.Resources) != 1 || in.Resources[0].Value != "git push" {
			t.Errorf("approval resources: %+v", in.Resources)
		}
		return PermissionAllow, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Run(context.Background(), "push"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sequence, []string{"recorded", "approval"}) {
		t.Fatalf("sequence = %v", sequence)
	}
}

func TestPermissionHookStopDeniesTheWholeTurn(t *testing.T) {
	c := New()
	setBrain(t, c, &continueBrain{fakeBrain{turns: [][]protocol.Action{
		{{ID: "a", Kind: "run"}, {ID: "b", Kind: "run"}, {ID: "c", Kind: "run"}},
		{Finish("done")},
	}}})
	ran := 0
	if err := c.AddTool("run", ToolDef{Handler: func(context.Context, protocol.Action) (protocol.ToolResult, error) {
		ran++
		return protocol.ToolResult{OK: true}, nil
	}}); err != nil {
		t.Fatal(err)
	}
	addHooks(t, c, Hooks{
		OnToolCallStart: func(_ context.Context, in ToolCallStartInput) (ToolCallStartOutput, error) {
			if in.Action.ID == "b" {
				return ToolCallStartOutput{Permission: PermissionAsk}, nil
			}
			return ToolCallStartOutput{}, nil
		},
		OnPermissionRequest: func(context.Context, PermissionRequestInput) (PermissionRequestOutput, error) {
			return PermissionRequestOutput{Permission: PermissionAllow, Stop: true, StopReason: "operator stop"}, nil
		},
	})
	result, err := c.Run(context.Background(), "goal")
	if err != nil {
		t.Fatal(err)
	}
	if ran != 0 || result.Turns != 1 {
		t.Fatalf("ran = %d, result = %+v", ran, result)
	}
	for _, tr := range result.History[0].Results {
		if tr.OK || !strings.Contains(tr.Error, "run_stopped") {
			t.Fatalf("result = %+v", tr)
		}
	}
}

func TestRepeatedDenialCountsTheProposedCall(t *testing.T) {
	c := New()
	c.MaxTurns = 6
	setBrain(t, c, endlessBrain{})
	if err := c.AddTool("ping", ToolDef{Handler: func(context.Context, protocol.Action) (protocol.ToolResult, error) {
		t.Fatal("denied tool ran")
		return protocol.ToolResult{}, nil
	}}); err != nil {
		t.Fatal(err)
	}
	checks := 0
	addHooks(t, c, Hooks{OnToolCallStart: func(_ context.Context, in ToolCallStartInput) (ToolCallStartOutput, error) {
		checks++
		if len(in.Action.Args) == 0 {
			return ToolCallStartOutput{UpdatedInput: json.RawMessage(`{"normalized":true}`)}, nil
		}
		return ToolCallStartOutput{Permission: PermissionDeny, Reason: "blocked"}, nil
	}})
	result, err := c.Run(context.Background(), "goal")
	if err != nil {
		t.Fatal(err)
	}
	// Two hook passes per decided call; the breaker then skips the hooks.
	if checks != 2*repeatedDenialLimit || !strings.Contains(result.History[repeatedDenialLimit].Results[0].Error, "repeated_denial") {
		t.Fatalf("checks = %d, result = %+v", checks, result.History[repeatedDenialLimit].Results[0])
	}
}
