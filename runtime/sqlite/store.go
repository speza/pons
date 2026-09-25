// Package sqlite implements runtime.Store with a local SQLite database.
package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ncruces/go-sqlite3/driver"
	"github.com/samperrin/pons/environment"
	"github.com/samperrin/pons/protocol"
	ponsruntime "github.com/samperrin/pons/runtime"
)

const (
	runtimeDBName        = "runtime.db"
	currentSchemaVersion = 10
)

// Store is the local, exclusive-manager runtime backend. Its transactions
// prevent duplicate claims and concurrent conversation/workspace ownership,
// but they are not renewable leases or a distributed fencing mechanism.
type Store struct {
	db       *sql.DB
	lockFile *os.File
}

var (
	_ ponsruntime.Store      = (*Store)(nil)
	_ environment.StateStore = (*Store)(nil)
)

func newID() string { return ponsruntime.NewID() }

func cloneRaw[T ~[]byte](value T) T { return append(T(nil), value...) }

func Open(stateDir string) (*Store, error) {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("runtime: create state directory: %w", err)
	}
	if err := os.Chmod(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("runtime: secure state directory: %w", err)
	}
	lockFile, err := os.OpenFile(filepath.Join(stateDir, ".pons.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("runtime: open state directory lock: %w", err)
	}
	if err := lockStateFile(lockFile); err != nil {
		_ = lockFile.Close()
		return nil, fmt.Errorf("runtime: state directory %q is already in use or cannot be locked: %w", stateDir, err)
	}
	path := filepath.Join(stateDir, runtimeDBName)
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: "_txlock=immediate&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=synchronous(FULL)"}).String()
	db, err := driver.Open(dsn)
	if err != nil {
		_ = lockFile.Close()
		return nil, fmt.Errorf("runtime: open database: %w", err)
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	store := &Store{db: db, lockFile: lockFile}
	if err := store.initializeSchema(context.Background()); err != nil {
		_ = store.Close()
		return nil, err
	}
	if err := store.rebuildIndexes(context.Background()); err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("runtime: rebuild event indexes: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("runtime: secure database: %w", err)
	}
	return store, nil
}

