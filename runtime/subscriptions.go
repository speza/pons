package runtime

import (
	"context"
	"sync"
)

type subscriber struct {
	ch         chan Event
	lastCursor uint64
}

// liveConversation holds only process-local state needed to deliver events.
// Canonical conversation state and execution ownership remain in Store.
type liveConversation struct {
	manager      *Manager
	conversation Conversation
	mu           sync.Mutex
	subscribers  map[uint64]*subscriber
	nextSubID    uint64
}

func newLiveConversation(manager *Manager, conversation Conversation) *liveConversation {
	return &liveConversation{
		manager:      manager,
		conversation: conversation,
		subscribers:  make(map[uint64]*subscriber),
	}
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

func (c *liveConversation) broadcastLocked(events ...Event) {
	// The caller holds c.mu so catch-up registration and live delivery share one
	// local order. Durable cursor checks remove the overlap where a subscriber
	// reads a committed event before its process-local broadcast arrives.
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
