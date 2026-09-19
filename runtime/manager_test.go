package runtime_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samperrin/pons/protocol"
	ponsruntime "github.com/samperrin/pons/runtime"
	runtimesqlite "github.com/samperrin/pons/runtime/sqlite"
)

type (
	Event      = ponsruntime.Event
	Manager    = ponsruntime.Manager
	RunEvent   = ponsruntime.RunEvent
	RunRequest = ponsruntime.RunRequest
	RunResult  = ponsruntime.RunResult
	Runner     = ponsruntime.Runner
	RunnerFunc = ponsruntime.RunnerFunc
	TextPart   = ponsruntime.TextPart
)

const (
	EventAssistantDelta   = ponsruntime.EventAssistantDelta
	EventMessageUpserted  = ponsruntime.EventMessageUpserted
	EventRunUpdated       = ponsruntime.EventRunUpdated
	EventToolCallUpdated  = ponsruntime.EventToolCallUpdated
	RunEventAssistantTurn = ponsruntime.RunEventAssistantTurn
	RunEventToolCompleted = ponsruntime.RunEventToolCompleted
	RunFailed             = ponsruntime.RunFailed
	ToolCompleted         = ponsruntime.ToolCompleted
	ToolInterrupted       = ponsruntime.ToolInterrupted
	ToolRequested         = ponsruntime.ToolRequested
)

var New = ponsruntime.New

type Config = ponsruntime.Config

type claimErrorStore struct {
	ponsruntime.Store
	err error
}

func (s claimErrorStore) ClaimNext(context.Context, string) (*ponsruntime.ClaimedRun, error) {
	return nil, s.err
}

func testManager(t *testing.T, runner Runner) *Manager {
	t.Helper()
	store, err := runtimesqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m, err := New(Config{Store: store, Workspace: t.TempDir(), Runner: runner, MaxConcurrent: 4})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close(); _ = store.Close() })
	return m
}

