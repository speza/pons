// Command pons runs a complete agent with a real LLM brain:
//
//	core (loop) + fs + edit + bash + JSONL sessions + LLM brain
//
// The model plans, executes tools through the harness (concurrently within
// a turn), sees tool results back as observations, and finishes with a
// text-only reply. Sessions are durable per project; --resume continues one.
//
// Authentication: ANTHROPIC_API_KEY (provider anthropic, default) or
// OPENAI_API_KEY (provider openai, also works with any OpenAI-compatible
// server via -base-url, e.g. llama.cpp or Ollama). ChatGPT subscription
// codex auth comes from pons's own auth file via --login.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/samperrin/pons"
	"github.com/samperrin/pons/plugins/bash"
	"github.com/samperrin/pons/plugins/brain/llm"
	"github.com/samperrin/pons/plugins/edit"
	"github.com/samperrin/pons/plugins/fs"
	"github.com/samperrin/pons/plugins/sessionrecorder"
	"github.com/samperrin/pons/plugins/sessionsjsonl"
	"github.com/samperrin/pons/protocol"
)

func main() {
	logger := log.New(os.Stderr, "", log.LstdFlags)

	provider := flag.String("provider", "anthropic", "LLM provider: anthropic | openai | codex")
	model := flag.String("model", "", "model id (default: provider default)")
	baseURL := flag.String("base-url", "", "override provider endpoint (for OpenAI-compatible servers)")
	workspace := flag.String("workspace", "", "workspace root the tools are jailed to (default: current directory)")
	sessionDir := flag.String("session-dir", "", "session directory (default: ~/.pons/sessions/<munged-project-path>/ — durable per project, one JSONL file per session)")
	compactChars := flag.Int("compact-chars", 0, "conversation size (chars) before auto-compaction; 0 = default ~400k, negative = off")
	debug := flag.Bool("debug", false, "verbose: raw tool results, provider turn details")
	message := flag.String("message", "", "the task (required unless --resume, where a continuation prompt is implied)")
	resume := flag.String("resume", "", `continue a previous session: "latest" or a session id`)
	authFile := flag.String("auth-file", "", "codex credentials file (default: ~/.pons/auth.json)")
	login := flag.Bool("login", false, "codex only: authenticate with ChatGPT (browser flow), save credentials, and exit")
	maxTurns := flag.Int("max-turns", 12, "loop budget")
	interactive := flag.Bool("i", false, "interactive: read tasks from stdin, one per line; the conversation continues in-process")
	flag.Parse()

	// Workspace: the real agent works on the current directory by default.
	ws := *workspace
	if ws == "" {
		abs, err := filepath.Abs(".")
		if err != nil {
			panic(err)
		}
		ws = abs
	}

	brainLogger := logger
	if !*debug {
		brainLogger = nil // quiet: no provider chatter
	}
	if *login {
		if *provider != "codex" {
			logger.Printf("--login applies to the codex provider")
			os.Exit(1)
		}
		authPath := *authFile
		if authPath == "" {
			def, perr := llm.DefaultCodexAuthPath()
			if perr != nil {
				panic(perr)
			}
			authPath = def
		}
		if lerr := llm.RunLogin(context.Background(), authPath); lerr != nil {
			logger.Printf("login: %v", lerr)
			os.Exit(1)
		}
		return
	}

	brain, err := llm.New(llm.Config{
		Provider:     *provider,
		Model:        *model,
		BaseURL:      *baseURL,
		Logger:       brainLogger,
		AuthPath:     *authFile,
		CompactChars: *compactChars,
	})
	if err != nil {
		logger.Printf("brain: %v", err)
		os.Exit(1)
	}

	// Session store: a composition policy, not a plugin convention. The
	// binary decides where state lives (durable, per-project); the session
	// plugin only opens what it is given.
	// Sessions cluster per project, named after the project: the absolute
	// path with "/" → "-", so the directory is self-describing
	// (Claude Code's scheme) and collision-free by construction.
	sessionPath := *sessionDir
	if sessionDir == nil || *sessionDir == "" {
		abs, err := filepath.Abs(".")
		if err != nil {
			panic(err)
		}
		home, err := os.UserHomeDir()
		if err != nil {
			panic(err)
		}
		sessionPath = filepath.Join(home, ".pons", "sessions", strings.ReplaceAll(abs, "/", "-"))
	}
	store, err := sessionsjsonl.New(sessionPath)
	if err != nil {
		panic(err)
	}
	defer store.Close()
	logger.Printf("sessions: %s", sessionPath)

	fsTools, err := fs.New(fs.Config{Root: ws})
	if err != nil {
		panic(err)
	}
	editTool, err := edit.New(edit.Config{Root: ws})
	if err != nil {
		panic(err)
	}
	rec := sessionrecorder.New(store)
	rec.TranscriptPath = store.SessionFile

	instruction := *message
	// Interactive mode: each stdin line is a task; the brain keeps its
	// conversation across lines and the recorder appends each instruction
	// to the same session tree.
	interactiveMode := *interactive || (*message == "" && *resume == "")
	if *resume != "" {
		ctx := context.Background()
		id := *resume
		if id == "latest" {
			id, err = store.LatestSessionID()
			if err != nil {
				logger.Printf("resume: %v", err)
				os.Exit(1)
			}
		}
		if err := rec.Resume(ctx, id); err != nil {
			logger.Printf("resume: %v", err)
			os.Exit(1)
		}
		entries, err := store.PathToLeaf(ctx, id)
		if err != nil {
			logger.Printf("resume: %v", err)
			os.Exit(1)
		}
		if instruction == "" {
			instruction = "Continue the work recorded in this session. Report the current state, finish anything incomplete, and summarize."
		}
		brain.Seed(llm.TurnsFromEntries(entries), instruction, ws)
		logger.Printf("resumed session %s (%d entries)", id, len(entries))
	}

	core := pons.New()
	core.Workspace, core.MaxTurns = ws, *maxTurns
	if *debug {
		core.Log = logger
	}
	// Presentation lives in the composition. Quiet mode: labeled tool
	// calls with results indented beneath them; --debug adds full raw
	// inputs/outputs and provider details.
	core.OnEvent(func(e pons.Event) {
		switch e.Type {
		case pons.EventTurnStart:
			if e.Turn > 1 {
				fmt.Println()
			}
			if *debug {
				fmt.Printf("── turn %d ──\n", e.Turn)
			}
		case pons.EventActionStart:
			if *debug {
				fmt.Printf("▸ %s  %s\n", e.Action.Kind, argsJSON(e.Action))
			} else {
				fmt.Printf("▸ %s: %s\n", e.Action.Kind, primaryArg(e.Action))
			}
		case pons.EventActionEnd:
			obs := e.Result.Observation()
			if e.Result.Error != "" && !e.Result.OK {
				fmt.Printf("  ✗ %s\n", firstLine(e.Result.Error, 160))
				return
			}
			if *debug {
				fmt.Printf("  ◀ result ──\n%s\n", indent(obs))
				return
			}
			showResult(obs)
		}
	})
	if err := core.Use(
		fsTools,
		editTool,
		bash.New(bash.Config{Root: ws, Timeout: 60, MaxLines: 200, MaxBytes: 50 * 1024}),
		rec,
		brain,
	); err != nil {
		logger.Printf("compose failed: %v", err)
		os.Exit(1)
	}

	logger.Printf("workspace: %s", ws)

	runOnce := func(line string) {
		res, err := core.Run(context.Background(), line)
		if err != nil {
			logger.Printf("run failed: %v", err)
			return
		}
		fmt.Printf("\n── pons · %d turn(s) ──\n%s\n\n", res.Turns, res.Answer)
	}
	if *resume != "" {
		runOnce(instruction) // the continuation prompt (or an explicit --message)
	} else if *message != "" {
		runOnce(*message)
	}

	if !interactiveMode {
		return
	}

	// Interactive: one task per line; the brain keeps its conversation in
	// process (compaction applies) and the recorder appends every
	// instruction to the same session tree.
	if isTTY(os.Stdin) {
		fmt.Printf("pons — type a task per line (exit/Ctrl+D to quit)\n")
	}
	sc := bufio.NewScanner(os.Stdin)
	for {
		if isTTY(os.Stdin) {
			fmt.Print("> ")
		}
		if !sc.Scan() {
			break
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if line == "exit" || line == "quit" {
			break
		}
		runOnce(line)
	}
}

func isTTY(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// primaryArg picks the human-meaningful argument of a tool call for the
// compact transcript line.
func primaryArg(a *protocol.Action) string {
	for _, key := range []string{"command", "path", "query"} {
		if v, ok := a.Args[key]; ok {
			return firstLine(v, 100)
		}
	}
	for _, v := range a.Args {
		return firstLine(v, 100)
	}
	return ""
}

func firstLine(s string, n int) string {
	s = strings.SplitN(s, "\n", 2)[0]
	if len(s) > n {
		s = s[:n] + "…"
	}
	return s
}

// showResult prints a tool result indented under its call, capped so a
// huge output doesn't drown the transcript; --debug shows everything.
func showResult(obs string) {
	if strings.TrimSpace(obs) == "" {
		fmt.Printf("  ⇢ (no output)\n")
		return
	}
	const maxLines, maxChars = 8, 480
	lines := strings.Split(strings.TrimRight(obs, "\n"), "\n")
	shown, used := 0, 0
	for _, l := range lines {
		if shown >= maxLines || used+len(l) > maxChars {
			fmt.Printf("  ⇢ … %d more line(s) — run with --debug for the full output\n", len(lines)-shown)
			return
		}
		fmt.Printf("  ⇢ %s\n", l)
		used += len(l)
		shown++
	}
}

// indent prefixes every line with two spaces (debug mode).
func indent(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = "  " + l
	}
	return strings.Join(lines, "\n")
}

func argsJSON(a *protocol.Action) string {
	b, err := json.Marshal(a.Args)
	if err != nil {
		return "{}"
	}
	s := string(b)
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}
