// Package runtime implements long-lived orchestration above the finite pons
// Core. It owns conversations, durable inbox state, run scheduling, events,
// and delivery; agent construction remains an injected composition policy.
package runtime

import (
	"context"
	"encoding/json"
	"time"

	"github.com/samperrin/pons/protocol"
)

const (
	EventMessageUpserted     = "message.upserted"
	EventSubmissionUpdated   = "submission.updated"
	EventToolCallUpdated     = "tool_call.updated"
	EventRunUpdated          = "run.updated"
	EventConversationUpdated = "conversation.updated"
	EventEnvironmentProgress = "environment.progress"

	// Transient events are delivered live but never persisted and never advance
	// the durable reconnect cursor.
	EventAssistantDelta = "assistant.delta"
	EventToolProgress   = "tool.progress"
	EventHeartbeat      = "heartbeat"

	// Runner events are internal signals translated by Manager into the public
	// entity-shaped event contract above.
	RunEventAssistantTurn = "runner.assistant.turn"
	RunEventToolCompleted = "runner.tool.completed"

	RunQueued    = "queued"
	RunRunning   = "running"
	RunCompleted = "completed"
	RunFailed    = "failed"

	ToolRequested   = "requested"
	ToolCompleted   = "completed"
	ToolFailed      = "failed"
	ToolInterrupted = "interrupted"
)

type TextPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type InboundMessage struct {
	ID             string     `json:"id"`
	IdempotencyKey string     `json:"idempotency_key"`
	Parts          []TextPart `json:"parts"`
	AcceptedAt     time.Time  `json:"accepted_at"`
}

// Message is the provider-neutral semantic record rendered by clients and
// projected into provider context. Complete messages are durable; streamed
// assistant deltas are not messages until the provider response completes.
type Message struct {
	ID               string        `json:"id"`
	ConversationID   string        `json:"conversation_id"`
	InboundMessageID string        `json:"inbound_message_id,omitempty"`
	RunID            string        `json:"run_id,omitempty"`
	Role             string        `json:"role"`
	Parts            []MessagePart `json:"parts"`
	Complete         bool          `json:"complete"`
	Final            bool          `json:"final,omitempty"`
	CreatedAt        time.Time     `json:"created_at"`
}

type MessagePart struct {
	Type       string                     `json:"type"`
	Text       string                     `json:"text,omitempty"`
	ToolCallID string                     `json:"tool_call_id,omitempty"`
	ToolKind   string                     `json:"tool_kind,omitempty"`
	Arguments  json.RawMessage            `json:"arguments,omitempty"`
	Result     *protocol.ToolResult       `json:"result,omitempty"`
	Metadata   map[string]json.RawMessage `json:"metadata,omitempty"`
}

type Run struct {
	ID               string     `json:"id"`
	ConversationID   string     `json:"conversation_id"`
	InboundMessageID string     `json:"inbound_message_id"`
	AgentRevision    string     `json:"agent_revision"`
	Status           string     `json:"status"`
	Error            string     `json:"error,omitempty"`
	StartedAt        time.Time  `json:"started_at"`
	CompletedAt      *time.Time `json:"completed_at,omitempty"`
}

type ToolCall struct {
	ID               string               `json:"id"`
	ConversationID   string               `json:"conversation_id"`
	RunID            string               `json:"run_id"`
	InboundMessageID string               `json:"inbound_message_id"`
	Kind             string               `json:"kind"`
	Arguments        json.RawMessage      `json:"arguments"`
	Status           string               `json:"status"`
	Result           *protocol.ToolResult `json:"result,omitempty"`
	UpdatedAt        time.Time            `json:"updated_at"`
}

type Submission struct {
	ID             string    `json:"id"`
	ConversationID string    `json:"conversation_id"`
	MessageID      string    `json:"message_id"`
	AgentRevision  string    `json:"agent_revision"`
	Status         string    `json:"status"`
	Error          string    `json:"error,omitempty"`
	AcceptedAt     time.Time `json:"accepted_at"`
}

