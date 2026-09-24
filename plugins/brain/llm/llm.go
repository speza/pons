// Package llm is the LLM-driven brain plugin.
//
// Architecture (mirrors how pi's agent loop works):
//
//   - One model call per agent turn. Respond sends the conversation and the
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
	"slices"
	"strings"
	"time"

	"github.com/samperrin/pons"
	"github.com/samperrin/pons/protocol"
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

// Fallback is one named provider slot in the brain's provider chain: the
// primary slot plus each fallback in order. The ID is a stable label for
// selection and logs — and for codex slots, the auth-store entry the
// slot's credentials live under; the rest configures one provider.
type Fallback struct {
	ID       string // stable name (e.g. "codex-personal"); optional
	Provider string // "anthropic", "openai", "codex", "openai-responses"
	Model    string
	BaseURL  string
	APIKey   string // falls back to the provider's env key
}

// failoverClient tries provider slots in order. The conversation model
// (sealed Blocks) is provider-agnostic, so switching mid-run is a clean
// handoff: the next provider replays the same history. Any Complete error
// except caller cancellation triggers the next slot; when every slot
// fails, the last error is returned.
type failoverClient struct {
	clients []Client
	names   []string
	logf    func(string, ...any)
}

func (f *failoverClient) Complete(ctx context.Context, system string, turns []Turn, tools []pons.ToolSpec) (Turn, error) {
	var lastErr error
	for i, c := range f.clients {
		turn, err := c.Complete(ctx, system, turns, tools)
		if err == nil {
			if lastErr != nil && f.logf != nil {
				f.logf("[llm] recovered on provider %s", f.names[i])
			}
			return turn, nil
		}
		if ctx.Err() != nil || errors.Is(err, context.Canceled) {
			return Turn{}, err
		}
		lastErr = err
		if f.logf != nil {
			f.logf("[llm] provider %s failed: %v", f.names[i], err)
		}
	}
	return Turn{}, lastErr
}

// Config tunes the brain.
type Config struct {
	ID        string // primary slot's stable name, as in Fallback.ID; empty = "primary"
	Provider  string // "anthropic", "openai", "codex" (ChatGPT subscription), "openai-responses"
	Model     string // provider-specific; defaults per provider
	APIKey    string // falls back to ANTHROPIC_API_KEY / OPENAI_API_KEY
	BaseURL   string // override the provider endpoint
	MaxTokens int    // default 4096
	Persona   string // the agent's identity; empty uses a neutral default
	// Memory, when set, adds the harness rules for keeping memory and loads
	// the memory index, hydrated as attributed data into a conversation's
	// first message and again after each compaction.
	Memory func() (Memory, error)
	Logger *log.Logger
	// OnEvent receives completed model output and compaction before the brain
	// advances. Runtime persistence errors stop the run.
	OnEvent func(BrainEvent) error

	// Compaction: when the conversation (estimate) exceeds CompactChars,
	// older turns are summarized into one user message, keeping the last
	// CompactKeep turns verbatim. 0 = default (~400k chars); negative
	// disables.
	CompactChars int
	CompactKeep  int

	// Fallbacks are tried in order when the primary provider fails to
	// complete a turn (provider outage, rate limit). Each slot is a full
	// provider configuration; see Fallback.
	Fallbacks []Fallback
}

type BrainEvent struct {
	Type           string
	Turn           int
	Blocks         []Block
	Summary        string
	CompactedTurns int
	RetainedTurns  int
	Turns          []Turn
}

var errEventPersistence = errors.New("llm: persist event")

// Brain is the LLM-driven ControlPort. It holds the conversation in
// process and is not safe for concurrent use: drive one Core.Run at a
// time (the interactive CLI does this).
type Brain struct {
	cfg           Config
	model         string
	client        Client
	core          *pons.Core
	turns         []Turn
	pending       []Result
	seenMessage   string // last message folded into context
	seededPending bool   // first response may replace the seed after an agent-start hook
	hands         Hands  // what the seeded run's hands see; shown in <env>
	logf          func(string, ...any)
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

	primaryID := cfg.ID
	if primaryID == "" {
		primaryID = "primary"
	}
	slots := make([]Fallback, 0, 1+len(cfg.Fallbacks))
	slots = append(slots, Fallback{
		ID:       primaryID,
		Provider: provider,
		Model:    cfg.Model,
		BaseURL:  cfg.BaseURL,
		APIKey:   cfg.APIKey,
	})
	slots = append(slots, cfg.Fallbacks...)

	clients := make([]Client, 0, len(slots))
	names := make([]string, 0, len(slots))
	var model string
	for i, slot := range slots {
		c, m, err := providerClient(slot, b.maxTokens())
		if err != nil {
			if i == 0 {
				return nil, err
			}
			return nil, fmt.Errorf("llm: fallback %q: %w", slot.ID, err)
		}
		clients = append(clients, c)
		names = append(names, fmt.Sprintf("%s(%s/%s)", slot.ID, slot.Provider, m))
		if i == 0 {
			model = m
		}
	}
	b.model = model
	if len(clients) == 1 {
		b.client = clients[0]
	} else {
		b.client = &failoverClient{clients: clients, names: names, logf: b.logf}
	}
	return b, nil
}

