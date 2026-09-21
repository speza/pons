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
	"time"

	"github.com/ncruces/go-sqlite3/driver"
	"github.com/samperrin/pons/environment"
	"github.com/samperrin/pons/protocol"
	ponsruntime "github.com/samperrin/pons/runtime"
)

const runtimeDBName = "runtime.db"

// Store is the local, exclusive-manager runtime backend. Its transactions
// prevent duplicate claims and concurrent conversation/workspace ownership,
// but they are not renewable leases or a distributed fencing mechanism.
type Store struct{ db *sql.DB }

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
	path := filepath.Join(stateDir, runtimeDBName)
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: "_txlock=immediate&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=synchronous(FULL)"}).String()
	db, err := driver.Open(dsn)
	if err != nil {
		return nil, fmt.Errorf("runtime: open database: %w", err)
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	store := &Store{db: db}
	if err := store.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("runtime: secure database: %w", err)
	}
	return store, nil
}

func (s *Store) migrate(ctx context.Context) error {
	const schema = `
CREATE TABLE IF NOT EXISTS conversations (
  id TEXT PRIMARY KEY,
  workspace TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  next_event_cursor INTEGER NOT NULL DEFAULT 1
);
CREATE TABLE IF NOT EXISTS messages (
  id TEXT PRIMARY KEY,
  conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
  inbound_message_id TEXT NOT NULL DEFAULT '',
	  run_id TEXT NOT NULL DEFAULT '',
	  role TEXT NOT NULL,
	  complete BOOLEAN NOT NULL DEFAULT TRUE,
	  final BOOLEAN NOT NULL DEFAULT FALSE,
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS messages_conversation_order
  ON messages(conversation_id, created_at, id);
CREATE TABLE IF NOT EXISTS message_parts (
  message_id TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  position INTEGER NOT NULL,
  type TEXT NOT NULL,
  text TEXT NOT NULL DEFAULT '',
  tool_call_id TEXT NOT NULL DEFAULT '',
  tool_kind TEXT NOT NULL DEFAULT '',
  arguments BLOB,
  result BLOB,
	metadata BLOB,
  PRIMARY KEY(message_id, position)
);
CREATE TABLE IF NOT EXISTS submissions (
  id TEXT PRIMARY KEY,
  conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
  idempotency_key TEXT NOT NULL,
  message_id TEXT NOT NULL REFERENCES messages(id),
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
  message_id TEXT NOT NULL REFERENCES messages(id),
  kind TEXT NOT NULL,
  arguments BLOB NOT NULL,
  status TEXT NOT NULL,
  result BLOB,
  result_message_id TEXT,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY(run_id, id)
);
CREATE INDEX IF NOT EXISTS tool_calls_conversation
  ON tool_calls(conversation_id, run_id, updated_at);
CREATE TABLE IF NOT EXISTS events (
  conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
  cursor INTEGER NOT NULL,
  type TEXT NOT NULL,
  payload BLOB NOT NULL,
  created_at INTEGER NOT NULL,
  PRIMARY KEY(conversation_id, cursor)
);
CREATE TABLE IF NOT EXISTS execution_environments (
  environment_key TEXT PRIMARY KEY,
  provider TEXT NOT NULL,
  environment_id TEXT NOT NULL,
  template TEXT NOT NULL,
  network_policy TEXT NOT NULL DEFAULT 'disabled'
    CHECK (network_policy IN ('disabled', 'enabled')),
  workspace_strategy TEXT NOT NULL DEFAULT 'local_archive',
  workspace_source_ref TEXT NOT NULL DEFAULT '',
  workspace_revision TEXT NOT NULL DEFAULT '',
  checkpoint_revision TEXT NOT NULL DEFAULT '',
  setup_generation INTEGER NOT NULL DEFAULT 1,
  status TEXT NOT NULL CHECK (status IN ('active', 'idle')),
  lease_id TEXT NOT NULL DEFAULT '',
  idle_until INTEGER,
  expires_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS execution_environments_expiry
  ON execution_environments(provider, status, idle_until);
CREATE INDEX IF NOT EXISTS execution_environments_provider_expiry
  ON execution_environments(provider, expires_at);
CREATE UNIQUE INDEX IF NOT EXISTS execution_environments_provider_id
  ON execution_environments(provider, environment_id);
`
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("runtime: migrate database: %w", err)
	}
	if err := s.ensureColumn(ctx, "message_parts", "metadata", "BLOB"); err != nil {
		return fmt.Errorf("runtime: migrate message part metadata: %w", err)
	}
	if err := s.ensureColumn(ctx, "messages", "complete", "BOOLEAN NOT NULL DEFAULT TRUE"); err != nil {
		return fmt.Errorf("runtime: migrate message completeness: %w", err)
	}
	for _, column := range []struct {
		name       string
		definition string
	}{
		{"network_policy", "TEXT NOT NULL DEFAULT 'disabled'"},
		{"workspace_strategy", "TEXT NOT NULL DEFAULT 'local_archive'"},
		{"workspace_source_ref", "TEXT NOT NULL DEFAULT ''"},
		{"workspace_revision", "TEXT NOT NULL DEFAULT ''"},
		{"checkpoint_revision", "TEXT NOT NULL DEFAULT ''"},
		{"setup_generation", "INTEGER NOT NULL DEFAULT 1"},
	} {
		if err := s.ensureColumn(ctx, "execution_environments", column.name, column.definition); err != nil {
			return fmt.Errorf("runtime: migrate environment %s: %w", column.name, err)
		}
	}
	var invalid int
	if err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM execution_environments
