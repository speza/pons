package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

var ErrClosed = errors.New("runtime: manager is closed")

type Config struct {
	Store         Store
	Workspace     string
	Runner        Runner
	MaxConcurrent int
	// RepairInterval controls the low-frequency runnable-work reconciliation
	// scan. Zero uses 30 seconds; wake signals remain the primary path.
	RepairInterval time.Duration
	OnError        func(error)
}

type Manager struct {
	cfg    Config
	store  Store
	ctx    context.Context
	cancel context.CancelFunc
	wake   chan struct{}
	mu     sync.Mutex
	convs  map[string]*managedConversation
	active int
	closed bool
	wg     sync.WaitGroup
}

type subscriber struct {
	ch         chan Event
	lastCursor uint64
}

type managedConversation struct {
	manager      *Manager
	conversation Conversation
	mu           sync.Mutex
	subscribers  map[uint64]*subscriber
	nextSubID    uint64
	cancel       context.CancelFunc
}

func New(cfg Config) (*Manager, error) {
	if cfg.Store == nil {
		return nil, errors.New("runtime: store is required")
	}
	if cfg.Runner == nil {
		return nil, errors.New("runtime: runner is required")
	}
	if cfg.Workspace == "" {
		return nil, errors.New("runtime: workspace is required")
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
		cfg: cfg, store: cfg.Store, ctx: ctx, cancel: cancel,
		wake: make(chan struct{}, 1), convs: make(map[string]*managedConversation),
	}
	m.wg.Add(1)
	go m.schedule()
	m.notify()
	return m, nil
}

func (m *Manager) newManaged(value Conversation) *managedConversation {
	return &managedConversation{manager: m, conversation: value, subscribers: make(map[uint64]*subscriber)}
}

func (m *Manager) CreateConversation(ctx context.Context) (Conversation, error) {
	if err := m.checkOpen(); err != nil {
		return Conversation{}, err
	}
	value := Conversation{ID: NewID(), Workspace: m.cfg.Workspace, CreatedAt: time.Now().UTC()}
	if err := m.store.CreateConversation(ctx, value); err != nil {
		return Conversation{}, err
	}
	if err := m.checkOpen(); err != nil {
		return Conversation{}, err
	}
	return value, nil
}

func (m *Manager) Submit(ctx context.Context, conversationID, key string, parts []TextPart) (AcceptedMessage, error) {
	if strings.TrimSpace(key) == "" {
		return AcceptedMessage{}, errors.New("runtime: idempotency key is required")
	}
	if err := validateParts(parts); err != nil {
		return AcceptedMessage{}, err
	}
	c, err := m.conversation(ctx, conversationID)
	if err != nil {
		return AcceptedMessage{}, err
	}
	c.mu.Lock()
	accepted, events, err := m.store.Accept(ctx, conversationID, key, parts)
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

func (m *Manager) schedule() {
	defer m.wg.Done()
	ticker := time.NewTicker(m.cfg.RepairInterval)
	defer ticker.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-m.wake:
		case <-ticker.C:
		}
		m.dispatch()
	}
}

func (m *Manager) dispatch() {
	for m.reserveSlot() {
		claim, err := m.store.ClaimRunnable(m.ctx)
		if err != nil {
			m.releaseSlot(false)
			if !errors.Is(err, context.Canceled) {
				m.report(fmt.Errorf("runtime: claim runnable work: %w", err))
			}
			return
		}
		if claim == nil {
			m.releaseSlot(false)
			return
		}
		c := m.managed(claim.Conversation)
		ctx, cancel := context.WithCancel(m.ctx)
		c.mu.Lock()
		c.cancel = cancel
		c.broadcastLocked(claim.Events...)
		c.mu.Unlock()
		m.wg.Add(1)
		go c.work(ctx, cancel, claim)
	}
}

func (c *managedConversation) work(ctx context.Context, cancel context.CancelFunc, claim *ClaimedRun) {
	defer c.manager.wg.Done()
	defer c.manager.releaseSlot(true)
	result, runErr := c.execute(ctx, claim.Message, claim.History, claim.Run)
	cancel()
	c.mu.Lock()
	c.cancel = nil
	var backgroundErr error
	if runErr != nil {
		toolEvents, interruptErr := c.manager.store.InterruptRequestedTools(context.Background(), claim.Run)
		if interruptErr == nil {
			c.broadcastLocked(toolEvents...)
		} else {
			runErr = errors.Join(runErr, interruptErr)
		}
		events, persistErr := c.manager.store.FailRun(context.Background(), claim.Run, runErr.Error())
		if persistErr == nil {
			c.broadcastLocked(events...)
		} else {
			backgroundErr = fmt.Errorf("runtime: persist failed run %s: %w", claim.Run.ID, persistErr)
		}
	} else {
		_, events, persistErr := c.manager.store.FinishRun(context.Background(), claim.Run, result.Answer)
		if persistErr == nil {
			c.broadcastLocked(events...)
		} else {
			backgroundErr = fmt.Errorf("runtime: persist completed run %s: %w", claim.Run.ID, persistErr)
		}
	}
	c.mu.Unlock()
	c.manager.report(backgroundErr)
}

func (m *Manager) report(err error) {
	if err != nil && m.cfg.OnError != nil {
		m.cfg.OnError(err)
	}
}

func (c *managedConversation) execute(ctx context.Context, message InboundMessage, history []Message, run Run) (RunResult, error) {
	m := c.manager
	return m.cfg.Runner.Run(ctx, RunRequest{ConversationID: c.conversation.ID, RunID: run.ID, InboundMessageID: message.ID, Workspace: c.conversation.Workspace, Text: messageText(message.Parts), Messages: history, Emit: func(event RunEvent) error { return c.emitRunEvent(run, event) }})
}

