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
	Accept(context.Context, string, string, []TextPart) (AcceptedMessage, []Event, error)
	// ClaimRunnable atomically selects and claims the oldest queued submission
	// whose conversation and workspace have no active run. It returns nil when
	// no work is currently eligible. This transition is not, by itself, a
	// renewable lease or distributed fencing contract.
	ClaimRunnable(context.Context) (*ClaimedRun, error)
	CommitAssistantTurn(context.Context, Run, []MessagePart) ([]Event, error)
	ToolCompleted(context.Context, Run, protocol.ToolResult) ([]Event, error)
	FinishRun(context.Context, Run, string) (Message, []Event, error)
	// FailRun atomically resolves every still-requested tool as interrupted and
	// marks the run and submission failed. It must not strand requested tools
	// behind a terminal run if any part of that transition fails.
	FailRun(context.Context, Run, string) ([]Event, error)
	// RecoverRunning resolves work abandoned by the previous exclusive store
	// owner. Distributed implementations must recover only expired fenced
	// claims; SQLite supports one Manager and calls this during startup.
	RecoverRunning(context.Context) error
	Events(context.Context, string, uint64) ([]Event, error)
	View(context.Context, string) (ConversationView, error)
}

type ClaimedRun struct {
	Conversation Conversation
	Message      InboundMessage
	History      []Message
	Run          Run
	Events       []Event
}
