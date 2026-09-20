package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/samperrin/pons/protocol"
	ponsruntime "github.com/samperrin/pons/runtime"
)

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

func TestToolCallIDsAreScopedToTheirRun(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	conversation := ponsruntime.Conversation{
		ID: "conversation", Workspace: t.TempDir(), CreatedAt: time.Now().UTC(),
	}
	if err := store.CreateConversation(ctx, conversation); err != nil {
		t.Fatal(err)
	}
	var runIDs []string
	for _, key := range []string{"first", "second"} {
		if _, _, err := store.Accept(ctx, conversation.ID, key, []ponsruntime.TextPart{{Type: "text", Text: key}}); err != nil {
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
		if _, _, err := store.FinishRun(ctx, claim.Run, "done"); err != nil {
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
}

func TestFailRunAtomicallyInterruptsRequestedTools(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	conversation := ponsruntime.Conversation{
		ID: "conversation", Workspace: t.TempDir(), CreatedAt: time.Now().UTC(),
	}
	if err := store.CreateConversation(ctx, conversation); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Accept(ctx, conversation.ID, "key", []ponsruntime.TextPart{{Type: "text", Text: "go"}}); err != nil {
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
		`SELECT COUNT(*) FROM messages WHERE run_id = ? AND role = 'tool'`, claim.Run.ID,
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
	if len(events) != 4 {
		t.Fatalf("failure events = %d, want 4", len(events))
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