func waitForEvent(t *testing.T, m *Manager, conversationID, eventType string, count int) []Event {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		events, err := m.Events(context.Background(), conversationID, 0)
		if err != nil {
			t.Fatal(err)
		}
		seen := 0
		for _, event := range events {
			if event.Type == eventType {
				seen++
			}
		}
		if seen >= count {
			return events
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d %s event(s)", count, eventType)
	return nil
}

func TestConversationSerializesMessagesAndDeduplicates(t *testing.T) {
	var active, maximum atomic.Int32
	var mu sync.Mutex
	var seen []string
	runner := RunnerFunc(func(_ context.Context, request RunRequest) (RunResult, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for {
			old := maximum.Load()
			if n <= old || maximum.CompareAndSwap(old, n) {
				break
			}
		}
		mu.Lock()
		seen = append(seen, request.Text)
		mu.Unlock()
		time.Sleep(30 * time.Millisecond)
		return RunResult{Answer: "answer: " + request.Text}, nil
	})
	m := testManager(t, runner)
	conversation, err := m.CreateConversation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	first, err := m.Submit(context.Background(), conversation.ID, "first", []TextPart{{Type: "text", Text: "one"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Submit(context.Background(), conversation.ID, "second", []TextPart{{Type: "text", Text: "two"}})
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := m.Submit(context.Background(), conversation.ID, "first", []TextPart{{Type: "text", Text: "ignored"}})
	if err != nil {
		t.Fatal(err)
	}
	if !duplicate.Duplicate || duplicate.InboundMessageID != first.InboundMessageID {
		t.Fatalf("duplicate = %+v", duplicate)
	}
	events := waitForEvent(t, m, conversation.ID, EventRunUpdated, 4)
	if maximum.Load() != 1 {
		t.Fatalf("conversation concurrency = %d", maximum.Load())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || seen[0] != "one" || seen[1] != "two" {
		t.Fatalf("run order = %v", seen)
	}
	for i := 1; i < len(events); i++ {
		if events[i].ID <= events[i-1].ID {
			t.Fatalf("non-monotonic cursors: %+v", events)
		}
	}
}

func TestBackgroundStoreFailureIsReported(t *testing.T) {
	base, err := runtimesqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	want := errors.New("claim unavailable")
	reported := make(chan error, 1)
	var runnerCalls atomic.Int32
	manager, err := New(Config{
		Store: claimErrorStore{Store: base, err: want}, Workspace: t.TempDir(),
		Runner: RunnerFunc(func(context.Context, RunRequest) (RunResult, error) {
			runnerCalls.Add(1)
			return RunResult{}, nil
		}),
		OnError: func(err error) { reported <- err },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	conversation, err := manager.CreateConversation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Submit(context.Background(), conversation.ID, "key", []TextPart{{Type: "text", Text: "go"}}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-reported:
		if !errors.Is(err, want) {
			t.Fatalf("reported error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("background store failure was not reported")
	}
	if runnerCalls.Load() != 0 {
		t.Fatalf("runner calls = %d, want 0", runnerCalls.Load())
	}
}

func TestSnapshotThenEventsHasNoDurableGap(t *testing.T) {
	release := make(chan struct{})
	m := testManager(t, RunnerFunc(func(ctx context.Context, _ RunRequest) (RunResult, error) {
		select {
		case <-release:
			return RunResult{Answer: "done"}, nil
		case <-ctx.Done():
			return RunResult{}, ctx.Err()
		}
	}))
	conversation, _ := m.CreateConversation(context.Background())
	view, err := m.View(context.Background(), conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	stream, err := m.Subscribe(ctx, conversation.ID, view.EventCursor)
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := m.Submit(context.Background(), conversation.ID, "key", []TextPart{{Type: "text", Text: "hello"}})
	if err != nil {
		t.Fatal(err)
	}
	close(release)
	deadline := time.After(3 * time.Second)
	foundUser, foundFinal := false, false
	for !foundFinal {
		select {
		case event := <-stream:
			if event.ID <= view.EventCursor {
				t.Fatalf("event cursor %d <= snapshot cursor %d", event.ID, view.EventCursor)
			}
			if event.Type == EventMessageUpserted && event.Message != nil && event.Message.ID == accepted.InboundMessageID {
				foundUser = true
			}
			if event.Type == EventMessageUpserted && event.Message != nil && event.Message.Final {
				foundFinal = true
			}
		case <-deadline:
			t.Fatal("timed out waiting for catch-up/live events")
		}
	}
	if !foundUser {
		t.Fatal("user message upsert was missed")
	}
}

func TestTransientDeltaIsLiveOnlyAndDoesNotAdvanceCursor(t *testing.T) {
	release := make(chan struct{})
	m := testManager(t, RunnerFunc(func(ctx context.Context, request RunRequest) (RunResult, error) {
		if err := request.Emit(RunEvent{Type: EventAssistantDelta, MessageID: "draft", PartID: "text", Text: "hel"}); err != nil {
			return RunResult{}, err
		}
		select {
		case <-release:
			return RunResult{Answer: "hello"}, nil
		case <-ctx.Done():
			return RunResult{}, ctx.Err()
		}
	}))
	conversation, _ := m.CreateConversation(context.Background())
	ctx := t.Context()
	stream, err := m.Subscribe(ctx, conversation.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Submit(context.Background(), conversation.ID, "key", []TextPart{{Type: "text", Text: "go"}})
	if err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case event := <-stream:
			if event.Type == EventAssistantDelta {
				if event.ID != 0 || event.Delta == nil || event.Delta.Text != "hel" {
					t.Fatalf("delta = %+v", event)
				}
				persisted, loadErr := m.Events(context.Background(), conversation.ID, 0)
				if loadErr != nil {
					t.Fatal(loadErr)
				}
				for _, durable := range persisted {
					if durable.Type == EventAssistantDelta {
						t.Fatal("transient delta was persisted")
					}
				}
				view, viewErr := m.View(context.Background(), conversation.ID)
				if viewErr != nil {
					t.Fatal(viewErr)
				}
				for _, message := range view.Messages {
					if message.ID == "draft" {
						t.Fatal("draft leaked into snapshot")
					}
				}
				close(release)
				return
			}
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for delta")
		}
	}
}

func TestToolLifecycleUsesEntityUpserts(t *testing.T) {
	m := testManager(t, RunnerFunc(func(_ context.Context, request RunRequest) (RunResult, error) {
		action := protocol.Action{ID: "call-1", Kind: "fake", Args: protocol.MustArgsJSON(map[string]string{"value": request.Text})}
		if err := request.Emit(RunEvent{Type: RunEventAssistantTurn, Parts: []ponsruntime.MessagePart{{Type: "tool_call", ToolCallID: action.ID, ToolKind: string(action.Kind), Arguments: action.Args}}}); err != nil {
			return RunResult{}, err
		}
		result := protocol.ToolResult{ActionID: action.ID, Kind: "fake", OK: true, Output: "tool output"}
		if err := request.Emit(RunEvent{Type: RunEventToolCompleted, Result: &result}); err != nil {
			return RunResult{}, err
		}
		return RunResult{Answer: "hello " + request.Text}, nil
	}))
	conversation, err := m.CreateConversation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Submit(context.Background(), conversation.ID, "stable", []TextPart{{Type: "text", Text: "world"}}); err != nil {
		t.Fatal(err)
	}
	observed := waitForEvent(t, m, conversation.ID, EventRunUpdated, 2)
	requested, completed := false, false
	for _, event := range observed {
		if event.Type == EventToolCallUpdated && event.ToolCall != nil && event.ToolCall.Status == ToolRequested {
			requested = true
		}
		if event.Type == EventToolCallUpdated && event.ToolCall != nil && event.ToolCall.Status == ToolCompleted {
			completed = true
		}
	}
	if !requested || !completed {
		t.Fatalf("tool lifecycle missing: %+v", observed)
	}
	view, err := m.View(context.Background(), conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Messages) != 4 || view.EventCursor == 0 {
		t.Fatalf("view = %+v", view)
	}
}

func TestFailedRunIsDurableAndNotRetried(t *testing.T) {
	var executions atomic.Int32
	m := testManager(t, RunnerFunc(func(_ context.Context, request RunRequest) (RunResult, error) {
		executions.Add(1)
		action := protocol.Action{ID: "started", Kind: "bash", Args: protocol.MustArgsJSON(map[string]string{"command": "side effect"})}
		if err := request.Emit(RunEvent{Type: RunEventAssistantTurn, Parts: []ponsruntime.MessagePart{{Type: "tool_call", ToolCallID: action.ID, ToolKind: string(action.Kind), Arguments: action.Args}}}); err != nil {
			return RunResult{}, err
		}
		return RunResult{}, errors.New("failed after intent")
	}))
	conversation, _ := m.CreateConversation(context.Background())
	_, err := m.Submit(context.Background(), conversation.ID, "once", []TextPart{{Type: "text", Text: "go"}})
	if err != nil {
		t.Fatal(err)
	}
	events := waitForEvent(t, m, conversation.ID, EventRunUpdated, 2)
	last := events[len(events)-2]
	_ = last
	if executions.Load() != 1 {
		t.Fatalf("executions = %d", executions.Load())
	}
	view, err := m.View(context.Background(), conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if view.ActiveRun != nil || len(view.ToolCalls) != 1 || view.ToolCalls[0].Status != ToolInterrupted {
		t.Fatalf("view = %+v", view)
	}
}

func TestRestartMarksRequestedToolInterruptedWithoutRetry(t *testing.T) {
	stateDir, workspace := t.TempDir(), t.TempDir()
	store, err := runtimesqlite.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	first, err := New(Config{Store: store, Workspace: workspace, Runner: RunnerFunc(func(context.Context, RunRequest) (RunResult, error) {
		return RunResult{}, errors.New("unexpected run")
	})})
	if err != nil {
		t.Fatal(err)
	}
	conversation, err := first.CreateConversation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	accepted, _, err := store.Accept(context.Background(), conversation.ID, "stable", []TextPart{{Type: "text", Text: "do it"}})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := store.ClaimNext(context.Background(), conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	action := protocol.Action{ID: "dangerous", Kind: "bash", Args: protocol.MustArgsJSON(map[string]string{"command": "side effect"})}
	_, err = store.CommitAssistantTurn(context.Background(), claim.Run, []ponsruntime.MessagePart{{Type: "tool_call", ToolCallID: action.ID, ToolKind: string(action.Kind), Arguments: action.Args}})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	var executions atomic.Int32
	second, err := New(Config{Store: store, Workspace: workspace, Runner: RunnerFunc(func(context.Context, RunRequest) (RunResult, error) {
		executions.Add(1)
		return RunResult{}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	view, err := second.View(context.Background(), conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if executions.Load() != 0 || view.ActiveRun != nil || len(view.ToolCalls) != 1 || view.ToolCalls[0].Status != ToolInterrupted {
		t.Fatalf("recovered view = %+v; executions = %d", view, executions.Load())
	}
	if len(view.Submissions) != 1 || view.Submissions[0].Status != RunFailed {
		t.Fatalf("recovered submissions = %+v", view.Submissions)
	}
	duplicate, err := second.Submit(context.Background(), conversation.ID, "stable", []TextPart{{Type: "text", Text: "retry"}})
	if err != nil {
		t.Fatal(err)
	}
	if !duplicate.Duplicate || duplicate.InboundMessageID != accepted.InboundMessageID {
		t.Fatalf("duplicate = %+v", duplicate)
	}
}

func TestClaimHistoryExcludesLaterQueuedMessages(t *testing.T) {
	ctx := context.Background()
	store, err := runtimesqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	conversation := ponsruntime.Conversation{ID: ponsruntime.NewID(), Workspace: t.TempDir(), CreatedAt: time.Now().UTC()}
	if err := store.CreateConversation(ctx, conversation); err != nil {
		t.Fatal(err)
	}
	first, _, err := store.Accept(ctx, conversation.ID, "first", []TextPart{{Type: "text", Text: "A"}})
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := store.Accept(ctx, conversation.ID, "second", []TextPart{{Type: "text", Text: "B"}})
	if err != nil {
		t.Fatal(err)
	}
	claimA, err := store.ClaimNext(ctx, conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if claimA.Message.ID != first.InboundMessageID || len(claimA.History) != 0 {
		t.Fatalf("first claim = %+v", claimA)
	}
	if _, _, err := store.FinishRun(ctx, claimA.Run, "answer A"); err != nil {
		t.Fatal(err)
	}
	claimB, err := store.ClaimNext(ctx, conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if claimB.Message.ID != second.InboundMessageID || len(claimB.History) != 2 {
		t.Fatalf("second claim = %+v", claimB)
	}
	if got := claimB.History[0]; got.InboundMessageID != first.InboundMessageID || got.Role != "user" {
		t.Fatalf("history[0] = %+v", got)
	}
	if got := claimB.History[1]; got.InboundMessageID != first.InboundMessageID || got.Role != "assistant" || !got.Final {
		t.Fatalf("history[1] = %+v", got)
	}
}

func TestAssistantToolTurnsRemainOrderedAndComplete(t *testing.T) {
	ctx := context.Background()
	store, err := runtimesqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	conversation := ponsruntime.Conversation{ID: ponsruntime.NewID(), Workspace: t.TempDir(), CreatedAt: time.Now().UTC()}
	if err := store.CreateConversation(ctx, conversation); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Accept(ctx, conversation.ID, "one", []TextPart{{Type: "text", Text: "go"}}); err != nil {
		t.Fatal(err)
	}
	claim, err := store.ClaimNext(ctx, conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"call-1", "call-2"} {
		args := protocol.MustArgsJSON(map[string]any{"round": i + 1})
		if i == 0 {
			args = nil
		}
		parts := []ponsruntime.MessagePart{
			{Type: "text", Text: "before " + id},
			{Type: "tool_call", ToolCallID: id, ToolKind: "fake", Arguments: args},
			{Type: "text", Text: "after " + id},
		}
		if _, err := store.CommitAssistantTurn(ctx, claim.Run, parts); err != nil {
			t.Fatal(err)
		}
		if _, err := store.ToolCompleted(ctx, claim.Run, protocol.ToolResult{ActionID: id, Kind: "fake", OK: true, Output: id + " result"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := store.FinishRun(ctx, claim.Run, "done"); err != nil {
		t.Fatal(err)
	}
	view, err := store.View(ctx, conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Messages) != 6 {
		t.Fatalf("messages = %+v", view.Messages)
	}
	for _, index := range []int{1, 3} {
		message := view.Messages[index]
		if message.Role != "assistant" || !message.Complete || message.Final || len(message.Parts) != 3 || message.Parts[0].Type != "text" || message.Parts[1].Type != "tool_call" || message.Parts[2].Type != "text" {
			t.Fatalf("assistant turn %d = %+v", index, message)
		}
	}
	if view.Messages[1].ID == view.Messages[3].ID {
		t.Fatal("separate planning rounds were merged")
	}
	if got := string(view.Messages[1].Parts[1].Arguments); got != "{}" {
		t.Fatalf("normalized arguments = %q", got)
	}
}

func TestRestartRunsDurablyQueuedSubmission(t *testing.T) {
	stateDir, workspace := t.TempDir(), t.TempDir()
	store, err := runtimesqlite.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	first, err := New(Config{Store: store, Workspace: workspace, Runner: RunnerFunc(func(context.Context, RunRequest) (RunResult, error) {
		return RunResult{}, errors.New("unexpected run")
	})})
	if err != nil {
		t.Fatal(err)
	}
	conversation, err := first.CreateConversation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Accept(context.Background(), conversation.ID, "queued", []TextPart{{Type: "text", Text: "resume me"}}); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	ran := make(chan string, 1)
	second, err := New(Config{Store: store, Workspace: workspace, Runner: RunnerFunc(func(_ context.Context, request RunRequest) (RunResult, error) {
		ran <- request.Text
		return RunResult{Answer: "resumed"}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	select {
	case text := <-ran:
		if text != "resume me" {
			t.Fatalf("resumed text = %q", text)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("queued submission did not resume")
	}
	waitForEvent(t, second, conversation.ID, EventRunUpdated, 2)
}