// providerClient builds one provider slot: key resolution from env, the
// provider's default model and endpoint, and the Client constructor.
// codex slots resolve credentials from the pons auth store; entries are
// keyed by slot id.
func providerClient(slot Fallback, maxTokens int) (Client, string, error) {
	provider := slot.Provider
	if provider == "" {
		provider = "anthropic"
	}
	switch provider {
	case "anthropic":
		key := slot.APIKey
		if key == "" {
			key = os.Getenv("ANTHROPIC_API_KEY")
		}
		if key == "" {
			return nil, "", errors.New("llm: no API key — set ANTHROPIC_API_KEY (or Config.APIKey)")
		}
		model := slot.Model
		if model == "" {
			model = "claude-sonnet-4-5"
		}
		baseURL := slot.BaseURL
		if baseURL == "" {
			baseURL = "https://api.anthropic.com"
		}
		ac := newAnthropicClient(key, baseURL, maxTokens)
		ac.model = model
		return ac, model, nil
	case "codex":
		// ChatGPT-subscription auth from pons's auth store (~/.pons/auth.json,
		// created by --login -as <id>; refreshed tokens are written back to it).
		authPath, derr := DefaultCodexAuthPath()
		if derr != nil {
			return nil, "", derr
		}
		// Auth resolution: the slot's own id names the store entry —
		// login with -as <id> to match. Synthetic slot ids (unnamed
		// primary, ad-hoc flag) resolve to the default "codex" entry,
		// which is what a bare --login writes.
		authID := slot.ID
		switch slot.ID {
		case "", "primary", "flag":
			authID = LegacyAuthID
		}
		auth, err := loadCodexAuth(authPath, authID)
		if err != nil {
			return nil, "", err
		}
		model := slot.Model
		if model == "" {
			model = "gpt-5.6-terra"
		}
		baseURL := slot.BaseURL
		if baseURL == "" {
			baseURL = codexBaseURL
		}
		return &responsesClient{auth: auth, baseURL: baseURL, model: model, maxTokens: maxTokens}, model, nil
	case "openai-responses":
		// Responses API with a plain API key.
		key := slot.APIKey
		if key == "" {
			key = os.Getenv("OPENAI_API_KEY")
		}
		if key == "" {
			return nil, "", errors.New("llm: no API key — set OPENAI_API_KEY (or Config.APIKey)")
		}
		model := slot.Model
		if model == "" {
			model = "gpt-5.6-terra"
		}
		baseURL := slot.BaseURL
		if baseURL == "" {
			baseURL = "https://api.openai.com/v1"
		}
		return &responsesClient{apiKey: key, baseURL: baseURL, model: model, maxTokens: maxTokens}, model, nil
	case "openai":
		key := slot.APIKey
		if key == "" {
			key = os.Getenv("OPENAI_API_KEY") // empty is fine for local servers
		}
		model := slot.Model
		if model == "" {
			model = "gpt-4o-mini"
		}
		baseURL := slot.BaseURL
		if baseURL == "" {
			baseURL = "https://api.openai.com/v1"
		}
		oc := newOpenAIClient(key, baseURL, maxTokens)
		oc.model = model
		return oc, model, nil
	default:
		return nil, "", fmt.Errorf("llm: unknown provider %q (want \"anthropic\", \"openai\", \"codex\", or \"openai-responses\")", provider)
	}
}

