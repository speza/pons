// Package llm is the LLM-driven brain plugin.
//
// Architecture (mirrors how pi's agent loop works):
//
//   - ONE model call per turn. NextActions sends the conversation and the
//     tool schemas; tool_use blocks in the response become protocol.Actions.
//   - Interpret is bookkeeping, not a second model call: tool results are
//     queued and sent back as tool_result blocks with the next call.
//   - A text-only response (no tool calls) is the finish signal.
//
// The conversation is modeled with a sealed Block interface — invalid
// content states are unrepresentable. Provider adapters (anthropic, openai)
// translate that model to their wire formats; the brain↔hands protocol
// carries action arguments as typed JSON bytes and leaves decoding to tools.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/samperrin/pons"
	"github.com/samperrin/pons/protocol"
	"github.com/samperrin/pons/sessions"
)

// Block is one content block in a conversation turn. Sealed: only the
// three concrete types below are valid.
type Block interface{ isBlock() }

// Text is plain assistant/user text.
type Text struct{ Value string }

// ToolUse is the model requesting an action (tool call).
type ToolUse struct {
	ID    string
	Name  string
	Input map[string]any
}

// Result is a tool result returned to the model.
type Result struct {
	ToolUseID string
	Content   string
	IsError   bool
}

func (Text) isBlock()    {}
func (ToolUse) isBlock() {}
func (Result) isBlock()  {}

// Raw carries one provider-native conversation item verbatim (e.g. a
// Responses reasoning item that must be replayed between turns). Adapters
// that don't understand it pass it through untouched.
type Raw struct{ Item json.RawMessage }

func (Raw) isBlock() {}

// Turn is one message in the conversation.
type Turn struct {
	Role   string // "user" | "assistant"
	Blocks []Block
}

// Client speaks to one provider. Adapters: anthropic.go, openai.go.
type Client interface {
	// Complete sends the conversation and returns the assistant turn.
	Complete(ctx context.Context, system string, turns []Turn, tools []pons.ToolSpec) (Turn, error)
}

// Config tunes the brain.
type Config struct {
	Provider    string // "anthropic", "openai", "codex" (pi subscription), "openai-responses"
	Model       string // provider-specific; defaults per provider
	APIKey      string // falls back to ANTHROPIC_API_KEY / OPENAI_API_KEY
	BaseURL     string // override the provider endpoint
	AuthPath    string // codex only: pons auth-file path (default ~/.pons/auth.json)
	MaxTokens   int    // default 4096
	SystemExtra string // appended to the system prompt
	Logger      *log.Logger

	// Compaction: when the conversation (estimate) exceeds CompactChars,
	// older turns are summarized into one user message, keeping the last
	// CompactKeep turns verbatim. 0 = default (~400k chars); negative
	// disables. The session transcript file is never touched — it keeps
	// everything, greppable via $PONS_SESSION_FILE.
	CompactChars int
	CompactKeep  int
}

// Brain is the LLM-driven ControlPort.
type Brain struct {
	cfg         Config
	model       string
	client      Client
	core        *pons.Core
	turns       []Turn
	pending     []Result
	seenMessage string // last message folded into context
	logf        func(string, ...any)
}

