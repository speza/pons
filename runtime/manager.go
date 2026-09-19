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
	OnError       func(error)
}

type Manager struct {
	cfg        Config
	store      Store
	sem        chan struct{}
	mu         sync.Mutex
	convs      map[string]*managedConversation
	workspaces map[string]*workspaceLease
	closed     bool
	wg         sync.WaitGroup
}

type workspaceLease struct{ token chan struct{} }
type managedConversation struct {
	manager      *Manager
	conversation Conversation
	mu           sync.Mutex
	subscribers  map[uint64]chan Event
	nextSubID    uint64
	running      bool
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
	m := &Manager{cfg: cfg, store: cfg.Store, sem: make(chan struct{}, cfg.MaxConcurrent), convs: make(map[string]*managedConversation), workspaces: make(map[string]*workspaceLease)}
	ids, err := cfg.Store.ConversationIDs(context.Background())
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		value, loadErr := cfg.Store.Conversation(context.Background(), id)
		if loadErr != nil {
			return nil, loadErr
		}
		managed := m.newManaged(value)
		m.convs[id] = managed
		if recoverErr := cfg.Store.RecoverRunning(context.Background(), id); recoverErr != nil {
			return nil, recoverErr
		}
		pending, pendingErr := cfg.Store.HasPending(context.Background(), id)
		if pendingErr != nil {
			return nil, pendingErr
		}
		if pending {
			managed.running = true
			m.wg.Add(1)
			go managed.work()
		}
	}
	return m, nil
}

func (m *Manager) newManaged(value Conversation) *managedConversation {
	return &managedConversation{manager: m, conversation: value, subscribers: make(map[uint64]chan Event)}
}

func (m *Manager) CreateConversation(ctx context.Context) (Conversation, error) {
	if err := m.checkOpen(); err != nil {
		return Conversation{}, err
	}
	value := Conversation{ID: NewID(), Workspace: m.cfg.Workspace, CreatedAt: time.Now().UTC()}
	if err := m.store.CreateConversation(ctx, value); err != nil {
		return Conversation{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return Conversation{}, ErrClosed
	}
	m.convs[value.ID] = m.newManaged(value)
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
	start := !accepted.Duplicate && !c.running
	if start {
		c.running = true
	}
	c.mu.Unlock()
	if start && !m.launch(c) {
		c.mu.Lock()
		c.running = false
		c.mu.Unlock()
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

func (c *managedConversation) work() {
	defer c.manager.wg.Done()
	for {
		c.mu.Lock()
		claim, err := c.manager.store.ClaimNext(context.Background(), c.conversation.ID)
		if err != nil || claim == nil {
			c.running = false
			c.cancel = nil
			c.mu.Unlock()
			if err != nil {
				c.manager.report(fmt.Errorf("runtime: claim conversation %s: %w", c.conversation.ID, err))
			}
			return
		}
		ctx, cancel := context.WithCancel(context.Background())
		c.cancel = cancel
		c.broadcastLocked(claim.Events...)
		c.mu.Unlock()
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
}

func (m *Manager) report(err error) {
	if err != nil && m.cfg.OnError != nil {
		m.cfg.OnError(err)
	}
}

func (c *managedConversation) execute(ctx context.Context, message InboundMessage, history []Message, run Run) (RunResult, error) {
	m := c.manager
	select {
	case m.sem <- struct{}{}:
		defer func() { <-m.sem }()
	case <-ctx.Done():
		return RunResult{}, ctx.Err()
	}
	lease := m.workspace(c.conversation.Workspace)
	select {
	case lease.token <- struct{}{}:
		defer func() { <-lease.token }()
	case <-ctx.Done():
		return RunResult{}, ctx.Err()
	}
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
	managed := m.newManaged(value)
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing = m.convs[id]; existing != nil {
		return existing, nil
	}
	if m.closed {
		return nil, ErrClosed
	}
	m.convs[id] = managed
	return managed, nil
}

func (m *Manager) launch(c *managedConversation) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return false
	}
	m.wg.Add(1)
	go c.work()
	return true
}

func (m *Manager) workspace(path string) *workspaceLease {
	m.mu.Lock()
	defer m.mu.Unlock()
	lease := m.workspaces[path]
	if lease == nil {
		lease = &workspaceLease{token: make(chan struct{}, 1)}
		m.workspaces[path] = lease
	}
	return lease
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

// Subscribe loads catch-up events and registers the live subscriber while
// holding the same conversation lock used by durable writers.
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
	c.subscribers[id] = ch
	c.mu.Unlock()
	go func() {
		<-ctx.Done()
		c.mu.Lock()
		if current, ok := c.subscribers[id]; ok {
			delete(c.subscribers, id)
			close(current)
		}
		c.mu.Unlock()
	}()
	return ch, nil
}

func (c *managedConversation) broadcastLocked(events ...Event) {
	for _, event := range events {
		for id, subscriber := range c.subscribers {
			select {
			case subscriber <- event:
			default:
				delete(c.subscribers, id)
				close(subscriber)
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
			close(subscriber)
		}
		c.mu.Unlock()
	}
	m.wg.Wait()
	return nil
}
