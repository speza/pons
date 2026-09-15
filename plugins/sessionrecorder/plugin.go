// Package sessionrecorder is the persistence capability: it records the
// loop's turns into any sessions.Store and nothing else.
//
// There is deliberately no session-search tool. Transcripts are plain
// NDJSON files (see plugins/sessionsjsonl); the recorder publishes their
// path via the PONS_SESSION_FILE environment variable so the agent — or a
// human — inspects them with bash/grep/jq, the same way pi sessions work.
// Post-compaction, the same variable is how the agent reaches details that
// fell out of context.
package sessionrecorder

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/samperrin/pons"
	"github.com/samperrin/pons/protocol"
	"github.com/samperrin/pons/sessions"
)

// EnvSessionFile names the current transcript file for shell tools.
const EnvSessionFile = "PONS_SESSION_FILE"

// Recorder persists one TurnLog per completed turn into a sessions.Store.
type Recorder struct {
	store sessions.Store
	// TranscriptPath optionally maps a session id to its transcript file
	// path; when set, EnvSessionFile is exported for the bash tool.
	TranscriptPath func(sessionID string) string

	session  sessions.Session
	started  bool
	lastID   string // in-memory leaf tracker; OnTurn is called sequentially
	lastMessage string // last instruction recorded (dedupes per-run goal entries)
}

// New wraps a storage engine (any sessions.Store) as a pons plugin.
func New(store sessions.Store) *Recorder { return &Recorder{store: store} }

// Setup hooks turn persistence into the core loop. No tools are
// registered: transcript inspection is a bash/grep job over a plain file.
func (r *Recorder) Setup(c *pons.Core) error {
	c.OnTurn(r.record)
	return nil
}

// Resume continues an existing session: the recorder appends to the same
// tree (leaf = last entry) and re-exports the transcript path. The first
// recorded turn of the resumed run also writes the new instruction as a
// user entry, keeping the tree in sync with the brain's context.
func (r *Recorder) Resume(ctx context.Context, sessionID string) error {
	sess, err := r.store.GetSession(ctx, sessionID)
	if err != nil {
		return err
	}
	leaf, err := r.store.Leaf(ctx, sessionID)
	if err != nil {
		return err
	}
	r.session, r.started, r.lastID = sess, true, leaf.ID
	if r.TranscriptPath != nil {
		_ = os.Setenv(EnvSessionFile, r.TranscriptPath(sess.ID))
	}
	r.lastMessage = "" // next record writes the new instruction as a user entry
	return nil
}

// SessionFile returns the current session's transcript path, if recording
// has started.
func (r *Recorder) SessionFile() string {
	if !r.started {
		return ""
	}
	return os.Getenv(EnvSessionFile)
}

// record persists one completed turn: lazily creates the session with the
// goal as root entry, then appends an action entry and a result entry.
// Called sequentially by the loop, so the in-memory leaf tracker is safe.
func (r *Recorder) record(obs protocol.Observation, turn protocol.TurnLog) {
	ctx := context.Background()
	if !r.started {
		sess, err := r.store.CreateSession(ctx, obs.Workspace)
		if err != nil {
			return
		}
		r.session = sess
		r.started = true
		root, err := r.store.Append(ctx, sess.ID, "", sessions.Entry{Kind: sessions.KindUser, Text: obs.Message})
		if err != nil {
			return
		}
		r.lastID, r.lastMessage = root.ID, obs.Message
		if r.TranscriptPath != nil {
			_ = os.Setenv(EnvSessionFile, r.TranscriptPath(sess.ID))
		}
	} else if obs.Message != "" && obs.Message != r.lastMessage {
		// A new instruction (interactive follow-up, resumed run): record it
		// as a user entry so the tree mirrors the brain's context.
		u, err := r.store.Append(ctx, r.session.ID, r.lastID,
			sessions.Entry{Kind: sessions.KindUser, Text: obs.Message})
		if err != nil {
			return
		}
		r.lastID, r.lastMessage = u.ID, obs.Message
	}

	var sb strings.Builder
	for _, a := range turn.Actions {
		fmt.Fprintf(&sb, "%s %v\n", a.Kind, a.Args)
	}
	actionEntry, err := r.store.Append(ctx, r.session.ID, r.lastID,
		sessions.Entry{Kind: sessions.KindAction, Text: sb.String(), Payload: mustJSON(turn.Actions)})
	if err != nil {
		return
	}
	r.lastID = actionEntry.ID

	var rb strings.Builder
	for _, res := range turn.Results {
		rb.WriteString(res.Observation() + "\n")
	}
	resultEntry, err := r.store.Append(ctx, r.session.ID, actionEntry.ID,
		sessions.Entry{Kind: sessions.KindResult, Text: rb.String(), Payload: mustJSON(turn.Results)})
	if err != nil {
		return
	}
	r.lastID = resultEntry.ID
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}
