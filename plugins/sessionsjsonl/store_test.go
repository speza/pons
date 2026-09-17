package sessionsjsonl

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/samperrin/pons/sessions"
)

func openStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestAppendAndPath(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)

	sess, err := s.CreateSession(ctx, "/tmp/ws")
	if err != nil {
		t.Fatal(err)
	}
	root, err := s.Append(ctx, sess.ID, "", sessions.Entry{Kind: sessions.KindUser, Text: "goal"})
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.Append(ctx, sess.ID, root.ID, sessions.Entry{Kind: sessions.KindAction, Text: "act"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(ctx, sess.ID, a.ID, sessions.Entry{Kind: sessions.KindResult, Text: "res"}); err != nil {
		t.Fatal(err)
	}

	path, err := s.PathToLeaf(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(path) != 3 || path[0].Kind != sessions.KindUser || path[2].Kind != sessions.KindResult {
		t.Fatalf("unexpected path: %+v", path)
	}
	if leaf, err := s.Leaf(ctx, sess.ID); err != nil || leaf.Kind != sessions.KindResult {
		t.Fatalf("leaf = %+v err=%v", leaf, err)
	}
}

func TestBranchAndScopedSearch(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)

	sess, _ := s.CreateSession(ctx, "/tmp/ws")
	root, _ := s.Append(ctx, sess.ID, "", sessions.Entry{Kind: sessions.KindUser, Text: "goal"})
	b1, _ := s.Append(ctx, sess.ID, root.ID, sessions.Entry{Kind: sessions.KindAction, Text: "first branch attempt"})
	s.Append(ctx, sess.ID, b1.ID, sessions.Entry{Kind: sessions.KindResult, Text: "first branch result"})

	// Branch back to the root; the abandoned branch must stay searchable.
	if _, err := s.Branch(ctx, sess.ID, root.ID); err != nil {
		t.Fatal(err)
	}
	path, err := s.PathToLeaf(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(path) != 1 {
		t.Fatalf("path after branch = %d entries, want 1", len(path))
	}
	b2, _ := s.Append(ctx, sess.ID, root.ID, sessions.Entry{Kind: sessions.KindAction, Text: "new branch attempt"})
	b3, _ := s.Append(ctx, sess.ID, b2.ID, sessions.Entry{Kind: sessions.KindResult, Text: "tail entry"})

	kids, err := s.Children(ctx, sess.ID, root.ID)
	if err != nil || len(kids) != 2 {
		t.Fatalf("children = %+v err=%v", kids, err)
	}

	// Scoped search must walk UP the whole path, not just the leaf.
	scoped, err := s.Search(ctx, sess.ID, "attempt", "", false, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(scoped) != 1 || scoped[0].ID != b2.ID {
		t.Fatalf("scoped search for non-leaf hit = %+v", scoped)
	}
	leafHits, err := s.Search(ctx, sess.ID, "tail entry", "", false, 20)
	if err != nil || len(leafHits) != 1 || leafHits[0].ID != b3.ID {
		t.Fatalf("scoped search for leaf hit = %+v err=%v", leafHits, err)
	}

	all, err := s.Search(ctx, sess.ID, "attempt", "", true, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 { // first + new branch action entries
		t.Fatalf("whole-tree search = %d hits, want 2", len(all))
	}
}

func TestSearchKindAndPayload(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)

	sess, _ := s.CreateSession(ctx, "/tmp/ws")
	root, _ := s.Append(ctx, sess.ID, "", sessions.Entry{Kind: sessions.KindUser, Text: "goal mentions deploy"})
	action, _ := s.Append(ctx, sess.ID, root.ID, sessions.Entry{Kind: sessions.KindAction, Text: "deploy the thing"})
	s.Append(ctx, sess.ID, action.ID, sessions.Entry{Kind: sessions.KindResult, Text: "deploy ok"})

	hits, err := s.Search(ctx, sess.ID, "deploy", string(sessions.KindAction), false, 20)
	if err != nil || len(hits) != 1 || hits[0].Kind != sessions.KindAction {
		t.Fatalf("kind filter = %+v err=%v", hits, err)
	}
	all, err := s.Search(ctx, sess.ID, "deploy", "", false, 20)
	if err != nil || len(all) != 3 {
		t.Fatalf("unfiltered = %d hits err=%v", len(all), err)
	}

	// payload text is searchable (mirrors FTS indexing text+payload)
	s.Append(ctx, sess.ID, action.ID, sessions.Entry{
		Kind:    sessions.KindNote,
		Text:    "",
		Payload: json.RawMessage(`{"command":"cat notes.txt > upper.txt"}`),
	})
	payloadHits, err := s.Search(ctx, sess.ID, "upper.txt", "", true, 20)
	if err != nil || len(payloadHits) != 1 {
		t.Fatalf("payload search = %+v err=%v", payloadHits, err)
	}
}

func TestExportJSONLIsGreppable(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)

	sess, _ := s.CreateSession(ctx, "/tmp/ws")
	root, _ := s.Append(ctx, sess.ID, "", sessions.Entry{Kind: sessions.KindUser, Text: "goal: uppercase notes"})
	a, _ := s.Append(ctx, sess.ID, root.ID, sessions.Entry{Kind: sessions.KindAction, Text: "transform"})
	s.Append(ctx, sess.ID, a.ID, sessions.Entry{Kind: sessions.KindResult, Text: "wrote upper.txt"})

	var buf bytes.Buffer
	path, err := s.PathToLeaf(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := sessions.WriteJSONL(&buf, path); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Fatalf("export = %d lines, want 3", len(lines))
	}
	if !strings.Contains(out, "parentId") || !strings.Contains(out, "upper.txt") {
		t.Fatalf("export not shaped as expected:\n%s", out)
	}
}

func TestRejectsUnsafeSessionIDs(t *testing.T) {
	s := openStore(t)
	for _, id := range []string{"../escape", "..\\escape", "/tmp/escape", ""} {
		if _, err := s.GetSession(context.Background(), id); err == nil {
			t.Fatalf("session id %q should be rejected", id)
		}
	}
}

func TestRejectsDuplicateEntryIDs(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	sess, err := s.CreateSession(ctx, "/tmp/ws")
	if err != nil {
		t.Fatal(err)
	}
	root, err := s.Append(ctx, sess.ID, "", sessions.Entry{ID: "fixed", Kind: sessions.KindUser})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(ctx, sess.ID, root.ID, sessions.Entry{ID: root.ID, Kind: sessions.KindNote}); err == nil {
		t.Fatal("duplicate entry id should be rejected")
	}
}

func TestLoadsEntryLargerThanScannerLimit(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	sess, err := s.CreateSession(ctx, "/tmp/ws")
	if err != nil {
		t.Fatal(err)
	}
	payload := strings.Repeat("x", 16*1024*1024+1)
	if _, err := s.Append(ctx, sess.ID, "", sessions.Entry{Kind: sessions.KindUser, Text: payload}); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.GetSession(ctx, sess.ID)
	if err != nil || loaded.ID != sess.ID {
		t.Fatalf("large transcript load: %+v err=%v", loaded, err)
	}
	path, err := s.PathToLeaf(ctx, sess.ID)
	if err != nil || len(path) != 1 || len(path[0].Text) != len(payload) {
		t.Fatalf("large transcript path: len=%d err=%v", len(path), err)
	}
}

func TestSessionScoping(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)

	sessA, _ := s.CreateSession(ctx, "/a")
	sessB, _ := s.CreateSession(ctx, "/b")
	rootA, _ := s.Append(ctx, sessA.ID, "", sessions.Entry{Kind: sessions.KindUser, Text: "alpha"})
	s.Append(ctx, sessB.ID, "", sessions.Entry{Kind: sessions.KindUser, Text: "beta"})

	// Entries from another session are invisible.
	if _, err := s.GetEntry(ctx, sessB.ID, rootA.ID); err != sessions.ErrNotFound {
		t.Fatalf("cross-session read: err=%v (want ErrNotFound)", err)
	}
	// Appending under a foreign parent is rejected.
	if _, err := s.Append(ctx, sessB.ID, rootA.ID, sessions.Entry{Kind: sessions.KindNote}); err == nil {
		t.Fatal("cross-session append should fail")
	}
}

// The headline property of the JSONL engine: the file on disk is the
// transcript — greppable with plain unix tools, no export step.
func TestFileIsTheTranscript(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)

	sess, _ := s.CreateSession(ctx, "/tmp/ws")
	root, _ := s.Append(ctx, sess.ID, "", sessions.Entry{Kind: sessions.KindUser, Text: "goal: uppercase notes"})
	a, _ := s.Append(ctx, sess.ID, root.ID, sessions.Entry{Kind: sessions.KindAction, Text: "cat notes.txt | tr a-z A-Z > upper.txt"})
	s.Append(ctx, sess.ID, a.ID, sessions.Entry{Kind: sessions.KindResult, Text: "wrote upper.txt"})

	raw, err := os.ReadFile(s.file(sess.ID))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(raw), "upper.txt"); n < 2 {
		t.Fatalf("grep-ability: found %d occurrences of 'upper.txt' in the raw file, want >=2", n)
	}
	if !strings.Contains(string(raw), `"type":"session"`) || !strings.Contains(string(raw), `"kind":"result"`) {
		t.Fatalf("file not self-describing:\n%s", raw)
	}
}

func TestCorruptRecordErrorNamesLine(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	sess, err := s.CreateSession(ctx, "/tmp/ws")
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(s.file(sess.ID), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	// Line 2 is corrupt; the error must say so for debuggability.
	if _, err := f.WriteString("{not json}\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	_, err = s.GetSession(ctx, sess.ID)
	if err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("expected line-numbered error, got: %v", err)
	}
}