WHERE status NOT IN ('active', 'idle')
   OR network_policy NOT IN ('disabled', 'enabled')`).Scan(&invalid); err != nil {
		return fmt.Errorf("runtime: validate environment lifecycle values: %w", err)
	}
	if invalid != 0 {
		return fmt.Errorf("runtime: %d execution environment rows have invalid lifecycle values", invalid)
	}
	const environmentTriggers = `
CREATE TRIGGER IF NOT EXISTS execution_environments_validate_insert
BEFORE INSERT ON execution_environments
WHEN NEW.status NOT IN ('active', 'idle')
  OR NEW.network_policy NOT IN ('disabled', 'enabled')
BEGIN
  SELECT RAISE(ABORT, 'invalid execution environment lifecycle value');
END;
CREATE TRIGGER IF NOT EXISTS execution_environments_validate_update
BEFORE UPDATE OF status, network_policy ON execution_environments
WHEN NEW.status NOT IN ('active', 'idle')
  OR NEW.network_policy NOT IN ('disabled', 'enabled')
BEGIN
  SELECT RAISE(ABORT, 'invalid execution environment lifecycle value');
END;
`
	if _, err := s.db.ExecContext(ctx, environmentTriggers); err != nil {
		return fmt.Errorf("runtime: constrain environment lifecycle values: %w", err)
	}
	return nil
}

func (s *Store) ensureColumn(ctx context.Context, table, column, definition string) error {
	var exists bool
	query := fmt.Sprintf("SELECT EXISTS(SELECT 1 FROM pragma_table_info('%s') WHERE name = ?)", table)
	if err := s.db.QueryRowContext(ctx, query, column).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	_, err := s.db.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, definition))
	return err
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) EnvironmentState(ctx context.Context, key string) (environment.State, error) {
	var state environment.State
	var idleUntil sql.NullInt64
	var expiresAt, updatedAt int64
	err := s.db.QueryRowContext(ctx, `
SELECT environment_key, provider, environment_id, template, network_policy,
       workspace_strategy, workspace_source_ref, workspace_revision,
       checkpoint_revision, setup_generation, status, lease_id,
       idle_until, expires_at, updated_at
FROM execution_environments WHERE environment_key = ?`, key).Scan(
		&state.Key,
		&state.Provider,
		&state.EnvironmentID,
		&state.Template,
		&state.Network,
		&state.WorkspaceStrategy,
		&state.WorkspaceSourceRef,
		&state.WorkspaceRevision,
		&state.CheckpointRevision,
		&state.SetupGeneration,
		&state.Status,
		&state.LeaseID,
		&idleUntil,
		&expiresAt,
		&updatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return environment.State{}, environment.ErrStateNotFound
	}
	if err != nil {
		return environment.State{}, fmt.Errorf("runtime: read environment state: %w", err)
	}
	if idleUntil.Valid {
		state.IdleUntil = decodeTime(idleUntil.Int64)
	}
	state.ExpiresAt = decodeTime(expiresAt)
	state.UpdatedAt = decodeTime(updatedAt)
	return state, nil
}

func (s *Store) SaveEnvironmentState(ctx context.Context, state environment.State) error {
	var idleUntil any
	if !state.IdleUntil.IsZero() {
		idleUntil = encodeTime(state.IdleUntil)
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO execution_environments (
  environment_key, provider, environment_id, template, network_policy,
  workspace_strategy, workspace_source_ref, workspace_revision,
  checkpoint_revision, setup_generation, status, lease_id,
  idle_until, expires_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(environment_key) DO UPDATE SET
  provider = excluded.provider,
  environment_id = excluded.environment_id,
  template = excluded.template,
  network_policy = excluded.network_policy,
  workspace_strategy = excluded.workspace_strategy,
  workspace_source_ref = excluded.workspace_source_ref,
  workspace_revision = excluded.workspace_revision,
  checkpoint_revision = excluded.checkpoint_revision,
  setup_generation = excluded.setup_generation,
  status = excluded.status,
  lease_id = excluded.lease_id,
  idle_until = excluded.idle_until,
  expires_at = excluded.expires_at,
  updated_at = excluded.updated_at`,
		state.Key,
		state.Provider,
		state.EnvironmentID,
		state.Template,
		state.Network,
		state.WorkspaceStrategy,
		state.WorkspaceSourceRef,
		state.WorkspaceRevision,
		state.CheckpointRevision,
		state.SetupGeneration,
		state.Status,
		state.LeaseID,
		idleUntil,
		encodeTime(state.ExpiresAt),
		encodeTime(state.UpdatedAt),
	)
	if err != nil {
		return fmt.Errorf("runtime: save environment state: %w", err)
	}
	return nil
}

