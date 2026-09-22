package llm

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/samperrin/pons"
	"github.com/samperrin/pons/protocol"
)

// fakeClient is a scripted provider for testing the brain logic without HTTP.
func TestToolInputPreservesLargeInteger(t *testing.T) {
	const raw = `{"value":9007199254740993}`
	input := decodeToolInput([]byte(raw))
	if got := string(stringify(input)); got != raw {
		t.Fatalf("tool input lost numeric precision: got %s, want %s", got, raw)
	}
}

func TestSystemPromptContract(t *testing.T) {
	b := &Brain{}
	prompt := b.systemPrompt()
	for _, want := range []string{
		"The available tool schemas are the complete capability set for this run.",
		"Calls in the same turn may run concurrently",
		"A text-only response ends the task and becomes the final answer.",
		"Treat user reports and proposed causes as claims to verify",
		"Preserve unrelated work already present in the workspace",
		"Never claim a check passed unless its result was observed.",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("system prompt missing %q", want)
		}
	}
	for _, legacy := range []string{"PONS_SESSION_FILE", "NDJSON"} {
		if strings.Contains(prompt, legacy) {
			t.Errorf("legacy transcript term %q leaked into system prompt", legacy)
		}
	}
}

func TestSystemPromptFramesAdditionalInstructions(t *testing.T) {
	b := &Brain{cfg: Config{SystemExtra: "Prefer table-driven tests."}}
	prompt := b.systemPrompt()
	want := "<additional_instructions>\nPrefer table-driven tests.\n</additional_instructions>\n"
	if !strings.HasSuffix(prompt, want) {
		t.Fatalf("additional instructions not framed at end of system prompt:\n%s", prompt)
	}
}

type fakeClient struct {
	responses     []Turn          // assistant turns to return, in order
	seen          [][]Turn        // what each call received
	capturedTools []pons.ToolSpec // tools from the last call
}

func (f *fakeClient) Complete(ctx context.Context, system string, turns []Turn, tools []pons.ToolSpec) (Turn, error) {
	f.seen = append(f.seen, turns)
	f.capturedTools = tools
	i := len(f.seen) - 1
	if i >= len(f.responses) {
		return Turn{}, context.DeadlineExceeded
	}
	return f.responses[i], nil
}

func newBrain(t *testing.T, c Client) *Brain {
	t.Helper()
	b := &Brain{cfg: Config{Model: "test"}, client: c, core: pons.New()}
	return b
}

func TestBrainMapsToolUseToActions(t *testing.T) {
	fake := &fakeClient{responses: []Turn{
		{Role: "assistant", Blocks: []Block{
			Text{Value: "Let me look around."},
			ToolUse{ID: "call_1", Name: "bash", Input: map[string]any{"command": "ls", "timeout": float64(10)}},
		}},
		{Role: "assistant", Blocks: []Block{Text{Value: "All done."}}},
	}}
	b := newBrain(t, fake)
	ctx := context.Background()
	obs := protocol.Observation{Turn: 1, Message: "g", Workspace: "/w"}

	response, err := b.Respond(ctx, obs)
	if err != nil {
		t.Fatal(err)
	}
	actions := response.Actions
	if len(actions) != 1 || actions[0].Kind != "bash" || actions[0].ID != "call_1" {
		t.Fatalf("actions: %+v", actions)
	}
	if len(response.Parts) != 2 || response.Parts[0].Type != pons.AssistantPartText || response.Parts[0].Text != "Let me look around." || response.Parts[1].Type != pons.AssistantPartToolCall || response.Parts[1].Action.ID != "call_1" {
		t.Fatalf("assistant parts: %+v", response.Parts)
	}
	var input map[string]any
	if err := json.Unmarshal(actions[0].Args, &input); err != nil {
		t.Fatal(err)
	}
	if input["command"] != "ls" || input["timeout"] != float64(10) {
		t.Fatalf("typed args not preserved: %+v", input)
	}

	// Result flows back as a tool_result block on the next call.
	if _, err := b.Interpret(ctx, obs, protocol.ToolResult{ActionID: "call_1", OK: true, Output: "hi\n"}); err != nil {
		t.Fatal(err)
	}
	response, err = b.Respond(ctx, obs)
	if err != nil {
		t.Fatal(err)
	}
	actions = response.Actions
	reason, _ := protocol.StringArg(actions[0].Args, "reason")
	if len(actions) != 1 || actions[0].Kind != protocol.ActFinish || reason != "All done." {
		t.Fatalf("expected finish, got: %+v", actions)
	}

	// Second call must contain: goal, assistant tool_use, tool_result.
	got := fake.seen[1]
	if len(got) != 3 {
		t.Fatalf("expected 3 turns, got %d", len(got))
	}
	var sawResult bool
	for _, blk := range got[2].Blocks {
		if r, ok := blk.(Result); ok && r.ToolUseID == "call_1" && r.Content == "hi\n" && !r.IsError {
			sawResult = true
		}
	}
	if !sawResult {
		t.Fatalf("tool_result not forwarded: %+v", got[2].Blocks)
	}
}

