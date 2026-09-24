// Package agentdir keeps owner-authored agent definitions as files under the
// runtime state directory:
//
//	agents/<id>/agent.json         settings: name, provider slot, model, limits
//	agents/<id>/PERSONA.md         free-form persona instructions
//	agents/<id>/revisions/<sha>.json  immutable snapshots of resolved definitions
//
// The owner edits agent.json and PERSONA.md; the runtime only reads them at
// startup. Revision snapshots are content-addressed and write-once, so work
// accepted under a revision can still resolve it after the files change.
package agentdir

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	ponsruntime "github.com/samperrin/pons/runtime"
)

const (
	SettingsFile = "agent.json"
	PersonaFile  = "PERSONA.md"
	revisionsDir = "revisions"
)

// Settings is the owner-authored agent.json. Empty fields fall back to the
// server's defaults when the definition is composed.
type Settings struct {
	Name     string `json:"name"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	MaxTurns int    `json:"max_turns"`
}

// Store reads agent directories and records their revisions.
type Store struct {
	root string
}

var _ ponsruntime.AgentRevisions = (*Store)(nil)

// Open prepares the agents directory under stateDir.
func Open(stateDir string) (*Store, error) {
	root := filepath.Join(stateDir, "agents")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("agents: create directory: %w", err)
	}
	return &Store{root: root}, nil
}

// Dir returns the directory holding an agent's files.
func (s *Store) Dir(agentID string) string {
	return filepath.Join(s.root, agentID)
}

// Load returns an agent's settings and trimmed persona. On first start it
// creates the directory with a default agent.json and an empty PERSONA.md;
// existing files are never overwritten.
func (s *Store) Load(agentID string) (Settings, string, error) {
	if err := (ponsruntime.AgentDefinition{ID: agentID}).Validate(); err != nil {
		return Settings{}, "", err
	}
	dir := s.Dir(agentID)
	if err := os.MkdirAll(filepath.Join(dir, revisionsDir), 0o700); err != nil {
		return Settings{}, "", fmt.Errorf("agents: create %s: %w", dir, err)
	}

	defaults, err := json.MarshalIndent(Settings{}, "", "  ")
	if err != nil {
		return Settings{}, "", err
	}
	settingsPath := filepath.Join(dir, SettingsFile)
	personaPath := filepath.Join(dir, PersonaFile)
	if err := createIfMissing(settingsPath, append(defaults, '\n')); err != nil {
		return Settings{}, "", err
	}
	if err := createIfMissing(personaPath, nil); err != nil {
		return Settings{}, "", err
	}

	settings, err := readSettings(settingsPath)
	if err != nil {
		return Settings{}, "", err
	}
	persona, err := os.ReadFile(personaPath)
	if err != nil {
		return Settings{}, "", fmt.Errorf("agents: read %s: %w", personaPath, err)
	}
	return settings, strings.TrimSpace(string(persona)), nil
}

// Record writes the definition's revision snapshot. An existing snapshot is
// kept when it still matches its name and rewritten when it was damaged.
func (s *Store) Record(definition ponsruntime.AgentDefinition) error {
	if err := definition.Validate(); err != nil {
		return err
	}
	revision := definition.Revision()
	if existing, err := s.AgentRevision(context.Background(), definition.ID, revision); err == nil && existing.Revision() == revision {
		return nil
	}

	encoded, err := json.MarshalIndent(definition, "", "  ")
	if err != nil {
		return fmt.Errorf("agents: encode revision: %w", err)
	}
	path := s.revisionPath(definition.ID, revision)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("agents: create revisions directory: %w", err)
	}
	return writeFileAtomic(path, append(encoded, '\n'))
}

// AgentRevision resolves a recorded revision. It fails when the snapshot is
// missing, malformed, or no longer matches its fingerprint.
func (s *Store) AgentRevision(_ context.Context, agentID, revision string) (ponsruntime.AgentDefinition, error) {
	if err := (ponsruntime.AgentDefinition{ID: agentID}).Validate(); err != nil {
		return ponsruntime.AgentDefinition{}, err
	}
	if decoded, err := hex.DecodeString(revision); err != nil || len(decoded) != 32 {
		return ponsruntime.AgentDefinition{}, fmt.Errorf("agents: invalid revision %q", revision)
	}

	data, err := os.ReadFile(s.revisionPath(agentID, revision))
	if err != nil {
		return ponsruntime.AgentDefinition{}, fmt.Errorf("agents: read revision: %w", err)
	}
	var definition ponsruntime.AgentDefinition
	if err := decodeStrict(data, &definition); err != nil {
		return ponsruntime.AgentDefinition{}, fmt.Errorf("agents: decode revision %s: %w", revision, err)
	}
	if definition.ID != agentID || definition.Revision() != revision {
		return ponsruntime.AgentDefinition{}, fmt.Errorf("agents: revision %s does not match its contents", revision)
	}
	return definition, nil
}

func (s *Store) revisionPath(agentID, revision string) string {
	return filepath.Join(s.Dir(agentID), revisionsDir, revision+".json")
}

func readSettings(path string) (Settings, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Settings{}, fmt.Errorf("agents: read %s: %w", path, err)
	}
	var settings Settings
	if err := decodeStrict(data, &settings); err != nil {
		return Settings{}, fmt.Errorf("agents: %s: %w", path, err)
	}

	settings.Provider = strings.TrimSpace(settings.Provider)
	settings.Model = strings.TrimSpace(settings.Model)
	if settings.Name != strings.TrimSpace(settings.Name) || strings.ContainsAny(settings.Name, "\r\n") {
		return Settings{}, fmt.Errorf("agents: %s: name must be a single line without surrounding spaces", path)
	}
	if settings.MaxTurns < 0 {
		return Settings{}, fmt.Errorf("agents: %s: max_turns must not be negative", path)
	}
	return settings, nil
}

// decodeStrict rejects unknown fields and trailing data, matching the CLI's
// configuration files.
func decodeStrict(data []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("trailing data")
	}
	return nil
}

func createIfMissing(path string, content []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("agents: create %s: %w", path, err)
	}

	_, writeErr := file.Write(content)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("agents: write %s: %w", path, err)
	}
	return nil
}

func writeFileAtomic(path string, content []byte) error {
	temp, err := os.CreateTemp(filepath.Dir(path), ".revision-*")
	if err != nil {
		return fmt.Errorf("agents: create revision: %w", err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)

	_, writeErr := temp.Write(content)
	if err := errors.Join(writeErr, temp.Sync(), temp.Close()); err != nil {
		return fmt.Errorf("agents: write revision: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("agents: commit revision: %w", err)
	}
	return nil
}
