// Package sessionsjsonl is a sessions.Store engine: append-only NDJSON,
// one file per session. The pi/Claude Code lineage — the file is the
// transcript: greppable with unix tools, tail-able while the agent runs,
// zero dependencies beyond stdlib.
//
// File format (self-describing records, one JSON object per line):
//
//	{"type":"session","id":"…","cwd":"…","createdAt":…}
//	{"type":"entry","entry":{…sessions.Entry…}}
//	{"type":"leaf","leafId":"…"}   // branch control: moves the leaf pointer
//
// The leaf is the target of the last leaf record, or the last entry if
// none — so the file is append-only and crash-tolerant: a truncated final
// line is skipped on load.
package sessionsjsonl

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/samperrin/pons/sessions"
)

// Store is the NDJSON engine for sessions.Store.
type Store struct {
	dir string
	mu  sync.Mutex // protects reads and appends within this Store instance
}

func validateSessionID(id string) error {
	if id == "" || id == "." || id == ".." || strings.ContainsAny(id, `/\\`) || strings.ContainsRune(id, 0) {
		return fmt.Errorf("sessionsjsonl: invalid session id %q", id)
	}
	return nil
}

func (s *Store) checkedFile(sessionID string) (string, error) {
	if err := validateSessionID(sessionID); err != nil {
		return "", err
	}
	return s.file(sessionID), nil
}

// New creates the private storage directory (one .jsonl file per session).
func New(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

func (s *Store) file(sessionID string) string {
	return filepath.Join(s.dir, sessionID+".jsonl")
}

// SessionFile is the on-disk transcript path for a session — what
// sessionrecorder exports as PONS_SESSION_FILE.
func (s *Store) SessionFile(sessionID string) string {
	path, err := s.checkedFile(sessionID)
	if err != nil {
		return ""
	}
	return path
}

type wireRecord struct {
	Type      string          `json:"type"` // session | entry | leaf
	ID        string          `json:"id,omitempty"`
	CWD       string          `json:"cwd,omitempty"`
	CreatedAt time.Time       `json:"createdAt,omitempty"`
	Entry     *sessions.Entry `json:"entry,omitempty"`
	LeafID    string          `json:"leafId,omitempty"`
}

// fileData is the parsed state of one session file.
type fileData struct {
	sess    sessions.Session
	entries []sessions.Entry
	leaf    string // last entry id, or last leaf-record target
}

func (s *Store) load(sessionID string) (*fileData, error) {
	path, err := s.checkedFile(sessionID)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, sessions.ErrNotFound
		}
		return nil, err
	}
	defer f.Close()

	d := &fileData{sess: sessions.Session{ID: sessionID}}
	entryIDs := make(map[string]struct{})
	reader := bufio.NewReader(f)
	for {
		line, readErr := reader.ReadBytes('\n')
		if readErr == io.EOF {
			// Appends always terminate records with a newline. A final
			// unterminated record is a crash tail and is deliberately ignored.
			break
		}
		if readErr != nil {
			return nil, readErr
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var rec wireRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			return nil, fmt.Errorf("sessionsjsonl: invalid record: %w", err)
		}
		switch rec.Type {
		case "session":
			if rec.ID != "" && rec.ID != sessionID {
				return nil, fmt.Errorf("sessionsjsonl: session id %q does not match file %q", rec.ID, sessionID)
			}
			d.sess.CWD, d.sess.CreatedAt = rec.CWD, rec.CreatedAt
		case "entry":
			if rec.Entry != nil {
				if rec.Entry.ID == "" {
					return nil, fmt.Errorf("sessionsjsonl: entry has empty id")
				}
				if _, exists := entryIDs[rec.Entry.ID]; exists {
					return nil, fmt.Errorf("sessionsjsonl: duplicate entry id %q", rec.Entry.ID)
				}
				entryIDs[rec.Entry.ID] = struct{}{}
				rec.Entry.SessionID = sessionID
				d.entries = append(d.entries, *rec.Entry)
				d.leaf = rec.Entry.ID // last leaf-relevant record wins
			}
		case "leaf":
			d.leaf = rec.LeafID
		}
	}
	d.sess.LeafID = d.leaf
	return d, nil
}