func TestPendingResultsDoNotDropFollowUp(t *testing.T) {
	fake := &fakeClient{responses: []Turn{
		{Role: "assistant", Blocks: []Block{ToolUse{ID: "c1", Name: "ping", Input: map[string]any{}}}},
		{Role: "assistant", Blocks: []Block{Text{Value: "done"}}},
	}}
	b := newBrain(t, fake)
	ctx := context.Background()
	if _, err := b.Respond(ctx, protocol.Observation{Turn: 1, Message: "first", Workspace: "/w"}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Interpret(ctx, protocol.Observation{}, protocol.ToolResult{ActionID: "c1", OK: true, Output: "pong"}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Respond(ctx, protocol.Observation{Turn: 1, Message: "follow-up", Workspace: "/w"}); err != nil {
		t.Fatal(err)
	}
	seen := fake.seen[1]
	var gotResult, gotFollowUp bool
	for _, turn := range seen {
		for _, blk := range turn.Blocks {
			switch v := blk.(type) {
			case Result:
				gotResult = v.ToolUseID == "c1"
			case Text:
				gotFollowUp = gotFollowUp || strings.Contains(v.Value, "follow-up")
			}
		}
	}
	if !gotResult || !gotFollowUp {
		t.Fatalf("follow-up context lost pending result or message: %+v", seen)
	}
}

func TestFailedToolIsErrorResult(t *testing.T) {
	fake := &fakeClient{responses: []Turn{
		{Role: "assistant", Blocks: []Block{ToolUse{ID: "c1", Name: "nope", Input: map[string]any{}}}},
		{Role: "assistant", Blocks: []Block{Text{Value: "giving up"}}},
	}}
	b := newBrain(t, fake)
	ctx := context.Background()
	obs := protocol.Observation{Turn: 1}

	if _, err := b.Respond(ctx, obs); err != nil {
		t.Fatal(err)
	}
	b.Interpret(ctx, obs, protocol.ToolResult{ActionID: "c1", OK: false, Error: "no plugin provides"})
	b.Respond(ctx, obs)

	for _, blk := range fake.seen[1][2].Blocks {
		if r, ok := blk.(Result); ok && r.IsError {
			return // expected
		}
	}
	t.Fatal("expected IsError on failed tool result")
}

func TestEmptyResponseIsSurfacedNotSwallowed(t *testing.T) {
	fake := &fakeClient{responses: []Turn{
		{Role: "assistant", Blocks: []Block{}}, // degenerate: nothing at all
	}}
	b := &Brain{cfg: Config{}, client: fake, core: pons.New()}
	_, err := b.Respond(context.Background(), protocol.Observation{Turn: 1, Message: "test"})
	if err == nil || !strings.Contains(err.Error(), "empty response") {
		t.Fatalf("empty response should error, got: %v", err)
	}
}

func TestCompactionKeepsToolExchangeTogether(t *testing.T) {
	fake := &fakeClient{responses: []Turn{
		{Role: "assistant", Blocks: []Block{ToolUse{ID: "c1", Name: "ping", Input: map[string]any{}}}},
		{Role: "assistant", Blocks: []Block{ToolUse{ID: "c2", Name: "ping", Input: map[string]any{}}}},
		{Role: "assistant", Blocks: []Block{Text{Value: "summary"}}},
		{Role: "assistant", Blocks: []Block{Text{Value: "done"}}},
	}}
	b := &Brain{cfg: Config{CompactChars: 1, CompactKeep: 1}, client: fake, core: pons.New()}
	ctx := context.Background()
	obs := protocol.Observation{Turn: 1, Message: "goal"}
	if _, err := b.Respond(ctx, obs); err != nil {
		t.Fatal(err)
	}
	b.Interpret(ctx, obs, protocol.ToolResult{ActionID: "c1", OK: true, Output: "one"})
	if _, err := b.Respond(ctx, obs); err != nil {
		t.Fatal(err)
	}
	b.Interpret(ctx, obs, protocol.ToolResult{ActionID: "c2", OK: true, Output: "two"})
	if _, err := b.Respond(ctx, obs); err != nil {
		t.Fatal(err)
	}

	last := fake.seen[len(fake.seen)-1]
	var sawC2, sawC2Result bool
	for _, turn := range last {
		for _, blk := range turn.Blocks {
			switch v := blk.(type) {
			case ToolUse:
				sawC2 = sawC2 || v.ID == "c2"
			case Result:
				sawC2Result = sawC2Result || v.ToolUseID == "c2"
			}
		}
	}
	if !sawC2 || !sawC2Result {
		t.Fatalf("compaction split the retained tool exchange: %+v", last)
	}
}

func TestCompactionCollapsesMiddleTurns(t *testing.T) {
	fake := &fakeClient{responses: []Turn{
		{Role: "assistant", Blocks: []Block{ToolUse{ID: "c1", Name: "ping", Input: map[string]any{}}}},
		{Role: "assistant", Blocks: []Block{ToolUse{ID: "c2", Name: "ping", Input: map[string]any{}}}},
		{Role: "assistant", Blocks: []Block{Text{Value: "SUMMARY: two pings ran; state is fine."}}},
		{Role: "assistant", Blocks: []Block{Text{Value: "all done"}}},
	}}
	b := &Brain{cfg: Config{CompactChars: 10, CompactKeep: 2}, client: fake, core: pons.New()}

	ctx := context.Background()
	obs := protocol.Observation{Turn: 1, Message: "test goal"}
	// Turn 1: tool call (no compaction — turns ≤ keep+1).
	if _, err := b.Respond(ctx, obs); err != nil {
		t.Fatal(err)
	}
	b.Interpret(ctx, obs, protocol.ToolResult{ActionID: "c1", OK: true, Output: "FIRST-PONG " + strings.Repeat("mid ", 30)})
	// Turn 2: still ≤ keep+1 → compaction no-op; second tool call.
	if _, err := b.Respond(ctx, obs); err != nil {
		t.Fatal(err)
	}
	b.Interpret(ctx, obs, protocol.ToolResult{ActionID: "c2", OK: true, Output: "SECOND-PONG " + strings.Repeat("tail ", 30)})
	// Turn 3: now there is a real middle (2 tool turns) — the summarizer
	// call (seen[2]) fires, then the real Complete (seen[3]) gets the
	// collapsed context.
	if _, err := b.Respond(ctx, obs); err != nil {
		t.Fatal(err)
	}

	// seen[2] is the summarizer request: it carries the collapsed middle
	// and no tools.
	if len(fake.seen) != 4 {
		t.Fatalf("expected 4 client calls (2 turns, 1 summarizer, 1 final), got %d", len(fake.seen))
	}
	sumReq := fake.seen[2]
	if len(sumReq) != 1 || sumReq[0].Role != "user" {
		t.Fatalf("summarizer input shape: %+v", sumReq)
	}
	middleRendered := false
	for _, blk := range sumReq[0].Blocks {
		if txt, ok := blk.(Text); ok && strings.Contains(txt.Value, "FIRST-PONG") {
			middleRendered = true
		}
	}
	if !middleRendered {
		t.Fatalf("summarizer did not receive the middle turns: %+v", sumReq[0])
	}

	// The post-compaction context: goal + summary, middle gone.
	last := fake.seen[len(fake.seen)-1]
	var sawMessage, sawSummary, sawStale bool
	for _, t2 := range last {
		for _, blk := range t2.Blocks {
			if txt, ok := blk.(Text); ok {
				if strings.Contains(txt.Value, "test goal") {
					sawMessage = true
				}
				if strings.Contains(txt.Value, "SUMMARY: two pings") {
					sawSummary = true
				}
			}
			if r, ok := blk.(Result); ok && strings.Contains(r.Content, "FIRST-PONG") {
				sawStale = true
			}
		}
	}
	if !sawMessage || !sawSummary {
		t.Fatalf("compacted context missing message or summary: %+v", last)
	}
	if sawStale {
		t.Fatalf("collapsed middle result leaked into context: %+v", last)
	}
	if len(b.turns) > 5 { // goal + summary + tail(2) + new assistant turn
		t.Fatalf("turns did not collapse: %d", len(b.turns))
	}
}

func TestToolSpecsReachTheModel(t *testing.T) {
	fake := &fakeClient{responses: []Turn{{Role: "assistant", Blocks: []Block{Text{Value: "done"}}}}}
	b := newBrain(t, fake)
	if err := b.core.AddTool("bash", pons.ToolDef{
		Handler:     func(context.Context, protocol.Action) (protocol.ToolResult, error) { return protocol.ToolResult{}, nil },
		Description: "run a command",
		Params:      []pons.ToolParam{{Name: "command", Type: "string", Required: true}},
	}); err != nil {
		t.Fatal(err)
	}
	b.Respond(context.Background(), protocol.Observation{Turn: 1})
	if len(fake.capturedTools) != 1 || fake.capturedTools[0].Kind != "bash" {
		t.Fatalf("tools not passed to client: %+v", fake.capturedTools)
	}
}