// New creates the brain plugin (installed with pons.Core.Use).
func New(cfg Config) (*Brain, error) {
	provider := cfg.Provider
	if provider == "" {
		provider = "anthropic"
	}
	b := &Brain{cfg: cfg}
	if cfg.Logger != nil {
		b.logf = cfg.Logger.Printf
	} else {
		b.logf = func(string, ...any) {}
	}

	var client Client
	switch provider {
	case "anthropic":
		key := cfg.APIKey
		if key == "" {
			key = os.Getenv("ANTHROPIC_API_KEY")
		}
		if key == "" {
			return nil, errors.New("llm: no API key — set ANTHROPIC_API_KEY (or Config.APIKey)")
		}
		model := cfg.Model
		if model == "" {
			model = "claude-sonnet-4-5"
		}
		baseURL := cfg.BaseURL
		if baseURL == "" {
			baseURL = "https://api.anthropic.com"
		}
		ac := newAnthropicClient(key, baseURL, b.maxTokens())
		ac.model = model
		client = ac
		b.model = model
	case "codex":
		// ChatGPT-subscription auth from pons's own auth file (~/.pons/auth.json,
		// created by --login; refreshed tokens are written back to it).
		authPath := cfg.AuthPath
		if authPath == "" {
			def, derr := DefaultCodexAuthPath()
			if derr != nil {
				return nil, derr
			}
			authPath = def
		}
		auth, err := loadCodexAuth(authPath)
		if err != nil {
			return nil, err
		}
		model := cfg.Model
		if model == "" {
			model = "gpt-5.6-terra"
		}
		baseURL := cfg.BaseURL
		if baseURL == "" {
			baseURL = codexBaseURL
		}
		client = &responsesClient{auth: auth, baseURL: baseURL, model: model, maxTokens: b.maxTokens()}
		b.model = model
	case "openai-responses":
		// Responses API with a plain API key.
		key := cfg.APIKey
		if key == "" {
			key = os.Getenv("OPENAI_API_KEY")
		}
		if key == "" {
			return nil, errors.New("llm: no API key — set OPENAI_API_KEY (or Config.APIKey)")
		}
		model := cfg.Model
		if model == "" {
			model = "gpt-5.6-terra"
		}
		baseURL := cfg.BaseURL
		if baseURL == "" {
			baseURL = "https://api.openai.com/v1"
		}
		client = &responsesClient{apiKey: key, baseURL: baseURL, model: model, maxTokens: b.maxTokens()}
		b.model = model
	case "openai":
		key := cfg.APIKey
		if key == "" {
			key = os.Getenv("OPENAI_API_KEY") // empty is fine for local servers
		}
		model := cfg.Model
		if model == "" {
			model = "gpt-4o-mini"
		}
		baseURL := cfg.BaseURL
		if baseURL == "" {
			baseURL = "https://api.openai.com/v1"
		}
		oc := newOpenAIClient(key, baseURL, b.maxTokens())
		oc.model = model
		client = oc
		b.model = model
	default:
		return nil, fmt.Errorf("llm: unknown provider %q (want \"anthropic\", \"openai\", \"codex\", or \"openai-responses\")", provider)
	}
	b.client = client
	return b, nil
}

func (b *Brain) maxTokens() int {
	if b.cfg.MaxTokens > 0 {
		return b.cfg.MaxTokens
	}
	return 4096
}

// Setup installs the brain and captures the core (for ToolSpecs discovery).
func (b *Brain) Setup(c *pons.Core) error {
	b.core = c
	return c.SetBrain(b)
}