func (s *Store) DeleteEnvironmentState(ctx context.Context, key, environmentID string) error {
	_, err := s.db.ExecContext(ctx, `
DELETE FROM execution_environments
WHERE environment_key = ? AND environment_id = ?`, key, environmentID)
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
SELECT environment_key, provider, environment_id, template, network_policy,
       workspace_strategy, workspace_source_ref, workspace_revision,
       checkpoint_revision, setup_generation, status, lease_id,
       idle_until, expires_at, updated_at
FROM execution_environments
WHERE provider = ?
  AND ((status = ? AND idle_until <= ?) OR expires_at <= ?)
ORDER BY COALESCE(idle_until, expires_at), environment_key
LIMIT ?`, provider, environment.StateIdle, encodeTime(now), encodeTime(now), limit)
	if err != nil {
		return nil, fmt.Errorf("runtime: list expired environment states: %w", err)
	}
	defer rows.Close()
	var states []environment.State
	for rows.Next() {
		var state environment.State
		var idleUntil sql.NullInt64
		var expiresAt, updatedAt int64
		if err := rows.Scan(
			&state.Key,
			&state.Provider,
			&state.EnvironmentID,
			&state.Template,
			&state.Network,
			&state.WorkspaceStrategy,
			&state.WorkspaceSourceRef,
			&state.WorkspaceRevision,
			&state.CheckpointRevision,
			&state.SetupGeneration,
			&state.Status,
			&state.LeaseID,
			&idleUntil,
			&expiresAt,
			&updatedAt,
		); err != nil {
			return nil, err
		}
		if idleUntil.Valid {
			state.IdleUntil = decodeTime(idleUntil.Int64)
		}
		state.ExpiresAt = decodeTime(expiresAt)
		state.UpdatedAt = decodeTime(updatedAt)
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
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO conversations(id, workspace, created_at) VALUES(?, ?, ?)`,
		conversation.ID, conversation.Workspace, encodeTime(conversation.CreatedAt))
	if err != nil {
		return fmt.Errorf("runtime: persist conversation: %w", err)
	}
	return nil
}

func (s *Store) Conversation(ctx context.Context, id string) (ponsruntime.Conversation, error) {
	var conversation ponsruntime.Conversation
	var created int64
	err := s.db.QueryRowContext(ctx,
		`SELECT id, workspace, created_at FROM conversations WHERE id = ?`, id,
	).Scan(&conversation.ID, &conversation.Workspace, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return ponsruntime.Conversation{}, ponsruntime.ErrNotFound
	}
	if err != nil {
		return ponsruntime.Conversation{}, fmt.Errorf("runtime: read conversation: %w", err)
	}
	conversation.CreatedAt = decodeTime(created)
	return conversation, nil
}

func insertMessageTx(ctx context.Context, tx *sql.Tx, message ponsruntime.Message) error {
	_, err := tx.ExecContext(ctx, `
	INSERT INTO messages(id, conversation_id, inbound_message_id, run_id, role, complete, final, created_at)
	VALUES(?, ?, ?, ?, ?, ?, ?, ?)`, message.ID, message.ConversationID, message.InboundMessageID,
		message.RunID, message.Role, message.Complete, message.Final, encodeTime(message.CreatedAt))
	if err != nil {
		return err
	}
	for i, part := range message.Parts {
		var result, metadata []byte
		if part.Result != nil {
			result, err = json.Marshal(part.Result)
			if err != nil {
				return err
			}
		}
		if len(part.Metadata) > 0 {
			metadata, err = json.Marshal(part.Metadata)
			if err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `
INSERT INTO message_parts(message_id, position, type, text, tool_call_id, tool_kind, arguments, result, metadata)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`, message.ID, i, part.Type, part.Text, part.ToolCallID,
			part.ToolKind, []byte(part.Arguments), result, metadata)
		if err != nil {
			return err
		}
	}
	return nil
}