type ConversationView struct {
	Conversation      Conversation `json:"conversation"`
	Agent             AgentSummary `json:"agent"`
	Messages          []Message    `json:"messages"`
	Submissions       []Submission `json:"submissions,omitempty"`
	ActiveRun         *Run         `json:"active_run,omitempty"`
	ToolCalls         []ToolCall   `json:"tool_calls,omitempty"`
	EnvironmentEvents []Event      `json:"environment_events,omitempty"`
	EventCursor       uint64       `json:"event_cursor"`
}

// Event is either a durable entity update (ID > 0) or a transient live event
// (ID == 0). Durable events are replayable after ID; transient events never
// advance Last-Event-ID.
type Event struct {
	ID                  uint64                     `json:"cursor,omitempty"`
	Type                string                     `json:"type"`
	ConversationID      string                     `json:"conversation_id"`
	RunID               string                     `json:"run_id,omitempty"`
	InboundMessageID    string                     `json:"inbound_message_id,omitempty"`
	Message             *Message                   `json:"message,omitempty"`
	Submission          *Submission                `json:"submission,omitempty"`
	ToolCall            *ToolCall                  `json:"tool_call,omitempty"`
	Run                 *Run                       `json:"run,omitempty"`
	Delta               *TextDelta                 `json:"delta,omitempty"`
	Progress            *ToolProgress              `json:"progress,omitempty"`
	EnvironmentProgress *EnvironmentProgress       `json:"environment_progress,omitempty"`
	Metadata            map[string]json.RawMessage `json:"metadata,omitempty"`
	CreatedAt           time.Time                  `json:"created_at"`
}

type TextDelta struct {
	MessageID string `json:"message_id"`
	PartID    string `json:"part_id"`
	Text      string `json:"text"`
}

type ToolProgress struct {
	ToolCallID string `json:"tool_call_id"`
	Text       string `json:"text,omitempty"`
}

type EnvironmentProgress struct {
	Step    string `json:"step"`
	Message string `json:"message"`
}

type RunRequest struct {
	// Agent is the definition revision the submission was accepted under. The
	// runner composes the brain from it rather than from current settings.
	Agent              AgentDefinition
	ConversationID     string
	RunID              string
	InboundMessageID   string
	Workspace          string
	Environment        string
	GitRepository      string
	GitRevision        string
	GitAllRepositories bool
	Text               string
	Messages           []Message
	Emit               func(RunEvent) error
}

type RunEvent struct {
	Type       string
	Step       string
	Message    string
	Text       string
	MessageID  string
	PartID     string
	ToolCallID string
	Action     *protocol.Action
	Result     *protocol.ToolResult
	Parts      []MessagePart
}

type RunResult struct {
	Answer string
}

// Runner executes one active agent burst. Implementations construct fresh
// brain/core/environment state and release it before returning.
type Runner interface {
	Run(context.Context, RunRequest) (RunResult, error)
}

type RunnerFunc func(context.Context, RunRequest) (RunResult, error)

func (f RunnerFunc) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	return f(ctx, req)
}

type Conversation struct {
	ID                 string    `json:"conversation_id"`
	AgentID            string    `json:"agent_id"`
	Workspace          string    `json:"workspace"`
	WorkspaceLock      string    `json:"-"`
	Environment        string    `json:"environment,omitempty"`
	GitRepository      string    `json:"git_repository,omitempty"`
	GitRevision        string    `json:"git_revision,omitempty"`
	GitAllRepositories bool      `json:"git_all_repositories,omitempty"`
	CreatedAt          time.Time `json:"created_at"`
}

type ConversationOptions struct {
	Workspace          string `json:"workspace,omitempty"`
	Environment        string `json:"environment,omitempty"`
	GitRepository      string `json:"git_repository,omitempty"`
	GitRevision        string `json:"git_revision,omitempty"`
	GitAllRepositories bool   `json:"git_all_repositories,omitempty"`
}

type AcceptedMessage struct {
	ConversationID   string `json:"conversation_id"`
	InboundMessageID string `json:"inbound_message_id"`
	Duplicate        bool   `json:"duplicate,omitempty"`
}

func cloneRaw[T ~[]byte](value T) T {
	return append(T(nil), value...)
}
