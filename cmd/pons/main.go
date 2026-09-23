// Command pons is the client and server entry point for the pons runtime.
//
// Authentication: ANTHROPIC_API_KEY (provider anthropic, default) or
// OPENAI_API_KEY (provider openai, also works with any OpenAI-compatible
// server via -base-url, e.g. llama.cpp or Ollama). ChatGPT subscription
// codex auth comes from pons's own auth file via --login.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/samperrin/pons/environment/gitworkspace"
	"github.com/samperrin/pons/plugins/brain/llm"
	"github.com/samperrin/pons/protocol"
	ponsruntime "github.com/samperrin/pons/runtime"
)

func main() {
	logger := log.New(os.Stderr, "", log.LstdFlags)
	mode := "bundled"
	if len(os.Args) > 1 && (os.Args[1] == "serve" || os.Args[1] == "client") {
		mode = os.Args[1]
		os.Args = append([]string{os.Args[0]}, os.Args[2:]...)
	}

	// First Ctrl+C/SIGTERM cancels the current run (external plugin
	// children are shut down by their defers); a second signal kills.
	rootCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	provider := flag.String("provider", "anthropic", "LLM provider: anthropic | openai | codex | openai-responses")
	model := flag.String("model", "", "model id (default: provider default)")
	baseURL := flag.String("base-url", "", "override provider endpoint (for OpenAI-compatible servers)")
	workspace := flag.String("workspace", "", "host workspace selected for a new local, Seatbelt, or E2B archive conversation (default: current directory)")
	workspaceRoot := flag.String("workspace-root", "", "server-approved root for client-selected host workspaces (default: home directory)")
	stateDir := flag.String("state-dir", "", "runtime state directory (default: ~/.pons/runtime/server/)")
	compactChars := flag.Int("compact-chars", 0, "conversation size (chars) before auto-compaction; 0 = default ~400k, negative = off")
	debug := flag.Bool("debug", false, "verbose client tool results and structured server lifecycle logs")
	message := flag.String("message", "", "task to submit to the runtime")
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
	interactive := flag.Bool("i", false, "interactive runtime client: read tasks from stdin, one per line")
	pluginPath := flag.String("plugin-path", "", "PATH supplied to external plugin children (credentials are not inherited by default)")
	sandbox := flag.String("sandbox", "", `default execution environment for conversations: "seatbelt" | "e2b" (default: in-process)`)
	sandboxNetwork := flag.Bool("sandbox-network", false, "allow network access inside the per-run sandbox")
	handsCommand := flag.String("hands-command", "", "local pons-hands executable used by Seatbelt (default: find pons-hands on PATH)")
	e2bTemplate := flag.String("e2b-template", "", `E2B template containing pons-hands (default: "pons-hands")`)
	e2bHandsPath := flag.String("e2b-hands-path", "", "absolute pons-hands path inside the E2B template (default: /usr/local/bin/pons-hands)")
	gitRepository := flag.String("git-repository", "", "credential-free HTTPS repository to clone into a new E2B workspace")
	gitRevision := flag.String("git-revision", "", "full immutable commit ID to check out in a new E2B Git workspace")
	gitBranch := flag.String("git-branch", "", "remote branch to use for a new E2B Git workspace (alternative to --git-revision)")
	gitAllRepositories := flag.Bool("git-all-repositories", false, "allow this session to clone any repository available to the GitHub App installation")
	githubAppID := flag.Int64("github-app-id", 0, "deployment-owned GitHub App ID for private repository access")
	githubAppInstallationID := flag.Int64("github-app-installation-id", 0, "GitHub App installation ID whose repositories are available to E2B hands")
	githubAppPrivateKey := flag.String("github-app-private-key", "", "host path to the GitHub App private key PEM")
	sandboxIdleTimeout := flag.Duration("sandbox-idle-timeout", 10*time.Minute, "idle time before a retained remote sandbox is deleted")
	runtimeAddress := flag.String("addr", "127.0.0.1:7337", "runtime server listen address (serve mode; loopback only)")
	serverURL := flag.String("server", "http://127.0.0.1:7337", "runtime server URL (client mode)")
	conversationID := flag.String("conversation", "", "existing runtime conversation id (client mode)")
	idempotencyKey := flag.String("idempotency-key", "", "stable message retry key (client mode; generated by default)")
	runtimeConcurrency := flag.Int("runtime-concurrency", 4, "maximum active agent runs (serve mode)")
	var pluginPaths repeatableStrings
	flag.Var(&pluginPaths, "plugin", "explicit external hands plugin manifest (repeatable; never discovered implicitly)")
	flag.Parse()
	selectedGitRevision := *gitRevision
	if *gitBranch != "" {
		if *gitRevision != "" || *gitRepository == "" {
			logger.Print("--git-branch requires --git-repository and cannot be combined with --git-revision")
			os.Exit(1)
		}
		var err error
		selectedGitRevision, err = gitworkspace.BranchRef(*gitBranch)
		if err != nil {
			logger.Printf("--git-branch: %v", err)
			os.Exit(1)
		}
	}
	conversationOptions := ponsruntime.ConversationOptions{
		GitRepository: *gitRepository, GitRevision: selectedGitRevision,
		GitAllRepositories: *gitAllRepositories,
	}
	if *conversationID == "" && *gitRepository == "" {
		selected := *workspace
		if selected == "" {
			selected = "."
		}
		var err error
		conversationOptions.Workspace, err = filepath.Abs(selected)
		if err != nil {
			logger.Printf("workspace: %v", err)
			os.Exit(1)
		}
	}

	if *gitRepository != "" && *workspace != "" {
		logger.Print("--workspace and --git-repository cannot be combined")
		os.Exit(1)
	}
	if *conversationID != "" && *workspace != "" {
		logger.Print("--workspace applies only when creating a conversation")
		os.Exit(1)
	}

	if mode == "client" {
		if err := runClient(rootCtx, *serverURL, *conversationID, *idempotencyKey, *message, *interactive, *debug, conversationOptions); err != nil && !errors.Is(err, context.Canceled) {
			logger.Printf("client: %v", err)
			os.Exit(1)
		}
		return
	}

	// The current directory supplies bundled project settings, not a server-owned workspace.
	// Standalone servers use global settings. Bundled mode also reads the
	// current directory's .pons.json; explicit flags win over both files.
	home, _ := os.UserHomeDir()
	settingsWorkspace := "."
	if mode == "serve" {
		settingsWorkspace = ""
	}
	cfg, err := loadSettings(home, settingsWorkspace)
	if err != nil {
		logger.Printf("%v", err)
		os.Exit(1)
	}

	e2bAPIKey := ""
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
	applyString("state-dir", stateDir, cfg.StateDir)
	applyString("workspace-root", workspaceRoot, cfg.WorkspaceRoot)
	if cfg.Environment != nil {
		applyString("sandbox", sandbox, cfg.Environment.Sandbox)
		if cfg.Environment.E2B != nil {
			applyString("e2b-template", e2bTemplate, cfg.Environment.E2B.Template)
			if cfg.Environment.E2B.APIKey != nil {
				e2bAPIKey = *cfg.Environment.E2B.APIKey
			}
			if !setFlags["sandbox-idle-timeout"] && cfg.Environment.E2B.IdleTimeout != nil {
				*sandboxIdleTimeout, err = time.ParseDuration(*cfg.Environment.E2B.IdleTimeout)
				if err != nil {
					logger.Printf("config environment.e2b.idle_timeout: %v", err)
					os.Exit(1)
				}
			}
		}
		if cfg.Environment.GitHubApp != nil {
			app := cfg.Environment.GitHubApp
			applyString("github-app-private-key", githubAppPrivateKey, app.PrivateKey)
			if !setFlags["github-app-id"] && app.AppID != nil {
				*githubAppID = *app.AppID
			}
			if !setFlags["github-app-installation-id"] && app.InstallationID != nil {
				*githubAppInstallationID = *app.InstallationID
			}
		}
	}

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

	brainConfig := llm.Config{
		Provider:     primary.Provider,
		Model:        primary.Model,
		BaseURL:      primary.BaseURL,
		APIKey:       primary.APIKey,
		CompactChars: *compactChars,
		Fallbacks:    fallbacks,
	}

	if mode == "serve" && (*message != "" || *interactive) {
		logger.Printf("serve: --message and -i are client options")
		os.Exit(1)
	}
	if mode == "serve" && *workspace != "" {
		logger.Print("serve: --workspace belongs to the client creating a conversation; use --workspace-root to allow host paths")
		os.Exit(1)
	}
	if mode == "serve" && (conversationOptions.GitRepository != "" || conversationOptions.GitRevision != "" || conversationOptions.GitAllRepositories) {
		logger.Printf("serve: Git repository and access options belong to the client creating a conversation")
		os.Exit(1)
	}

	statePath := *stateDir
	if statePath == "" {
		statePath = filepath.Join(home, ".pons", "runtime", "server")
	}
	root := *workspaceRoot
	if root == "" {
		root = home
	}
	root, err = filepath.Abs(root)
	if err != nil {
		logger.Printf("workspace root: %v", err)
		os.Exit(1)
	}

	serverLogger := newServerLogger(os.Stderr, *debug)
	serverLogger.Info("runtime state selected", "state_dir", statePath)
	serverOpts := serverOptions{
		Address: *runtimeAddress, StateDir: statePath, WorkspaceRoot: root, ClientWorkspace: conversationOptions.Workspace,
		MaxConcurrent: *runtimeConcurrency, MaxTurns: *maxTurns, Brain: brainConfig,
		FSReadBytes: *fsReadBytes, BashTimeout: *bashTimeout, BashMaxLines: *bashMaxLines, BashMaxBytes: *bashMaxBytes,
		PluginPaths: pluginPaths, PluginPath: *pluginPath, PluginMaxResultBytes: *pluginMaxResultBytes, Debug: *debug,
		Sandbox: *sandbox, E2BTemplate: *e2bTemplate, E2BHandsPath: *e2bHandsPath,
		GitRepository: *gitRepository, GitRevision: selectedGitRevision,
		GitAllRepositories: *gitAllRepositories,
		GitHubAppID:        *githubAppID, GitHubAppInstallationID: *githubAppInstallationID, GitHubAppPrivateKey: *githubAppPrivateKey,
		E2BAPIKey:          e2bAPIKey,
		SandboxIdleTimeout: *sandboxIdleTimeout,
		EnvironmentError: func(err error) {
			serverLogger.Error("environment failure", "error", err)
		},
	}
	if *debug {
		serverOpts.EnvironmentDebug = func(event string) {
			logE2BDebug(serverLogger, event)
		}
	}
	serverOpts.Environment, serverOpts.EnvironmentSpec, err = executionEnvironment(*sandbox, *handsCommand, *sandboxNetwork, serverOpts)
	if err != nil {
		logger.Printf("sandbox: %v", err)
		os.Exit(1)
	}

	if mode == "serve" {
		if err := runServer(rootCtx, serverLogger, serverOpts); err != nil {
			serverLogger.Error("server stopped with error", "error", err)
			os.Exit(1)
		}
		return
	}
	bundledInteractive := *interactive || *message == ""
	if err := runBundled(rootCtx, serverLogger, serverOpts, *conversationID, *idempotencyKey, *message, bundledInteractive); err != nil && !errors.Is(err, context.Canceled) {
		logger.Printf("run: %v", err)
		os.Exit(1)
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
func showResult(obs string, debug bool) {
	if strings.TrimSpace(obs) == "" {
		fmt.Printf("  ⇢ (no output)\n")
		return
	}
	const maxLines, maxChars = 8, 480
	lines := strings.Split(strings.TrimRight(obs, "\n"), "\n")
	shown, used := 0, 0
	for _, l := range lines {
		if !debug && (shown >= maxLines || used+len(l) > maxChars) {
			fmt.Printf("  ⇢ … %d more line(s) — run with --debug for the full output\n", len(lines)-shown)
			return
		}
		fmt.Printf("  ⇢ %s\n", l)
		used += len(l)
		shown++
	}
}
