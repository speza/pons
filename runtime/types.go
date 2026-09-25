// Package runtime implements long-lived orchestration above the finite pons
// Core. It owns conversations, durable inbox state, run scheduling, events,
// and delivery; agent construction remains an injected composition policy.
package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/samperrin/pons/protocol"
)

const (
	EventInputAccepted       = "input.accepted"
	EventInputAdmitted       = "input.admitted"
	EventAssistantCommitted  = "assistant.output.committed"
	EventToolOutcomeRecorded = "tool.outcome.recorded"
	EventRunStarted          = "run.started"
	EventRunCompleted        = "run.completed"
	EventRunFailed           = "run.failed"
	EventEnvironmentProgress = "environment.progress"

	// Transient events are delivered live but never persisted and never advance
	// the durable reconnect cursor.
	EventAssistantDelta = "assistant.delta"
	EventToolProgress   = "tool.progress"
	EventHeartbeat      = "heartbeat"

	// Runner events are internal signals translated by Manager into durable facts.
	RunEventAssistantTurn = "runner.assistant.turn"
	RunEventToolCompleted = "runner.tool.completed"
	RunEventAgentEvent    = "runner.agent.event"

	EventAgentStarted         = "agent.started"
	EventAgentFinished        = "agent.finished"
	EventAgentStopped         = "agent.stopped"
	EventAgentExhausted       = "agent.exhausted"
	EventIterationStarted     = "agent.iteration.started"
	EventIterationCompleted   = "agent.iteration.completed"
	EventModelCompleted       = "agent.model.completed"
	EventToolExecutionStarted = "agent.tool.execution.started"
	EventContextCompacted     = "agent.context.compacted"
	EventInputPrepared        = "agent.input.prepared"

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

// Message is a provider-neutral client projection of content events. It is not
// persisted directly; streamed assistant deltas enter the projection only
// when an assistant output commits.
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

// InputSource identifies the trusted ingress that accepted an input. The
// HTTP transport always uses a human source; internal producers supply their
// own source rather than trusting a client-provided value.
type InputSource struct {
	Kind                 string `json:"kind"` // human, system, or agent
	Adapter              string `json:"adapter"`
	PrincipalID          string `json:"principal_id,omitempty"`
	SourceConversationID string `json:"source_conversation_id,omitempty"`
	SourceAgentID        string `json:"source_agent_id,omitempty"`
}

// InputSubmission is the ingress command. Its fields become one immutable
// input.accepted fact; the submissions table is only a claimable index.
type InputSubmission struct {
	IdempotencyKey string      `json:"idempotency_key"`
	Parts          []TextPart  `json:"parts"`
	Source         InputSource `json:"source"`
	CausationID    string      `json:"causation_id,omitempty"`
	TargetAgentID  string      `json:"target_agent_id,omitempty"`
	AgentRevision  string      `json:"agent_revision,omitempty"`
}

// These payloads record facts. Message is a client projection of them.
type UserInput struct {
	InputSubmission
}

type AssistantOutput struct {
	ID    string        `json:"id"`
	Parts []MessagePart `json:"parts"`
	Final bool          `json:"final,omitempty"`
}

type ToolOutcome struct {
	ID         string               `json:"id"`
	ToolCallID string               `json:"tool_call_id"`
	ToolKind   string               `json:"tool_kind"`
	Status     string               `json:"status"`
	Result     *protocol.ToolResult `json:"result"`
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

// Event is either an immutable event log entry (ID > 0) or a transient live
// event (ID == 0). Durable entries are replayable after ID; transient events
// never advance Last-Event-ID.
type Event struct {
	ID                  uint64                     `json:"cursor,omitempty"`
	Type                string                     `json:"type"`
	ConversationID      string                     `json:"conversation_id"`
	RunID               string                     `json:"run_id,omitempty"`
	InboundMessageID    string                     `json:"inbound_message_id,omitempty"`
	Input               *UserInput                 `json:"input,omitempty"`
	AssistantOutput     *AssistantOutput           `json:"assistant_output,omitempty"`
	ToolOutcome         *ToolOutcome               `json:"tool_outcome,omitempty"`
	Run                 *Run                       `json:"run,omitempty"`
	Delta               *TextDelta                 `json:"delta,omitempty"`
	Progress            *ToolProgress              `json:"progress,omitempty"`
	EnvironmentProgress *EnvironmentProgress       `json:"environment_progress,omitempty"`
	AgentBoundary       *AgentBoundary             `json:"agent_boundary,omitempty"`
	Iteration           *IterationEvent            `json:"iteration,omitempty"`
	ModelOutput         *ModelOutput               `json:"model_output,omitempty"`
	ToolExecution       *ToolExecution             `json:"tool_execution,omitempty"`
	Compaction          *ContextCompaction         `json:"compaction,omitempty"`
	PreparedInput       *ContextTurn               `json:"prepared_input,omitempty"`
	Metadata            map[string]json.RawMessage `json:"metadata,omitempty"`
	CreatedAt           time.Time                  `json:"created_at"`
}

// ProjectMessage derives the client-facing message for a content event.
func (e Event) ProjectMessage() (Message, error) {
	message := Message{
		ConversationID: e.ConversationID, InboundMessageID: e.InboundMessageID,
		RunID: e.RunID, Complete: true, CreatedAt: e.CreatedAt,
	}
	switch e.Type {
	case EventInputAccepted:
		if e.Input == nil || e.InboundMessageID == "" {
			return Message{}, errors.New("runtime: input event has no input")
		}
		message.ID, message.Role = e.InboundMessageID, "user"
		for _, part := range e.Input.Parts {
			message.Parts = append(message.Parts, MessagePart{Type: part.Type, Text: part.Text})
		}
	case EventAssistantCommitted:
		if e.AssistantOutput == nil || e.AssistantOutput.ID == "" {
			return Message{}, errors.New("runtime: assistant event has no output")
		}
		message.ID, message.Role = e.AssistantOutput.ID, "assistant"
		message.Parts, message.Final = e.AssistantOutput.Parts, e.AssistantOutput.Final
	case EventToolOutcomeRecorded:
		if e.ToolOutcome == nil || e.ToolOutcome.ID == "" || e.ToolOutcome.Result == nil {
			return Message{}, errors.New("runtime: tool outcome event has no result")
		}
		message.ID, message.Role = e.ToolOutcome.ID, "tool"
		message.Parts = []MessagePart{{
			Type: "tool_result", ToolCallID: e.ToolOutcome.ToolCallID,
			ToolKind: e.ToolOutcome.ToolKind, Result: e.ToolOutcome.Result,
		}}
	default:
		return Message{}, errors.New("runtime: event does not project a message")
	}
	return message, nil
}

// These payloads belong to distinct private event types in the durable log.
// Clients never receive agent.* events. Model content preserves provider items
// such as opaque encrypted reasoning when a provider returns them.
type AgentBoundary struct {
	Turn int    `json:"turn,omitempty"`
	Text string `json:"text,omitempty"`
}

type IterationEvent struct {
	Turn int `json:"turn"`
}

type ModelOutput struct {
	Turn    int            `json:"turn"`
	Content []AgentContent `json:"content"`
}

type ToolExecution struct {
	Turn       int    `json:"turn"`
	ToolCallID string `json:"tool_call_id"`
	ToolKind   string `json:"tool_kind"`
}

type ContextCompaction struct {
	Turn           int           `json:"turn"`
	Summary        string        `json:"summary"`
	CompactedTurns int           `json:"compacted_turns"`
	RetainedTurns  int           `json:"retained_turns"`
	Context        []ContextTurn `json:"context"`
}

type AgentContent struct {
	Type       string          `json:"type"`
	Text       string          `json:"text,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	ToolKind   string          `json:"tool_kind,omitempty"`
	Arguments  json.RawMessage `json:"arguments,omitempty"`
	IsError    bool            `json:"is_error,omitempty"`
	Item       json.RawMessage `json:"item,omitempty"`
}

type ContextTurn struct {
	Role    string         `json:"role"`
	Content []AgentContent `json:"content"`
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
	Context            []ContextTurn
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
	AgentEvent *Event
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
