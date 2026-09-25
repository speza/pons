package runtime_test

import (
	"context"
	"errors"
	"testing"

	"github.com/samperrin/pons/protocol"
	ponsruntime "github.com/samperrin/pons/runtime"
)

func TestStopRunEndsRunGracefullyWithDurableNotice(t *testing.T) {
	started := make(chan struct{})
	m := testManager(t, RunnerFunc(func(ctx context.Context, request RunRequest) (RunResult, error) {
		action := protocol.Action{ID: "call_1", Kind: "bash", Args: protocol.MustArgsJSON(map[string]string{"command": "sleep"})}
		if err := request.Emit(RunEvent{Type: RunEventAssistantTurn, Parts: []ponsruntime.MessagePart{{
			Type: "tool_call", ToolCallID: action.ID, ToolKind: string(action.Kind), Arguments: action.Args,
		}}}); err != nil {
			return RunResult{}, err
		}
		close(started)
		<-ctx.Done()
		return RunResult{}, ctx.Err()
	}))
	ctx := context.Background()
	conversation, err := m.CreateConversation(ctx, ponsruntime.ConversationOptions{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.StopRun(ctx, conversation.ID); !errors.Is(err, ponsruntime.ErrNoActiveRun) {
		t.Fatalf("stop with nothing running error = %v", err)
	}
	if _, err := m.Submit(ctx, conversation.ID, "work", []TextPart{{Type: "text", Text: "work"}}); err != nil {
		t.Fatal(err)
	}
	<-started

	run, err := m.StopRun(ctx, conversation.ID)
	if err != nil || run.ConversationID != conversation.ID {
		t.Fatalf("stop = %+v, error = %v", run, err)
	}
	waitForEvent(t, m, conversation.ID, ponsruntime.EventRunStopped, 1)

	view, err := m.View(ctx, conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if view.ActiveRun != nil || len(view.Submissions) != 1 || view.Submissions[0].Status != ponsruntime.RunStopped {
		t.Fatalf("view after stop = %+v", view)
	}
	if len(view.ToolCalls) != 1 || view.ToolCalls[0].Status != ToolInterrupted {
		t.Fatalf("tool after stop = %+v", view.ToolCalls)
	}
	notice := view.Messages[len(view.Messages)-1]
	if notice.Role != "notice" || notice.Notice == nil || notice.Notice.Status != ponsruntime.RunStopped ||
		notice.Parts[0].Text != ponsruntime.NoticeStoppedText || notice.ID != run.ID+":notice" {
		t.Fatalf("stop notice = %+v", notice)
	}
	if _, err := m.StopRun(ctx, conversation.ID); !errors.Is(err, ponsruntime.ErrNoActiveRun) {
		t.Fatalf("second stop error = %v", err)
	}
}

func TestFailedRunProjectsErrorNotice(t *testing.T) {
	m := testManager(t, RunnerFunc(func(context.Context, RunRequest) (RunResult, error) {
		return RunResult{}, errors.New("provider unavailable")
	}))
	ctx := context.Background()
	conversation, err := m.CreateConversation(ctx, ponsruntime.ConversationOptions{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Submit(ctx, conversation.ID, "work", []TextPart{{Type: "text", Text: "work"}}); err != nil {
		t.Fatal(err)
	}
	events := waitForEvent(t, m, conversation.ID, ponsruntime.EventRunFailed, 1)

	view, err := m.View(ctx, conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Messages) != 2 {
		t.Fatalf("messages = %+v, want the input and its error notice", view.Messages)
	}
	notice := view.Messages[1]
	if notice.Role != "notice" || notice.Notice.Status != ponsruntime.RunFailed ||
		notice.Notice.Error != "provider unavailable" || notice.Parts[0].Text != ponsruntime.NoticeFailedText {
		t.Fatalf("error notice = %+v", notice)
	}
	// Live replay projects the same message from the same durable event.
	replayed, err := events[len(events)-1].ProjectMessage()
	if err != nil || replayed.ID != notice.ID || replayed.Parts[0].Text != notice.Parts[0].Text {
		t.Fatalf("replayed notice = %+v, error = %v", replayed, err)
	}
}
