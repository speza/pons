package environment

import (
	"context"
	"errors"
	"time"
)

// ErrStateNotFound reports that no durable environment exists for a key.
var ErrStateNotFound = errors.New("environment: state not found")

const (
	StateActive = "active"
	StateIdle   = "idle"

	WorkspaceStrategyLocalArchive = "local_archive"
)

// State is non-secret durable placement metadata. Provider access tokens and
// API keys must never be stored here.
type State struct {
	Key                string
	Provider           string
	EnvironmentID      string
	Template           string
	Network            NetworkPolicy
	WorkspaceStrategy  string
	WorkspaceSourceRef string
	WorkspaceRevision  string
	CheckpointRevision string
	SetupGeneration    int
	Status             string
	LeaseID            string
	IdleUntil          time.Time
	ExpiresAt          time.Time
	UpdatedAt          time.Time
}

// StateStore persists remote environment affinity independently of process
// memory. Implementations must make Save and Delete durable before returning.
type StateStore interface {
	EnvironmentState(context.Context, string) (State, error)
	SaveEnvironmentState(context.Context, State) error
	DeleteEnvironmentState(context.Context, string, string) error
	ExpiredEnvironmentStates(context.Context, string, time.Time, int) ([]State, error)
}

// StatefulProvider receives the runtime's durable environment store after the
// store is opened and before any run starts.
type StatefulProvider interface {
	SetStateStore(StateStore) error
}