func (s *Store) initializeSchema(ctx context.Context) error {
	var version, tableCount int
	if err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("runtime: read schema version: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM sqlite_schema
WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`).Scan(&tableCount); err != nil {
		return fmt.Errorf("runtime: inspect schema: %w", err)
	}
	if version != currentSchemaVersion && (version != 0 || tableCount != 0) {
		return fmt.Errorf(
			"runtime: database schema version %d is unsupported; recreate the runtime state directory",
			version,
		)
	}
	const schema = `
CREATE TABLE IF NOT EXISTS conversations (
  id TEXT PRIMARY KEY,
  agent_id TEXT NOT NULL CHECK (agent_id <> ''),
  workspace TEXT NOT NULL,
  workspace_lock TEXT NOT NULL,
  git_repository TEXT NOT NULL DEFAULT '',
  git_revision TEXT NOT NULL DEFAULT '',
  git_all_repositories BOOLEAN NOT NULL DEFAULT FALSE,
  environment TEXT NOT NULL DEFAULT '',

  created_at INTEGER NOT NULL,
  next_event_cursor INTEGER NOT NULL DEFAULT 1
);
CREATE TABLE IF NOT EXISTS submissions (
  id TEXT PRIMARY KEY,
  conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
  idempotency_key TEXT NOT NULL,
  message_id TEXT NOT NULL,
  agent_revision TEXT NOT NULL CHECK (agent_revision <> ''),
  status TEXT NOT NULL,
  error TEXT NOT NULL DEFAULT '',
  accepted_at INTEGER NOT NULL,
  UNIQUE(conversation_id, idempotency_key)
);
CREATE INDEX IF NOT EXISTS submissions_pending
  ON submissions(conversation_id, status, accepted_at, id);
CREATE INDEX IF NOT EXISTS submissions_runnable
  ON submissions(status, accepted_at, id, conversation_id);
CREATE UNIQUE INDEX IF NOT EXISTS submissions_one_running_per_conversation
  ON submissions(conversation_id) WHERE status = 'running';
CREATE TABLE IF NOT EXISTS runs (
  id TEXT PRIMARY KEY,
  conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
  submission_id TEXT NOT NULL REFERENCES submissions(id),
  inbound_message_id TEXT NOT NULL,
  agent_revision TEXT NOT NULL CHECK (agent_revision <> ''),
  status TEXT NOT NULL,
  error TEXT NOT NULL DEFAULT '',
  started_at INTEGER NOT NULL,
  completed_at INTEGER
);
CREATE INDEX IF NOT EXISTS runs_conversation_status
  ON runs(conversation_id, status, started_at);
CREATE TABLE IF NOT EXISTS tool_calls (
  id TEXT NOT NULL,
  conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
  run_id TEXT NOT NULL REFERENCES runs(id),
  inbound_message_id TEXT NOT NULL,
  kind TEXT NOT NULL,
  status TEXT NOT NULL,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY(run_id, id)
);
CREATE INDEX IF NOT EXISTS tool_calls_conversation
  ON tool_calls(conversation_id, run_id, updated_at);
CREATE TABLE IF NOT EXISTS events (
  conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
  cursor INTEGER NOT NULL,
  type TEXT NOT NULL,
  message_id TEXT NOT NULL DEFAULT '',
  payload BLOB NOT NULL,
  created_at INTEGER NOT NULL,
  PRIMARY KEY(conversation_id, cursor)
);
CREATE UNIQUE INDEX IF NOT EXISTS events_message_id
  ON events(conversation_id, message_id) WHERE message_id <> '';
CREATE TABLE IF NOT EXISTS workspaces (
  id TEXT PRIMARY KEY CHECK (id <> ''),
  strategy TEXT NOT NULL CHECK (strategy <> ''),
  source_ref TEXT NOT NULL,
  base_revision TEXT NOT NULL,
  checkpoint_ref TEXT NOT NULL,
  setup_generation INTEGER NOT NULL CHECK (setup_generation > 0),
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS execution_environments (
  workspace_id TEXT PRIMARY KEY REFERENCES workspaces(id),
  provider TEXT NOT NULL,
  environment_id TEXT NOT NULL,
  template TEXT NOT NULL,
  network_policy TEXT NOT NULL DEFAULT 'disabled'
    CHECK (network_policy IN ('disabled', 'enabled')),
  status TEXT NOT NULL CHECK (status IN ('active', 'idle', 'recovery')),
  run_id TEXT NOT NULL DEFAULT '',
  idle_until INTEGER,
  expires_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  CHECK (
    (status = 'active' AND run_id <> '' AND idle_until IS NULL)
    OR
    (status = 'idle' AND run_id = '' AND idle_until IS NOT NULL)
    OR
    (status = 'recovery' AND run_id = '' AND idle_until IS NULL)
  )
);
CREATE INDEX IF NOT EXISTS execution_environments_expiry
  ON execution_environments(provider, status, idle_until);
CREATE INDEX IF NOT EXISTS execution_environments_provider_expiry
  ON execution_environments(provider, expires_at);
CREATE UNIQUE INDEX IF NOT EXISTS execution_environments_provider_id
  ON execution_environments(provider, environment_id);
`
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("runtime: begin schema initialization: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("runtime: initialize database: %w", err)
	}
	versionStatement := fmt.Sprintf("PRAGMA user_version = %d", currentSchemaVersion)
	if _, err := tx.ExecContext(ctx, versionStatement); err != nil {
		return fmt.Errorf("runtime: record schema version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("runtime: commit schema initialization: %w", err)
	}
	return nil
}

func (s *Store) Close() error { return errors.Join(s.db.Close(), s.lockFile.Close()) }

type rowScanner interface {
	Scan(...any) error
}

func scanWorkspaceState(row rowScanner) (environment.WorkspaceState, error) {
	var state environment.WorkspaceState
	var createdAt, updatedAt int64
	if err := row.Scan(
		&state.ID,
		&state.Strategy,
		&state.SourceRef,
		&state.BaseRevision,
		&state.CheckpointRef,
		&state.SetupGeneration,
		&createdAt,
		&updatedAt,
	); err != nil {
		return environment.WorkspaceState{}, err
	}
	state.CreatedAt = decodeTime(createdAt)
	state.UpdatedAt = decodeTime(updatedAt)
	return state, nil
}

func (s *Store) WorkspaceState(ctx context.Context, id string) (environment.WorkspaceState, error) {
	state, err := scanWorkspaceState(s.db.QueryRowContext(ctx, `
SELECT id, strategy, source_ref, base_revision, checkpoint_ref,
       setup_generation, created_at, updated_at
FROM workspaces WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return environment.WorkspaceState{}, environment.ErrStateNotFound
	}
	if err != nil {
		return environment.WorkspaceState{}, fmt.Errorf("runtime: read workspace state: %w", err)
	}
	return state, nil
}

func (s *Store) SaveWorkspaceState(ctx context.Context, state environment.WorkspaceState) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO workspaces (
  id, strategy, source_ref, base_revision, checkpoint_ref,
  setup_generation, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
  strategy = excluded.strategy,
  source_ref = excluded.source_ref,
  base_revision = excluded.base_revision,
  checkpoint_ref = excluded.checkpoint_ref,
  setup_generation = excluded.setup_generation,
  updated_at = excluded.updated_at`,
		state.ID,
		state.Strategy,
		state.SourceRef,
		state.BaseRevision,
		state.CheckpointRef,
		state.SetupGeneration,
		encodeTime(state.CreatedAt),
		encodeTime(state.UpdatedAt),
	)
	if err != nil {
		return fmt.Errorf("runtime: save workspace state: %w", err)
	}
	return nil
}

func scanEnvironmentState(row rowScanner) (environment.State, error) {
	var state environment.State
	var idleUntil sql.NullInt64
	var expiresAt, updatedAt int64
	if err := row.Scan(
		&state.WorkspaceID,
		&state.Provider,
		&state.EnvironmentID,
		&state.Template,
		&state.Network,
		&state.Status,
		&state.RunID,
		&idleUntil,
		&expiresAt,
		&updatedAt,
	); err != nil {
		return environment.State{}, err
	}
	if idleUntil.Valid {
		state.IdleUntil = decodeTime(idleUntil.Int64)
	}
	state.ExpiresAt = decodeTime(expiresAt)
	state.UpdatedAt = decodeTime(updatedAt)
	return state, nil
}

func (s *Store) EnvironmentState(ctx context.Context, workspaceID string) (environment.State, error) {
	state, err := scanEnvironmentState(s.db.QueryRowContext(ctx, `
SELECT workspace_id, provider, environment_id, template, network_policy,
       status, run_id, idle_until, expires_at, updated_at
FROM execution_environments WHERE workspace_id = ?`, workspaceID))
	if errors.Is(err, sql.ErrNoRows) {
		return environment.State{}, environment.ErrStateNotFound
	}
	if err != nil {
		return environment.State{}, fmt.Errorf("runtime: read environment state: %w", err)
	}
	return state, nil
}

func (s *Store) SaveEnvironmentState(ctx context.Context, state environment.State) error {
	var idleUntil any
	if !state.IdleUntil.IsZero() {
		idleUntil = encodeTime(state.IdleUntil)
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO execution_environments (
  workspace_id, provider, environment_id, template, network_policy,
  status, run_id, idle_until, expires_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(workspace_id) DO UPDATE SET
  provider = excluded.provider,
  environment_id = excluded.environment_id,
  template = excluded.template,
  network_policy = excluded.network_policy,
  status = excluded.status,
  run_id = excluded.run_id,
  idle_until = excluded.idle_until,
  expires_at = excluded.expires_at,
  updated_at = excluded.updated_at`,
		state.WorkspaceID,
		state.Provider,
		state.EnvironmentID,
		state.Template,
		state.Network,
		state.Status,
		state.RunID,
		idleUntil,
		encodeTime(state.ExpiresAt),
		encodeTime(state.UpdatedAt),
	)
	if err != nil {
		return fmt.Errorf("runtime: save environment state: %w", err)
	}
	return nil
}