func (b *Brain) NextActions(ctx context.Context, obs protocol.Observation) ([]protocol.Action, error) {
	// Fold queued tool results into the conversation before the next call.
	if len(b.pending) > 0 {
		blocks := make([]Block, 0, len(b.pending))
		for _, r := range b.pending {
			blocks = append(blocks, r)
		}
		b.turns = append(b.turns, Turn{Role: "user", Blocks: blocks})
		b.pending = nil
	}
	if len(b.turns) == 0 {
		// First turn: the goal as the opening user message.
		b.turns = append(b.turns, Turn{Role: "user", Blocks: []Block{Text{Value: userPrompt(obs)}}})
	} else if obs.Message != "" && obs.Message != b.seenMessage {
		// A new instruction arrived (interactive follow-up, resumed run
		// with a fresh goal): append it with a fresh <env> block. This is
		// intentionally independent of pending results: an instruction
		// arriving after an exhausted run must not be discarded.
		b.turns = append(b.turns, Turn{Role: "user", Blocks: []Block{Text{Value: userPrompt(obs)}}})
	}
	b.seenMessage = obs.Message

	if b.shouldCompact() {
		if err := b.compact(ctx); err != nil {
			if b.logf != nil {
				b.logf("[llm] compaction skipped: %v", err)
			}
		}
	}

	assistant, err := b.client.Complete(ctx, b.systemPrompt(), b.turns, b.core.ToolSpecs())
	if err != nil {
		return nil, fmt.Errorf("llm: %w", err)
	}
	b.turns = append(b.turns, assistant)

	var calls []ToolUse
	var text []string
	for _, blk := range assistant.Blocks {
		switch t := blk.(type) {
		case ToolUse:
			calls = append(calls, t)
		case Text:
			text = append(text, t.Value)
		}
	}
	summary := strings.TrimSpace(strings.Join(text, "\n"))
	if b.logf != nil {
		b.logf("[llm] turn %d: model=%s calls=%d text=%d chars", obs.Turn, b.model, len(calls), len(summary))
	}

	if len(calls) == 0 {
		// A text-only reply is the finish signal — but an empty one is a
		// degenerate response (vague instruction, provider hiccup). Surface
		// it as an error instead of a silent no-op answer.
		if summary == "" {
			return nil, fmt.Errorf("llm: model returned an empty response (no tool calls, no text) — rephrase or retry")
		}
		return []protocol.Action{pons.Finish(summary)}, nil
	}
	actions := make([]protocol.Action, 0, len(calls))
	for _, c := range calls {
		actions = append(actions, protocol.Action{
			ID:   c.ID, // tool_use id correlates with the tool_result
			Kind: protocol.ActionKind(c.Name),
			Args: stringify(c.Input),
		})
	}
	return actions, nil
}

func (b *Brain) Interpret(ctx context.Context, obs protocol.Observation, tr protocol.ToolResult) (protocol.Interpretation, error) {
	// One canonical observation text for every provider: tool-rendered
	// output plus the failure note, as defined by the protocol.
	content := tr.Observation()
	if strings.TrimSpace(content) == "" {
		content = "(no output)"
	}
	b.pending = append(b.pending, Result{ToolUseID: tr.ActionID, Content: content, IsError: !tr.OK})
	return protocol.Interpretation{Continue: true, Summary: content}, nil
}

func (b *Brain) Close(ctx context.Context) error { return nil }

// --- context compaction ---------------------------------------------------

// contextChars is a rough size estimate of what the next Complete call will
// send (chars, not tokens — a tokenizer-free approximation is fine for a
// trigger threshold).
func (b *Brain) contextChars() int {
	n := len(b.systemPrompt())
	for _, t := range b.turns {
		for _, blk := range t.Blocks {
			switch blk := blk.(type) {
			case Text:
				n += len(blk.Value)
			case ToolUse:
				n += len(blk.Name)
				if bts, err := json.Marshal(blk.Input); err == nil {
					n += len(bts)
				}
			case Result:
				n += len(blk.Content)
			case Raw:
				n += len(blk.Item)
			}
		}
	}
	return n
}

func (b *Brain) shouldCompact() bool {
	budget := b.cfg.CompactChars
	if budget == 0 {
		budget = 400_000 // ≈100k tokens
	}
	return budget >= 0 && b.contextChars() > budget
}

