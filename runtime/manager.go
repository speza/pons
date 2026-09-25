package runtime

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

var ErrClosed = errors.New("runtime: manager is closed")

// ErrNoActiveRun reports a stop request for a conversation with nothing
// running.
var ErrNoActiveRun = errors.New("runtime: conversation has no active run")

type Config struct {
	// Agent is the current definition of the server's agent; new
	// conversations and submissions use it. AgentRevisions must already
	// resolve its revision, and resolves the revision of each claimed run.
	Agent          AgentDefinition
	AgentRevisions AgentRevisions

	Store               Store
	Runner              Runner
	PrepareConversation func(ConversationOptions) (ConversationOptions, error)
	MaxConcurrent       int
	EnvironmentOptions  []string
	DefaultEnvironment  string

	// RepairInterval controls the low-frequency runnable-work reconciliation
	// scan. Zero uses 30 seconds; wake signals remain the primary path.
	RepairInterval time.Duration
	OnError        func(error)
}

// Manager coordinates bounded execution and process-local event delivery.
// Store remains authoritative for conversations, runnable work, ownership,
// and durable event order; Manager's memory is only a live acceleration layer.
type Manager struct {
	cfg      Config
	store    Store
	revision string
	ctx      context.Context
	cancel   context.CancelFunc
	wake     chan struct{}
	// mu protects convs, runs, active, and closed. It is never held across
	// store calls or while acquiring a liveConversation mutex.
	mu    sync.Mutex
	convs map[string]*liveConversation
	// runs holds this process's executing run for each conversation.
	runs   map[string]*activeRun
	active int
	closed bool
	wg     sync.WaitGroup
}

type activeRun struct {
	run     Run
	cancel  context.CancelFunc
	stopped bool
}

func New(cfg Config) (*Manager, error) {
	if cfg.Store == nil {
		return nil, errors.New("runtime: store is required")
	}
	if cfg.Runner == nil {
		return nil, errors.New("runtime: runner is required")
	}
	if err := cfg.Agent.Validate(); err != nil {
		return nil, err
	}
	if cfg.AgentRevisions == nil {
		return nil, errors.New("runtime: agent revisions are required")
	}
	if _, err := cfg.AgentRevisions.AgentRevision(context.Background(), cfg.Agent.ID, cfg.Agent.Revision()); err != nil {
		return nil, fmt.Errorf("runtime: current agent revision is not recorded: %w", err)
	}

	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 4
	}
	if cfg.RepairInterval <= 0 {
		cfg.RepairInterval = 30 * time.Second
	}

	if err := cfg.Store.RecoverRunning(context.Background()); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		cfg: cfg, store: cfg.Store, revision: cfg.Agent.Revision(), ctx: ctx, cancel: cancel,
		wake: make(chan struct{}, 1), convs: make(map[string]*liveConversation),
		runs: make(map[string]*activeRun),
	}
	// The scheduler remains in the wait group for the Manager's whole open
	// lifetime. Runs are added only by that scheduler, so Close cannot race a
	// zero counter with a new Add.
	m.wg.Add(1)
	go m.schedule()
	m.notify()
	return m, nil
}