// NewProviderClient resolves one configured provider for trusted host plugins.
// It uses the same credentials and transport as the brain, without failover.
func NewProviderClient(slot Fallback) (Client, error) {
	client, _, err := providerClient(slot, 0)
	return client, err
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

// Respond preserves the ordered assistant blocks alongside the actions the
// core executes. Runtimes use this response to commit a complete semantic
// assistant message before tools begin.
func (b *Brain) Respond(ctx context.Context, obs protocol.Observation) (pons.AssistantResponse, error) {
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
		b.turns = append(b.turns, Turn{Role: "user", Blocks: []Block{Text{Value: b.userPrompt(obs)}}})
	} else if b.seededPending && obs.Message != b.seenMessage {
		// Agent-start hooks can replace the new instruction before the
		// first model call. Keep the memory block while replacing the seed.
		blocks := b.turns[len(b.turns)-1].Blocks
		if len(blocks) > 0 && isMemoryBlock(blocks[0]) {
			blocks = blocks[:1]
		} else {
			blocks = nil
		}
		blocks = append(blocks, Text{Value: b.userPrompt(obs)})
		b.turns[len(b.turns)-1] = Turn{Role: "user", Blocks: blocks}
	} else if obs.Message != "" && obs.Message != b.seenMessage {
		// A new instruction arrived (interactive follow-up, resumed run
		// with a fresh goal): append it with a fresh <env> block. This is
		// intentionally independent of pending results: an instruction
		// arriving after an exhausted run must not be discarded.
		b.turns = append(b.turns, Turn{Role: "user", Blocks: []Block{Text{Value: b.userPrompt(obs)}}})
	}
	b.seenMessage = obs.Message
	b.seededPending = false

	if b.shouldCompact() {
		if err := b.compact(ctx, obs.Turn); err != nil {
			if errors.Is(err, errEventPersistence) {
				return pons.AssistantResponse{}, err
			}
			if b.logf != nil {
				b.logf("[llm] compaction skipped: %v", err)
			}
		}
	}

	assistant, err := b.client.Complete(ctx, b.systemPrompt(), b.turns, b.core.ToolSpecs())
	if err != nil {
		return pons.AssistantResponse{}, fmt.Errorf("llm: %w", err)
	}
	if b.cfg.OnEvent != nil {
		if err := b.cfg.OnEvent(BrainEvent{Type: "model.completed", Turn: obs.Turn, Blocks: assistant.Blocks}); err != nil {
			return pons.AssistantResponse{}, fmt.Errorf("llm: record model output: %w", err)
		}
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
			return pons.AssistantResponse{}, fmt.Errorf("llm: model returned an empty response (no tool calls, no text) — rephrase or retry")
		}
		return pons.AssistantResponse{Actions: []protocol.Action{pons.Finish(summary)}}, nil
	}
	actions := make([]protocol.Action, 0, len(calls))
	for _, c := range calls {
		actions = append(actions, protocol.Action{
			ID:   c.ID, // tool_use id correlates with the tool_result
			Kind: protocol.ActionKind(c.Name),
			Args: stringify(c.Input),
		})
	}

	byID := make(map[string]protocol.Action, len(actions))
	for _, action := range actions {
		byID[action.ID] = action
	}

	parts := make([]pons.AssistantPart, 0, len(assistant.Blocks))
	for _, block := range assistant.Blocks {
		switch block := block.(type) {
		case Text:
			if block.Value != "" {
				parts = append(parts, pons.AssistantPart{Type: pons.AssistantPartText, Text: block.Value})
			}
		case ToolUse:
			parts = append(parts, pons.AssistantPart{Type: pons.AssistantPartToolCall, Action: byID[block.ID]})
		}
	}
	return pons.AssistantResponse{Parts: parts, Actions: actions}, nil
}