func (s *Store) DeleteEnvironmentState(ctx context.Context, workspaceID, environmentID string) error {
	_, err := s.db.ExecContext(ctx, `
DELETE FROM execution_environments
WHERE workspace_id = ? AND environment_id = ?`, workspaceID, environmentID)
	if err != nil {
		return fmt.Errorf("runtime: delete environment state: %w", err)
	}
	return nil
}

func (s *Store) ExpiredEnvironmentStates(
	ctx context.Context,
	provider string,
	now time.Time,
	limit int,
) ([]environment.State, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT workspace_id, provider, environment_id, template, network_policy,
       status, run_id, idle_until, expires_at, updated_at
FROM execution_environments
WHERE provider = ?
  AND ((status = ? AND idle_until <= ?) OR expires_at <= ?)
ORDER BY COALESCE(idle_until, expires_at), workspace_id
LIMIT ?`, provider, environment.StateIdle, encodeTime(now), encodeTime(now), limit)
	if err != nil {
		return nil, fmt.Errorf("runtime: list expired environment states: %w", err)
	}
	defer rows.Close()
	var states []environment.State
	for rows.Next() {
		state, err := scanEnvironmentState(rows)
		if err != nil {
			return nil, err
		}
		states = append(states, state)
	}
	return states, rows.Err()
}

func canonicalTime(value time.Time) time.Time {
	return value.UTC().Truncate(time.Microsecond)
}

func encodeTime(value time.Time) int64 { return canonicalTime(value).UnixMicro() }

func decodeTime(value int64) time.Time { return time.UnixMicro(value).UTC() }

func (s *Store) CreateConversation(ctx context.Context, conversation ponsruntime.Conversation) error {
	workspaceLock := conversation.WorkspaceLock
	if workspaceLock == "" {
		workspaceLock = conversation.Workspace
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO conversations(id, agent_id, workspace, workspace_lock, git_repository, git_revision, git_all_repositories, environment, created_at) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		conversation.ID, conversation.AgentID, conversation.Workspace, workspaceLock, conversation.GitRepository, conversation.GitRevision,
		conversation.GitAllRepositories, conversation.Environment, encodeTime(conversation.CreatedAt))

	if err != nil {
		return fmt.Errorf("runtime: persist conversation: %w", err)
	}
	return nil
}

func (s *Store) Conversation(ctx context.Context, id string) (ponsruntime.Conversation, error) {
	var conversation ponsruntime.Conversation
	var created int64
	err := s.db.QueryRowContext(ctx,
		`SELECT id, agent_id, workspace, workspace_lock, git_repository, git_revision, git_all_repositories, environment, created_at FROM conversations WHERE id = ?`, id,
	).Scan(&conversation.ID, &conversation.AgentID, &conversation.Workspace, &conversation.WorkspaceLock, &conversation.GitRepository, &conversation.GitRevision,
		&conversation.GitAllRepositories, &conversation.Environment, &created)

	if errors.Is(err, sql.ErrNoRows) {
		return ponsruntime.Conversation{}, ponsruntime.ErrNotFound
	}
	if err != nil {
		return ponsruntime.Conversation{}, fmt.Errorf("runtime: read conversation: %w", err)
	}
	conversation.CreatedAt = decodeTime(created)
	return conversation, nil
}

func (s *Store) Conversations(ctx context.Context) ([]ponsruntime.Conversation, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, agent_id, workspace, workspace_lock, git_repository, git_revision, git_all_repositories, environment, created_at
FROM conversations
ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("runtime: list conversations: %w", err)
	}
	defer rows.Close()

	conversations := make([]ponsruntime.Conversation, 0)
	for rows.Next() {
		var conversation ponsruntime.Conversation
		var created int64
		if err := rows.Scan(&conversation.ID, &conversation.AgentID, &conversation.Workspace, &conversation.WorkspaceLock, &conversation.GitRepository, &conversation.GitRevision, &conversation.GitAllRepositories, &conversation.Environment, &created); err != nil {
			return nil, fmt.Errorf("runtime: list conversations: %w", err)
		}
		conversation.CreatedAt = decodeTime(created)
		conversations = append(conversations, conversation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("runtime: list conversations: %w", err)
	}
	return conversations, nil
}

func appendEventTx(ctx context.Context, tx *sql.Tx, event ponsruntime.Event) (ponsruntime.Event, error) {
	var next uint64
	if err := tx.QueryRowContext(ctx,
		`SELECT next_event_cursor FROM conversations WHERE id = ?`, event.ConversationID,
	).Scan(&next); err != nil {
		return ponsruntime.Event{}, err
	}
	event.ID = next
	if event.CreatedAt.IsZero() {
		event.CreatedAt = canonicalTime(time.Now())
	} else {
		event.CreatedAt = canonicalTime(event.CreatedAt)
	}
	messageID := ""
	switch event.Type {
	case ponsruntime.EventInputAccepted:
		messageID = event.InboundMessageID
	case ponsruntime.EventAssistantCommitted:
		messageID = event.AssistantOutput.ID
	case ponsruntime.EventToolOutcomeRecorded:
		messageID = event.ToolOutcome.ID
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return ponsruntime.Event{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO events(conversation_id, cursor, type, message_id, payload, created_at) VALUES(?, ?, ?, ?, ?, ?)`,
		event.ConversationID, event.ID, event.Type, messageID, payload, encodeTime(event.CreatedAt)); err != nil {
		return ponsruntime.Event{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE conversations SET next_event_cursor = ? WHERE id = ?`, next+1, event.ConversationID); err != nil {
		return ponsruntime.Event{}, err
	}
	return event, nil
}

func (s *Store) AppendEnvironmentProgress(ctx context.Context, run ponsruntime.Run, progress ponsruntime.EnvironmentProgress) (ponsruntime.Event, error) {
	if run.ID == "" || run.ConversationID == "" || progress.Step == "" || progress.Message == "" {
		return ponsruntime.Event{}, errors.New("runtime: incomplete environment progress")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return ponsruntime.Event{}, err
	}
	defer tx.Rollback()
	var status string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM runs WHERE id = ? AND conversation_id = ?`, run.ID, run.ConversationID).Scan(&status); err != nil {
		return ponsruntime.Event{}, err
	}
	if status != ponsruntime.RunRunning {
		return ponsruntime.Event{}, errors.New("runtime: environment progress requires a running run")
	}
	event, err := appendEventTx(ctx, tx, ponsruntime.Event{
		Type: ponsruntime.EventEnvironmentProgress, ConversationID: run.ConversationID,
		RunID: run.ID, InboundMessageID: run.InboundMessageID, EnvironmentProgress: &progress,
	})
	if err != nil {
		return ponsruntime.Event{}, err
	}
	if err := tx.Commit(); err != nil {
		return ponsruntime.Event{}, err
	}
	return event, nil
}

