package environment

import (
	"context"
	"errors"
	"io"
	"time"
)

// ErrStateNotFound reports that no durable workspace or environment exists for
// a requested identity.
var ErrStateNotFound = errors.New("environment: state not found")

// LifecycleStatus is the persisted lifecycle of a retained environment.
type LifecycleStatus string

const (
	StateActive   LifecycleStatus = "active"
	StateIdle     LifecycleStatus = "idle"
	StateRecovery LifecycleStatus = "recovery"
)

// WorkspaceStrategy identifies versioned workspace provisioning behavior.
type WorkspaceStrategy string

const (
	WorkspaceStrategyArchive WorkspaceStrategy = "archive/v1"
	WorkspaceStrategyGit     WorkspaceStrategy = "git/v1"
	// WorkspaceStrategyEmpty seeds a workspace with no files and no host
	// source, then persists it like archive/v1.
	WorkspaceStrategyEmpty WorkspaceStrategy = "empty"
)

// WorkspaceState is durable logical workspace metadata. It survives the loss
// or deletion of any execution environment. SourceRef never contains
// credentials. The workspace's checkpoints are its WorkspaceCheckpoints
// history.
type WorkspaceState struct {
	ID              string
	Strategy        WorkspaceStrategy
	SourceRef       string
	BaseRevision    string
	SetupGeneration int
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// State is non-secret durable placement metadata. Active states have a RunID
// and no IdleUntil; idle states have an IdleUntil and no RunID. Provider access
// tokens and API keys must never be stored here. Recovery states have neither
// RunID nor IdleUntil, and reserve a sandbox for manual recovery until ExpiresAt.
type State struct {
	WorkspaceID   string
	Provider      string
	EnvironmentID string
	Template      string
	Network       NetworkPolicy
	Status        LifecycleStatus
	RunID         string
	IdleUntil     time.Time
	ExpiresAt     time.Time
	UpdatedAt     time.Time
}

// StateStore persists logical workspace metadata and replaceable environment
// placement, and each workspace's checkpoint history. Implementations must
// make writes and deletes durable before returning.
type StateStore interface {
	WorkspaceState(context.Context, string) (WorkspaceState, error)
	SaveWorkspaceState(context.Context, WorkspaceState) error
	// AppendWorkspaceCheckpoint assigns the next Seq of a recorded workspace
	// and returns the stored entry.
	AppendWorkspaceCheckpoint(context.Context, WorkspaceCheckpoint) (WorkspaceCheckpoint, error)
	// WorkspaceCheckpoints returns the history in ascending Seq order.
	WorkspaceCheckpoints(context.Context, string) ([]WorkspaceCheckpoint, error)
	// CurrentWorkspaceCheckpoint returns the highest Seq, or ErrStateNotFound.
	CurrentWorkspaceCheckpoint(context.Context, string) (WorkspaceCheckpoint, error)
	// DeleteWorkspaceCheckpoints removes entries atomically. It never removes
	// the current checkpoint.
	DeleteWorkspaceCheckpoints(context.Context, string, []int64) error
	EnvironmentState(context.Context, string) (State, error)
	SaveEnvironmentState(context.Context, State) error
	DeleteEnvironmentState(context.Context, string, string) error
	ExpiredEnvironmentStates(context.Context, string, time.Time, int) ([]State, error)
}

// CheckpointStore persists bounded workspace archives outside source
// checkouts. Pruning removes every archive for a workspace except the
// supplied references. A local filesystem implementation may be replaced by
// object storage without changing workspace or environment state.
type CheckpointStore interface {
	// Put consumes at most limit+1 bytes, publishes only complete archives,
	// and returns the reference and size.
	PutWorkspaceCheckpoint(context.Context, string, io.Reader, int64) (string, int64, error)
	// Get verifies size and integrity before returning. The caller closes the reader.
	WorkspaceCheckpoint(context.Context, string, string, int64) (io.ReadCloser, error)
	PruneWorkspaceCheckpoints(context.Context, string, []string) error
}

// DurableProvider receives the runtime's metadata and checkpoint stores after
// they are opened and before any run starts.
type DurableProvider interface {
	SetStores(StateStore, CheckpointStore) error
}
