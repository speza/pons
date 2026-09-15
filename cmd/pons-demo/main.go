// Command pons-demo composes a complete agent entirely from plugins:
//
//	core (loop) + fs + shell + sqlite sessions + scripted brain
//
// The brain even searches its own recorded session through the hands
// layer (search_session) — the plugin-model answer to "how does the
// agent grep its session tree".
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/samperrin/pons"
	"github.com/samperrin/pons/plugins/bash"
	"github.com/samperrin/pons/plugins/brain/scripted"
	"github.com/samperrin/pons/plugins/edit"
	"github.com/samperrin/pons/plugins/fs"
	"github.com/samperrin/pons/plugins/sessionrecorder"
	"github.com/samperrin/pons/plugins/sessionsjsonl"
	"github.com/samperrin/pons/protocol"
)

func main() {
	logger := log.New(os.Stderr, "", log.LstdFlags)

	// Workspace the fs plugin jails everything to.
	ws, err := os.MkdirTemp("", "pons-ws-*")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(ws)

	// Seed a file so the brain has something to inspect.
	seed := filepath.Join(ws, "notes.txt")
	os.WriteFile(seed, []byte("alpha\nbeta\ngamma\n"), 0o644)

	// Session store lives outside the workspace (sessions ≠ task files).
	sessionDir, err := os.MkdirTemp("", "pons-sessions-*")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(sessionDir)
	store, err := sessionsjsonl.New(sessionDir)
	if err != nil {
		panic(err)
	}
	defer store.Close()

	rec := sessionrecorder.New(store)
	rec.TranscriptPath = store.SessionFile

	// Compose the agent. Nothing else exists: no tools, no brain, no
	// persistence beyond exactly these plugins.
	core := pons.New()
	core.Workspace, core.MaxTurns, core.Log = ws, 10, logger
	const goal = "Inspect notes.txt, write an uppercase copy, verify, review own session"

	err = core.Use(
		mustFS(ws),
		mustEdit(ws),
		bash.New(bash.Config{Root: ws, Timeout: 30, MaxLines: 200, MaxBytes: 50 * 1024}),
		rec,
		scripted.New(
			scripted.Step{
				Reason:  "Look at what's in the workspace",
				Actions: []protocol.Action{fs.Read(seed)},
			},
			scripted.Step{
				Reason:  "Apply a patch: add a line after gamma",
				Actions: []protocol.Action{edit.Patch(seed, "<<<<<<< SEARCH\ngamma\n=======\ngamma\ndelta\n>>>>>>> REPLACE")},
			},
			scripted.Step{
				Reason:  "Transform with an unrestricted bash command",
				Actions: []protocol.Action{bash.Run("cat notes.txt | tr a-z A-Z > upper.txt", 10)},
			},
			scripted.Step{
				Reason:  "Verify with a command",
				Actions: []protocol.Action{bash.Run("grep -c [A-Z] upper.txt", 10)},
			},
			scripted.Step{
				Reason:  "Review own transcript with bash (plain NDJSON at $PONS_SESSION_FILE)",
				Actions: []protocol.Action{bash.Run("grep -c delta $PONS_SESSION_FILE", 10)},
			},
		),
	)
	if err != nil {
		logger.Printf("compose failed: %v", err)
		os.Exit(1)
	}

	result, err := core.Run(context.Background(), goal)
	if err != nil {
		logger.Printf("FAILED: %v", err)
		os.Exit(1)
	}
	if result.Exhausted {
		logger.Printf("stopped: exhausted %d turns", result.Turns)
	} else {
		fmt.Printf("finished after %d turn(s): %s\n", result.Turns, result.Answer)
	}

	// Prove the output exists and show the session tree is real.
	out, _ := os.ReadFile(filepath.Join(ws, "upper.txt"))
	fmt.Printf("\nfinal upper.txt:\n%s", out)

	fmt.Println("\nsession transcript: plain NDJSON at $PONS_SESSION_FILE — greppable with bash, no export step")
}

func mustFS(ws string) *fs.FS {
	p, err := fs.New(fs.Config{Root: ws})
	if err != nil {
		panic(err)
	}
	return p
}

func mustEdit(ws string) *edit.Edit {
	p, err := edit.New(edit.Config{Root: ws})
	if err != nil {
		panic(err)
	}
	return p
}