// AppendAgentEvent records one typed internal fact before the caller proceeds.
// Client replay deliberately excludes agent.* entries.
func (s *Store) AppendAgentEvent(ctx context.Context, run ponsruntime.Run, event ponsruntime.Event) error {
	if run.ID == "" || run.ConversationID == "" || !validAgentEvent(event) {
		return errors.New("runtime: invalid agent event")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var status string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM runs WHERE id = ? AND conversation_id = ?`, run.ID, run.ConversationID).Scan(&status); err != nil {
		return err
	}
	if status != ponsruntime.RunRunning {
		return fmt.Errorf("runtime: agent step for non-running run %q", run.ID)
	}
	event.ConversationID = run.ConversationID
	event.RunID = run.ID
	event.InboundMessageID = run.InboundMessageID
	if _, err := appendEventTx(ctx, tx, event); err != nil {
		return err
	}
	return tx.Commit()
}

func validAgentEvent(event ponsruntime.Event) bool {
	payloads := 0
	for _, present := range []bool{
		event.AgentBoundary != nil, event.Iteration != nil, event.ModelOutput != nil,
		event.ToolExecution != nil, event.Compaction != nil, event.PreparedInput != nil,
	} {
		if present {
			payloads++
		}
	}
	if payloads != 1 || event.Input != nil || event.AssistantOutput != nil || event.ToolOutcome != nil || event.Run != nil {
		return false
	}
	switch event.Type {
	case ponsruntime.EventAgentStarted, ponsruntime.EventAgentFinished,
		ponsruntime.EventAgentStopped, ponsruntime.EventAgentExhausted:
		return event.AgentBoundary != nil
	case ponsruntime.EventIterationStarted, ponsruntime.EventIterationCompleted:
		return event.Iteration != nil && event.Iteration.Turn > 0
	case ponsruntime.EventModelCompleted:
		return event.ModelOutput != nil && event.ModelOutput.Turn > 0
	case ponsruntime.EventToolExecutionStarted:
		return event.ToolExecution != nil && event.ToolExecution.Turn > 0 && event.ToolExecution.ToolCallID != ""
	case ponsruntime.EventContextCompacted:
		return event.Compaction != nil && event.Compaction.Turn > 0 && len(event.Compaction.Context) > 0
	case ponsruntime.EventInputPrepared:
		return event.PreparedInput != nil && event.PreparedInput.Role == "user" && len(event.PreparedInput.Content) > 0
	default:
		return false
	}
}

func (s *Store) Accept(ctx context.Context, conversationID string, input ponsruntime.InputSubmission) (ponsruntime.AcceptedMessage, []ponsruntime.Event, error) {
	if err := ponsruntime.ValidateInputSubmission(input); err != nil {
		return ponsruntime.AcceptedMessage{}, nil, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return ponsruntime.AcceptedMessage{}, nil, err
	}
	defer tx.Rollback()
	var existing string
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM submissions WHERE conversation_id = ? AND idempotency_key = ?`, conversationID, input.IdempotencyKey,
	).Scan(&existing)
	if err == nil {
		return ponsruntime.AcceptedMessage{ConversationID: conversationID, InboundMessageID: existing, Duplicate: true}, nil, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return ponsruntime.AcceptedMessage{}, nil, err
	}
	conversation, err := conversationTx(ctx, tx, conversationID)
	if err != nil {
		return ponsruntime.AcceptedMessage{}, nil, err
	}
	if input.TargetAgentID != conversation.AgentID || input.AgentRevision == "" {
		return ponsruntime.AcceptedMessage{}, nil, errors.New("runtime: input target or agent revision does not match conversation")
	}

	now := canonicalTime(time.Now())
	id := newID()
	if _, err := tx.ExecContext(ctx, `
INSERT INTO submissions(id, conversation_id, idempotency_key, message_id, agent_revision, status, accepted_at)
VALUES(?, ?, ?, ?, ?, ?, ?)`, id, conversationID, input.IdempotencyKey, id, input.AgentRevision, ponsruntime.RunQueued, encodeTime(now)); err != nil {
		return ponsruntime.AcceptedMessage{}, nil, err
	}
	inputEvent, err := appendEventTx(ctx, tx, ponsruntime.Event{
		Type: ponsruntime.EventInputAccepted, ConversationID: conversationID,
		InboundMessageID: id, Input: &ponsruntime.UserInput{InputSubmission: input},
		CreatedAt: now,
	})
	if err != nil {
		return ponsruntime.AcceptedMessage{}, nil, err
	}
	if err := tx.Commit(); err != nil {
		return ponsruntime.AcceptedMessage{}, nil, err
	}
	return ponsruntime.AcceptedMessage{ConversationID: conversationID, InboundMessageID: id}, []ponsruntime.Event{inputEvent}, nil
}

func conversationTx(ctx context.Context, tx *sql.Tx, id string) (ponsruntime.Conversation, error) {
	var conversation ponsruntime.Conversation
	var created int64
	err := tx.QueryRowContext(ctx,
		`SELECT id, agent_id, workspace, workspace_lock, git_repository, git_revision, git_all_repositories, environment, created_at FROM conversations WHERE id = ?`, id,
	).Scan(&conversation.ID, &conversation.AgentID, &conversation.Workspace, &conversation.WorkspaceLock, &conversation.GitRepository, &conversation.GitRevision,
		&conversation.GitAllRepositories, &conversation.Environment, &created)

	if errors.Is(err, sql.ErrNoRows) {
		return ponsruntime.Conversation{}, ponsruntime.ErrNotFound
	}
	if err != nil {
		return ponsruntime.Conversation{}, err
	}
	conversation.CreatedAt = decodeTime(created)
	return conversation, nil
}