func updateMessageEventTx(ctx context.Context, tx *sql.Tx, message ponsruntime.Message) (ponsruntime.Event, error) {
	copy := cloneMessage(message)
	return appendEventTx(ctx, tx, ponsruntime.Event{
		Type: ponsruntime.EventMessageUpserted, ConversationID: message.ConversationID,
		RunID: message.RunID, InboundMessageID: message.InboundMessageID, Message: &copy,
	})
}

func appendEventTx(ctx context.Context, tx *sql.Tx, event ponsruntime.Event) (ponsruntime.Event, error) {
	var next uint64
	if err := tx.QueryRowContext(ctx,
		`SELECT next_event_cursor FROM conversations WHERE id = ?`, event.ConversationID,
	).Scan(&next); err != nil {
		return ponsruntime.Event{}, err
	}
	event.ID = next
	event.CreatedAt = canonicalTime(time.Now())
	payload, err := json.Marshal(event)
	if err != nil {
		return ponsruntime.Event{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO events(conversation_id, cursor, type, payload, created_at) VALUES(?, ?, ?, ?, ?)`,
		event.ConversationID, event.ID, event.Type, payload, encodeTime(event.CreatedAt)); err != nil {
		return ponsruntime.Event{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE conversations SET next_event_cursor = ? WHERE id = ?`, next+1, event.ConversationID); err != nil {
		return ponsruntime.Event{}, err
	}
	return event, nil
}

func (s *Store) Accept(ctx context.Context, conversationID, key string, parts []ponsruntime.TextPart) (ponsruntime.AcceptedMessage, []ponsruntime.Event, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return ponsruntime.AcceptedMessage{}, nil, err
	}
	defer tx.Rollback()
	var existing string
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM submissions WHERE conversation_id = ? AND idempotency_key = ?`, conversationID, key,
	).Scan(&existing)
	if err == nil {
		return ponsruntime.AcceptedMessage{ConversationID: conversationID, InboundMessageID: existing, Duplicate: true}, nil, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return ponsruntime.AcceptedMessage{}, nil, err
	}
	if _, err := conversationTx(ctx, tx, conversationID); err != nil {
		return ponsruntime.AcceptedMessage{}, nil, err
	}
	now := canonicalTime(time.Now())
	id := newID()
	messageParts := make([]ponsruntime.MessagePart, len(parts))
	for i, part := range parts {
		messageParts[i] = ponsruntime.MessagePart{Type: "text", Text: part.Text}
	}
	message := ponsruntime.Message{
		ID: id, ConversationID: conversationID, InboundMessageID: id,
		Role: "user", Parts: messageParts, Complete: true, CreatedAt: now,
	}
	if err := insertMessageTx(ctx, tx, message); err != nil {
		return ponsruntime.AcceptedMessage{}, nil, err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO submissions(id, conversation_id, idempotency_key, message_id, status, accepted_at)
VALUES(?, ?, ?, ?, ?, ?)`, id, conversationID, key, message.ID, ponsruntime.RunQueued, encodeTime(now)); err != nil {
		return ponsruntime.AcceptedMessage{}, nil, err
	}
	messageEvent, err := updateMessageEventTx(ctx, tx, message)
	if err != nil {
		return ponsruntime.AcceptedMessage{}, nil, err
	}
	submission := ponsruntime.Submission{ID: id, ConversationID: conversationID, MessageID: message.ID, Status: ponsruntime.RunQueued, AcceptedAt: now}
	submissionEvent, err := appendEventTx(ctx, tx, ponsruntime.Event{
		Type: ponsruntime.EventSubmissionUpdated, ConversationID: conversationID,
		InboundMessageID: id, Submission: &submission,
	})
	if err != nil {
		return ponsruntime.AcceptedMessage{}, nil, err
	}
	if err := tx.Commit(); err != nil {
		return ponsruntime.AcceptedMessage{}, nil, err
	}
	return ponsruntime.AcceptedMessage{ConversationID: conversationID, InboundMessageID: id}, []ponsruntime.Event{messageEvent, submissionEvent}, nil
}