func (s *Store) appendRecord(sessionID string, rec any) error {
	path, err := s.checkedFile(sessionID)
	if err != nil {
		return err
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Chmod(0o600); err != nil {
		return err
	}
	line := append(b, '\n')
	n, err := f.Write(line)
	if err != nil {
		return err
	}
	if n != len(line) {
		return io.ErrShortWrite
	}
	return nil
}

func (s *Store) CreateSession(ctx context.Context, cwd string) (sessions.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := sessions.Session{ID: sessions.NewID(), CWD: cwd, CreatedAt: time.Now()}
	err := s.appendRecord(sess.ID, map[string]any{
		"type": "session", "id": sess.ID, "cwd": sess.CWD, "createdAt": sess.CreatedAt,
	})
	return sess, err
}

func (s *Store) GetSession(ctx context.Context, id string) (sessions.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.load(id)
	if err != nil {
		return sessions.Session{}, err
	}
	return d.sess, nil
}

func (s *Store) Append(ctx context.Context, sessionID, parentID string, e sessions.Entry) (sessions.Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.load(sessionID)
	if err != nil {
		return sessions.Entry{}, fmt.Errorf("sessionsjsonl: parent lookup: %w", err)
	}
	if parentID != "" {
		found := false
		for _, en := range d.entries {
			if en.ID == parentID {
				found = true
				break
			}
		}
		if !found {
			return sessions.Entry{}, fmt.Errorf("sessionsjsonl: parent: %w", sessions.ErrNotFound)
		}
	}
	if e.ID == "" {
		e.ID = sessions.NewID()
	}
	for _, en := range d.entries {
		if en.ID == e.ID {
			return sessions.Entry{}, fmt.Errorf("sessionsjsonl: duplicate entry id %q", e.ID)
		}
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now()
	}
	e.SessionID, e.ParentID = sessionID, parentID
	stored := e
	err = s.appendRecord(sessionID, map[string]any{"type": "entry", "entry": &stored})
	return e, err
}

func (s *Store) GetEntry(ctx context.Context, sessionID, entryID string) (sessions.Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.load(sessionID)
	if err != nil {
		return sessions.Entry{}, err
	}
	return findEntry(d, entryID)
}

func (s *Store) Leaf(ctx context.Context, sessionID string) (sessions.Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.load(sessionID)
	if err != nil {
		return sessions.Entry{}, err
	}
	return findEntry(d, d.sess.LeafID)
}

// PathToLeaf walks parent links from the leaf and returns the
// conversation root-first.
func (s *Store) PathToLeaf(ctx context.Context, sessionID string) ([]sessions.Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.load(sessionID)
	if err != nil {
		return nil, err
	}
	return pathToLeaf(d)
}

func findEntry(d *fileData, entryID string) (sessions.Entry, error) {
	if entryID == "" {
		return sessions.Entry{}, sessions.ErrNotFound
	}
	for _, e := range d.entries {
		if e.ID == entryID {
			return e, nil
		}
	}
	return sessions.Entry{}, sessions.ErrNotFound
}

func pathToLeaf(d *fileData) ([]sessions.Entry, error) {
	if d.sess.LeafID == "" {
		return nil, sessions.ErrNotFound
	}
	byID := make(map[string]sessions.Entry, len(d.entries))
	for _, e := range d.entries {
		byID[e.ID] = e
	}
	cur := d.sess.LeafID
	seen := make(map[string]bool, len(d.entries))
	var out []sessions.Entry
	for {
		if seen[cur] {
			return nil, fmt.Errorf("sessionsjsonl: cycle at %q", cur)
		}
		seen[cur] = true
		e, ok := byID[cur]
		if !ok {
			return nil, fmt.Errorf("sessionsjsonl: broken chain at %q", cur)
		}
		out = append(out, e)
		if e.ParentID == "" {
			break
		}
		cur = e.ParentID
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

func (s *Store) Children(ctx context.Context, sessionID, entryID string) ([]sessions.Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.load(sessionID)
	if err != nil {
		return nil, err
	}
	var out []sessions.Entry
	for _, e := range d.entries {
		if e.ParentID == entryID {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (s *Store) Branch(ctx context.Context, sessionID, entryID string) (sessions.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.load(sessionID)
	if err != nil {
		return sessions.Session{}, fmt.Errorf("sessionsjsonl: branch target: %w", err)
	}
	if _, err := findEntry(d, entryID); err != nil {
		return sessions.Session{}, fmt.Errorf("sessionsjsonl: branch target: %w", err)
	}
	if err := s.appendRecord(sessionID, map[string]any{"type": "leaf", "leafId": entryID}); err != nil {
		return sessions.Session{}, err
	}
	d.sess.LeafID = entryID
	return d.sess, nil
}

func (s *Store) Search(ctx context.Context, sessionID, query, kind string, allBranches bool, limit int) ([]sessions.Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.load(sessionID)
	if err != nil {
		return nil, err
	}
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 20
	}

	scope := map[string]bool{}
	if !allBranches {
		path, err := pathToLeaf(d)
		if err != nil {
			return nil, err
		}
		for _, e := range path {
			scope[e.ID] = true
		}
	}

	var hits []sessions.Entry
	for _, e := range slices.Backward(d.entries) { // newest first
		if kind != "" && e.Kind != sessions.Kind(kind) {
			continue
		}
		if !allBranches && !scope[e.ID] {
			continue
		}
		body := strings.ToLower(e.Text + "\n" + string(e.Payload))
		if strings.Contains(body, q) {
			hits = append(hits, e)
			if len(hits) >= limit {
				break
			}
		}
	}
	return hits, nil
}

// LatestSessionID returns the id of the most recently modified session
// file in the store — the default target for resume.
func (s *Store) LatestSessionID() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	matches, err := filepath.Glob(filepath.Join(s.dir, "*.jsonl"))
	if err != nil {
		return "", err
	}
	var best string
	var bestMod time.Time
	for _, m := range matches {
		info, err := os.Lstat(m)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		id := strings.TrimSuffix(filepath.Base(m), ".jsonl")
		if validateSessionID(id) != nil {
			continue
		}
		if best == "" || info.ModTime().After(bestMod) {
			best, bestMod = m, info.ModTime()
		}
	}
	if best == "" {
		return "", sessions.ErrNotFound
	}
	return strings.TrimSuffix(filepath.Base(best), ".jsonl"), nil
}

func (s *Store) Close() error { return nil }
