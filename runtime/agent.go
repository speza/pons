package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// DefaultAgentID identifies the one agent every server creates on first start.
const DefaultAgentID = "default"

var ErrInvalidAgent = errors.New("runtime: invalid agent definition")

// Workspace policies choose where an agent's conversations work.
const (
	// WorkspaceAgent gives every conversation the agent owns one shared
	// workspace that outlives any single conversation.
	WorkspaceAgent = "agent"
	// WorkspacePerConversation lets the client choose a host directory or Git
	// repository for each new conversation.
	WorkspacePerConversation = "per_conversation"
)

// AgentWorkspaceID is the logical workspace every conversation of an agent
// shares under the agent policy. Conversation IDs are hex, so it never
// collides with a per-conversation workspace ID.
func AgentWorkspaceID(agentID string) string {
	return "agent-" + agentID
}

// AgentDefinition is the non-secret configuration a run depends on. Provider
// credentials are referenced by slot ID and never enter the definition, so
// its revision is safe to persist and display.
type AgentDefinition struct {
	ID string `json:"id"`
	// Name is the owner-set display name. It is a structured field, never
	// parsed from the persona text.
	Name         string   `json:"name"`
	Persona      string   `json:"persona"`
	ProviderSlot string   `json:"provider_slot"`
	Model        string   `json:"model"`
	MaxTurns     int      `json:"max_turns"`
	PluginPaths  []string `json:"plugin_paths"`
	// WorkspacePolicy is WorkspaceAgent or WorkspacePerConversation. It
	// applies when a conversation is created; the conversation then keeps
	// the workspace it was given.
	WorkspacePolicy string `json:"workspace_policy"`
}

// AgentSummary is the read-only agent identity shown to clients.
type AgentSummary struct {
	ID              string `json:"id"`
	Name            string `json:"name,omitempty"`
	WorkspacePolicy string `json:"workspace_policy"`
}

// ValidateAgentID rejects IDs that cannot name an agent directory.
func ValidateAgentID(id string) error {
	if id == "" || strings.ContainsAny(id, `/\`) || id != strings.TrimSpace(id) {
		return fmt.Errorf("%w: id %q", ErrInvalidAgent, id)
	}
	return nil
}

func (d AgentDefinition) Validate() error {
	if err := ValidateAgentID(d.ID); err != nil {
		return err
	}
	if d.Name != strings.TrimSpace(d.Name) || strings.ContainsAny(d.Name, "\r\n") {
		return fmt.Errorf("%w: name must be a single trimmed line", ErrInvalidAgent)
	}
	if d.WorkspacePolicy != WorkspaceAgent && d.WorkspacePolicy != WorkspacePerConversation {
		return fmt.Errorf("%w: workspace policy %q", ErrInvalidAgent, d.WorkspacePolicy)
	}
	return nil
}

// Revision returns the SHA-256 fingerprint of the definition's canonical JSON.
// Struct field order fixes the encoding; a nil plugin list encodes like an
// empty one so equivalent definitions share a revision.
func (d AgentDefinition) Revision() string {
	canonical := d
	if canonical.PluginPaths == nil {
		canonical.PluginPaths = []string{}
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		panic(fmt.Sprintf("runtime: encode agent definition: %v", err))
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// AgentRevisions resolves immutable agent definition revisions recorded by
// the composition. Implementations verify that the returned definition's
// ID and Revision match the request.
type AgentRevisions interface {
	AgentRevision(ctx context.Context, agentID, revision string) (AgentDefinition, error)
}

func (d AgentDefinition) Summary() AgentSummary {
	return AgentSummary{ID: d.ID, Name: d.Name, WorkspacePolicy: d.WorkspacePolicy}
}