func (s *Store) ClaimRunnable(ctx context.Context) (*ponsruntime.ClaimedRun, error) {
	// Open configures SQLite transactions with _txlock=immediate, so eligibility
	// checks and the queued-to-running transition serialize with competing local
	// writers. Both conversation and workspace exclusion are decided inside this
	// transaction; Manager's in-memory state is not an ownership lock.
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var id, conversationID, key, agentRevision, agentID, workspace, workspaceLock, gitRepository, gitRevision, environment string
	var gitAllRepositories bool
	var accepted, conversationCreated int64
	err = tx.QueryRowContext(ctx, `
SELECT queued.id, queued.conversation_id, queued.idempotency_key, queued.agent_revision, queued.accepted_at,
       conversation.agent_id, conversation.workspace, conversation.workspace_lock, conversation.git_repository, conversation.git_revision,
       conversation.git_all_repositories, conversation.environment, conversation.created_at

FROM submissions AS queued
JOIN conversations AS conversation ON conversation.id = queued.conversation_id
WHERE queued.status = ?
  AND NOT EXISTS (
    SELECT 1 FROM submissions AS active
    WHERE active.conversation_id = queued.conversation_id AND active.status = ?
  )
  AND NOT EXISTS (
    SELECT 1
    FROM submissions AS active
    JOIN conversations AS active_conversation ON active_conversation.id = active.conversation_id
    WHERE active.status = ? AND active_conversation.workspace_lock = conversation.workspace_lock
  )
ORDER BY queued.accepted_at, queued.id
LIMIT 1`, ponsruntime.RunQueued, ponsruntime.RunRunning, ponsruntime.RunRunning,
	).Scan(&id, &conversationID, &key, &agentRevision, &accepted, &agentID, &workspace, &workspaceLock, &gitRepository, &gitRevision,
		&gitAllRepositories, &environment, &conversationCreated)

	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	parts, err := loadInputTx(ctx, tx, conversationID, id)
	if err != nil {
		return nil, err
	}
	acceptedAt := decodeTime(accepted)
	createdAt := decodeTime(conversationCreated)
	now := canonicalTime(time.Now())
	run := ponsruntime.Run{
		ID: newID(), ConversationID: conversationID, InboundMessageID: id,
		AgentRevision: agentRevision, Status: ponsruntime.RunRunning, StartedAt: now,
	}

	result, err := tx.ExecContext(ctx, `UPDATE submissions SET status = ? WHERE id = ? AND status = ?`, ponsruntime.RunRunning, id, ponsruntime.RunQueued)
	if err != nil {
		return nil, err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if updated != 1 {
		return nil, errors.New("runtime: runnable submission lost its claim race")
	}

	if _, err := tx.ExecContext(ctx, `
INSERT INTO runs(id, conversation_id, submission_id, inbound_message_id, agent_revision, status, started_at)
VALUES(?, ?, ?, ?, ?, ?, ?)`, run.ID, conversationID, id, id, agentRevision, run.Status, encodeTime(now)); err != nil {
		return nil, err
	}

	compaction, compactCursor, err := loadLatestCompactionTx(ctx, tx, conversationID)
	if err != nil {
		return nil, err
	}
	contextAfterCheckpoint, err := loadContextAfterTx(ctx, tx, conversationID, compactCursor)
	if err != nil {
		return nil, err
	}
	var contextTurns []ponsruntime.ContextTurn
	if compaction != nil {
		contextTurns = append(contextTurns, compaction.Context...)
	}
	contextTurns = append(contextTurns, contextAfterCheckpoint...)
	admittedEvent, err := appendEventTx(ctx, tx, ponsruntime.Event{
		Type: ponsruntime.EventInputAdmitted, ConversationID: conversationID,
		RunID: run.ID, InboundMessageID: id, CreatedAt: now,
	})
	if err != nil {
		return nil, err
	}
	runEvent, err := appendEventTx(ctx, tx, ponsruntime.Event{
		Type: ponsruntime.EventRunStarted, ConversationID: conversationID, RunID: run.ID,
		InboundMessageID: id, Run: &run, CreatedAt: now,
	})
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &ponsruntime.ClaimedRun{
		Conversation: ponsruntime.Conversation{
			ID: conversationID, AgentID: agentID, Workspace: workspace, WorkspaceLock: workspaceLock, GitRepository: gitRepository,
			GitRevision: gitRevision, GitAllRepositories: gitAllRepositories, Environment: environment, CreatedAt: createdAt,
		},
		Message: ponsruntime.InboundMessage{ID: id, IdempotencyKey: key, Parts: parts, AcceptedAt: acceptedAt},
		Context: contextTurns,
		Run:     run, Events: []ponsruntime.Event{admittedEvent, runEvent},
	}, nil
}

func (s *Store) CommitAssistantTurn(ctx context.Context, run ponsruntime.Run, parts []ponsruntime.MessagePart) ([]ponsruntime.Event, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := canonicalTime(time.Now())
	normalizedParts := cloneMessageParts(parts)
	toolParts := make([]ponsruntime.MessagePart, 0, len(parts))
	for i := range normalizedParts {
		part := &normalizedParts[i]
		switch part.Type {
		case "text":
			if part.Text == "" {
				return nil, fmt.Errorf("runtime: assistant part %d has empty text", i)
			}
		case "tool_call":
			if part.ToolCallID == "" || part.ToolKind == "" {
				return nil, fmt.Errorf("runtime: assistant part %d has an incomplete tool call", i)
			}
			if _, err := protocol.ObjectArgs(part.Arguments); err != nil {
				return nil, fmt.Errorf("runtime: assistant part %d arguments: %w", i, err)
			}
			arguments := bytes.TrimSpace(part.Arguments)
			if len(arguments) == 0 || bytes.Equal(arguments, []byte("null")) {
				part.Arguments = json.RawMessage(`{}`)
			}
			toolParts = append(toolParts, *part)
		default:
			return nil, fmt.Errorf("runtime: assistant part %d has unsupported type %q", i, part.Type)
		}
	}

	if len(toolParts) == 0 {
		return nil, errors.New("runtime: assistant tool turn has no tool calls")
	}

	outputID := newID()
	outputEvent, err := appendEventTx(ctx, tx, ponsruntime.Event{
		Type: ponsruntime.EventAssistantCommitted, ConversationID: run.ConversationID,
		RunID: run.ID, InboundMessageID: run.InboundMessageID,
		AssistantOutput: &ponsruntime.AssistantOutput{ID: outputID, Parts: normalizedParts},
		CreatedAt:       now,
	})
	if err != nil {
		return nil, err
	}
	for _, part := range toolParts {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO tool_calls(id, conversation_id, run_id, inbound_message_id, kind, status, updated_at)
VALUES(?, ?, ?, ?, ?, ?, ?)`, part.ToolCallID, run.ConversationID, run.ID, run.InboundMessageID,
			part.ToolKind, ponsruntime.ToolRequested, encodeTime(now)); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return []ponsruntime.Event{outputEvent}, nil
}

func (s *Store) ToolCompleted(ctx context.Context, run ponsruntime.Run, result protocol.ToolResult) ([]ponsruntime.Event, error) {
	status := ponsruntime.ToolCompleted
	if !result.OK {
		status = ponsruntime.ToolFailed
	}
	return s.toolFinished(ctx, run, result, status)
}

func (s *Store) toolFinished(ctx context.Context, run ponsruntime.Run, result protocol.ToolResult, status string) ([]ponsruntime.Event, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	events, err := toolFinishedTx(ctx, tx, run, result, status, canonicalTime(time.Now()))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return events, nil
}

func toolFinishedTx(
	ctx context.Context,
	tx *sql.Tx,
	run ponsruntime.Run,
	result protocol.ToolResult,
	status string,
	now time.Time,
) ([]ponsruntime.Event, error) {
	var kindText string
	if err := tx.QueryRowContext(ctx,
		`SELECT kind FROM tool_calls WHERE id = ? AND run_id = ? AND status = ?`,
		result.ActionID, run.ID, ponsruntime.ToolRequested,
	).Scan(&kindText); err != nil {
		return nil, err
	}
	updated, err := tx.ExecContext(ctx, `
UPDATE tool_calls SET status = ?, updated_at = ?
WHERE id = ? AND run_id = ? AND status = ?`, status, encodeTime(now),
		result.ActionID, run.ID, ponsruntime.ToolRequested)
	if err != nil {
		return nil, err
	}
	count, err := updated.RowsAffected()
	if err != nil {
		return nil, err
	}
	if count != 1 {
		return nil, fmt.Errorf("runtime: requested tool %q lost its completion race", result.ActionID)
	}
	outcomeEvent, err := appendEventTx(ctx, tx, ponsruntime.Event{
		Type: ponsruntime.EventToolOutcomeRecorded, ConversationID: run.ConversationID,
		RunID: run.ID, InboundMessageID: run.InboundMessageID, CreatedAt: now,
		ToolOutcome: &ponsruntime.ToolOutcome{
			ID: newID(), ToolCallID: result.ActionID, ToolKind: kindText,
			Status: status, Result: cloneResult(result),
		},
	})
	if err != nil {
		return nil, err
	}
	return []ponsruntime.Event{outcomeEvent}, nil
}

func cloneResult(result protocol.ToolResult) *protocol.ToolResult {
	copy := result
	copy.Payload = cloneRaw(result.Payload)
	return &copy
}

func cloneMessageParts(parts []ponsruntime.MessagePart) []ponsruntime.MessagePart {
	out := make([]ponsruntime.MessagePart, len(parts))
	for i, part := range parts {
		out[i] = part
		out[i].Arguments = cloneRaw(part.Arguments)
		out[i].Metadata = cloneMetadata(part.Metadata)
		if part.Result != nil {
			out[i].Result = cloneResult(*part.Result)
		}
	}
	return out
}

func (s *Store) FinishRun(ctx context.Context, run ponsruntime.Run, answer string) ([]ponsruntime.Event, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := canonicalTime(time.Now())
	if _, err := tx.ExecContext(ctx, `UPDATE submissions SET status = ? WHERE id = ?`, ponsruntime.RunCompleted, run.InboundMessageID); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET status = ?, completed_at = ? WHERE id = ?`, ponsruntime.RunCompleted, encodeTime(now), run.ID); err != nil {
		return nil, err
	}
	completed := run
	completed.Status, completed.CompletedAt = ponsruntime.RunCompleted, &now
	outputEvent, err := appendEventTx(ctx, tx, ponsruntime.Event{
		Type: ponsruntime.EventAssistantCommitted, ConversationID: run.ConversationID,
		RunID: run.ID, InboundMessageID: run.InboundMessageID, CreatedAt: now,
		AssistantOutput: &ponsruntime.AssistantOutput{
			ID: newID(), Parts: []ponsruntime.MessagePart{{Type: "text", Text: answer}}, Final: true,
		},
	})
	if err != nil {
		return nil, err
	}
	runEvent, err := appendEventTx(ctx, tx, ponsruntime.Event{
		Type: ponsruntime.EventRunCompleted, ConversationID: run.ConversationID, RunID: run.ID,
		InboundMessageID: run.InboundMessageID, Run: &completed, CreatedAt: now,
	})
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return []ponsruntime.Event{outputEvent, runEvent}, nil
}

func (s *Store) FailRun(ctx context.Context, run ponsruntime.Run, message string) ([]ponsruntime.Event, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := canonicalTime(time.Now())
	events, err := interruptRequestedToolsTx(ctx, tx, run, now)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE submissions SET status = ?, error = ? WHERE id = ?`, ponsruntime.RunFailed, message, run.InboundMessageID); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET status = ?, error = ?, completed_at = ? WHERE id = ?`, ponsruntime.RunFailed, message, encodeTime(now), run.ID); err != nil {
		return nil, err
	}
	failed := run
	failed.Status, failed.Error, failed.CompletedAt = ponsruntime.RunFailed, message, &now
	runEvent, err := appendEventTx(ctx, tx, ponsruntime.Event{
		Type: ponsruntime.EventRunFailed, ConversationID: run.ConversationID, RunID: run.ID,
		InboundMessageID: run.InboundMessageID, Run: &failed, CreatedAt: now,
	})
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return append(events, runEvent), nil
}

