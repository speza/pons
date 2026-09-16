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
	"sync"

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

	mu          sync.Mutex
	session     sessions.Session
	started     bool
	lastID      string // in-memory leaf tracker
	lastMessage string // last instruction recorded (dedupes per-run goal entries)
	sessionFile string
	failure     error
}

// New wraps a storage engine (any sessions.Store) as a pons plugin.
func New(store sessions.Store) *Recorder { return &Recorder{store: store} }

// Setup hooks turn persistence into the core loop. No tools are
// registered: transcript inspection is a bash/grep job over a plain file.
func (r *Recorder) Setup(c *pons.Core) error {
	c.OnTurnError(r.record)
	return nil
}

// Resume continues an existing session: the recorder appends to the same
// tree (leaf = last entry) and re-exports the transcript path. The first
// recorded turn of the resumed run also writes the new instruction as a
// user entry, keeping the tree in sync with the brain's context.
func (r *Recorder) Resume(ctx context.Context, sessionID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	sess, err := r.store.GetSession(ctx, sessionID)
	if err != nil {
		return err
	}
	leaf, err := r.store.Leaf(ctx, sessionID)
	if err != nil {
		return err
	}
	r.session, r.started, r.lastID = sess, true, leaf.ID
	r.lastMessage = "" // next record writes the new instruction as a user entry
	r.failure = nil
	r.sessionFile = ""
	if err := r.publishLocked(sess.ID); err != nil {
		return r.fail(err)
	}
	return nil
}

// SessionFile returns the current session's transcript path, if recording
// has started.
func (r *Recorder) SessionFile() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.started {
		return ""
	}
	return r.sessionFile
}

// record persists one completed turn: lazily creates the session with the
// goal as root entry, then appends an action entry and a result entry.
// It runs under a mutex because a Core may be driven by more than one caller.
func (r *Recorder) record(obs protocol.Observation, turn protocol.TurnLog) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failure != nil {
		return r.failure
	}

	ctx := context.Background()
	if !r.started {
		sess, err := r.store.CreateSession(ctx, obs.Workspace)
		if err != nil {
			return r.fail(fmt.Errorf("create session: %w", err))
		}
		root, err := r.store.Append(ctx, sess.ID, "", sessions.Entry{Kind: sessions.KindUser, Text: obs.Message})
		if err != nil {
			return r.fail(fmt.Errorf("append session root: %w", err))
		}
		r.session, r.started = sess, true
		r.lastID, r.lastMessage = root.ID, obs.Message
		if err := r.publishLocked(sess.ID); err != nil {
			return r.fail(err)
		}
	} else if obs.Message != "" && obs.Message != r.lastMessage {
		// A new instruction (interactive follow-up, resumed run): record it
		// as a user entry so the tree mirrors the brain's context.
		u, err := r.store.Append(ctx, r.session.ID, r.lastID,
			sessions.Entry{Kind: sessions.KindUser, Text: obs.Message})
		if err != nil {
			return r.fail(fmt.Errorf("append user message: %w", err))
		}
		r.lastID, r.lastMessage = u.ID, obs.Message
	}

	var sb strings.Builder
	for _, a := range turn.Actions {
		args := string(a.Args)
		if args == "" {
			args = "{}"
		}
		fmt.Fprintf(&sb, "%s %s\n", a.Kind, args)
	}
	actionPayload, err := json.Marshal(turn.Actions)
	if err != nil {
		return r.fail(fmt.Errorf("encode actions: %w", err))
	}
	actionEntry, err := r.store.Append(ctx, r.session.ID, r.lastID,
		sessions.Entry{Kind: sessions.KindAction, Text: sb.String(), Payload: actionPayload})
	if err != nil {
		return r.fail(fmt.Errorf("append actions: %w", err))
	}
	r.lastID = actionEntry.ID

	var rb strings.Builder
	for _, res := range turn.Results {
		rb.WriteString(res.Observation() + "\n")
	}
	resultPayload, err := json.Marshal(turn.Results)
	if err != nil {
		return r.fail(fmt.Errorf("encode results: %w", err))
	}
	resultEntry, err := r.store.Append(ctx, r.session.ID, actionEntry.ID,
		sessions.Entry{Kind: sessions.KindResult, Text: rb.String(), Payload: resultPayload})
	if err != nil {
		return r.fail(fmt.Errorf("append results: %w", err))
	}
	r.lastID = resultEntry.ID
	return nil
}

func (r *Recorder) fail(err error) error {
	r.failure = err
	return err
}

func (r *Recorder) publishLocked(sessionID string) error {
	if r.TranscriptPath == nil {
		r.sessionFile = ""
		return nil
	}
	path := r.TranscriptPath(sessionID)
	if path == "" {
		return fmt.Errorf("session transcript path is empty")
	}
	if err := os.Setenv(EnvSessionFile, path); err != nil {
		return fmt.Errorf("publish %s: %w", EnvSessionFile, err)
	}
	r.sessionFile = path
	return nil
}
