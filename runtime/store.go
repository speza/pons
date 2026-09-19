package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"

	"github.com/samperrin/pons/protocol"
)

var ErrNotFound = errors.New("runtime: conversation not found")

// NewID returns a random opaque identifier suitable for runtime entities.
func NewID() string {
	value := make([]byte, 8)
	if _, err := rand.Read(value); err != nil {
		panic(err)
	}
	return hex.EncodeToString(value)
}

// Store is the authoritative persistence boundary consumed by the server runtime.
// Implementations commit each returned event batch atomically with the state
// mutation that produced it.
type Store interface {
	CreateConversation(context.Context, Conversation) error
	Conversation(context.Context, string) (Conversation, error)
	ConversationIDs(context.Context) ([]string, error)
	Accept(context.Context, string, string, []TextPart) (AcceptedMessage, []Event, error)
	ClaimNext(context.Context, string) (*ClaimedRun, error)
	CommitAssistantTurn(context.Context, Run, []MessagePart) ([]Event, error)
	ToolCompleted(context.Context, Run, protocol.ToolResult) ([]Event, error)
	FinishRun(context.Context, Run, string) (Message, []Event, error)
	FailRun(context.Context, Run, string) ([]Event, error)
	HasPending(context.Context, string) (bool, error)
	RecoverRunning(context.Context, string) error
	InterruptRequestedTools(context.Context, Run) ([]Event, error)
	Events(context.Context, string, uint64) ([]Event, error)
	View(context.Context, string) (ConversationView, error)
}

type ClaimedRun struct {
	Message InboundMessage
	History []Message
	Run     Run
	Events  []Event
}
