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
