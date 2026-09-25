package runtime

import (
	"errors"
	"fmt"
)

// ProjectView builds the public snapshot from the ordered public event stream.
// Operational indexes are deliberately absent from this projection.
func ProjectView(conversation Conversation, events []Event) (ConversationView, error) {
	view := ConversationView{Conversation: conversation}
	submissions := make(map[string]int)
	type toolKey struct{ runID, callID string }
	tools := make(map[toolKey]int)

	for _, event := range events {
		if event.ConversationID != conversation.ID {
			return ConversationView{}, errors.New("runtime: event belongs to another conversation")
		}
		if event.ID > view.EventCursor {
			view.EventCursor = event.ID
		}

		switch event.Type {
		case EventInputAccepted, EventAssistantCommitted, EventToolOutcomeRecorded:
			message, err := event.ProjectMessage()
			if err != nil {
				return ConversationView{}, err
			}
			view.Messages = append(view.Messages, message)
			switch event.Type {
			case EventInputAccepted:
				if event.Input.IdempotencyKey == "" {
					return ConversationView{}, errors.New("runtime: accepted input has no idempotency key")
				}
				if _, exists := submissions[event.InboundMessageID]; exists {
					return ConversationView{}, fmt.Errorf("runtime: input %q accepted twice", event.InboundMessageID)
				}
				submissions[event.InboundMessageID] = len(view.Submissions)
				view.Submissions = append(view.Submissions, Submission{
					ID:             event.InboundMessageID,
					ConversationID: conversation.ID,
					MessageID:      event.InboundMessageID,
					AgentRevision:  event.Input.AgentRevision,
					Status:         RunQueued,
					AcceptedAt:     event.CreatedAt,
				})
			case EventAssistantCommitted:
				for _, part := range event.AssistantOutput.Parts {
					if part.Type != "tool_call" {
						continue
					}
					key := toolKey{event.RunID, part.ToolCallID}
					if part.ToolCallID == "" || event.RunID == "" {
						return ConversationView{}, errors.New("runtime: tool request has no identity")
					}
					if _, exists := tools[key]; exists {
						return ConversationView{}, fmt.Errorf("runtime: tool call %q recorded twice", part.ToolCallID)
					}
					tools[key] = len(view.ToolCalls)
					view.ToolCalls = append(view.ToolCalls, ToolCall{
						ID:               part.ToolCallID,
						ConversationID:   conversation.ID,
						RunID:            event.RunID,
						InboundMessageID: event.InboundMessageID,
						Kind:             part.ToolKind,
						Arguments:        append([]byte(nil), part.Arguments...),
						Status:           ToolRequested,
						UpdatedAt:        event.CreatedAt,
					})
				}
			case EventToolOutcomeRecorded:
				key := toolKey{event.RunID, event.ToolOutcome.ToolCallID}
				index, ok := tools[key]
				if !ok {
					return ConversationView{}, fmt.Errorf("runtime: tool outcome %q has no request", key.callID)
				}
				view.ToolCalls[index].Status = event.ToolOutcome.Status
				view.ToolCalls[index].Result = event.ToolOutcome.Result
				view.ToolCalls[index].UpdatedAt = event.CreatedAt
			}
		case EventRunStarted, EventRunCompleted, EventRunFailed:
			if event.Run == nil {
				return ConversationView{}, errors.New("runtime: run event has no run")
			}
			index, ok := submissions[event.Run.InboundMessageID]
			if !ok {
				return ConversationView{}, fmt.Errorf("runtime: run %q has no accepted input", event.Run.ID)
			}
			view.Submissions[index].Status = event.Run.Status
			view.Submissions[index].Error = event.Run.Error
			if event.Type == EventRunStarted {
				view.ActiveRun = event.Run
			} else {
				view.ActiveRun = nil
			}
		case EventEnvironmentProgress:
			if event.EnvironmentProgress != nil {
				view.EnvironmentEvents = append(view.EnvironmentEvents, event)
			}
		}
	}
	return view, nil
}
