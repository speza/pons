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

// Manager coordinates bounded execution and process-local event delivery.
// Store remains authoritative for conversations, runnable work, ownership,
// and durable event order; Manager's memory is only a live acceleration layer.
type Manager struct {
	cfg    Config
	store  Store
	ctx    context.Context
	cancel context.CancelFunc
	wake   chan struct{}
	// mu protects convs, active, and closed. It is never held across store calls
	// or while acquiring a liveConversation mutex.
	mu     sync.Mutex
	convs  map[string]*liveConversation
	active int
	closed bool
	wg     sync.WaitGroup
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
		wake: make(chan struct{}, 1), convs: make(map[string]*liveConversation),
	}
	// The scheduler remains in the wait group for the Manager's whole open
	// lifetime. Runs are added only by that scheduler, so Close cannot race a
	// zero counter with a new Add.
	m.wg.Add(1)
	go m.schedule()
	m.notify()
	return m, nil
}

func (m *Manager) CreateConversation(ctx context.Context) (Conversation, error) {
	if err := m.checkOpen(); err != nil {
		return Conversation{}, err
	}
	value := Conversation{
		ID:        NewID(),
		Workspace: m.cfg.Workspace,
		CreatedAt: time.Now().UTC().Truncate(time.Microsecond),
	}
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
