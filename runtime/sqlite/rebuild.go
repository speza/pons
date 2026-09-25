package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	ponsruntime "github.com/samperrin/pons/runtime"
)

// rebuildIndexes reconstructs claim and recovery state from the durable log.
// Open runs it before a Manager can claim work. A failed replay rolls back the
// entire replacement, leaving the previous indexes intact for inspection.
func (s *Store) rebuildIndexes(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, table := range []string{"tool_calls", "runs", "submissions"} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table); err != nil {
			return fmt.Errorf("clear %s: %w", table, err)
		}
	}

	rows, err := tx.QueryContext(ctx, `
SELECT conversation_id, cursor, type, payload FROM events
WHERE type IN (?, ?, ?, ?, ?, ?, ?)
ORDER BY conversation_id, cursor`,
		ponsruntime.EventInputAccepted,
		ponsruntime.EventInputAdmitted,
		ponsruntime.EventRunStarted,
		ponsruntime.EventRunCompleted,
		ponsruntime.EventRunFailed,
		ponsruntime.EventAssistantCommitted,
		ponsruntime.EventToolOutcomeRecorded,
	)
	if err != nil {
		return err
	}
	defer rows.Close()

	admitted := make(map[inputKey]string)
	for rows.Next() {
		var conversationID, eventType string
		var cursor uint64
		var payload []byte
		if err := rows.Scan(&conversationID, &cursor, &eventType, &payload); err != nil {
			return err
		}
		var event ponsruntime.Event
		if err := json.Unmarshal(payload, &event); err != nil {
			return fmt.Errorf("decode event: %w", err)
		}
		if event.ConversationID != conversationID || event.ID != cursor || event.Type != eventType {
			return fmt.Errorf("event %s/%d payload does not match its log position", conversationID, cursor)
		}
		if err := replayIndexEvent(ctx, tx, event, admitted); err != nil {
			return fmt.Errorf("event %s/%d (%s): %w", event.ConversationID, event.ID, event.Type, err)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(admitted) != 0 {
		return errors.New("input admission has no matching run start")
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE conversations SET next_event_cursor = COALESCE(
  (SELECT MAX(cursor) + 1 FROM events WHERE events.conversation_id = conversations.id), 1)`); err != nil {
		return err
	}
	return tx.Commit()
}

type inputKey struct{ conversationID, inputID string }

func replayIndexEvent(ctx context.Context, tx *sql.Tx, event ponsruntime.Event, admitted map[inputKey]string) error {
	switch event.Type {
	case ponsruntime.EventInputAccepted:
		if event.Input == nil || event.InboundMessageID == "" {
			return errors.New("accepted input has no identity or payload")
		}
		if err := ponsruntime.ValidateInputSubmission(event.Input.InputSubmission); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
INSERT INTO submissions(id, conversation_id, idempotency_key, message_id, agent_revision, status, accepted_at)
VALUES(?, ?, ?, ?, ?, ?, ?)`, event.InboundMessageID, event.ConversationID,
			event.Input.IdempotencyKey, event.InboundMessageID, event.Input.AgentRevision,
			ponsruntime.RunQueued, encodeTime(event.CreatedAt))
		return err

	case ponsruntime.EventInputAdmitted:
		if event.RunID == "" || event.InboundMessageID == "" {
			return errors.New("admission has no run or input")
		}
		key := inputKey{event.ConversationID, event.InboundMessageID}
		if admitted[key] != "" {
			return errors.New("input admitted twice")
		}
		admitted[key] = event.RunID
		return nil

	case ponsruntime.EventRunStarted:
		run := event.Run
		if run == nil || run.ID != event.RunID || run.ConversationID != event.ConversationID ||
			run.InboundMessageID != event.InboundMessageID || run.Status != ponsruntime.RunRunning {
			return errors.New("invalid run start")
		}
		key := inputKey{event.ConversationID, run.InboundMessageID}
		if admitted[key] != run.ID {
			return errors.New("run start has no matching admission")
		}
		delete(admitted, key)
		if err := updateOne(ctx, tx, `
UPDATE submissions SET status = ? WHERE id = ? AND conversation_id = ? AND agent_revision = ? AND status = ?`,
			ponsruntime.RunRunning, run.InboundMessageID, event.ConversationID,
			run.AgentRevision, ponsruntime.RunQueued); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
INSERT INTO runs(id, conversation_id, submission_id, inbound_message_id, agent_revision, status, started_at)
VALUES(?, ?, ?, ?, ?, ?, ?)`, run.ID, event.ConversationID, run.InboundMessageID,
			run.InboundMessageID, run.AgentRevision, run.Status, encodeTime(run.StartedAt))
		return err

	case ponsruntime.EventAssistantCommitted:
		if event.AssistantOutput == nil {
			return errors.New("assistant output has no payload")
		}
		for _, part := range event.AssistantOutput.Parts {
			if part.Type != "tool_call" {
				continue
			}
			if part.ToolCallID == "" || part.ToolKind == "" || event.RunID == "" {
				return errors.New("tool request has no identity or kind")
			}
			if _, err := tx.ExecContext(ctx, `
INSERT INTO tool_calls(id, conversation_id, run_id, inbound_message_id, kind, status, updated_at)
VALUES(?, ?, ?, ?, ?, ?, ?)`, part.ToolCallID, event.ConversationID, event.RunID,
				event.InboundMessageID, part.ToolKind, ponsruntime.ToolRequested, encodeTime(event.CreatedAt)); err != nil {
				return err
			}
		}
		return nil

	case ponsruntime.EventToolOutcomeRecorded:
		outcome := event.ToolOutcome
		if outcome == nil || outcome.ToolCallID == "" || outcome.Result == nil ||
			(outcome.Status != ponsruntime.ToolCompleted && outcome.Status != ponsruntime.ToolFailed && outcome.Status != ponsruntime.ToolInterrupted) {
			return errors.New("invalid tool outcome")
		}
		return updateOne(ctx, tx, `
UPDATE tool_calls SET status = ?, updated_at = ?
WHERE conversation_id = ? AND run_id = ? AND id = ? AND kind = ? AND status = ?`,
			outcome.Status, encodeTime(event.CreatedAt), event.ConversationID, event.RunID,
			outcome.ToolCallID, outcome.ToolKind, ponsruntime.ToolRequested)

	case ponsruntime.EventRunCompleted, ponsruntime.EventRunFailed:
		run := event.Run
		status := ponsruntime.RunCompleted
		if event.Type == ponsruntime.EventRunFailed {
			status = ponsruntime.RunFailed
		}
		if run == nil || run.ID != event.RunID || run.ConversationID != event.ConversationID ||
			run.InboundMessageID != event.InboundMessageID || run.Status != status || run.CompletedAt == nil {
			return errors.New("invalid terminal run")
		}
		if err := updateOne(ctx, tx, `
UPDATE runs SET status = ?, error = ?, completed_at = ?
WHERE id = ? AND conversation_id = ? AND status = ?`,
			status, run.Error, encodeTime(*run.CompletedAt), run.ID, event.ConversationID, ponsruntime.RunRunning); err != nil {
			return err
		}
		return updateOne(ctx, tx, `
UPDATE submissions SET status = ?, error = ?
WHERE id = ? AND conversation_id = ? AND status = ?`,
			status, run.Error, run.InboundMessageID, event.ConversationID, ponsruntime.RunRunning)
	}
	return nil
}

func updateOne(ctx context.Context, tx *sql.Tx, query string, args ...any) error {
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return fmt.Errorf("expected one index row, updated %d", count)
	}
	return nil
}
