package environment

import (
	"context"
	"errors"
	"time"
)

// ErrStateNotFound reports that no durable environment exists for a key.
var ErrStateNotFound = errors.New("environment: state not found")

// LifecycleStatus is the persisted lifecycle of a retained environment.
type LifecycleStatus string

const (
	StateActive LifecycleStatus = "active"
	StateIdle   LifecycleStatus = "idle"
)

// WorkspaceStrategy identifies versioned workspace provisioning behavior.
type WorkspaceStrategy string

const WorkspaceStrategyArchive WorkspaceStrategy = "archive/v1"

// State is non-secret durable placement metadata. Active states have a RunID
// and no IdleUntil; idle states have an IdleUntil and no RunID. Provider access
// tokens and API keys must never be stored here.
type State struct {
	Key                string
	Provider           string
	EnvironmentID      string
	Template           string
	Network            NetworkPolicy
	WorkspaceStrategy  WorkspaceStrategy
	WorkspaceSourceRef string
	WorkspaceRevision  string
	CheckpointRevision string
	SetupGeneration    int
	Status             LifecycleStatus
	RunID              string
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
