package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/samperrin/pons/environment"
	"github.com/samperrin/pons/protocol"
	ponsruntime "github.com/samperrin/pons/runtime"
)

var (
	testAgent    = ponsruntime.AgentDefinition{ID: ponsruntime.DefaultAgentID, Name: "Test"}
	testRevision = testAgent.Revision()
)

func acceptInput(store *Store, ctx context.Context, conversationID, key string, parts []ponsruntime.TextPart) (ponsruntime.AcceptedMessage, []ponsruntime.Event, error) {
	return store.Accept(ctx, conversationID, ponsruntime.InputSubmission{
		IdempotencyKey: key, Parts: parts,
		Source:        ponsruntime.InputSource{Kind: "human", Adapter: "test"},
		TargetAgentID: testAgent.ID, AgentRevision: testRevision,
	})
}

func TestOpenRejectsOlderExistingDatabase(t *testing.T) {
	for _, statement := range []string{"PRAGMA user_version = 0", "PRAGMA user_version = 1", "PRAGMA user_version = 2", "PRAGMA user_version = 3", "PRAGMA user_version = 4", "PRAGMA user_version = 5", "PRAGMA user_version = 6", "PRAGMA user_version = 7", "PRAGMA user_version = 8", "PRAGMA user_version = 9", "PRAGMA user_version = 10"} {
		t.Run(statement, func(t *testing.T) {
			stateDir := t.TempDir()
			store, err := Open(stateDir)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.db.Exec(statement); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			if reopened, err := Open(stateDir); err == nil {
				_ = reopened.Close()
				t.Fatal("accepted older database")
			} else if !strings.Contains(err.Error(), "recreate the runtime state directory") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestOpenConfiguresBusyTimeout(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var timeout int
	if err := store.db.QueryRowContext(context.Background(), `PRAGMA busy_timeout`).Scan(&timeout); err != nil {
		t.Fatal(err)
	}
	if timeout <= 0 {
		t.Fatalf("busy_timeout = %d", timeout)
	}
}

func TestConversationGitSelectionSurvivesClaim(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	for _, selection := range []struct {
		id, repository string
		all            bool
	}{
		{"session-a", "https://github.com/acme/a.git", false},
		{"session-b", "https://github.com/acme/b.git", true},
		{"session-a-again", "https://github.com/acme/a.git", false},
	} {
		conversation := ponsruntime.Conversation{
			ID: selection.id, AgentID: testAgent.ID, Workspace: t.TempDir(), GitRepository: selection.repository,
			GitRevision: strings.Repeat("a", 40), GitAllRepositories: selection.all,
			CreatedAt: time.Now().UTC(),
		}
		if err := store.CreateConversation(ctx, conversation); err != nil {
			t.Fatal(err)
		}
		if _, _, err := acceptInput(store, ctx, selection.id, "first", []ponsruntime.TextPart{{Type: "text", Text: "work"}}); err != nil {
			t.Fatal(err)
		}
		claim, err := store.ClaimRunnable(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if claim == nil || claim.Conversation.ID != selection.id ||
			claim.Conversation.GitRepository != selection.repository ||
			claim.Conversation.GitRevision != conversation.GitRevision ||
			claim.Conversation.GitAllRepositories != selection.all {
			t.Fatalf("claim = %+v, want selection %+v", claim, selection)
		}
		if _, err := store.FinishRun(ctx, claim.Run, "done"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestContentEventsProjectSnapshotAndReplay(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	conversation := ponsruntime.Conversation{ID: "event-log", AgentID: testAgent.ID, Workspace: t.TempDir(), CreatedAt: time.Now().UTC()}
	if err := store.CreateConversation(ctx, conversation); err != nil {
		t.Fatal(err)
	}
	first, acceptedEvents, err := acceptInput(store, ctx, conversation.ID, "first", []ponsruntime.TextPart{{Type: "text", Text: "A"}})
	if err != nil {
		t.Fatal(err)
	}
	claimA, err := store.ClaimRunnable(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if claimA.Message.ID != first.InboundMessageID || len(claimA.Context) != 0 ||
		len(claimA.Events) != 2 || claimA.Events[0].Type != ponsruntime.EventInputAdmitted {
		t.Fatalf("first claim = %+v", claimA)
	}
	second, _, err := acceptInput(store, ctx, conversation.ID, "second", []ponsruntime.TextPart{{Type: "text", Text: "B"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.FinishRun(ctx, claimA.Run, "answer A"); err != nil {
		t.Fatal(err)
	}
	claimB, err := store.ClaimRunnable(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if claimB.Message.ID != second.InboundMessageID || len(claimB.Context) != 2 ||
		claimB.Context[0].Content[0].Text != "A" || claimB.Context[1].Content[0].Text != "answer A" {
		t.Fatalf("second claim history = %+v", claimB.Context)
	}
	view, err := store.View(ctx, conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Messages) != 3 || view.Messages[0].ID != first.InboundMessageID ||
		view.Messages[1].ID != second.InboundMessageID || view.Messages[2].Role != "assistant" || !view.Messages[2].Final {
		t.Fatalf("view messages = %+v", view.Messages)
	}
	allEvents, err := store.Events(ctx, conversation.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var projected []ponsruntime.Message
	for _, event := range allEvents {
		switch event.Type {
		case ponsruntime.EventInputAccepted, ponsruntime.EventAssistantCommitted, ponsruntime.EventToolOutcomeRecorded:
			message, err := event.ProjectMessage()
			if err != nil {
				t.Fatal(err)
			}
			projected = append(projected, message)
		}
	}
	if !reflect.DeepEqual(projected, view.Messages) {
		t.Fatalf("event projection = %+v, snapshot = %+v", projected, view.Messages)
	}
	projectedView, err := ponsruntime.ProjectView(view.Conversation, allEvents)
	if err != nil {
		t.Fatal(err)
	}
	projectedView.EventCursor = view.EventCursor // private events share the cursor
	if !reflect.DeepEqual(projectedView, view) {
		t.Fatalf("event view = %+v, snapshot = %+v", projectedView, view)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE submissions SET status = ?, error = ? WHERE id = ?`,
		ponsruntime.RunFailed, "stale index", second.InboundMessageID); err != nil {
		t.Fatal(err)
	}
	indexIndependentView, err := store.View(ctx, conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(indexIndependentView, view) {
		t.Fatalf("snapshot changed with operational index: %+v", indexIndependentView)
	}
	replayed, err := store.Events(ctx, conversation.ID, acceptedEvents[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(replayed) == 0 || replayed[len(replayed)-1].ID != view.EventCursor {
		t.Fatalf("replayed events = %+v; view cursor = %d", replayed, view.EventCursor)
	}
	var messageRecords int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE conversation_id = ? AND type IN (?, ?, ?)`,
		conversation.ID, ponsruntime.EventInputAccepted, ponsruntime.EventAssistantCommitted,
		ponsruntime.EventToolOutcomeRecorded).Scan(&messageRecords); err != nil {
		t.Fatal(err)
	}
	if messageRecords != len(view.Messages) {
		t.Fatalf("message records = %d, view messages = %d", messageRecords, len(view.Messages))
	}
}

func TestAcceptedInputEnvelopeAndCommonAdmission(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	conversation := ponsruntime.Conversation{ID: "sources", AgentID: testAgent.ID, Workspace: t.TempDir(), CreatedAt: time.Now().UTC()}
	if err := store.CreateConversation(ctx, conversation); err != nil {
		t.Fatal(err)
	}
	inputs := []ponsruntime.InputSubmission{
		{IdempotencyKey: "human-1", Parts: []ponsruntime.TextPart{{Type: "text", Text: "human"}}, Source: ponsruntime.InputSource{Kind: "human", Adapter: "http", PrincipalID: "sam"}, TargetAgentID: testAgent.ID, AgentRevision: testRevision},
		{IdempotencyKey: "timer-1", Parts: []ponsruntime.TextPart{{Type: "text", Text: "scheduled"}}, Source: ponsruntime.InputSource{Kind: "system", Adapter: "scheduler"}, CausationID: "timer:daily", TargetAgentID: testAgent.ID, AgentRevision: testRevision},
		{IdempotencyKey: "child-1", Parts: []ponsruntime.TextPart{{Type: "text", Text: "task result"}}, Source: ponsruntime.InputSource{Kind: "agent", Adapter: "delegation", SourceConversationID: "child", SourceAgentID: "agent-child"}, CausationID: "task-1", TargetAgentID: testAgent.ID, AgentRevision: testRevision},
	}
	for _, input := range inputs {
		accepted, events, err := store.Accept(ctx, conversation.ID, input)
		if err != nil {
			t.Fatal(err)
		}
		if len(events) != 1 || events[0].Input == nil || !reflect.DeepEqual(events[0].Input.InputSubmission, input) {
			t.Fatalf("accepted event = %+v", events)
		}
		duplicate, duplicateEvents, err := store.Accept(ctx, conversation.ID, input)
		if err != nil || !duplicate.Duplicate || duplicate.InboundMessageID != accepted.InboundMessageID || len(duplicateEvents) != 0 {
			t.Fatalf("duplicate = %+v, events = %+v, error = %v", duplicate, duplicateEvents, err)
		}
		claim, err := store.ClaimRunnable(ctx)
		if err != nil || claim == nil || claim.Message.ID != accepted.InboundMessageID {
			t.Fatalf("claim = %+v, error = %v", claim, err)
		}
		if len(claim.Events) != 2 || claim.Events[0].Type != ponsruntime.EventInputAdmitted || claim.Events[0].Input != nil || claim.Events[0].InboundMessageID != accepted.InboundMessageID {
			t.Fatalf("admission = %+v", claim.Events)
		}
		if _, err := store.FinishRun(ctx, claim.Run, "done"); err != nil {
			t.Fatal(err)
		}
	}
	events, err := store.Events(ctx, conversation.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var acceptedInputs []ponsruntime.InputSubmission
	for _, event := range events {
		if event.Type == ponsruntime.EventInputAccepted {
			acceptedInputs = append(acceptedInputs, event.Input.InputSubmission)
		}
	}
	if !reflect.DeepEqual(acceptedInputs, inputs) {
		t.Fatalf("persisted input envelopes = %+v", acceptedInputs)
	}
}

func TestAgentEventsShareCursorButStayOutOfClientReplay(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	conversation := ponsruntime.Conversation{ID: "agent-log", AgentID: testAgent.ID, Workspace: t.TempDir(), CreatedAt: time.Now().UTC()}
	if err := store.CreateConversation(ctx, conversation); err != nil {
		t.Fatal(err)
	}
	if _, _, err := acceptInput(store, ctx, conversation.ID, "first", []ponsruntime.TextPart{{Type: "text", Text: "work"}}); err != nil {
		t.Fatal(err)
	}
	claim, err := store.ClaimRunnable(ctx)
	if err != nil || claim == nil {
		t.Fatalf("claim = %+v, err = %v", claim, err)
	}
	private := ponsruntime.Event{
		Type: ponsruntime.EventModelCompleted,
		ModelOutput: &ponsruntime.ModelOutput{
			Turn:    1,
			Content: []ponsruntime.AgentContent{{Type: "provider_item", Item: []byte(`{"type":"reasoning","encrypted_content":"secret"}`)}},
		},
	}
	invalid := private
	invalid.Iteration = &ponsruntime.IterationEvent{Turn: 1}
	if err := store.AppendAgentEvent(ctx, claim.Run, invalid); err == nil {
		t.Fatal("accepted an agent event with multiple payload types")
	}
	if err := store.AppendAgentEvent(ctx, claim.Run, private); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendAgentEvent(ctx, claim.Run, ponsruntime.Event{
		Type:          ponsruntime.EventToolExecutionStarted,
		ToolExecution: &ponsruntime.ToolExecution{Turn: 1, ToolCallID: "call-1", ToolKind: "bash"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FinishRun(ctx, claim.Run, "done"); err != nil {
		t.Fatal(err)
	}
	internal, err := store.AgentEvents(ctx, conversation.ID, 0)
	if err != nil || len(internal) != 2 || internal[0].ModelOutput == nil || internal[0].ModelOutput.Content[0].Type != "provider_item" ||
		internal[1].Type != ponsruntime.EventToolExecutionStarted || internal[1].ToolExecution == nil || internal[1].ToolExecution.ToolCallID != "call-1" {
		t.Fatalf("agent events = %+v, err = %v", internal, err)
	}
	public, err := store.Events(ctx, conversation.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range public {
		if event.ID == internal[0].ID || event.ID == internal[1].ID || event.ModelOutput != nil || event.ToolExecution != nil {
			t.Fatalf("private event leaked into client replay: %+v", event)
		}
	}
	view, err := store.View(ctx, conversation.ID)
	if err != nil || view.EventCursor != public[len(public)-1].ID {
		t.Fatalf("view cursor = %d, public tail = %+v, err = %v", view.EventCursor, public[len(public)-1], err)
	}
}

func TestClaimReplaysCompactionAndCommittedModelOutput(t *testing.T) {
	stateDir := t.TempDir()
	store, err := Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	ctx := context.Background()
	conversation := ponsruntime.Conversation{ID: "compaction-replay", AgentID: testAgent.ID, Workspace: t.TempDir(), CreatedAt: time.Now().UTC()}
	if err := store.CreateConversation(ctx, conversation); err != nil {
		t.Fatal(err)
	}
	if _, _, err := acceptInput(store, ctx, conversation.ID, "first", []ponsruntime.TextPart{{Type: "text", Text: "first goal"}}); err != nil {
		t.Fatal(err)
	}
	first, err := store.ClaimRunnable(ctx)
	if err != nil || first == nil {
		t.Fatalf("first claim = %+v, err = %v", first, err)
	}
	// This input was accepted before compaction but is still queued.
	if _, _, err := acceptInput(store, ctx, conversation.ID, "second", []ponsruntime.TextPart{{Type: "text", Text: "second goal"}}); err != nil {
		t.Fatal(err)
	}
	checkpoint := &ponsruntime.ContextCompaction{
		Turn: 1, Summary: "summary", Context: []ponsruntime.ContextTurn{
			{Role: "user", Content: []ponsruntime.AgentContent{{Type: "text", Text: "first goal"}}},
			{Role: "user", Content: []ponsruntime.AgentContent{{Type: "text", Text: "summary"}}},
		},
	}
	if err := store.AppendAgentEvent(ctx, first.Run, ponsruntime.Event{
		Type: ponsruntime.EventContextCompacted, Compaction: checkpoint,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendAgentEvent(ctx, first.Run, ponsruntime.Event{
		Type: ponsruntime.EventModelCompleted,
		ModelOutput: &ponsruntime.ModelOutput{Turn: 1, Content: []ponsruntime.AgentContent{
			{Type: "provider_item", Item: []byte(`{"type":"reasoning","encrypted_content":"opaque"}`)},
			{Type: "text", Text: "first answer"},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FinishRun(ctx, first.Run, "first answer"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.ClaimRunnable(ctx)
	if err != nil || second == nil {
		t.Fatalf("second claim = %+v, err = %v", second, err)
	}
	if len(second.Context) != 3 || second.Context[0].Content[0].Text != "first goal" ||
		second.Context[1].Content[0].Text != "summary" || second.Context[2].Content[0].Type != "provider_item" ||
		second.Context[2].Content[1].Text != "first answer" {
		t.Fatalf("compacted context = %+v", second.Context)
	}
	if err := store.AppendAgentEvent(ctx, second.Run, ponsruntime.Event{
		Type: ponsruntime.EventInputPrepared,
		PreparedInput: &ponsruntime.ContextTurn{
			Role: "user", Content: []ponsruntime.AgentContent{{Type: "text", Text: "prepared second goal"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FinishRun(ctx, second.Run, "second answer"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := acceptInput(store, ctx, conversation.ID, "third", []ponsruntime.TextPart{{Type: "text", Text: "third goal"}}); err != nil {
		t.Fatal(err)
	}
	third, err := store.ClaimRunnable(ctx)
	if err != nil || third == nil {
		t.Fatalf("third claim = %+v, err = %v", third, err)
	}
	if len(third.Context) != 5 || third.Context[3].Content[0].Text != "prepared second goal" ||
		third.Context[4].Content[0].Text != "second answer" {
		t.Fatalf("context tail after queued input admission = %+v", third.Context)
	}
	if err := store.AppendAgentEvent(ctx, third.Run, ponsruntime.Event{
		Type:        ponsruntime.EventModelCompleted,
		ModelOutput: &ponsruntime.ModelOutput{Turn: 1, Content: []ponsruntime.AgentContent{{Type: "text", Text: "uncommitted answer"}}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FailRun(ctx, third.Run, "failed before assistant commit"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := acceptInput(store, ctx, conversation.ID, "fourth", []ponsruntime.TextPart{{Type: "text", Text: "fourth goal"}}); err != nil {
		t.Fatal(err)
	}
	fourth, err := store.ClaimRunnable(ctx)
	if err != nil || fourth == nil {
		t.Fatalf("fourth claim = %+v, err = %v", fourth, err)
	}
	for _, turn := range fourth.Context {
		for _, part := range turn.Content {
			if part.Text == "uncommitted answer" {
				t.Fatal("uncommitted model response entered context")
			}
		}
	}
}

func TestClaimProjectsContextFromStartOfLog(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	conversation := ponsruntime.Conversation{ID: "full-context", AgentID: testAgent.ID, Workspace: t.TempDir(), CreatedAt: time.Now().UTC()}
	if err := store.CreateConversation(ctx, conversation); err != nil {
		t.Fatal(err)
	}
	if _, _, err := acceptInput(store, ctx, conversation.ID, "first", []ponsruntime.TextPart{{Type: "text", Text: "first"}}); err != nil {
		t.Fatal(err)
	}
	first, err := store.ClaimRunnable(ctx)
	if err != nil || first == nil {
		t.Fatalf("first claim = %+v, err = %v", first, err)
	}
	if err := store.AppendAgentEvent(ctx, first.Run, ponsruntime.Event{
		Type:          ponsruntime.EventInputPrepared,
		PreparedInput: &ponsruntime.ContextTurn{Role: "user", Content: []ponsruntime.AgentContent{{Type: "text", Text: "prepared first"}}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendAgentEvent(ctx, first.Run, ponsruntime.Event{
		Type: ponsruntime.EventModelCompleted,
		ModelOutput: &ponsruntime.ModelOutput{Turn: 1, Content: []ponsruntime.AgentContent{
			{Type: "provider_item", Item: []byte(`{"type":"reasoning","encrypted_content":"opaque"}`)},
			{Type: "text", Text: "answer"},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FinishRun(ctx, first.Run, "answer"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := acceptInput(store, ctx, conversation.ID, "second", []ponsruntime.TextPart{{Type: "text", Text: "next"}}); err != nil {
		t.Fatal(err)
	}
	second, err := store.ClaimRunnable(ctx)
	if err != nil || second == nil {
		t.Fatalf("second claim = %+v, err = %v", second, err)
	}
	if len(second.Context) != 2 || second.Context[0].Content[0].Text != "prepared first" ||
		second.Context[1].Content[0].Type != "provider_item" {
		t.Fatalf("projected context = %+v", second.Context)
	}
}

func TestProjectedContextGroupsParallelToolResults(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	conversation := ponsruntime.Conversation{ID: "tool-tail", AgentID: testAgent.ID, Workspace: t.TempDir(), CreatedAt: time.Now().UTC()}
	if err := store.CreateConversation(ctx, conversation); err != nil {
		t.Fatal(err)
	}
	if _, _, err := acceptInput(store, ctx, conversation.ID, "first", []ponsruntime.TextPart{{Type: "text", Text: "work"}}); err != nil {
		t.Fatal(err)
	}
	first, err := store.ClaimRunnable(ctx)
	if err != nil || first == nil {
		t.Fatalf("first claim = %+v, err = %v", first, err)
	}
	if err := store.AppendAgentEvent(ctx, first.Run, ponsruntime.Event{
		Type: ponsruntime.EventContextCompacted,
		Compaction: &ponsruntime.ContextCompaction{Turn: 1, Context: []ponsruntime.ContextTurn{
			{Role: "user", Content: []ponsruntime.AgentContent{{Type: "text", Text: "summary"}}},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	parts := []ponsruntime.MessagePart{
		{Type: "tool_call", ToolCallID: "a", ToolKind: "fake", Arguments: []byte(`{}`)},
		{Type: "tool_call", ToolCallID: "b", ToolKind: "fake", Arguments: []byte(`{}`)},
	}
	if _, err := store.CommitAssistantTurn(ctx, first.Run, parts); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b"} {
		if _, err := store.ToolCompleted(ctx, first.Run, protocol.ToolResult{ActionID: id, Kind: "fake", OK: true, Output: id + " result"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.FinishRun(ctx, first.Run, "done"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := acceptInput(store, ctx, conversation.ID, "second", []ponsruntime.TextPart{{Type: "text", Text: "next"}}); err != nil {
		t.Fatal(err)
	}
	second, err := store.ClaimRunnable(ctx)
	if err != nil || second == nil {
		t.Fatalf("second claim = %+v, err = %v", second, err)
	}
	if len(second.Context) != 4 || len(second.Context[2].Content) != 2 ||
		second.Context[2].Content[0].ToolCallID != "a" ||
		second.Context[2].Content[1].ToolCallID != "b" {
		t.Fatalf("tool result tail = %+v", second.Context)
	}
}

func TestIndependentRemoteWorkspacesCanBeClaimedTogether(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	seed := t.TempDir()
	for _, id := range []string{"repo-a", "repo-b"} {
		if err := store.CreateConversation(ctx, ponsruntime.Conversation{
			ID: id, AgentID: testAgent.ID, Workspace: seed, WorkspaceLock: id, CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := acceptInput(store, ctx, id, "first", []ponsruntime.TextPart{{Type: "text", Text: "work"}}); err != nil {
			t.Fatal(err)
		}
	}
	first, err := store.ClaimRunnable(ctx)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.ClaimRunnable(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first == nil || second == nil || first.Conversation.ID == second.Conversation.ID {
		t.Fatalf("independent claims = %+v, %+v", first, second)
	}
}

func TestTimeEncodingIsChronologicalAndUsesMicrosecondPrecision(t *testing.T) {
	earlier := time.Date(2026, time.September, 21, 12, 0, 5, 100_000_001, time.FixedZone("offset", 3600))
	later := earlier.Add(10 * time.Millisecond)
	if encodeTime(earlier) >= encodeTime(later) {
		t.Fatalf("encoded timestamps are not chronological: %v >= %v", encodeTime(earlier), encodeTime(later))
	}
	want := earlier.UTC().Truncate(time.Microsecond)
	if got := decodeTime(encodeTime(earlier)); !got.Equal(want) || got.Location() != time.UTC {
		t.Fatalf("round trip = %v, want %v UTC", got, want)
	}
}

func TestTimestampColumnsUseIntegerStorage(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for table, columns := range map[string][]string{
		"conversations":          {"created_at"},
		"submissions":            {"accepted_at"},
		"runs":                   {"started_at", "completed_at"},
		"tool_calls":             {"updated_at"},
		"events":                 {"created_at"},
		"workspaces":             {"created_at", "updated_at"},
		"execution_environments": {"idle_until", "expires_at", "updated_at"},
	} {
		for _, column := range columns {
			var dataType string
			if err := store.db.QueryRowContext(context.Background(),
				`SELECT type FROM pragma_table_info(?) WHERE name = ?`, table, column,
			).Scan(&dataType); err != nil {
				t.Fatalf("%s.%s: %v", table, column, err)
			}
			if dataType != "INTEGER" {
				t.Fatalf("%s.%s type = %q, want INTEGER", table, column, dataType)
			}
		}
	}
}

func TestConversationsAreReturnedNewestFirst(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ctx := context.Background()
	old := ponsruntime.Conversation{
		ID: "old", AgentID: testAgent.ID, Workspace: "/old", Environment: "seatbelt",
		CreatedAt: time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC),
	}
	newest := ponsruntime.Conversation{
		ID: "new", AgentID: testAgent.ID, Workspace: "/new", Environment: "e2b",
		CreatedAt: time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC),
	}
	if err := store.CreateConversation(ctx, old); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateConversation(ctx, newest); err != nil {
		t.Fatal(err)
	}
	conversations, err := store.Conversations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(conversations) != 2 || conversations[0].ID != newest.ID || conversations[1].ID != old.ID {
		t.Fatalf("conversations = %+v", conversations)
	}
	if conversations[0].Environment != "e2b" || conversations[1].Environment != "seatbelt" {
		t.Fatalf("conversation environments = %+v", conversations)
	}
}

func TestEnvironmentStateRoundTripAndExpiry(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	checkpointRef := "opaque-checkpoint-reference"
	workspace := environment.WorkspaceState{
		ID: "workspace", Strategy: environment.WorkspaceStrategyArchive, SourceRef: "/source",
		BaseRevision: checkpointRef, CheckpointRef: checkpointRef, SetupGeneration: 2,
		CreatedAt: now.Add(-time.Minute), UpdatedAt: now,
	}
	if err := store.SaveWorkspaceState(ctx, workspace); err != nil {
		t.Fatal(err)
	}
	gotWorkspace, err := store.WorkspaceState(ctx, workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotWorkspace.Strategy != workspace.Strategy || gotWorkspace.SourceRef != workspace.SourceRef ||
		gotWorkspace.BaseRevision != workspace.BaseRevision || gotWorkspace.CheckpointRef != workspace.CheckpointRef ||
		gotWorkspace.SetupGeneration != workspace.SetupGeneration || !gotWorkspace.CreatedAt.Equal(workspace.CreatedAt) {
		t.Fatalf("workspace = %+v", gotWorkspace)
	}
	state := environment.State{
		WorkspaceID: "workspace", Provider: "e2b", EnvironmentID: "sandbox", Template: "pons-hands",
		Network: environment.NetworkDisabled, Status: environment.StateIdle, IdleUntil: now.Add(time.Minute),
		ExpiresAt: now.Add(2 * time.Minute), UpdatedAt: now,
	}
	if err := store.SaveEnvironmentState(ctx, state); err != nil {
		t.Fatal(err)
	}
	got, err := store.EnvironmentState(ctx, state.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if got.EnvironmentID != state.EnvironmentID || got.Network != state.Network ||
		got.Status != state.Status || !got.IdleUntil.Equal(state.IdleUntil) {
		t.Fatalf("state = %+v", got)
	}
	before, err := store.ExpiredEnvironmentStates(ctx, "e2b", now, 10)
	if err != nil || len(before) != 0 {
		t.Fatalf("before expiry = %+v, %v", before, err)
	}
	after, err := store.ExpiredEnvironmentStates(ctx, "e2b", now.Add(time.Minute), 10)
	if err != nil || len(after) != 1 {
		t.Fatalf("after expiry = %+v, %v", after, err)
	}
	if err := store.DeleteEnvironmentState(ctx, state.WorkspaceID, "other"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnvironmentState(ctx, state.WorkspaceID); err != nil {
		t.Fatalf("stale delete removed state: %v", err)
	}
	invalidStatus := state
	invalidStatus.Status = "unknown"
	if err := store.SaveEnvironmentState(ctx, invalidStatus); err == nil {
		t.Fatal("invalid environment status was accepted")
	}
	invalidNetwork := state
	invalidNetwork.Network = "unknown"
	if err := store.SaveEnvironmentState(ctx, invalidNetwork); err == nil {
		t.Fatal("invalid network policy was accepted")
	}
	activeWithoutRun := state
	activeWithoutRun.Status, activeWithoutRun.IdleUntil = environment.StateActive, time.Time{}
	if err := store.SaveEnvironmentState(ctx, activeWithoutRun); err == nil {
		t.Fatal("active environment without run ID was accepted")
	}
	idleWithRun := state
	idleWithRun.RunID = "run"
	if err := store.SaveEnvironmentState(ctx, idleWithRun); err == nil {
		t.Fatal("idle environment with run ID was accepted")
	}
	idleWithoutDeadline := state
	idleWithoutDeadline.IdleUntil = time.Time{}
	if err := store.SaveEnvironmentState(ctx, idleWithoutDeadline); err == nil {
		t.Fatal("idle environment without deadline was accepted")
	}
	otherWorkspace := workspace
	otherWorkspace.ID = "other-workspace"
	if err := store.SaveWorkspaceState(ctx, otherWorkspace); err != nil {
		t.Fatal(err)
	}
	duplicate := state
	duplicate.WorkspaceID = otherWorkspace.ID
	if err := store.SaveEnvironmentState(ctx, duplicate); err == nil {
		t.Fatal("duplicate provider environment id was accepted")
	}
	if err := store.DeleteEnvironmentState(ctx, state.WorkspaceID, state.EnvironmentID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnvironmentState(ctx, state.WorkspaceID); !errors.Is(err, environment.ErrStateNotFound) {
		t.Fatalf("error = %v", err)
	}
	if _, err := store.WorkspaceState(ctx, workspace.ID); err != nil {
		t.Fatalf("environment deletion removed logical workspace: %v", err)
	}
}

func TestRecoveryReservationSurvivesRestartAndExpiresAtDeadline(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := store.SaveWorkspaceState(ctx, environment.WorkspaceState{
		ID: "workspace", Strategy: environment.WorkspaceStrategyArchive, SetupGeneration: 1,
		CheckpointRef: "previous", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	state := environment.State{
		WorkspaceID: "workspace", Provider: "e2b", EnvironmentID: "recover-me", Template: "template",
		Network: environment.NetworkDisabled, Status: environment.StateRecovery,
		ExpiresAt: now.Add(time.Hour), UpdatedAt: now,
	}
	if err := store.SaveEnvironmentState(ctx, state); err != nil {
		t.Fatal(err)
	}
	invalid := state
	invalid.RunID = "run"
	if err := store.SaveEnvironmentState(ctx, invalid); err == nil {
		t.Fatal("recovery reservation accepted a run")
	}
	invalid = state
	invalid.IdleUntil = now
	if err := store.SaveEnvironmentState(ctx, invalid); err == nil {
		t.Fatal("recovery reservation accepted an idle deadline")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	got, err := store.EnvironmentState(ctx, "workspace")
	if err != nil || got.Status != environment.StateRecovery || !got.ExpiresAt.Equal(state.ExpiresAt) {
		t.Fatalf("reservation = %+v, %v", got, err)
	}
	before, err := store.ExpiredEnvironmentStates(ctx, "e2b", state.ExpiresAt.Add(-time.Microsecond), 10)
	if err != nil || len(before) != 0 {
		t.Fatalf("before expiry = %+v, %v", before, err)
	}
	after, err := store.ExpiredEnvironmentStates(ctx, "e2b", state.ExpiresAt, 10)
	if err != nil || len(after) != 1 || after[0].EnvironmentID != "recover-me" {
		t.Fatalf("at expiry = %+v, %v", after, err)
	}
}

func TestExpiredActiveEnvironmentHasNoIdleDeadline(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC().Truncate(time.Microsecond)
	workspace := environment.WorkspaceState{
		ID: "workspace", Strategy: environment.WorkspaceStrategyArchive,
		SetupGeneration: 1, CreatedAt: now.Add(-time.Minute), UpdatedAt: now,
	}
	if err := store.SaveWorkspaceState(context.Background(), workspace); err != nil {
		t.Fatal(err)
	}
	state := environment.State{
		WorkspaceID: "workspace", Provider: "e2b", EnvironmentID: "sandbox", Template: "pons-hands",
		Network: environment.NetworkDisabled, Status: environment.StateActive, RunID: "run",
		ExpiresAt: now.Add(-time.Second), UpdatedAt: now.Add(-time.Minute),
	}
	if err := store.SaveEnvironmentState(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	expired, err := store.ExpiredEnvironmentStates(context.Background(), "e2b", now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(expired) != 1 || expired[0].RunID != state.RunID || !expired[0].IdleUntil.IsZero() {
		t.Fatalf("expired states = %+v", expired)
	}
}

func TestToolCallIDsAreScopedToTheirRun(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	conversation := ponsruntime.Conversation{
		ID: "conversation", AgentID: testAgent.ID, Workspace: t.TempDir(), CreatedAt: time.Now().UTC(),
	}
	if err := store.CreateConversation(ctx, conversation); err != nil {
		t.Fatal(err)
	}
	var runIDs []string
	for _, key := range []string{"first", "second"} {
		if _, _, err := acceptInput(store, ctx, conversation.ID, key, []ponsruntime.TextPart{{Type: "text", Text: key}}); err != nil {
			t.Fatal(err)
		}
		claim, err := store.ClaimRunnable(ctx)
		if err != nil {
			t.Fatal(err)
		}
		runIDs = append(runIDs, claim.Run.ID)
		if _, err := store.CommitAssistantTurn(ctx, claim.Run, []ponsruntime.MessagePart{{
			Type: "tool_call", ToolCallID: "call_1", ToolKind: "fake", Arguments: protocol.MustArgsJSON(map[string]any{}),
		}}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.ToolCompleted(ctx, claim.Run, protocol.ToolResult{ActionID: "call_1", Kind: "fake", OK: true}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.FinishRun(ctx, claim.Run, "done"); err != nil {
			t.Fatal(err)
		}
	}
	view, err := store.View(ctx, conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.ToolCalls) != 2 {
		t.Fatalf("tool calls = %d, want 2", len(view.ToolCalls))
	}
	for i, tool := range view.ToolCalls {
		if tool.ID != "call_1" || tool.RunID != runIDs[i] {
			t.Fatalf("tool call %d = %+v, want id call_1 in run %s", i, tool, runIDs[i])
		}
	}
	events, err := store.Events(ctx, conversation.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	projected, err := ponsruntime.ProjectView(view.Conversation, events)
	if err != nil {
		t.Fatal(err)
	}
	projected.EventCursor = view.EventCursor
	if !reflect.DeepEqual(projected, view) {
		t.Fatalf("tool replay view = %+v, snapshot = %+v", projected, view)
	}
}

func TestOpenRebuildsIndexesAndRecoversUncertainTool(t *testing.T) {
	stateDir := t.TempDir()
	store, err := Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	conversation := ponsruntime.Conversation{ID: "rebuild", AgentID: testAgent.ID, Workspace: t.TempDir(), CreatedAt: time.Now().UTC()}
	if err := store.CreateConversation(ctx, conversation); err != nil {
		t.Fatal(err)
	}
	first, _, err := acceptInput(store, ctx, conversation.ID, "first", []ponsruntime.TextPart{{Type: "text", Text: "first"}})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := store.ClaimRunnable(ctx)
	if err != nil || claim == nil {
		t.Fatalf("claim = %+v, error = %v", claim, err)
	}
	if _, err := store.CommitAssistantTurn(ctx, claim.Run, []ponsruntime.MessagePart{
		{Type: "tool_call", ToolCallID: "finished", ToolKind: "fake", Arguments: protocol.MustArgsJSON(map[string]any{})},
		{Type: "tool_call", ToolCallID: "uncertain", ToolKind: "fake", Arguments: protocol.MustArgsJSON(map[string]any{})},
		{Type: "tool_call", ToolCallID: "denied", ToolKind: "fake", Arguments: protocol.MustArgsJSON(map[string]any{})},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ToolCompleted(ctx, claim.Run, protocol.ToolResult{ActionID: "finished", Kind: "fake", OK: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ToolDenied(ctx, claim.Run, protocol.ToolResult{ActionID: "denied", Kind: "fake", Error: "not allowed"}); err != nil {
		t.Fatal(err)
	}
	second, _, err := acceptInput(store, ctx, conversation.ID, "second", []ponsruntime.TextPart{{Type: "text", Text: "second"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"tool_calls", "runs", "submissions"} {
		if _, err := store.db.ExecContext(ctx, "DELETE FROM "+table); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE conversations SET next_event_cursor = 1 WHERE id = ?`, conversation.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	assertStoredStatus(t, store, "submissions", first.InboundMessageID, ponsruntime.RunRunning)
	assertStoredStatus(t, store, "submissions", second.InboundMessageID, ponsruntime.RunQueued)
	assertStoredStatus(t, store, "runs", claim.Run.ID, ponsruntime.RunRunning)
	assertToolStatus(t, store, claim.Run.ID, "finished", ponsruntime.ToolCompleted)
	assertToolStatus(t, store, claim.Run.ID, "denied", ponsruntime.ToolDenied)
	assertToolStatus(t, store, claim.Run.ID, "uncertain", ponsruntime.ToolRequested)
	duplicate, duplicateEvents, err := acceptInput(store, ctx, conversation.ID, "first", []ponsruntime.TextPart{{Type: "text", Text: "first"}})
	if err != nil || !duplicate.Duplicate || duplicate.InboundMessageID != first.InboundMessageID || len(duplicateEvents) != 0 {
		t.Fatalf("duplicate after rebuild = %+v, events = %+v, error = %v", duplicate, duplicateEvents, err)
	}
	if err := store.RecoverRunning(ctx); err != nil {
		t.Fatal(err)
	}
	assertStoredStatus(t, store, "runs", claim.Run.ID, ponsruntime.RunFailed)
	assertToolStatus(t, store, claim.Run.ID, "finished", ponsruntime.ToolCompleted)
	assertToolStatus(t, store, claim.Run.ID, "uncertain", ponsruntime.ToolInterrupted)
	events, err := store.Events(ctx, conversation.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var finished, interrupted int
	for _, event := range events {
		if event.Type != ponsruntime.EventToolOutcomeRecorded {
			continue
		}
		switch event.ToolOutcome.ToolCallID {
		case "finished":
			finished++
		case "uncertain":
			interrupted++
		}
	}
	if finished != 1 || interrupted != 1 {
		t.Fatalf("tool outcomes after recovery: finished=%d uncertain=%d", finished, interrupted)
	}
	next, err := store.ClaimRunnable(ctx)
	if err != nil || next == nil || next.Message.ID != second.InboundMessageID {
		t.Fatalf("next claim = %+v, error = %v", next, err)
	}
}

func TestIndexRebuildRollsBackWhenLogIsInvalid(t *testing.T) {
	stateDir := t.TempDir()
	store, err := Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	conversation := ponsruntime.Conversation{ID: "invalid-log", AgentID: testAgent.ID, Workspace: t.TempDir(), CreatedAt: time.Now().UTC()}
	if err := store.CreateConversation(ctx, conversation); err != nil {
		t.Fatal(err)
	}
	accepted, events, err := acceptInput(store, ctx, conversation.ID, "key", []ponsruntime.TextPart{{Type: "text", Text: "hello"}})
	if err != nil {
		t.Fatal(err)
	}
	bad := events[0]
	bad.ConversationID = "wrong-conversation"
	payload, err := json.Marshal(bad)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE events SET payload = ? WHERE conversation_id = ? AND cursor = ?`,
		payload, conversation.ID, events[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := store.rebuildIndexes(ctx); err == nil {
		t.Fatal("rebuilt indexes from an invalid event")
	}
	assertStoredStatus(t, store, "submissions", accepted.InboundMessageID, ponsruntime.RunQueued)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if reopened, err := Open(stateDir); err == nil {
		_ = reopened.Close()
		t.Fatal("opened a store with an invalid event log")
	}
}

func TestFailRunAtomicallyInterruptsRequestedTools(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	conversation := ponsruntime.Conversation{
		ID: "conversation", AgentID: testAgent.ID, Workspace: t.TempDir(), CreatedAt: time.Now().UTC(),
	}
	if err := store.CreateConversation(ctx, conversation); err != nil {
		t.Fatal(err)
	}
	if _, _, err := acceptInput(store, ctx, conversation.ID, "key", []ponsruntime.TextPart{{Type: "text", Text: "go"}}); err != nil {
		t.Fatal(err)
	}
	claim, err := store.ClaimRunnable(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CommitAssistantTurn(ctx, claim.Run, []ponsruntime.MessagePart{{
		Type: "tool_call", ToolCallID: "call_1", ToolKind: "fake", Arguments: protocol.MustArgsJSON(map[string]any{}),
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `
CREATE TRIGGER reject_tool_interruption
BEFORE UPDATE OF status ON tool_calls
WHEN OLD.status = 'requested' AND NEW.status = 'interrupted'
BEGIN
  SELECT RAISE(ABORT, 'reject tool interruption');
END`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FailRun(ctx, claim.Run, "runner failed"); err == nil {
		t.Fatal("FailRun succeeded despite rejected tool interruption")
	}
	assertStoredStatus(t, store, "runs", claim.Run.ID, ponsruntime.RunRunning)
	assertStoredStatus(t, store, "submissions", claim.Run.InboundMessageID, ponsruntime.RunRunning)
	assertToolStatus(t, store, claim.Run.ID, "call_1", ponsruntime.ToolRequested)
	var toolMessages int
	if err := store.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM events WHERE conversation_id = ? AND type = ?`, conversation.ID, ponsruntime.EventToolOutcomeRecorded,
	).Scan(&toolMessages); err != nil {
		t.Fatal(err)
	}
	if toolMessages != 0 {
		t.Fatalf("tool messages after rollback = %d, want 0", toolMessages)
	}
	if _, err := store.db.ExecContext(ctx, `DROP TRIGGER reject_tool_interruption`); err != nil {
		t.Fatal(err)
	}
	events, err := store.FailRun(ctx, claim.Run, "runner failed")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Type != ponsruntime.EventToolOutcomeRecorded ||
		events[0].ToolOutcome.Status != ponsruntime.ToolInterrupted || events[1].Type != ponsruntime.EventRunFailed {
		t.Fatalf("failure events = %+v, want interrupted tool outcome and failed run", events)
	}
	assertStoredStatus(t, store, "runs", claim.Run.ID, ponsruntime.RunFailed)
	assertStoredStatus(t, store, "submissions", claim.Run.InboundMessageID, ponsruntime.RunFailed)
	assertToolStatus(t, store, claim.Run.ID, "call_1", ponsruntime.ToolInterrupted)
}

func assertStoredStatus(t *testing.T, store *Store, table, id, want string) {
	t.Helper()
	var got string
	query := `SELECT status FROM ` + table + ` WHERE id = ?`
	if err := store.db.QueryRowContext(context.Background(), query, id).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("%s %s status = %q, want %q", table, id, got, want)
	}
}

func assertToolStatus(t *testing.T, store *Store, runID, id, want string) {
	t.Helper()
	var got string
	if err := store.db.QueryRowContext(context.Background(),
		`SELECT status FROM tool_calls WHERE run_id = ? AND id = ?`, runID, id,
	).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("tool %s/%s status = %q, want %q", runID, id, got, want)
	}
}

func TestAgentRevisionIsRecordedOnSubmissionAndRun(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	conversation := ponsruntime.Conversation{
		ID: "conversation", AgentID: testAgent.ID, Workspace: t.TempDir(), CreatedAt: time.Now().UTC(),
	}
	if err := store.CreateConversation(ctx, conversation); err != nil {
		t.Fatal(err)
	}
	parts := []ponsruntime.TextPart{{Type: "text", Text: "go"}}
	if _, _, err := store.Accept(ctx, conversation.ID, ponsruntime.InputSubmission{
		IdempotencyKey: "empty", Parts: parts,
		Source: ponsruntime.InputSource{Kind: "human", Adapter: "test"}, TargetAgentID: testAgent.ID,
	}); err == nil {
		t.Fatal("accepted a submission without an agent revision")
	}
	if _, _, err := acceptInput(store, ctx, conversation.ID, "key", parts); err != nil {
		t.Fatal(err)
	}
	claim, err := store.ClaimRunnable(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if claim.Run.AgentRevision != testRevision || claim.Conversation.AgentID != testAgent.ID {
		t.Fatalf("claim run = %+v, conversation = %+v", claim.Run, claim.Conversation)
	}
	if _, err := store.FinishRun(ctx, claim.Run, "done"); err != nil {
		t.Fatal(err)
	}
	view, err := store.View(ctx, conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Conversation.AgentID != testAgent.ID || len(view.Submissions) != 1 || view.Submissions[0].AgentRevision != testRevision {
		t.Fatalf("view = %+v", view)
	}
}