func interruptRequestedToolsTx(
	ctx context.Context,
	tx *sql.Tx,
	run ponsruntime.Run,
	now time.Time,
) ([]ponsruntime.Event, error) {
	rows, err := tx.QueryContext(ctx, `
SELECT id, kind FROM tool_calls
WHERE run_id = ? AND status = ? ORDER BY rowid`, run.ID, ponsruntime.ToolRequested)
	if err != nil {
		return nil, err
	}
	type requestedTool struct {
		id   string
		kind string
	}
	var tools []requestedTool
	for rows.Next() {
		var tool requestedTool
		if err := rows.Scan(&tool.id, &tool.kind); err != nil {
			rows.Close()
			return nil, err
		}
		tools = append(tools, tool)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	events := make([]ponsruntime.Event, 0, len(tools)*2)
	for _, tool := range tools {
		result := protocol.ToolResult{
			ActionID: tool.id,
			Kind:     tool.kind,
			OK:       false,
			Error:    "execution was interrupted; outcome unknown",
		}
		finished, err := toolFinishedTx(ctx, tx, run, result, ponsruntime.ToolInterrupted, now)
		if err != nil {
			return nil, err
		}
		events = append(events, finished...)
	}
	return events, nil
}

func (s *Store) RecoverRunning(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, conversation_id, inbound_message_id, agent_revision, started_at FROM runs
WHERE status = ? ORDER BY started_at`, ponsruntime.RunRunning)
	if err != nil {
		return err
	}
	type running struct {
		id, conversation, inbound, revision string
		started                             int64
	}
	var values []running
	for rows.Next() {
		var value running
		if err := rows.Scan(&value.id, &value.conversation, &value.inbound, &value.revision, &value.started); err != nil {
			rows.Close()
			return err
		}
		values = append(values, value)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, value := range values {
		started := decodeTime(value.started)
		run := ponsruntime.Run{
			ID: value.id, ConversationID: value.conversation, InboundMessageID: value.inbound,
			AgentRevision: value.revision, Status: ponsruntime.RunRunning, StartedAt: started,
		}
		if _, err := s.FailRun(ctx, run, "execution was interrupted; outcome unknown"); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) Events(ctx context.Context, conversationID string, after uint64) ([]ponsruntime.Event, error) {
	if _, err := s.Conversation(ctx, conversationID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT payload FROM events WHERE conversation_id = ? AND cursor > ? AND type NOT LIKE 'agent.%' ORDER BY cursor`, conversationID, after)
	if err != nil {
		return nil, err
	}
	return decodeEvents(rows)
}

