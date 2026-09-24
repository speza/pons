// Command pons-demo composes a finite agent entirely from plugins:
//
//	core (loop) + fs + edit + bash + scripted brain
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

	// Compose the finite agent. Runtime persistence belongs to the server
	// composition, not to a step-observer plugin.
	core := pons.New()
	core.Workspace, core.MaxSteps, core.Log = ws, 10, logger
	const goal = "Inspect notes.txt, write an uppercase copy, and verify it"

	err = core.Use(
		mustFS(ws),
		mustEdit(ws),
		bash.New(bash.Config{Root: ws, Timeout: 30, MaxLines: 200, MaxBytes: 50 * 1024}),
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
		logger.Printf("stopped: exhausted %d steps", result.Steps)
	} else {
		fmt.Printf("finished after %d step(s): %s\n", result.Steps, result.Answer)
	}

	// Prove the output exists.
	out, _ := os.ReadFile(filepath.Join(ws, "upper.txt"))
	fmt.Printf("\nfinal upper.txt:\n%s", out)
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