// compact summarizes all but the goal and the most recent turns into a
// single user message. Best effort: a failed summary call leaves the
// conversation untouched (the next turn simply retries). The session
// transcript file is not modified — it keeps everything, and
// $PONS_SESSION_FILE stays the agent's grep-able record.
func (b *Brain) compact(ctx context.Context) error {
	keep := b.cfg.CompactKeep
	if keep <= 0 {
		keep = 6
	}
	n := len(b.turns)
	if n <= keep+1 {
		return nil // only the goal + tail: nothing to summarize
	}
	start := n - keep
	// A tool-use turn and its result turn are one indivisible provider
	// exchange. Keeping a result without its preceding assistant tool call
	// makes both Anthropic and OpenAI reject the next request. Expand the
	// retained tail when the nominal boundary lands on a result turn.
	if start > 1 && turnHasResult(b.turns[start]) {
		start--
	}
	if start <= 1 {
		return nil
	}
	middle := b.turns[1:start]
	tail := b.turns[start:]

	sumTurn, err := b.client.Complete(ctx, summarizeSystem,
		[]Turn{{Role: "user", Blocks: []Block{Text{Value: renderTurns(middle)}}}}, nil)
	if err != nil {
		return err
	}
	var summary strings.Builder
	for _, blk := range sumTurn.Blocks {
		if t, ok := blk.(Text); ok {
			summary.WriteString(t.Value)
		}
	}
	compacted := Turn{Role: "user", Blocks: []Block{Text{Value: fmt.Sprintf(
		"[Earlier conversation compacted into this summary — the full transcript remains in $PONS_SESSION_FILE.]\n\n%s",
		strings.TrimSpace(summary.String()))}}}
	if b.logf != nil {
		b.logf("[llm] compacted %d turns into %d-char summary (keeping %d recent)", len(middle), summary.Len(), keep)
	}
	b.turns = append([]Turn{b.turns[0], compacted}, tail...)
	return nil
}

const summarizeSystem = "Summarize this portion of an agent session transcript. " +
	"Preserve: the goal, decisions made, file paths, commands run and their outcomes, " +
	"errors and how they were resolved, and the current state of work. Be terse but complete."

func turnHasResult(t Turn) bool {
	for _, blk := range t.Blocks {
		if _, ok := blk.(Result); ok {
			return true
		}
	}
	return false
}

// renderTurns flattens turns into bounded text for the summarizer.
func renderTurns(turns []Turn) string {
	var sb strings.Builder
	for i, t := range turns {
		fmt.Fprintf(&sb, "\n[%d] %s: ", i, t.Role)
		for _, blk := range t.Blocks {
			switch blk := blk.(type) {
			case Text:
				sb.WriteString(truncText(blk.Value, 2000))
			case ToolUse:
				input, _ := json.Marshal(blk.Input)
				fmt.Fprintf(&sb, " → tool %s(%s)", blk.Name, truncText(string(input), 500))
			case Result:
				fmt.Fprintf(&sb, " → result: %s", truncText(blk.Content, 2000))
			case Raw:
				sb.WriteString(" → (provider-native item)")
			}
		}
	}
	return sb.String()
}

func truncText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// userPrompt renders a new instruction with the environment the brain is
// working in: the jailed workspace (the model is blind to everything not
// stated here), the platform, and the date.
func userPrompt(obs protocol.Observation) string {
	now := obs.Now
	if now.IsZero() {
		now = time.Now()
	}
	var sb strings.Builder
	sb.WriteString("<env>\n")
	fmt.Fprintf(&sb, "cwd: %s\n", obs.Workspace)
	fmt.Fprintf(&sb, "os: %s/%s\n", runtime.GOOS, runtime.GOARCH)
	fmt.Fprintf(&sb, "date: %s\n", now.Format("2006-01-02"))
	sb.WriteString("</env>\n\n")
	sb.WriteString(obs.Message)
	return sb.String()
}

func (b *Brain) systemPrompt() string {
	var sb strings.Builder
	sb.WriteString("You are a meticulous coding agent (the brain) working through a harness. ")
	sb.WriteString("The harness executes the tools; you plan, verify, and decide when you are done.\n\n")
	sb.WriteString("Each turn you either:\n")
	sb.WriteString("- call one or more tools to make progress, or\n")
	sb.WriteString("- reply with text only — that ends the task; the reply is the final answer.\n\n")
	sb.WriteString("Guidelines:\n")
	sb.WriteString("- If the user's message needs no tools at all (a greeting, small talk, a question about you), reply directly — do not run orientation commands.\n")
	sb.WriteString("- Read a file before editing it; keep SEARCH blocks short and unique in the file.\n")
	sb.WriteString("- Prefer small, verifiable steps; verify your work with commands before finishing.\n")
	sb.WriteString("- Failed tools are observations, not crashes: adjust and retry instead of repeating identical calls.\n")
	sb.WriteString("- Your full transcript is recorded to $PONS_SESSION_FILE (when set) as NDJSON. Use bash (grep/tail) to review anything that has fallen out of context.\n")
	sb.WriteString("- Action kinds you call that no plugin provides will come back as errors; use the tools that exist.\n")
	if b.cfg.SystemExtra != "" {
		sb.WriteString("\n" + b.cfg.SystemExtra + "\n")
	}
	return sb.String()
}

