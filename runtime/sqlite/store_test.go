package sqlite

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/samperrin/pons/environment"
	"github.com/samperrin/pons/protocol"
	ponsruntime "github.com/samperrin/pons/runtime"
)

func TestOpenRejectsOlderExistingDatabase(t *testing.T) {
	for _, statement := range []string{"PRAGMA user_version = 0", "PRAGMA user_version = 1", "PRAGMA user_version = 2"} {
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
			ID: selection.id, Workspace: t.TempDir(), GitRepository: selection.repository,
			GitRevision: strings.Repeat("a", 40), GitAllRepositories: selection.all,
			CreatedAt: time.Now().UTC(),
		}
		if err := store.CreateConversation(ctx, conversation); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.Accept(ctx, selection.id, "first", []ponsruntime.TextPart{{Type: "text", Text: "work"}}); err != nil {
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
		if _, _, err := store.FinishRun(ctx, claim.Run, "done"); err != nil {
			t.Fatal(err)
		}
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
			ID: id, Workspace: seed, WorkspaceLock: id, CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.Accept(ctx, id, "first", []ponsruntime.TextPart{{Type: "text", Text: "work"}}); err != nil {
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
		"messages":               {"created_at"},
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
		ID: "old", Workspace: "/old", Environment: "none",
		CreatedAt: time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC),
	}
	newest := ponsruntime.Conversation{
		ID: "new", Workspace: "/new", Environment: "e2b",
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
	if conversations[0].Environment != "e2b" || conversations[1].Environment != "none" {
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
