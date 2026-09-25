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

		// Register the run before announcing it, so a client that sees
		// run.started can always stop it.
		ctx, cancel := context.WithCancel(m.ctx)
		m.mu.Lock()
		m.runs[claim.Run.ConversationID] = &activeRun{run: claim.Run, cancel: cancel}
		m.mu.Unlock()

		live := m.ensureLiveConversation(claim.Conversation)
		live.mu.Lock()
		live.broadcastLocked(claim.Events...)
		live.mu.Unlock()

		m.wg.Add(1)
		go live.work(ctx, cancel, claim)
	}
}

func (c *liveConversation) work(ctx context.Context, cancel context.CancelFunc, claim *ClaimedRun) {
	defer c.manager.wg.Done()
	defer c.manager.releaseSlot(true)
	var result RunResult
	// A revision that cannot be resolved never falls back to the current
	// definition: the run would execute under changed authority.
	agent, runErr := c.manager.cfg.AgentRevisions.AgentRevision(ctx, claim.Conversation.AgentID, claim.Run.AgentRevision)
	if runErr == nil && (agent.ID != claim.Conversation.AgentID || agent.Revision() != claim.Run.AgentRevision) {
		runErr = errors.New("resolved definition does not match")
	}
	if runErr != nil {
		runErr = fmt.Errorf("runtime: agent revision %q of agent %q is unavailable: %w",
			claim.Run.AgentRevision, claim.Conversation.AgentID, runErr)
	} else {
		result, runErr = c.execute(ctx, agent, claim)
	}
	cancel()
	c.manager.mu.Lock()
	stopped := c.manager.runs[claim.Run.ConversationID].stopped
	delete(c.manager.runs, claim.Run.ConversationID)
	c.manager.mu.Unlock()

	c.mu.Lock()
	var backgroundErr error
	// Terminal persistence deliberately outlives the canceled run context. On
	// shutdown or runner cancellation we still need to durably resolve the run
	// and mark any requested tool outcome as unknown rather than strand it.
	if runErr != nil && stopped {
		events, persistErr := c.manager.store.StopRun(context.Background(), claim.Run)
		if persistErr == nil {
			c.broadcastLocked(events...)
		} else {
			backgroundErr = fmt.Errorf("runtime: persist stopped run %s: %w", claim.Run.ID, persistErr)
		}
	} else if runErr != nil {
		events, persistErr := c.manager.store.FailRun(context.Background(), claim.Run, runErr.Error())
		if persistErr == nil {
			c.broadcastLocked(events...)
		} else {
			backgroundErr = fmt.Errorf("runtime: persist failed run %s: %w", claim.Run.ID, persistErr)
		}
	} else {
		events, persistErr := c.manager.store.FinishRun(context.Background(), claim.Run, result.Answer)
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
	agent AgentDefinition,
	claim *ClaimedRun,
) (RunResult, error) {
	m := c.manager
	return m.cfg.Runner.Run(ctx, RunRequest{
		Agent:              agent,
		ConversationID:     c.conversation.ID,
		RunID:              claim.Run.ID,
		InboundMessageID:   claim.Message.ID,
		Workspace:          c.conversation.Workspace,
		Environment:        c.conversation.Environment,
		GitRepository:      c.conversation.GitRepository,
		GitRevision:        c.conversation.GitRevision,
		GitAllRepositories: c.conversation.GitAllRepositories,
		Text:               messageText(claim.Message.Parts),
		Context:            claim.Context,

		Emit: func(event RunEvent) error {
			return c.emitRunEvent(claim.Run, event)
		},
	})
}

func (c *liveConversation) emitRunEvent(run Run, source RunEvent) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch source.Type {
	case RunEventAgentEvent:
		if source.AgentEvent == nil {
			return errors.New("runtime: agent event is missing")
		}
		return c.manager.store.AppendAgentEvent(context.Background(), run, *source.AgentEvent)
	case EventEnvironmentProgress:
		event, err := c.manager.store.AppendEnvironmentProgress(context.Background(), run, EnvironmentProgress{
			Step: source.Step, Message: source.Message,
		})
		if err == nil {
			c.broadcastLocked(event)
		}
		return err
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