func conversationTx(ctx context.Context, tx *sql.Tx, id string) (ponsruntime.Conversation, error) {
	var conversation ponsruntime.Conversation
	var created int64
	err := tx.QueryRowContext(ctx,
		`SELECT id, workspace, created_at FROM conversations WHERE id = ?`, id,
	).Scan(&conversation.ID, &conversation.Workspace, &created)
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
	var id, conversationID, key, workspace string
	var accepted, conversationCreated int64
	err = tx.QueryRowContext(ctx, `
SELECT queued.id, queued.conversation_id, queued.idempotency_key, queued.accepted_at,
       conversation.workspace, conversation.created_at
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
    WHERE active.status = ? AND active_conversation.workspace = conversation.workspace
  )
ORDER BY queued.accepted_at, queued.id
LIMIT 1`, ponsruntime.RunQueued, ponsruntime.RunRunning, ponsruntime.RunRunning,
	).Scan(&id, &conversationID, &key, &accepted, &workspace, &conversationCreated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	parts, err := messageTextPartsTx(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	acceptedAt := decodeTime(accepted)
	createdAt := decodeTime(conversationCreated)
	now := canonicalTime(time.Now())
	run := ponsruntime.Run{
		ID: newID(), ConversationID: conversationID, InboundMessageID: id,
		Status: ponsruntime.RunRunning, StartedAt: now,
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
INSERT INTO runs(id, conversation_id, submission_id, inbound_message_id, status, started_at)
VALUES(?, ?, ?, ?, ?, ?)`, run.ID, conversationID, id, id, run.Status, encodeTime(now)); err != nil {
		return nil, err
	}
	submission := ponsruntime.Submission{ID: id, ConversationID: conversationID, MessageID: id, Status: ponsruntime.RunRunning, AcceptedAt: acceptedAt}
	subEvent, err := appendEventTx(ctx, tx, ponsruntime.Event{
		Type: ponsruntime.EventSubmissionUpdated, ConversationID: conversationID, RunID: run.ID,
		InboundMessageID: id, Submission: &submission,
	})
	if err != nil {
		return nil, err
	}
	runEvent, err := appendEventTx(ctx, tx, ponsruntime.Event{
		Type: ponsruntime.EventRunUpdated, ConversationID: conversationID, RunID: run.ID,
		InboundMessageID: id, Run: &run,
	})
	if err != nil {
		return nil, err
	}
	history, err := loadHistoryThroughSubmissionTx(ctx, tx, conversationID, accepted, id)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &ponsruntime.ClaimedRun{
		Conversation: ponsruntime.Conversation{ID: conversationID, Workspace: workspace, CreatedAt: createdAt},
		Message:      ponsruntime.InboundMessage{ID: id, IdempotencyKey: key, Parts: parts, AcceptedAt: acceptedAt},
		History:      history, Run: run, Events: []ponsruntime.Event{subEvent, runEvent},
	}, nil
}

func loadHistoryThroughSubmissionTx(ctx context.Context, tx *sql.Tx, conversationID string, acceptedAt int64, submissionID string) ([]ponsruntime.Message, error) {
	rows, err := tx.QueryContext(ctx, `
SELECT messages.id
FROM messages
JOIN submissions ON submissions.id = messages.inbound_message_id
WHERE messages.conversation_id = ?
	  AND (messages.role != 'assistant' OR messages.complete = TRUE)
	  AND (submissions.accepted_at < ? OR (submissions.accepted_at = ? AND submissions.id <= ?))
ORDER BY submissions.accepted_at, submissions.id, messages.created_at, messages.rowid`,
		conversationID, acceptedAt, acceptedAt, submissionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if id != submissionID {
			ids = append(ids, id)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	messages := make([]ponsruntime.Message, 0, len(ids))
	for _, id := range ids {
		message, err := loadMessageTx(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		messages = append(messages, message)
	}
	return messages, nil
}

func messageTextPartsTx(ctx context.Context, tx *sql.Tx, messageID string) ([]ponsruntime.TextPart, error) {
	rows, err := tx.QueryContext(ctx, `
SELECT type, text FROM message_parts WHERE message_id = ? ORDER BY position`, messageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var parts []ponsruntime.TextPart
	for rows.Next() {
		var part ponsruntime.TextPart
		if err := rows.Scan(&part.Type, &part.Text); err != nil {
			return nil, err
		}
		parts = append(parts, part)
	}
	return parts, rows.Err()
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
	message := ponsruntime.Message{
		ID: newID(), ConversationID: run.ConversationID, InboundMessageID: run.InboundMessageID,
		RunID: run.ID, Role: "assistant", Parts: normalizedParts, Complete: true, CreatedAt: now,
	}
	if err := insertMessageTx(ctx, tx, message); err != nil {
		return nil, err
	}
	events := make([]ponsruntime.Event, 0, 1+len(toolParts))
	messageEvent, err := updateMessageEventTx(ctx, tx, message)
	if err != nil {
		return nil, err
	}
	events = append(events, messageEvent)
	for _, part := range toolParts {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO tool_calls(id, conversation_id, run_id, inbound_message_id, message_id, kind, arguments, status, updated_at)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`, part.ToolCallID, run.ConversationID, run.ID, run.InboundMessageID,
			message.ID, part.ToolKind, []byte(part.Arguments), ponsruntime.ToolRequested, encodeTime(now)); err != nil {
			return nil, err
		}
		tool := ponsruntime.ToolCall{
			ID: part.ToolCallID, ConversationID: run.ConversationID, RunID: run.ID,
			InboundMessageID: run.InboundMessageID, Kind: part.ToolKind,
			Arguments: cloneRaw(part.Arguments), Status: ponsruntime.ToolRequested, UpdatedAt: now,
		}
		toolEvent, err := appendEventTx(ctx, tx, ponsruntime.Event{
			Type: ponsruntime.EventToolCallUpdated, ConversationID: run.ConversationID, RunID: run.ID,
			InboundMessageID: run.InboundMessageID, ToolCall: &tool,
		})
		if err != nil {
			return nil, err
		}
		events = append(events, toolEvent)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return events, nil
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
	var args []byte
	var kindText string
	if err := tx.QueryRowContext(ctx,
		`SELECT kind, arguments FROM tool_calls WHERE id = ? AND run_id = ? AND status = ?`,
		result.ActionID, run.ID, ponsruntime.ToolRequested,
	).Scan(&kindText, &args); err != nil {
		return nil, err
	}
	message := ponsruntime.Message{
		ID: newID(), ConversationID: run.ConversationID, InboundMessageID: run.InboundMessageID,
		RunID: run.ID, Role: "tool", Complete: true, CreatedAt: now,
		Parts: []ponsruntime.MessagePart{{Type: "tool_result", ToolCallID: result.ActionID, ToolKind: kindText, Result: cloneResult(result)}},
	}
	if err := insertMessageTx(ctx, tx, message); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	updated, err := tx.ExecContext(ctx, `
UPDATE tool_calls SET status = ?, result = ?, result_message_id = ?, updated_at = ?
WHERE id = ? AND run_id = ? AND status = ?`, status, encoded, message.ID, encodeTime(now),
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
	tool := ponsruntime.ToolCall{
		ID: result.ActionID, ConversationID: run.ConversationID, RunID: run.ID,
		InboundMessageID: run.InboundMessageID, Kind: kindText, Arguments: cloneRaw(args),
		Status: status, Result: cloneResult(result), UpdatedAt: now,
	}
	toolEvent, err := appendEventTx(ctx, tx, ponsruntime.Event{
		Type: ponsruntime.EventToolCallUpdated, ConversationID: run.ConversationID, RunID: run.ID,
		InboundMessageID: run.InboundMessageID, ToolCall: &tool,
	})
	if err != nil {
		return nil, err
	}
	messageEvent, err := updateMessageEventTx(ctx, tx, message)
	if err != nil {
		return nil, err
	}
	return []ponsruntime.Event{toolEvent, messageEvent}, nil
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

func (s *Store) FinishRun(ctx context.Context, run ponsruntime.Run, answer string) (ponsruntime.Message, []ponsruntime.Event, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return ponsruntime.Message{}, nil, err
	}
	defer tx.Rollback()
	now := canonicalTime(time.Now())
	message := ponsruntime.Message{
		ID: newID(), ConversationID: run.ConversationID, InboundMessageID: run.InboundMessageID,
		RunID: run.ID, Role: "assistant", Complete: true, Final: true, CreatedAt: now,
		Parts: []ponsruntime.MessagePart{{Type: "text", Text: answer}},
	}
	if err := insertMessageTx(ctx, tx, message); err != nil {
		return ponsruntime.Message{}, nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE submissions SET status = ? WHERE id = ?`, ponsruntime.RunCompleted, run.InboundMessageID); err != nil {
		return ponsruntime.Message{}, nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET status = ?, completed_at = ? WHERE id = ?`, ponsruntime.RunCompleted, encodeTime(now), run.ID); err != nil {
		return ponsruntime.Message{}, nil, err
	}
	completed := run
	completed.Status, completed.CompletedAt = ponsruntime.RunCompleted, &now
	var accepted int64
	if err := tx.QueryRowContext(ctx, `SELECT accepted_at FROM submissions WHERE id = ?`, run.InboundMessageID).Scan(&accepted); err != nil {
		return ponsruntime.Message{}, nil, err
	}
	acceptedAt := decodeTime(accepted)
	submission := ponsruntime.Submission{ID: run.InboundMessageID, ConversationID: run.ConversationID, MessageID: run.InboundMessageID, Status: ponsruntime.RunCompleted, AcceptedAt: acceptedAt}
	messageEvent, err := updateMessageEventTx(ctx, tx, message)
	if err != nil {
		return ponsruntime.Message{}, nil, err
	}
	runEvent, err := appendEventTx(ctx, tx, ponsruntime.Event{
		Type: ponsruntime.EventRunUpdated, ConversationID: run.ConversationID, RunID: run.ID,
		InboundMessageID: run.InboundMessageID, Run: &completed,
	})
	if err != nil {
		return ponsruntime.Message{}, nil, err
	}
	submissionEvent, err := appendEventTx(ctx, tx, ponsruntime.Event{
		Type: ponsruntime.EventSubmissionUpdated, ConversationID: run.ConversationID, RunID: run.ID,
		InboundMessageID: run.InboundMessageID, Submission: &submission,
	})
	if err != nil {
		return ponsruntime.Message{}, nil, err
	}
	if err := tx.Commit(); err != nil {
		return ponsruntime.Message{}, nil, err
	}
	return message, []ponsruntime.Event{messageEvent, runEvent, submissionEvent}, nil
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
	var accepted int64
	if err := tx.QueryRowContext(ctx, `SELECT accepted_at FROM submissions WHERE id = ?`, run.InboundMessageID).Scan(&accepted); err != nil {
		return nil, err
	}
	acceptedAt := decodeTime(accepted)
	submission := ponsruntime.Submission{ID: run.InboundMessageID, ConversationID: run.ConversationID, MessageID: run.InboundMessageID, Status: ponsruntime.RunFailed, Error: message, AcceptedAt: acceptedAt}
	runEvent, err := appendEventTx(ctx, tx, ponsruntime.Event{
		Type: ponsruntime.EventRunUpdated, ConversationID: run.ConversationID, RunID: run.ID,
		InboundMessageID: run.InboundMessageID, Run: &failed,
	})
	if err != nil {
		return nil, err
	}
	submissionEvent, err := appendEventTx(ctx, tx, ponsruntime.Event{
		Type: ponsruntime.EventSubmissionUpdated, ConversationID: run.ConversationID, RunID: run.ID,
		InboundMessageID: run.InboundMessageID, Submission: &submission,
	})
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return append(events, runEvent, submissionEvent), nil
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
SELECT id, conversation_id, inbound_message_id, started_at FROM runs
WHERE status = ? ORDER BY started_at`, ponsruntime.RunRunning)
	if err != nil {
		return err
	}
	type running struct {
		id, conversation, inbound string
		started                   int64
	}
	var values []running
	for rows.Next() {
		var value running
		if err := rows.Scan(&value.id, &value.conversation, &value.inbound, &value.started); err != nil {
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
		run := ponsruntime.Run{ID: value.id, ConversationID: value.conversation, InboundMessageID: value.inbound, Status: ponsruntime.RunRunning, StartedAt: started}
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
SELECT payload FROM events WHERE conversation_id = ? AND cursor > ? ORDER BY cursor`, conversationID, after)
	if err != nil {
		return nil, err
	}
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
	view := ponsruntime.ConversationView{Conversation: conversation}
	var next uint64
	if err := tx.QueryRowContext(ctx, `SELECT next_event_cursor FROM conversations WHERE id = ?`, conversationID).Scan(&next); err != nil {
		return ponsruntime.ConversationView{}, err
	}
	if next > 0 {
		view.EventCursor = next - 1
	}
	view.Messages, err = loadMessagesTx(ctx, tx, conversationID)
	if err != nil {
		return ponsruntime.ConversationView{}, err
	}
	view.Submissions, err = loadSubmissionsTx(ctx, tx, conversationID)
	if err != nil {
		return ponsruntime.ConversationView{}, err
	}
	view.ToolCalls, err = loadToolCallsTx(ctx, tx, conversationID)
	if err != nil {
		return ponsruntime.ConversationView{}, err
	}
	view.ActiveRun, err = loadActiveRunTx(ctx, tx, conversationID)
	if err != nil {
		return ponsruntime.ConversationView{}, err
	}
	if err := tx.Commit(); err != nil {
		return ponsruntime.ConversationView{}, err
	}
	return view, nil
}

func loadSubmissionsTx(ctx context.Context, tx *sql.Tx, conversationID string) ([]ponsruntime.Submission, error) {
	rows, err := tx.QueryContext(ctx, `
SELECT id, message_id, status, error, accepted_at
FROM submissions WHERE conversation_id = ? ORDER BY accepted_at, rowid`, conversationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var submissions []ponsruntime.Submission
	for rows.Next() {
		value := ponsruntime.Submission{ConversationID: conversationID}
		var accepted int64
		if err := rows.Scan(&value.ID, &value.MessageID, &value.Status, &value.Error, &accepted); err != nil {
			return nil, err
		}
		value.AcceptedAt = decodeTime(accepted)
		submissions = append(submissions, value)
	}
	return submissions, rows.Err()
}

func loadMessagesTx(ctx context.Context, tx *sql.Tx, conversationID string) ([]ponsruntime.Message, error) {
	rows, err := tx.QueryContext(ctx, `
SELECT messages.id
FROM messages
JOIN submissions ON submissions.id = messages.inbound_message_id
WHERE messages.conversation_id = ?
ORDER BY submissions.accepted_at, submissions.id, messages.created_at, messages.rowid`, conversationID)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	messages := make([]ponsruntime.Message, 0, len(ids))
	for _, id := range ids {
		message, err := loadMessageTx(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		messages = append(messages, message)
	}
	return messages, nil
}

func loadMessageTx(ctx context.Context, tx *sql.Tx, id string) (ponsruntime.Message, error) {
	var message ponsruntime.Message
	var created int64
	err := tx.QueryRowContext(ctx, `
	SELECT id, conversation_id, inbound_message_id, run_id, role, complete, final, created_at
	FROM messages WHERE id = ?`, id).Scan(&message.ID, &message.ConversationID, &message.InboundMessageID,
		&message.RunID, &message.Role, &message.Complete, &message.Final, &created)
	if err != nil {
		return ponsruntime.Message{}, err
	}
	message.CreatedAt = decodeTime(created)
	rows, err := tx.QueryContext(ctx, `
SELECT type, text, tool_call_id, tool_kind, arguments, result, metadata
FROM message_parts WHERE message_id = ? ORDER BY position`, id)
	if err != nil {
		return ponsruntime.Message{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var part ponsruntime.MessagePart
		var arguments, result, metadata []byte
		if err := rows.Scan(&part.Type, &part.Text, &part.ToolCallID, &part.ToolKind, &arguments, &result, &metadata); err != nil {
			return ponsruntime.Message{}, err
		}
		part.Arguments = cloneRaw(arguments)
		if len(result) > 0 {
			var decoded protocol.ToolResult
			if err := json.Unmarshal(result, &decoded); err != nil {
				return ponsruntime.Message{}, err
			}
			part.Result = &decoded
		}
		if len(metadata) > 0 {
			if err := json.Unmarshal(metadata, &part.Metadata); err != nil {
				return ponsruntime.Message{}, err
			}
		}
		message.Parts = append(message.Parts, part)
	}
	return message, rows.Err()
}

func loadToolCallsTx(ctx context.Context, tx *sql.Tx, conversationID string) ([]ponsruntime.ToolCall, error) {
	rows, err := tx.QueryContext(ctx, `
SELECT id, run_id, inbound_message_id, kind, arguments, status, result, updated_at
FROM tool_calls WHERE conversation_id = ? ORDER BY updated_at, rowid`, conversationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tools []ponsruntime.ToolCall
	for rows.Next() {
		tool := ponsruntime.ToolCall{ConversationID: conversationID}
		var arguments, result []byte
		var updated int64
		if err := rows.Scan(&tool.ID, &tool.RunID, &tool.InboundMessageID, &tool.Kind, &arguments,
			&tool.Status, &result, &updated); err != nil {
			return nil, err
		}
		tool.Arguments = cloneRaw(arguments)
		tool.UpdatedAt = decodeTime(updated)
		if len(result) > 0 {
			var decoded protocol.ToolResult
			if err := json.Unmarshal(result, &decoded); err != nil {
				return nil, err
			}
			tool.Result = &decoded
		}
		tools = append(tools, tool)
	}
	return tools, rows.Err()
}

func loadActiveRunTx(ctx context.Context, tx *sql.Tx, conversationID string) (*ponsruntime.Run, error) {
	var run ponsruntime.Run
	var started int64
	err := tx.QueryRowContext(ctx, `
SELECT id, inbound_message_id, status, error, started_at
FROM runs WHERE conversation_id = ? AND status = ? ORDER BY started_at DESC LIMIT 1`, conversationID, ponsruntime.RunRunning,
	).Scan(&run.ID, &run.InboundMessageID, &run.Status, &run.Error, &started)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	run.ConversationID = conversationID
	run.StartedAt = decodeTime(started)
	return &run, nil
}

func cloneMessage(message ponsruntime.Message) ponsruntime.Message {
	copy := message
	copy.Parts = make([]ponsruntime.MessagePart, len(message.Parts))
	for i, part := range message.Parts {
		copy.Parts[i] = part
		copy.Parts[i].Arguments = cloneRaw(part.Arguments)
		copy.Parts[i].Metadata = cloneMetadata(part.Metadata)
		if part.Result != nil {
			copy.Parts[i].Result = cloneResult(*part.Result)
		}
	}
	return copy
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