// ReconcileAssistantResponse updates the provider transcript after Core's
// hooks settle the response and pending tool calls. Opaque reasoning blocks
// remain in their original positions relative to the visible blocks.
func (b *Brain) ReconcileAssistantResponse(response pons.AssistantResponse) error {
	if len(b.turns) == 0 || b.turns[len(b.turns)-1].Role != "assistant" {
		return errors.New("llm: no assistant turn to reconcile")
	}

	partBlock := func(part pons.AssistantPart) (Block, error) {
		switch part.Type {
		case pons.AssistantPartText:
			return Text{Value: part.Text}, nil
		case pons.AssistantPartToolCall:
			if _, err := protocol.ObjectArgs(part.Action.Args); err != nil {
				return nil, fmt.Errorf("llm: invalid reconciled tool arguments: %w", err)
			}
			input := decodeToolInput(part.Action.Args)
			if input == nil {
				input = map[string]any{}
			}
			return ToolUse{ID: part.Action.ID, Name: string(part.Action.Kind), Input: input}, nil
		default:
			return nil, fmt.Errorf("llm: unsupported assistant part %q", part.Type)
		}
	}

	original := b.turns[len(b.turns)-1]
	updated := make([]Block, 0, len(original.Blocks)+len(response.Parts))
	next := 0
	for _, block := range original.Blocks {
		if raw, ok := block.(Raw); ok {
			updated = append(updated, raw)
			continue
		}
		if next >= len(response.Parts) {
			continue
		}
		converted, err := partBlock(response.Parts[next])
		if err != nil {
			return err
		}
		updated = append(updated, converted)
		next++
	}
	for next < len(response.Parts) {
		converted, err := partBlock(response.Parts[next])
		if err != nil {
			return err
		}
		updated = append(updated, converted)
		next++
	}
	b.turns[len(b.turns)-1].Blocks = updated
	return nil
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
// conversation untouched (the next turn simply retries).
func (b *Brain) compact(ctx context.Context, turn int) error {
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
		"[Earlier conversation compacted into this summary.]\n\n%s",
		strings.TrimSpace(summary.String()))}}}
	first := b.turns[0]
	if b.cfg.Memory != nil {
		// Compaction invalidates the prompt cache anyway, so replace the
		// conversation's original memory block with memory as it is now.
		block, err := b.memoryBlock()
		if err != nil {
			return err
		}
		first.Blocks = slices.DeleteFunc(slices.Clone(first.Blocks), isMemoryBlock)
		compacted.Blocks = append(compacted.Blocks, block)
	}
	context := append([]Turn{first, compacted}, tail...)
	if b.cfg.OnEvent != nil {
		if err := b.cfg.OnEvent(BrainEvent{
			Type: "context.compacted", Turn: turn, Summary: compacted.Blocks[0].(Text).Value,
			CompactedTurns: len(middle), RetainedTurns: len(tail), Turns: context,
		}); err != nil {
			return fmt.Errorf("%w: %v", errEventPersistence, err)
		}
	}
	if b.logf != nil {
		b.logf("[llm] compacted %d turns into %d-char summary (keeping %d recent)", len(middle), summary.Len(), keep)
	}
	b.turns = context
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
// stated here), the platform, the hands' network policy, and the date.
func (b *Brain) userPrompt(obs protocol.Observation) string {
	now := obs.Now
	if now.IsZero() {
		now = time.Now()
	}
	var sb strings.Builder
	sb.WriteString("<env>\n")
	fmt.Fprintf(&sb, "cwd: %s\n", obs.Workspace)
	platform := obs.Platform
	if platform == "" {
		platform = runtime.GOOS + "/" + runtime.GOARCH
	}
	fmt.Fprintf(&sb, "os: %s\n", platform)
	if b.hands.Network != "" {
		fmt.Fprintf(&sb, "network: %s\n", b.hands.Network)
	}
	fmt.Fprintf(&sb, "date: %s\n", now.Format("2006-01-02"))
	sb.WriteString("</env>\n\n")
	sb.WriteString(obs.Message)
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

// Hands is what a run's hands report once their environment starts.
type Hands struct {
	Workspace string
	Platform  string
	Network   string
	// MemoryPath is where the hands see the memory directory; it is used
	// only when Config.Memory is set.
	MemoryPath string
}

// Seed installs a hydrated conversation as the brain's context and the
// next instruction as the new user turn. A conversation's first instruction
// also carries the memory block, which is then ordinary history: it is
// recorded with the prepared input and replayed to later runs until a
// compaction replaces it with a freshly loaded block. Hydration and
// compaction interplay for free: if the seeded context exceeds the budget,
// the next planning call compacts it.
func (b *Brain) Seed(turns []Turn, message string, hands Hands) error {
	b.pending = nil
	b.hands = hands
	var blocks []Block
	if b.cfg.Memory != nil && len(turns) == 0 {
		block, err := b.memoryBlock()
		if err != nil {
			return err
		}
		blocks = append(blocks, block)
	}
	blocks = append(blocks, Text{Value: b.userPrompt(protocol.Observation{
		Message: message, Workspace: hands.Workspace, Platform: hands.Platform, Now: time.Now(),
	})})
	b.turns = append(append([]Turn(nil), turns...), Turn{Role: "user", Blocks: blocks})
	b.seenMessage = message
	b.seededPending = true
	return nil
}

// memoryBlock loads the memory index and renders it where the hands see
// the memory directory.
func (b *Brain) memoryBlock() (Block, error) {
	memory, err := b.cfg.Memory()
	if err != nil {
		return nil, fmt.Errorf("llm: load memory: %w", err)
	}
	return Text{Value: memory.render(b.hands.MemoryPath)}, nil
}

// isMemoryBlock reports whether a block is a rendered memory block. Memory
// text cannot open its own frame, and instructions start with <env>.
func isMemoryBlock(block Block) bool {
	text, ok := block.(Text)
	return ok && strings.HasPrefix(text.Value, memoryFrameOpen)
}

// PreparedInput returns the exact user turn Seed added, including the
// environment header, for durable context replay.
func (b *Brain) PreparedInput() Turn {
	return b.turns[len(b.turns)-1]
}