// AgentEvents reads the internal part of the same cursor-ordered event log.
func (s *Store) AgentEvents(ctx context.Context, conversationID string, after uint64) ([]ponsruntime.Event, error) {
	if _, err := s.Conversation(ctx, conversationID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT payload FROM events WHERE conversation_id = ? AND cursor > ? AND type LIKE 'agent.%' ORDER BY cursor`, conversationID, after)
	if err != nil {
		return nil, err
	}
	return decodeEvents(rows)
}

func (s *Store) View(ctx context.Context, conversationID string) (ponsruntime.ConversationView, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ponsruntime.ConversationView{}, err
	}
	defer tx.Rollback()
	conversation, err := conversationTx(ctx, tx, conversationID)
	if err != nil {
		return ponsruntime.ConversationView{}, err
	}
	var next uint64
	if err := tx.QueryRowContext(ctx, `SELECT next_event_cursor FROM conversations WHERE id = ?`, conversationID).Scan(&next); err != nil {
		return ponsruntime.ConversationView{}, err
	}
	events, err := loadPublicEventsTx(ctx, tx, conversationID)
	if err != nil {
		return ponsruntime.ConversationView{}, err
	}
	view, err := ponsruntime.ProjectView(conversation, events)
	if err != nil {
		return ponsruntime.ConversationView{}, err
	}
	if next > 0 {
		view.EventCursor = next - 1
	}
	if err := tx.Commit(); err != nil {
		return ponsruntime.ConversationView{}, err
	}
	return view, nil
}

func loadPublicEventsTx(ctx context.Context, tx *sql.Tx, conversationID string) ([]ponsruntime.Event, error) {
	rows, err := tx.QueryContext(ctx, `
SELECT payload FROM events WHERE conversation_id = ? AND type NOT LIKE 'agent.%' ORDER BY cursor`, conversationID)
	if err != nil {
		return nil, err
	}
	return decodeEvents(rows)
}

func decodeEvents(rows *sql.Rows) ([]ponsruntime.Event, error) {
	defer rows.Close()
	var events []ponsruntime.Event
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var event ponsruntime.Event
		if err := json.Unmarshal(payload, &event); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func loadLatestCompactionTx(ctx context.Context, tx *sql.Tx, conversationID string) (*ponsruntime.ContextCompaction, uint64, error) {
	var payload []byte
	var cursor uint64
	err := tx.QueryRowContext(ctx, `
SELECT cursor, payload FROM events
WHERE conversation_id = ? AND type = ?
ORDER BY cursor DESC LIMIT 1`, conversationID, ponsruntime.EventContextCompacted).Scan(&cursor, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	var event ponsruntime.Event
	if err := json.Unmarshal(payload, &event); err != nil {
		return nil, 0, err
	}
	if event.Compaction == nil {
		return nil, 0, errors.New("runtime: compaction event has no payload")
	}
	if len(event.Compaction.Context) == 0 {
		return nil, 0, errors.New("runtime: compaction event has no context")
	}
	return event.Compaction, cursor, nil
}

// loadContextAfterTx rebuilds provider context after a compaction checkpoint.
// A model output becomes history only when its complete assistant message was
// committed; a model response whose subsequent persistence failed is ignored.
func loadContextAfterTx(ctx context.Context, tx *sql.Tx, conversationID string, after uint64) ([]ponsruntime.ContextTurn, error) {
	rows, err := tx.QueryContext(ctx, `
SELECT payload FROM events
WHERE conversation_id = ? AND type IN (?, ?, ?, ?, ?, ?)
ORDER BY cursor`, conversationID, ponsruntime.EventInputAccepted,
		ponsruntime.EventAssistantCommitted, ponsruntime.EventToolOutcomeRecorded,
		ponsruntime.EventInputAdmitted, ponsruntime.EventModelCompleted, ponsruntime.EventInputPrepared)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	accepted := make(map[string]ponsruntime.Message)
	admitted := make(map[string]bool)
	inputIndex := make(map[string]int)
	pendingModel := make(map[string][]ponsruntime.ContextTurn)
	var tail []ponsruntime.ContextTurn
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var event ponsruntime.Event
		if err := json.Unmarshal(payload, &event); err != nil {
			return nil, err
		}
		switch event.Type {
		case ponsruntime.EventInputAccepted, ponsruntime.EventAssistantCommitted, ponsruntime.EventToolOutcomeRecorded:
			message, err := event.ProjectMessage()
			if err != nil {
				return nil, err
			}
			if message.Role == "user" {
				accepted[message.ID] = message
				continue
			}
			if event.ID <= after || !admitted[event.InboundMessageID] || !message.Complete {
				continue
			}
			if message.Role == "assistant" && len(pendingModel[event.RunID]) > 0 {
				tail = append(tail, pendingModel[event.RunID][0])
				pendingModel[event.RunID] = pendingModel[event.RunID][1:]
				continue
			}
			turn, err := contextTurnFromMessage(message)
			if err != nil {
				return nil, err
			}
			if message.Role == "tool" && len(tail) > 0 && len(tail[len(tail)-1].Content) > 0 &&
				tail[len(tail)-1].Content[0].Type == "tool_result" {
				tail[len(tail)-1].Content = append(tail[len(tail)-1].Content, turn.Content...)
				continue
			}
			tail = append(tail, turn)
		case ponsruntime.EventInputAdmitted:
			message, ok := accepted[event.InboundMessageID]
			if !ok {
				return nil, fmt.Errorf("runtime: admitted message %q is missing", event.InboundMessageID)
			}
			if admitted[event.InboundMessageID] {
				return nil, fmt.Errorf("runtime: message %q admitted twice", event.InboundMessageID)
			}
			admitted[event.InboundMessageID] = true
			if event.ID > after {
				turn, err := contextTurnFromMessage(message)
				if err != nil {
					return nil, err
				}
				tail = append(tail, turn)
				inputIndex[event.InboundMessageID] = len(tail) - 1
			}
		case ponsruntime.EventInputPrepared:
			if event.ID > after && event.PreparedInput != nil {
				index, ok := inputIndex[event.InboundMessageID]
				if !ok {
					return nil, fmt.Errorf("runtime: prepared input %q was not admitted", event.InboundMessageID)
				}
				tail[index] = *event.PreparedInput
			}
		case ponsruntime.EventModelCompleted:
			if event.ID > after && event.ModelOutput != nil {
				pendingModel[event.RunID] = append(pendingModel[event.RunID], ponsruntime.ContextTurn{
					Role: "assistant", Content: event.ModelOutput.Content,
				})
			}
		}
	}
	return tail, rows.Err()
}

func contextTurnFromMessage(message ponsruntime.Message) (ponsruntime.ContextTurn, error) {
	turn := ponsruntime.ContextTurn{Role: message.Role}
	if turn.Role == "tool" {
		turn.Role = "user"
	}
	for _, part := range message.Parts {
		switch part.Type {
		case "text":
			turn.Content = append(turn.Content, ponsruntime.AgentContent{Type: "text", Text: part.Text})
		case "tool_call":
			turn.Content = append(turn.Content, ponsruntime.AgentContent{
				Type: "tool_call", ToolCallID: part.ToolCallID, ToolKind: part.ToolKind,
				Arguments: cloneRaw(part.Arguments),
			})
		case "tool_result":
			if part.Result == nil {
				return ponsruntime.ContextTurn{}, errors.New("runtime: tool result message has no result")
			}
			content := part.Result.Observation()
			if strings.TrimSpace(content) == "" {
				content = "(no output)"
			}
			turn.Content = append(turn.Content, ponsruntime.AgentContent{
				Type: "tool_result", ToolCallID: part.ToolCallID, Text: content, IsError: !part.Result.OK,
			})
		default:
			return ponsruntime.ContextTurn{}, fmt.Errorf("runtime: unsupported message part %q", part.Type)
		}
	}
	return turn, nil
}

func loadInputTx(ctx context.Context, tx *sql.Tx, conversationID, id string) ([]ponsruntime.TextPart, error) {
	var payload []byte
	err := tx.QueryRowContext(ctx, `
SELECT payload FROM events WHERE conversation_id = ? AND message_id = ?`, conversationID, id).Scan(&payload)
	if err != nil {
		return nil, err
	}
	var event ponsruntime.Event
	if err := json.Unmarshal(payload, &event); err != nil {
		return nil, err
	}
	if event.Type != ponsruntime.EventInputAccepted || event.Input == nil || event.InboundMessageID != id {
		return nil, fmt.Errorf("runtime: accepted input %q is missing", id)
	}
	return event.Input.Parts, nil
}

func cloneMetadata(metadata map[string]json.RawMessage) map[string]json.RawMessage {
	if metadata == nil {
		return nil
	}
	copy := make(map[string]json.RawMessage, len(metadata))
	for key, value := range metadata {
		copy[key] = cloneRaw(value)
	}
	return copy
}
