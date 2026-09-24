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
}

// AgentSummary is the read-only agent identity shown to clients.
type AgentSummary struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

func (d AgentDefinition) Validate() error {
	if d.ID == "" || strings.ContainsAny(d.ID, `/\`) || d.ID != strings.TrimSpace(d.ID) {
		return fmt.Errorf("%w: id %q", ErrInvalidAgent, d.ID)
	}
	if d.Name != strings.TrimSpace(d.Name) || strings.ContainsAny(d.Name, "\r\n") {
		return fmt.Errorf("%w: name must be a single trimmed line", ErrInvalidAgent)
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
	return AgentSummary{ID: d.ID, Name: d.Name}
}
