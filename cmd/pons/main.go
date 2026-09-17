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
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/samperrin/pons"
	"github.com/samperrin/pons/plugins/bash"
	"github.com/samperrin/pons/plugins/brain/llm"
	"github.com/samperrin/pons/plugins/edit"
	"github.com/samperrin/pons/plugins/external"
	"github.com/samperrin/pons/plugins/fs"
	"github.com/samperrin/pons/plugins/sessionrecorder"
	"github.com/samperrin/pons/plugins/sessionsjsonl"
	"github.com/samperrin/pons/protocol"
)

func main() {
	logger := log.New(os.Stderr, "", log.LstdFlags)

	// First Ctrl+C/SIGTERM cancels the current run (external plugin
	// children are shut down by their defers); a second signal kills.
	rootCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	provider := flag.String("provider", "anthropic", "LLM provider: anthropic | openai | codex | openai-responses")
	model := flag.String("model", "", "model id (default: provider default)")
	baseURL := flag.String("base-url", "", "override provider endpoint (for OpenAI-compatible servers)")
	workspace := flag.String("workspace", "", "workspace root the tools are jailed to (default: current directory)")
	sessionDir := flag.String("session-dir", "", "session directory (default: ~/.pons/sessions/<munged-project-path>/ — durable per project, one JSONL file per session)")
	compactChars := flag.Int("compact-chars", 0, "conversation size (chars) before auto-compaction; 0 = default ~400k, negative = off")
	debug := flag.Bool("debug", false, "verbose: raw tool results, provider turn details")
	message := flag.String("message", "", "the task (required unless --resume, where a continuation prompt is implied)")
	resume := flag.String("resume", "", `continue a previous session: "latest" or a session id`)
	asAuth := flag.String("as", "", "codex login: store credentials under this auth id (default: codex)")
	login := flag.Bool("login", false, "codex only: authenticate with ChatGPT (browser flow), save credentials, and exit")
	maxTurns := flag.Int("max-turns", 12, "loop budget")
	// Tool output caps: everything the model can be handed back is bounded,
	// and every bound is tunable from the composition.
	fsReadBytes := flag.Int("fs-read-bytes", 0, "read_file byte cap; 0 = 256KiB default, negative = unlimited")
	bashTimeout := flag.Int("bash-timeout", 60, "default bash timeout seconds; 0 = no default timeout")
	bashMaxLines := flag.Int("bash-max-lines", 200, "bash output tail-truncation lines")
	bashMaxBytes := flag.Int("bash-max-bytes", 50*1024, "bash output byte cap")
	pluginMaxResultBytes := flag.Int("plugin-max-result-bytes", 0, "external plugin tool result byte cap; 0 = 1MiB default")
	var fallbackFlags repeatableStrings
	flag.Var(&fallbackFlags, "fallback", "fallback provider as provider or provider:model (repeatable; tried in order when the primary fails)")
	interactive := flag.Bool("i", false, "interactive: read tasks from stdin, one per line; the conversation continues in-process")
	pluginPath := flag.String("plugin-path", "", "PATH supplied to external plugin children (credentials are not inherited by default)")
	var pluginPaths repeatableStrings
	flag.Var(&pluginPaths, "plugin", "explicit external hands plugin manifest (repeatable; never discovered implicitly)")
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

	// Durable settings: ~/.pons/config.json (global) + .pons.json in the
	// workspace (project). Explicit flags always win over both.
	home, _ := os.UserHomeDir()
	cfg, err := loadSettings(home, ws)
	if err != nil {
		logger.Printf("%v", err)
		os.Exit(1)
	}
	setFlags := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { setFlags[f.Name] = true })
	applyString := func(name string, dst *string, v *string) {
		if !setFlags[name] && v != nil {
			*dst = *v
		}
	}
	applyInt := func(name string, dst *int, v *int) {
		if !setFlags[name] && v != nil {
			*dst = *v
		}
	}
	applyString("provider", provider, cfg.Provider)
	applyString("model", model, cfg.Model)
	applyString("base-url", baseURL, cfg.BaseURL)
	applyString("session-dir", sessionDir, cfg.SessionDir)
	applyInt("max-turns", maxTurns, cfg.MaxTurns)
	applyInt("compact-chars", compactChars, cfg.CompactChars)
	applyInt("fs-read-bytes", fsReadBytes, cfg.FsReadBytes)
	applyInt("bash-timeout", bashTimeout, cfg.BashTimeout)
	applyInt("bash-max-lines", bashMaxLines, cfg.BashMaxLines)
	applyInt("bash-max-bytes", bashMaxBytes, cfg.BashMaxBytes)
	applyInt("plugin-max-result-bytes", pluginMaxResultBytes, cfg.PluginMaxResultBytes)

	// Provider failover chain: config providers + flags → ordered slots
	// (primary first, then failover entries).
	slots, serr := providerSlots(cfg, setFlags, *provider, *model, *baseURL, fallbackFlags)
	if serr != nil {
		logger.Printf("%v", serr)
		os.Exit(1)
	}
	primary, fallbacks := slots[0], slots[1:]

	brainLogger := logger
	if !*debug {
		brainLogger = nil // quiet: no provider chatter
	}
	if *asAuth != "" && !*login {
		logger.Printf("--as applies to --login")
		os.Exit(1)
	}
	if *login {
		if *provider != "codex" {
			logger.Printf("--login applies to the codex provider")
			os.Exit(1)
		}
		// Bare --login writes the singular "codex" entry; -as names a
		// specific subscription credential.
		if lerr := llm.RunLogin(context.Background(), *asAuth); lerr != nil {
			logger.Printf("login: %v", lerr)
			os.Exit(1)
		}
		return
	}

	brain, err := llm.New(llm.Config{
		Provider:     primary.Provider,
		Model:        primary.Model,
		BaseURL:      primary.BaseURL,
		APIKey:       primary.APIKey,
		Logger:       brainLogger,
		CompactChars: *compactChars,
		Fallbacks:    fallbacks,
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
	var sessionPath string
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
	} else {
		sessionPath = *sessionDir
	}
	store, err := sessionsjsonl.New(sessionPath)
	if err != nil {
		panic(err)
	}
	defer store.Close()
	logger.Printf("sessions: %s", sessionPath)

	fsTools, err := fs.New(fs.Config{Root: ws, MaxReadBytes: *fsReadBytes})
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

	// External plugins are explicit, repeatable hands-side additions. Their
	// manifests are never discovered from the workspace or current directory.
	// Built-ins are composed first so every conflict remains additive and
	// deterministic rather than last-wins.
	externalPlugins := make([]*external.Plugin, 0, len(pluginPaths))
	closeExternalPlugins := func(logErrors bool) {
		for _, externalPlugin := range slices.Backward(externalPlugins) {
			if err := externalPlugin.Close(); err != nil && logErrors {
				logger.Printf("plugin shutdown: %v", err)
			}
		}
	}
	for _, manifestPath := range pluginPaths {
		plugin, perr := external.NewHands(manifestPath, external.HostConfig{
			Workspace:   ws,
			CallTimeout: 60 * time.Second,
			Path:        *pluginPath,
			Limits:      external.Limits{MaxResultBytes: *pluginMaxResultBytes},
		})
		if perr != nil {
			closeExternalPlugins(false)
			logger.Printf("plugin %q: %v", manifestPath, perr)
			os.Exit(1)
		}
		externalPlugins = append(externalPlugins, plugin)
	}
	defer closeExternalPlugins(*debug)
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
				if e.Tool != nil && e.Tool.Source.External {
					fmt.Printf("▸ PLUGIN TOOL  %s@%s · %s  %s\n",
						e.Tool.Source.PluginName, e.Tool.Source.PluginVersion,
						e.Action.Kind, argsJSON(e.Action))
				} else {
					fmt.Printf("▸ %s  %s\n", e.Action.Kind, argsJSON(e.Action))
				}
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
	plugins := []pons.Plugin{
		fsTools,
		editTool,
		bash.New(bash.Config{Root: ws, Timeout: *bashTimeout, MaxLines: *bashMaxLines, MaxBytes: *bashMaxBytes}),
	}
	for _, plugin := range externalPlugins {
		plugins = append(plugins, plugin)
	}
	plugins = append(plugins, rec, brain)
	if err := core.Use(plugins...); err != nil {
		closeExternalPlugins(false)
		logger.Printf("compose failed: %v", err)
		os.Exit(1)
	}
	if *debug {
		for _, plugin := range externalPlugins {
			info := plugin.Host().PluginInfo()
			logger.Printf("external plugin loaded: %s@%s (%d tool(s))", info.Name, info.Version, len(plugin.Tools()))
		}
	}

	logger.Printf("workspace: %s", ws)

	runOnce := func(line string) {
		res, err := core.Run(rootCtx, line)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				logger.Printf("interrupted")
				return
			}
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

	if !interactiveMode || rootCtx.Err() != nil {
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
		if rootCtx.Err() != nil {
			break
		}
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

type repeatableStrings []string

func (r *repeatableStrings) String() string { return strings.Join(*r, ",") }

func (r *repeatableStrings) Set(value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("plugin path must not be empty")
	}
	*r = append(*r, value)
	return nil
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
	args, err := protocol.ObjectArgs(a.Args)
	if err != nil {
		return firstLine(string(a.Args), 100)
	}
	for _, key := range []string{"command", "path", "query"} {
		if v, ok := args[key]; ok {
			return firstLine(jsonValuePreview(v), 100)
		}
	}
	for _, v := range args {
		return firstLine(jsonValuePreview(v), 100)
	}
	return ""
}

func jsonValuePreview(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(raw)
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
	s := string(a.Args)
	if s == "" || s == "null" {
		s = "{}"
	}
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}
