// Package sessions defines the transcript data model and the storage
// contract for pons sessions.
//
// This is a contract package, like protocol/ is for the brain↔hands seam:
// pure JSON-serializable types, stdlib only, no engine. It answers the
// *fundamental* question — what a session IS — while leaving two other
// questions open by design:
//
//   - HOW it is stored: any engine may implement Store (SQLite with FTS5 is
//     the shipped one in plugins/sessionsqlite; an append-only JSONL backend
//     à la pi/Claude Code would implement the same contract).
//   - WHERE it lives — a policy of the composing binary, not of a plugin.
//
// The model, engine-independent by construction:
//
//   - entries are immutable, addressed by (session_id, id), linked by parent_id
//   - each session has a single mutable "leaf" pointer; the live conversation
//     is the path root → leaf
//   - branching moves the leaf pointer; abandoned branches stay queryable
//   - entry kinds classify the record (user goal, brain action, tool
//     result, note/summary)
package sessions

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"time"
)

// ErrNotFound is returned by Store implementations when a session or entry
// does not exist (or belongs to a different session).
var ErrNotFound = errors.New("sessions: not found")

// NewID returns a random identifier for sessions and entries.
func NewID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	return hex.EncodeToString(b)
}

// Kind classifies an entry.
type Kind string

const (
	KindUser      Kind = "user"      // user message (usually the root/first entry)
	KindAssistant Kind = "assistant" // brain narration
	KindAction    Kind = "action"    // actions proposed by the brain
	KindResult    Kind = "result"    // tool results
	KindNote      Kind = "note"      // labels, branch summaries, finish marks
)

// Entry is one immutable node in a session tree.
type Entry struct {
	ID        string          `json:"id"`
	SessionID string          `json:"-"`
	ParentID  string          `json:"parentId,omitempty"`
	Kind      Kind            `json:"kind"`
	Role      string          `json:"role,omitempty"`
	Text      string          `json:"text,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
	CreatedAt time.Time       `json:"timestamp"`
}

// Session is a named tree with a leaf pointer.
type Session struct {
	ID        string    `json:"id"`
	CWD       string    `json:"cwd,omitempty"`
	LeafID    string    `json:"leafId,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

// Store is the session storage contract. Implementations own the engine —
// SQL, files, anything — and must preserve the tree semantics above.
// Search is part of the contract (engines implement it their own way: FTS5,
// sidecar indexes, brute scan); its shape is substring-plus-scope, and it
// deliberately excludes regex (the JSONL export exists for that).
type Store interface {
	CreateSession(ctx context.Context, cwd string) (Session, error)
	GetSession(ctx context.Context, id string) (Session, error)

	// Append adds an entry as a child of parentID ("" = root) and moves the
	// session leaf to it. Entries are immutable once written.
	Append(ctx context.Context, sessionID, parentID string, e Entry) (Entry, error)

	// GetEntry fetches one entry scoped to a session.
	GetEntry(ctx context.Context, sessionID, entryID string) (Entry, error)

	// Leaf returns the entry the session's leaf points at.
	Leaf(ctx context.Context, sessionID string) (Entry, error)

	// PathToLeaf returns the current conversation, root first.
	PathToLeaf(ctx context.Context, sessionID string) ([]Entry, error)

	// Children returns the direct children of an entry (sibling branches).
	Children(ctx context.Context, sessionID, entryID string) ([]Entry, error)

	// Branch moves the leaf pointer to an earlier entry (fork point).
	Branch(ctx context.Context, sessionID, entryID string) (Session, error)

	// Search does substring search (grep-like). allBranches=false restricts
	// hits to the path root → leaf; kind (optional) filters by entry kind.
	Search(ctx context.Context, sessionID, query, kind string, allBranches bool, limit int) ([]Entry, error)

	Close() error
}

// WriteJSONL serializes entries as pi-shaped NDJSON — one entry per line
// with id/parentId/kind/text/payload/timestamp. This is the interchange
// format: greppable with unix tools, engine-independent, and the escape
// hatch for regex (the Store contract deliberately doesn't do regex).
func WriteJSONL(w io.Writer, entries []Entry) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	for _, e := range entries {
		if err := enc.Encode(e); err != nil {
			return err
		}
	}
	return nil
}