func (m *Manager) CreateConversation(ctx context.Context, selected ConversationOptions) (Conversation, error) {
	if err := m.checkOpen(); err != nil {
		return Conversation{}, err
	}
	environment, err := m.environment(selected.Environment)
	if err != nil {
		return Conversation{}, err
	}
	selected.Environment = environment
	if m.cfg.PrepareConversation != nil {
		var err error
		selected, err = m.cfg.PrepareConversation(selected)
		if err != nil {
			return Conversation{}, fmt.Errorf("%w: %w", ErrInvalidConversation, err)
		}
	}
	if selected.Workspace == "" && selected.GitRepository == "" {
		return Conversation{}, fmt.Errorf("%w: workspace or Git repository is required", ErrInvalidConversation)
	}
	if selected.Workspace != "" && selected.GitRepository != "" {
		return Conversation{}, fmt.Errorf("%w: workspace and Git repository are mutually exclusive", ErrInvalidConversation)
	}
	value := Conversation{
		ID:                 NewID(),
		AgentID:            m.cfg.Agent.ID,
		Workspace:          selected.Workspace,
		GitRepository:      selected.GitRepository,
		GitRevision:        selected.GitRevision,
		GitAllRepositories: selected.GitAllRepositories,
		Environment:        selected.Environment,
		CreatedAt:          time.Now().UTC().Truncate(time.Microsecond),
	}
	value.WorkspaceLock = value.Workspace
	if selected.Environment == "e2b" || selected.GitRepository != "" {
		value.WorkspaceLock = value.ID
	}

	if err := m.store.CreateConversation(ctx, value); err != nil {
		return Conversation{}, err
	}
	if err := m.checkOpen(); err != nil {
		return Conversation{}, err
	}
	return value, nil
}

func (m *Manager) environment(requested string) (string, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		requested = strings.TrimSpace(m.cfg.DefaultEnvironment)
	}
	if requested == "" || len(m.cfg.EnvironmentOptions) == 0 {
		return requested, nil
	}
	if slices.Contains(m.cfg.EnvironmentOptions, requested) {
		return requested, nil
	}
	return "", fmt.Errorf("%w: %q is not configured", ErrInvalidEnvironment, requested)
}

// Agent returns the read-only identity of the server's current agent.
func (m *Manager) Agent() AgentSummary {
	return m.cfg.Agent.Summary()
}

// Conversations returns the durable conversations without hydrating live
// delivery state for dormant conversations.
func (m *Manager) Conversations(ctx context.Context) ([]Conversation, error) {
	if err := m.checkOpen(); err != nil {
		return nil, err
	}
	return m.store.Conversations(ctx)
}

func (m *Manager) Submit(ctx context.Context, conversationID, key string, parts []TextPart) (AcceptedMessage, error) {
	return m.SubmitInput(ctx, conversationID, InputSubmission{
		IdempotencyKey: key, Parts: parts,
		Source: InputSource{Kind: "human", Adapter: "http"},
	})
}

// SubmitInput is the common admission path for trusted ingress adapters.
// The HTTP transport uses Submit so clients cannot forge system or agent input.
func (m *Manager) SubmitInput(ctx context.Context, conversationID string, input InputSubmission) (AcceptedMessage, error) {
	if err := ValidateInputSubmission(input); err != nil {
		return AcceptedMessage{}, err
	}

	c, err := m.conversation(ctx, conversationID)
	if err != nil {
		return AcceptedMessage{}, err
	}
	c.mu.Lock()
	if c.conversation.AgentID != m.cfg.Agent.ID {
		c.mu.Unlock()
		return AcceptedMessage{}, fmt.Errorf("runtime: conversation agent %q is not configured", c.conversation.AgentID)
	}
	if input.TargetAgentID != "" && input.TargetAgentID != c.conversation.AgentID {
		c.mu.Unlock()
		return AcceptedMessage{}, errors.New("runtime: input targets another agent")
	}
	if input.AgentRevision != "" && input.AgentRevision != m.revision {
		c.mu.Unlock()
		return AcceptedMessage{}, errors.New("runtime: input selects another agent revision")
	}
	input.TargetAgentID = c.conversation.AgentID
	input.AgentRevision = m.revision
	accepted, events, err := m.store.Accept(ctx, conversationID, input)
	if err != nil {
		c.mu.Unlock()
		return AcceptedMessage{}, err
	}
	c.broadcastLocked(events...)
	c.mu.Unlock()

	if !accepted.Duplicate {
		m.notify()
	}
	return accepted, nil
}

