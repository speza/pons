package sessionrecorder

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/samperrin/pons"
	"github.com/samperrin/pons/plugins/brain/scripted"
	"github.com/samperrin/pons/plugins/sessionsjsonl"
	"github.com/samperrin/pons/protocol"
	"github.com/samperrin/pons/sessions"
)

type appendFailStore struct {
	sessions.Store
	failAt int
	calls  int
}

func (s *appendFailStore) Append(ctx context.Context, sessionID, parentID string, e sessions.Entry) (sessions.Entry, error) {
	s.calls++
	if s.calls == s.failAt {
		return sessions.Entry{}, fmt.Errorf("injected append failure")
	}
	return s.Store.Append(ctx, sessionID, parentID, e)
}

func TestRecorderPropagatesStoreErrors(t *testing.T) {
	store, err := sessionsjsonl.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rec := New(&appendFailStore{Store: store, failAt: 3}) // result entry
	core := pons.New()
	if err := core.Use(rec, scripted.New()); err != nil {
		t.Fatal(err)
	}
	if _, err := core.Run(context.Background(), "goal"); err == nil || !strings.Contains(err.Error(), "append results") {
		t.Fatalf("store error was swallowed: %v", err)
	}
}

// Interactive/continued runs must record every new instruction as a user
// entry so the tree mirrors the brain's context.
func TestRecorderRecordsDistinctInstructions(t *testing.T) {
	store, err := sessionsjsonl.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rec := New(store)
	fmt.Println("DEBUG store type ok, runs begin")

	core := pons.New()
	core.MaxTurns = 5
	if err := core.AddTool("noop", pons.ToolDef{Handler: func(ctx context.Context, a protocol.Action) (protocol.ToolResult, error) {
		return protocol.ToolResult{ActionID: a.ID, OK: true}, nil
	}}); err != nil {
		t.Fatal(err)
	}
	if err := core.Use(rec, scripted.New(
		scripted.Step{Actions: []protocol.Action{{ID: "n1", Kind: "noop"}}},
	)); err != nil {
		t.Fatal(err)
	}

	if _, err := core.Run(context.Background(), "first instruction"); err != nil {
		t.Fatal(err)
	}
	if _, err := core.Run(context.Background(), "second instruction"); err != nil {
		t.Fatal(err)
	}

	t.Logf("after runs: started=%v file=%q", rec.started, rec.SessionFile())
	path, err := store.LatestSessionID()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := store.PathToLeaf(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	var goals []string
	for _, e := range entries {
		if e.Kind == sessions.KindUser {
			goals = append(goals, e.Text)
		}
	}
	if len(goals) != 2 ||
		!strings.Contains(goals[0], "first instruction") ||
		!strings.Contains(goals[1], "second instruction") {
		t.Fatalf("user entries = %v", goals)
	}
}