func (c *managedConversation) emitRunEvent(run Run, source RunEvent) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch source.Type {
	case EventAssistantDelta:
		messageID, partID := source.MessageID, source.PartID
		if messageID == "" {
			messageID = run.ID + ":draft"
		}
		if partID == "" {
			partID = "text"
		}
		c.broadcastLocked(Event{Type: EventAssistantDelta, ConversationID: run.ConversationID, RunID: run.ID, InboundMessageID: run.InboundMessageID, Delta: &TextDelta{MessageID: messageID, PartID: partID, Text: source.Text}, CreatedAt: time.Now().UTC()})
		return nil
	case EventToolProgress:
		c.broadcastLocked(Event{Type: EventToolProgress, ConversationID: run.ConversationID, RunID: run.ID, InboundMessageID: run.InboundMessageID, Progress: &ToolProgress{ToolCallID: source.ToolCallID, Text: source.Text}, CreatedAt: time.Now().UTC()})
		return nil
	case RunEventAssistantTurn:
		if len(source.Parts) == 0 {
			return errors.New("runtime: assistant turn is missing its parts")
		}
		events, err := c.manager.store.CommitAssistantTurn(context.Background(), run, source.Parts)
		if err == nil {
			c.broadcastLocked(events...)
		}
		return err
	case RunEventToolCompleted:
		if source.Result == nil {
			return errors.New("runtime: tool completion is missing its result")
		}
		events, err := c.manager.store.ToolCompleted(context.Background(), run, *source.Result)
		if err == nil {
			c.broadcastLocked(events...)
		}
		return err
	default:
		return fmt.Errorf("runtime: unsupported runner event %q", source.Type)
	}
}

func (m *Manager) conversation(ctx context.Context, id string) (*managedConversation, error) {
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
	return m.managedOpen(value)
}

func (m *Manager) managedOpen(value Conversation) (*managedConversation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, ErrClosed
	}
	if existing := m.convs[value.ID]; existing != nil {
		return existing, nil
	}
	managed := m.newManaged(value)
	m.convs[value.ID] = managed
	return managed, nil
}

func (m *Manager) managed(value Conversation) *managedConversation {
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing := m.convs[value.ID]; existing != nil {
		return existing
	}
	managed := m.newManaged(value)
	m.convs[value.ID] = managed
	return managed
}

func (m *Manager) reserveSlot() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.active >= m.cfg.MaxConcurrent {
		return false
	}
	m.active++
	return true
}

func (m *Manager) releaseSlot(wake bool) {
	m.mu.Lock()
	m.active--
	m.mu.Unlock()
	if wake {
		m.notify()
	}
}

func (m *Manager) notify() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *Manager) checkOpen() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrClosed
	}
	return nil
}

// View returns a coherent UI snapshot and its durable cursor.
func (m *Manager) View(ctx context.Context, conversationID string) (ConversationView, error) {
	c, err := m.conversation(ctx, conversationID)
	if err != nil {
		return ConversationView{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return m.store.View(ctx, conversationID)
}

// Subscribe loads catch-up events and registers the live subscriber under one
// local lock. Per-subscriber cursors suppress a claim event already observed
// in catch-up before its process-local broadcast arrives.
func (m *Manager) Subscribe(ctx context.Context, conversationID string, after uint64) (<-chan Event, error) {
	c, err := m.conversation(ctx, conversationID)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	backlog, err := m.store.Events(ctx, conversationID, after)
	if err != nil {
		c.mu.Unlock()
		return nil, err
	}
	ch := make(chan Event, len(backlog)+64)
	for _, event := range backlog {
		ch <- event
	}
	c.nextSubID++
	id := c.nextSubID
	c.subscribers[id] = &subscriber{ch: ch, lastCursor: after}
	if len(backlog) > 0 {
		c.subscribers[id].lastCursor = backlog[len(backlog)-1].ID
	}
	c.mu.Unlock()
	go func() {
		<-ctx.Done()
		c.mu.Lock()
		if current, ok := c.subscribers[id]; ok {
			delete(c.subscribers, id)
			close(current.ch)
		}
		c.mu.Unlock()
	}()
	return ch, nil
}

func (c *managedConversation) broadcastLocked(events ...Event) {
	for _, event := range events {
		for id, subscriber := range c.subscribers {
			if event.ID != 0 && event.ID <= subscriber.lastCursor {
				continue
			}
			select {
			case subscriber.ch <- event:
				if event.ID != 0 {
					subscriber.lastCursor = event.ID
				}
			default:
				delete(c.subscribers, id)
				close(subscriber.ch)
			}
		}
	}
}

func (m *Manager) Events(ctx context.Context, conversationID string, after uint64) ([]Event, error) {
	c, err := m.conversation(ctx, conversationID)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return m.store.Events(ctx, conversationID, after)
}

func (m *Manager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	m.cancel()
	conversations := make([]*managedConversation, 0, len(m.convs))
	for _, c := range m.convs {
		conversations = append(conversations, c)
	}
	m.mu.Unlock()
	for _, c := range conversations {
		c.mu.Lock()
		if c.cancel != nil {
			c.cancel()
		}
		for id, subscriber := range c.subscribers {
			delete(c.subscribers, id)
			close(subscriber.ch)
		}
		c.mu.Unlock()
	}
	m.wg.Wait()
	return nil
}
