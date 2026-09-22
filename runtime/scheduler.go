package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"
)

func (m *Manager) schedule() {
	defer m.wg.Done()
	ticker := time.NewTicker(m.cfg.RepairInterval)
	defer ticker.Stop()
	for {
		// Wakeups reduce latency but carry no authority and may coalesce. The
		// repair tick makes durable work discoverable even if a hint is lost.
		select {
		case <-m.ctx.Done():
			return
		case <-m.wake:
		case <-ticker.C:
		}
		m.dispatch()
	}
}

// dispatch reserves local capacity before claiming durable work. A successful
// claim therefore always has a bounded run goroutine ready to own it locally.
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
		live := m.ensureLiveConversation(claim.Conversation)
		live.mu.Lock()
		live.broadcastLocked(claim.Events...)
		live.mu.Unlock()
		ctx, cancel := context.WithCancel(m.ctx)
		m.wg.Add(1)
		go live.work(ctx, cancel, claim)
	}
}

func (c *liveConversation) work(ctx context.Context, cancel context.CancelFunc, claim *ClaimedRun) {
	defer c.manager.wg.Done()
	defer c.manager.releaseSlot(true)
	result, runErr := c.execute(ctx, claim.Message, claim.History, claim.Run)
	cancel()
	c.mu.Lock()
	var backgroundErr error
	// Terminal persistence deliberately outlives the canceled run context. On
	// shutdown or runner cancellation we still need to durably resolve the run
	// and mark any requested tool outcome as unknown rather than strand it.
	if runErr != nil {
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

func (c *liveConversation) execute(
	ctx context.Context,
	message InboundMessage,
	history []Message,
	run Run,
) (RunResult, error) {
	m := c.manager
	return m.cfg.Runner.Run(ctx, RunRequest{
		ConversationID:     c.conversation.ID,
		RunID:              run.ID,
		InboundMessageID:   message.ID,
		Workspace:          c.conversation.Workspace,
		GitRepository:      c.conversation.GitRepository,
		GitRevision:        c.conversation.GitRevision,
		GitAllRepositories: c.conversation.GitAllRepositories,
		Text:               messageText(message.Parts),
		Messages:           history,
		Emit: func(event RunEvent) error {
			return c.emitRunEvent(run, event)
		},
	})
}

func (c *liveConversation) emitRunEvent(run Run, source RunEvent) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch source.Type {
	case EventRunProgress:
		c.broadcastLocked(Event{
			Type:             EventRunProgress,
			ConversationID:   run.ConversationID,
			RunID:            run.ID,
			InboundMessageID: run.InboundMessageID,
			RunProgress:      &RunProgress{Stage: source.Stage},
			CreatedAt:        time.Now().UTC(),
		})
		return nil
	case EventAssistantDelta:
		messageID, partID := source.MessageID, source.PartID
		if messageID == "" {
			messageID = run.ID + ":draft"
		}
		if partID == "" {
			partID = "text"
		}
		c.broadcastLocked(Event{
			Type:             EventAssistantDelta,
			ConversationID:   run.ConversationID,
			RunID:            run.ID,
			InboundMessageID: run.InboundMessageID,
			Delta: &TextDelta{
				MessageID: messageID,
				PartID:    partID,
				Text:      source.Text,
			},
			CreatedAt: time.Now().UTC(),
		})
		return nil
	case EventToolProgress:
		c.broadcastLocked(Event{
			Type:             EventToolProgress,
			ConversationID:   run.ConversationID,
			RunID:            run.ID,
			InboundMessageID: run.InboundMessageID,
			Progress: &ToolProgress{
				ToolCallID: source.ToolCallID,
				Text:       source.Text,
			},
			CreatedAt: time.Now().UTC(),
		})
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
	// The single buffered value coalesces hints. ClaimRunnable and the repair
	// scan, rather than notification count, determine whether work exists.
	select {
	case m.wake <- struct{}{}:
	default:
	}
}