// decodeToolInput preserves JSON number lexemes with json.Number. Decoding
// provider arguments through float64 would round integers above 2^53 before
// they reach a typed external tool.
func decodeToolInput(raw []byte) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var input map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&input); err != nil {
		return nil
	}
	return input
}

// stringify preserves model tool input as typed JSON instead of coercing
// booleans, numbers, arrays, and nested objects to strings.
func stringify(in map[string]any) json.RawMessage {
	if in == nil {
		return json.RawMessage(`{}`)
	}
	b, err := json.Marshal(in)
	if err != nil {
		return nil
	}
	return b
}

// --- resume ----------------------------------------------------------------

// TurnsFromEntries converts recorded session entries into conversation
// turns for the brain: user entries become user messages, action entries
// become assistant tool-use turns (decoded from the recorded payload),
// result entries become user tool-result turns. Finish actions and empty
// records are skipped — they are loop bookkeeping, not conversation.
func TurnsFromEntries(entries []sessions.Entry) []Turn {
	var turns []Turn
	for _, e := range entries {
		switch e.Kind {
		case sessions.KindUser, sessions.KindNote:
			turns = append(turns, Turn{Role: "user", Blocks: []Block{Text{Value: e.Text}}})
		case sessions.KindAssistant:
			if e.Text != "" {
				turns = append(turns, Turn{Role: "assistant", Blocks: []Block{Text{Value: e.Text}}})
			}
		case sessions.KindAction:
			var actions []protocol.Action
			if len(e.Payload) > 0 {
				_ = json.Unmarshal(e.Payload, &actions)
			}
			var blocks []Block
			for _, a := range actions {
				if a.Kind == protocol.ActFinish {
					continue // loop bookkeeping, not model context
				}
				var input map[string]any
				if len(a.Args) > 0 {
					_ = json.Unmarshal(a.Args, &input)
				}
				if input == nil {
					input = map[string]any{}
				}
				blocks = append(blocks, ToolUse{ID: a.ID, Name: string(a.Kind), Input: input})
			}
			if len(blocks) > 0 {
				turns = append(turns, Turn{Role: "assistant", Blocks: blocks})
			}
		case sessions.KindResult:
			var results []protocol.ToolResult
			if len(e.Payload) > 0 {
				_ = json.Unmarshal(e.Payload, &results)
			}
			var blocks []Block
			for _, r := range results {
				blocks = append(blocks, Result{ToolUseID: r.ActionID, Content: r.Observation(), IsError: !r.OK})
			}
			if len(blocks) > 0 {
				turns = append(turns, Turn{Role: "user", Blocks: blocks})
			} else if e.Text != "" {
				turns = append(turns, Turn{Role: "user", Blocks: []Block{Text{Value: e.Text}}})
			}
		}
	}
	return turns
}

// Seed installs a hydrated conversation as the brain's context and the
// next instruction as the new user turn. Resume and compaction interplay
// for free: if the seeded context exceeds the budget, the next planning
// call compacts it.
func (b *Brain) Seed(turns []Turn, message, workspace string) {
	b.pending = nil
	b.turns = append(append([]Turn(nil), turns...), Turn{
		Role: "user", Blocks: []Block{Text{Value: userPrompt(protocol.Observation{
			Message: message, Workspace: workspace, Now: time.Now(),
		})}},
	})
	b.seenMessage = message
}
