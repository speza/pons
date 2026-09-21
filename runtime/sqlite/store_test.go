package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/samperrin/pons/environment"
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
		"messages":               {"created_at"},
		"submissions":            {"accepted_at"},
		"runs":                   {"started_at", "completed_at"},
		"tool_calls":             {"updated_at"},
		"events":                 {"created_at"},
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

func TestEnvironmentStateRoundTripAndExpiry(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	state := environment.State{
		Key: "/workspace", Provider: "e2b", EnvironmentID: "sandbox", Template: "pons-hands",
		Network:            environment.NetworkDisabled,
		WorkspaceStrategy:  environment.WorkspaceStrategyLocalArchive,
		WorkspaceSourceRef: "source", WorkspaceRevision: "base", CheckpointRevision: "checkpoint",
		SetupGeneration: 2, Status: environment.StateIdle, IdleUntil: now.Add(time.Minute),
		ExpiresAt: now.Add(2 * time.Minute), UpdatedAt: now,
	}
	if err := store.SaveEnvironmentState(ctx, state); err != nil {
		t.Fatal(err)
	}
	got, err := store.EnvironmentState(ctx, state.Key)
	if err != nil {
		t.Fatal(err)
	}
	if got.EnvironmentID != state.EnvironmentID || got.Network != state.Network ||
		got.WorkspaceStrategy != state.WorkspaceStrategy || got.WorkspaceSourceRef != state.WorkspaceSourceRef ||
		got.WorkspaceRevision != state.WorkspaceRevision || got.CheckpointRevision != state.CheckpointRevision ||
		got.SetupGeneration != state.SetupGeneration || !got.IdleUntil.Equal(state.IdleUntil) {
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
	if err := store.DeleteEnvironmentState(ctx, state.Key, "other"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnvironmentState(ctx, state.Key); err != nil {
		t.Fatalf("stale delete removed state: %v", err)
	}
	invalidStatus := state
	invalidStatus.Key, invalidStatus.EnvironmentID, invalidStatus.Status = "/invalid-status", "invalid-status", "unknown"
	if err := store.SaveEnvironmentState(ctx, invalidStatus); err == nil {
		t.Fatal("invalid environment status was accepted")
	}
	invalidNetwork := state
	invalidNetwork.Key, invalidNetwork.EnvironmentID, invalidNetwork.Network = "/invalid-network", "invalid-network", "unknown"
	if err := store.SaveEnvironmentState(ctx, invalidNetwork); err == nil {
		t.Fatal("invalid network policy was accepted")
	}
	duplicate := state
	duplicate.Key = "/other-workspace"
	if err := store.SaveEnvironmentState(ctx, duplicate); err == nil {
		t.Fatal("duplicate provider environment id was accepted")
	}
	if err := store.DeleteEnvironmentState(ctx, state.Key, state.EnvironmentID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnvironmentState(ctx, state.Key); !errors.Is(err, environment.ErrStateNotFound) {
		t.Fatalf("error = %v", err)
	}
}

func TestExpiredActiveEnvironmentHasNoIdleDeadline(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC().Truncate(time.Microsecond)
	state := environment.State{
		Key: "/workspace", Provider: "e2b", EnvironmentID: "sandbox", Template: "pons-hands",
		Network: environment.NetworkDisabled, WorkspaceStrategy: environment.WorkspaceStrategyLocalArchive,
		SetupGeneration: 1, Status: environment.StateActive, LeaseID: "run",
		ExpiresAt: now.Add(-time.Second), UpdatedAt: now.Add(-time.Minute),
	}
	if err := store.SaveEnvironmentState(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	expired, err := store.ExpiredEnvironmentStates(context.Background(), "e2b", now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(expired) != 1 || !expired[0].IdleUntil.IsZero() {
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