// StopRun asks the conversation's active run to stop. The run's context is
// cancelled and, once the runner returns, the run is recorded as stopped:
// requested tools are marked interrupted and run.stopped is appended to the
// log. A run that finishes its answer before noticing still completes.
func (m *Manager) StopRun(ctx context.Context, conversationID string) (Run, error) {
	if _, err := m.conversation(ctx, conversationID); err != nil {
		return Run{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	active := m.runs[conversationID]
	if active == nil {
		return Run{}, ErrNoActiveRun
	}
	active.stopped = true
	active.cancel()
	return active.run, nil
}

// ValidateInputSubmission checks the shared contract before an ingress fact is
// appended. The store repeats this check for callers outside Manager.
func ValidateInputSubmission(input InputSubmission) error {
	if strings.TrimSpace(input.IdempotencyKey) == "" {
		return errors.New("runtime: idempotency key is required")
	}
	if err := validateParts(input.Parts); err != nil {
		return err
	}
	if strings.TrimSpace(input.Source.Adapter) == "" {
		return errors.New("runtime: input source adapter is required")
	}
	switch input.Source.Kind {
	case "human":
		return nil
	case "system":
		return nil
	case "agent":
		if input.Source.SourceConversationID == "" || input.Source.SourceAgentID == "" || input.CausationID == "" {
			return errors.New("runtime: agent input requires source conversation, agent, and causation")
		}
		return nil
	default:
		return fmt.Errorf("runtime: unsupported input source %q", input.Source.Kind)
	}
}

func validateParts(parts []TextPart) error {
	if len(parts) == 0 {
		return errors.New("runtime: at least one text part is required")
	}
	for i, part := range parts {
		if part.Type != "text" {
			return fmt.Errorf("runtime: parts[%d].type %q is not supported", i, part.Type)
		}
		if part.Text == "" {
			return fmt.Errorf("runtime: parts[%d].text is required", i)
		}
	}
	return nil
}

func messageText(parts []TextPart) string {
	values := make([]string, len(parts))
	for i, part := range parts {
		values[i] = part.Text
	}
	return strings.Join(values, "\n")
}

func (m *Manager) report(err error) {
	if err != nil && m.cfg.OnError != nil {
		m.cfg.OnError(err)
	}
}

func (m *Manager) conversation(ctx context.Context, id string) (*liveConversation, error) {
	if id == "" || strings.ContainsAny(id, `/\\`) {
		return nil, ErrNotFound
	}
	if err := m.checkOpen(); err != nil {
		return nil, err
	}

	m.mu.Lock()
	existing := m.convs[id]
	m.mu.Unlock()
	if existing != nil {
		return existing, nil
	}
	value, err := m.store.Conversation(ctx, id)
	if err != nil {
		return nil, err
	}
	return m.openLiveConversation(value)
}

func (m *Manager) openLiveConversation(value Conversation) (*liveConversation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, ErrClosed
	}
	if existing := m.convs[value.ID]; existing != nil {
		return existing, nil
	}
	live := newLiveConversation(m, value)
	m.convs[value.ID] = live
	return live, nil
}

func (m *Manager) ensureLiveConversation(value Conversation) *liveConversation {
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing := m.convs[value.ID]; existing != nil {
		return existing
	}
	live := newLiveConversation(m, value)
	m.convs[value.ID] = live
	return live
}

func (m *Manager) checkOpen() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrClosed
	}
	return nil
}

func (m *Manager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true

	// Cancel scheduling and every per-run child context before closing local
	// delivery channels. Run goroutines still record their terminal state.
	m.cancel()
	conversations := make([]*liveConversation, 0, len(m.convs))
	for _, c := range m.convs {
		conversations = append(conversations, c)
	}
	m.mu.Unlock()
	for _, c := range conversations {
		c.mu.Lock()
		for id, subscriber := range c.subscribers {
			delete(c.subscribers, id)
			close(subscriber.ch)
		}
		c.mu.Unlock()
	}
	m.wg.Wait()
	return nil
}
