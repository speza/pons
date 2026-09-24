package runtime_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samperrin/pons/protocol"
	ponsruntime "github.com/samperrin/pons/runtime"
	"github.com/samperrin/pons/runtime/agentdir"
	runtimesqlite "github.com/samperrin/pons/runtime/sqlite"
)

func acceptInput(store ponsruntime.Store, ctx context.Context, conversationID, key string, parts []ponsruntime.TextPart) (ponsruntime.AcceptedMessage, []ponsruntime.Event, error) {
	return store.Accept(ctx, conversationID, ponsruntime.InputSubmission{
		IdempotencyKey: key, Parts: parts,
		Source:        ponsruntime.InputSource{Kind: "human", Adapter: "test"},
		TargetAgentID: testAgent.ID, AgentRevision: testRevision,
	})
}

type (
	Event      = ponsruntime.Event
	Manager    = ponsruntime.Manager
	RunEvent   = ponsruntime.RunEvent
	RunRequest = ponsruntime.RunRequest
	RunResult  = ponsruntime.RunResult
	Runner     = ponsruntime.Runner
	RunnerFunc = ponsruntime.RunnerFunc
	TextPart   = ponsruntime.TextPart
)

const (
	EventAssistantDelta     = ponsruntime.EventAssistantDelta
	EventAssistantCommitted = ponsruntime.EventAssistantCommitted
	EventRunCompleted       = ponsruntime.EventRunCompleted
	RunEventAssistantTurn   = ponsruntime.RunEventAssistantTurn
	RunEventToolCompleted   = ponsruntime.RunEventToolCompleted
	RunFailed               = ponsruntime.RunFailed
	ToolCompleted           = ponsruntime.ToolCompleted
	ToolInterrupted         = ponsruntime.ToolInterrupted
	ToolRequested           = ponsruntime.ToolRequested
)

var New = ponsruntime.New

var (
	testAgent    = ponsruntime.AgentDefinition{ID: ponsruntime.DefaultAgentID, Name: "Test", MaxTurns: 3}
	testRevision = testAgent.Revision()
)

// agentRevisions is an in-memory AgentRevisions keyed by revision.
type agentRevisions map[string]ponsruntime.AgentDefinition

func (r agentRevisions) AgentRevision(_ context.Context, agentID, revision string) (ponsruntime.AgentDefinition, error) {
	definition, ok := r[revision]
	if !ok || definition.ID != agentID {
		return ponsruntime.AgentDefinition{}, fmt.Errorf("revision %q not recorded", revision)
	}
	return definition, nil
}

var testRevisions = agentRevisions{testRevision: testAgent}

type Config = ponsruntime.Config

type claimErrorStore struct {
	ponsruntime.Store
	err error
}

func (s claimErrorStore) ClaimRunnable(context.Context) (*ponsruntime.ClaimedRun, error) {
	return nil, s.err
}

type observingStore struct {
	ponsruntime.Store
	conversationReads atomic.Int32
	claimCalls        atomic.Int32
}

type delayedClaimStore struct {
	ponsruntime.Store
	claimed chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *delayedClaimStore) ClaimRunnable(ctx context.Context) (*ponsruntime.ClaimedRun, error) {
	claim, err := s.Store.ClaimRunnable(ctx)
	if err != nil || claim == nil {
		return claim, err
	}
	s.once.Do(func() { close(s.claimed) })
	select {
	case <-s.release:
		return claim, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *observingStore) Conversation(ctx context.Context, id string) (ponsruntime.Conversation, error) {
	s.conversationReads.Add(1)
	return s.Store.Conversation(ctx, id)
}

func (s *observingStore) ClaimRunnable(ctx context.Context) (*ponsruntime.ClaimedRun, error) {
	s.claimCalls.Add(1)
	return s.Store.ClaimRunnable(ctx)
}

func testManager(t *testing.T, runner Runner) *Manager {
	t.Helper()
	store, err := runtimesqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m, err := New(Config{Agent: testAgent, AgentRevisions: testRevisions, Store: store, Runner: runner, MaxConcurrent: 4})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close(); _ = store.Close() })
	return m
}

func TestConversationUsesClientSelectedWorkspace(t *testing.T) {
	m := testManager(t, RunnerFunc(func(context.Context, RunRequest) (RunResult, error) {
		return RunResult{Answer: "ok"}, nil
	}))
	firstPath, secondPath := t.TempDir(), t.TempDir()
	for _, path := range []string{firstPath, secondPath} {
		conversation, err := m.CreateConversation(context.Background(), ponsruntime.ConversationOptions{Workspace: path})
		if err != nil {
			t.Fatal(err)
		}
		if conversation.Workspace != path || conversation.WorkspaceLock != path {
			t.Fatalf("conversation workspace = %+v, want %q", conversation, path)
		}
	}
	gitConversation, err := m.CreateConversation(context.Background(), ponsruntime.ConversationOptions{
		GitRepository: "https://github.com/example/repo.git", GitRevision: "pinned-commit",
	})
	if err != nil {
		t.Fatal(err)
	}
	if gitConversation.Workspace != "" || gitConversation.WorkspaceLock != gitConversation.ID {
		t.Fatalf("Git conversation workspace = %+v", gitConversation)
	}
	if _, err := m.CreateConversation(context.Background(), ponsruntime.ConversationOptions{}); err == nil {
		t.Fatal("conversation without a source accepted")
	}
	if _, err := m.CreateConversation(context.Background(), ponsruntime.ConversationOptions{
		Workspace: firstPath, GitRepository: "https://github.com/example/repo.git",
	}); err == nil {
		t.Fatal("conversation with both host and Git sources accepted")

	}
}

func TestManagerPersistsConfiguredConversationEnvironment(t *testing.T) {
	store, err := runtimesqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	manager, err := New(Config{
		Agent:              testAgent,
		AgentRevisions:     testRevisions,
		Store:              store,
		Runner:             RunnerFunc(func(context.Context, RunRequest) (RunResult, error) { return RunResult{}, nil }),
		EnvironmentOptions: []string{"local", "seatbelt", "e2b"},
		DefaultEnvironment: "seatbelt",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()

	workspace := t.TempDir()
	defaultConversation, err := manager.CreateConversation(context.Background(), ponsruntime.ConversationOptions{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	if defaultConversation.Environment != "seatbelt" {
		t.Fatalf("default environment = %q", defaultConversation.Environment)
	}
	explicit, err := manager.CreateConversation(context.Background(), ponsruntime.ConversationOptions{Workspace: workspace, Environment: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if explicit.Environment != "local" {
		t.Fatalf("explicit environment = %q", explicit.Environment)
	}
	if defaultConversation.WorkspaceLock != workspace || explicit.WorkspaceLock != workspace {
		t.Fatalf("host workspace locks = %q, %q, want %q", defaultConversation.WorkspaceLock, explicit.WorkspaceLock, workspace)
	}
	sandboxed, err := manager.CreateConversation(context.Background(), ponsruntime.ConversationOptions{Workspace: workspace, Environment: "e2b"})
	if err != nil {
		t.Fatal(err)
	}
	if sandboxed.WorkspaceLock != sandboxed.ID {
		t.Fatalf("E2B workspace lock = %q, want %q", sandboxed.WorkspaceLock, sandboxed.ID)
	}
	if _, err := manager.CreateConversation(context.Background(), ponsruntime.ConversationOptions{Workspace: workspace, Environment: "unknown"}); !errors.Is(err, ponsruntime.ErrInvalidEnvironment) {
		t.Fatalf("unknown environment error = %v", err)
	}
}

func waitForEvent(t *testing.T, m *Manager, conversationID, eventType string, count int) []Event {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		events, err := m.Events(context.Background(), conversationID, 0)
		if err != nil {
			t.Fatal(err)
		}
		seen := 0
		for _, event := range events {
			if event.Type == eventType {
				seen++
			}
		}
		if seen >= count {
			return events
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d %s event(s)", count, eventType)
	return nil
}

func waitUntil(t *testing.T, condition func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(message)
}

func TestManagerDoesNotEagerlyHydrateDormantConversations(t *testing.T) {
	base, err := runtimesqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	ctx := context.Background()
	var first ponsruntime.Conversation
	for range 50 {
		conversation := ponsruntime.Conversation{ID: ponsruntime.NewID(), AgentID: testAgent.ID, Workspace: t.TempDir(), CreatedAt: time.Now().UTC()}
		if err := base.CreateConversation(ctx, conversation); err != nil {
			t.Fatal(err)
		}
		if first.ID == "" {
			first = conversation
		}
	}
	store := &observingStore{Store: base}
	manager, err := New(Config{
		Agent:          testAgent,
		AgentRevisions: testRevisions,
		Store:          store, RepairInterval: time.Hour,
		Runner: RunnerFunc(func(context.Context, RunRequest) (RunResult, error) {
			return RunResult{}, errors.New("unexpected run")
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	waitUntil(t, func() bool { return store.claimCalls.Load() > 0 }, "initial runnable check did not run")
	if reads := store.conversationReads.Load(); reads != 0 {
		t.Fatalf("startup hydrated %d dormant conversations", reads)
	}
	if _, err := manager.View(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	if reads := store.conversationReads.Load(); reads != 1 {
		t.Fatalf("lazy view reads = %d, want 1", reads)
	}
}

func TestRepairScanFindsWorkAfterLostWake(t *testing.T) {
	base, err := runtimesqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	store := &observingStore{Store: base}
	ran := make(chan string, 1)
	manager, err := New(Config{
		Agent:          testAgent,
		AgentRevisions: testRevisions,
		Store:          store, RepairInterval: 10 * time.Millisecond,
		Runner: RunnerFunc(func(_ context.Context, request RunRequest) (RunResult, error) {
			ran <- request.Text
			return RunResult{Answer: "done"}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	waitUntil(t, func() bool { return store.claimCalls.Load() > 0 }, "initial runnable check did not run")
	conversation, err := manager.CreateConversation(context.Background(), ponsruntime.ConversationOptions{Workspace: t.TempDir()})

	if err != nil {
		t.Fatal(err)
	}
	// Write through the store to deliberately bypass Manager.Submit's wake.
	if _, _, err := acceptInput(base, context.Background(), conversation.ID, "lost-wake", []TextPart{{Type: "text", Text: "repair me"}}); err != nil {
		t.Fatal(err)
	}
	select {
	case text := <-ran:
		if text != "repair me" {
			t.Fatalf("run text = %q", text)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("repair scan did not discover durable work")
	}
}

func TestSchedulerBoundsActiveRunGoroutines(t *testing.T) {
	store, err := runtimesqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	for i := range 4 {
		conversation := ponsruntime.Conversation{ID: ponsruntime.NewID(), AgentID: testAgent.ID, Workspace: t.TempDir(), CreatedAt: time.Now().UTC()}
		if err := store.CreateConversation(ctx, conversation); err != nil {
			t.Fatal(err)
		}
		if _, _, err := acceptInput(store, ctx, conversation.ID, fmt.Sprintf("key-%d", i), []TextPart{{Type: "text", Text: "go"}}); err != nil {
			t.Fatal(err)
		}
	}
	started := make(chan struct{}, 4)
	var active, maximum atomic.Int32
	manager, err := New(Config{
		Agent:          testAgent,
		AgentRevisions: testRevisions,
		Store:          store, MaxConcurrent: 2,
		Runner: RunnerFunc(func(ctx context.Context, _ RunRequest) (RunResult, error) {
			current := active.Add(1)
			defer active.Add(-1)
			for {
				old := maximum.Load()
				if current <= old || maximum.CompareAndSwap(old, current) {
					break
				}
			}
			started <- struct{}{}
			<-ctx.Done()
			return RunResult{}, ctx.Err()
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("scheduler did not fill its bounded capacity")
		}
	}
	select {
	case <-started:
		t.Fatal("scheduler started more runs than MaxConcurrent")
	case <-time.After(100 * time.Millisecond):
	}
	if maximum.Load() != 2 {
		t.Fatalf("maximum active runs = %d, want 2", maximum.Load())
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceExclusionIsEnforcedByRunnableClaim(t *testing.T) {
	store, err := runtimesqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	workspace := t.TempDir()
	for i := range 2 {
		conversation := ponsruntime.Conversation{ID: ponsruntime.NewID(), AgentID: testAgent.ID, Workspace: workspace, CreatedAt: time.Now().UTC().Add(time.Duration(i) * time.Nanosecond)}
		if err := store.CreateConversation(ctx, conversation); err != nil {
			t.Fatal(err)
		}
		if _, _, err := acceptInput(store, ctx, conversation.ID, fmt.Sprintf("key-%d", i), []TextPart{{Type: "text", Text: "go"}}); err != nil {
			t.Fatal(err)
		}
	}
	started := make(chan string, 2)
	release := make(chan struct{}, 2)
	manager, err := New(Config{
		Agent:          testAgent,
		AgentRevisions: testRevisions,
		Store:          store, MaxConcurrent: 2,
		Runner: RunnerFunc(func(_ context.Context, request RunRequest) (RunResult, error) {
			started <- request.ConversationID
			<-release
			return RunResult{Answer: "done"}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("first workspace run did not start")
	}
	select {
	case <-started:
		t.Fatal("two runs owned the same workspace")
	case <-time.After(100 * time.Millisecond):
	}
	release <- struct{}{}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("second workspace run did not start after release")
	}
	release <- struct{}{}
}

func TestCompetingSQLiteClaimsDoNotDuplicateSubmission(t *testing.T) {
	stateDir, workspace := t.TempDir(), t.TempDir()
	store, err := runtimesqlite.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	conversation := ponsruntime.Conversation{ID: ponsruntime.NewID(), AgentID: testAgent.ID, Workspace: workspace, CreatedAt: time.Now().UTC()}
	if err := store.CreateConversation(context.Background(), conversation); err != nil {
		t.Fatal(err)
	}
	if _, _, err := acceptInput(store, context.Background(), conversation.ID, "once", []TextPart{{Type: "text", Text: "go"}}); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	type claimResult struct {
		claim *ponsruntime.ClaimedRun
		err   error
	}
	results := make(chan claimResult, 2)
	for range 2 {
		go func() {
			<-start
			claim, err := store.ClaimRunnable(context.Background())
			results <- claimResult{claim, err}
		}()
	}
	close(start)
	claimed := 0
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.claim != nil {
			claimed++
		}
	}
	if claimed != 1 {
		t.Fatalf("claims = %d, want 1", claimed)
	}
}

func TestSubscribeDuringClaimBroadcastDoesNotDuplicateDurableEvents(t *testing.T) {
	base, err := runtimesqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	conversation := ponsruntime.Conversation{ID: ponsruntime.NewID(), AgentID: testAgent.ID, Workspace: t.TempDir(), CreatedAt: time.Now().UTC()}
	if err := base.CreateConversation(context.Background(), conversation); err != nil {
		t.Fatal(err)
	}
	if _, _, err := acceptInput(base, context.Background(), conversation.ID, "key", []TextPart{{Type: "text", Text: "go"}}); err != nil {
		t.Fatal(err)
	}
	store := &delayedClaimStore{Store: base, claimed: make(chan struct{}), release: make(chan struct{})}
	manager, err := New(Config{
		Agent:          testAgent,
		AgentRevisions: testRevisions,
		Store:          store,
		Runner: RunnerFunc(func(context.Context, RunRequest) (RunResult, error) {
			return RunResult{Answer: "done"}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	select {
	case <-store.claimed:
	case <-time.After(3 * time.Second):
		t.Fatal("claim did not commit")
	}
	stream, err := manager.Subscribe(t.Context(), conversation.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	close(store.release)
	var cursors []uint64
	deadline := time.After(3 * time.Second)
	for {
		select {
		case event := <-stream:
			if event.ID != 0 {
				cursors = append(cursors, event.ID)
			}
			if event.Type == EventAssistantCommitted && event.AssistantOutput != nil && event.AssistantOutput.Final {
				for i, cursor := range cursors {
					if cursor != uint64(i+1) {
						t.Fatalf("durable cursors = %v", cursors)
					}
				}
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for final event; cursors = %v", cursors)
		}
	}
}

func TestConversationSerializesMessagesAndDeduplicates(t *testing.T) {
	var active, maximum atomic.Int32
	var mu sync.Mutex
	var seen []string
	runner := RunnerFunc(func(_ context.Context, request RunRequest) (RunResult, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for {
			old := maximum.Load()
			if n <= old || maximum.CompareAndSwap(old, n) {
				break
			}
		}
		mu.Lock()
		seen = append(seen, request.Text)
		mu.Unlock()
		time.Sleep(30 * time.Millisecond)
		return RunResult{Answer: "answer: " + request.Text}, nil
	})
	m := testManager(t, runner)
	conversation, err := m.CreateConversation(context.Background(), ponsruntime.ConversationOptions{Workspace: t.TempDir()})

	if err != nil {
		t.Fatal(err)
	}
	first, err := m.Submit(context.Background(), conversation.ID, "first", []TextPart{{Type: "text", Text: "one"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Submit(context.Background(), conversation.ID, "second", []TextPart{{Type: "text", Text: "two"}})
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := m.Submit(context.Background(), conversation.ID, "first", []TextPart{{Type: "text", Text: "ignored"}})
	if err != nil {
		t.Fatal(err)
	}
	if !duplicate.Duplicate || duplicate.InboundMessageID != first.InboundMessageID {
		t.Fatalf("duplicate = %+v", duplicate)
	}
	events := waitForEvent(t, m, conversation.ID, EventRunCompleted, 2)
	if maximum.Load() != 1 {
		t.Fatalf("conversation concurrency = %d", maximum.Load())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || seen[0] != "one" || seen[1] != "two" {
		t.Fatalf("run order = %v", seen)
	}
	for i := 1; i < len(events); i++ {
		if events[i].ID <= events[i-1].ID {
			t.Fatalf("non-monotonic cursors: %+v", events)
		}
	}
}

func TestBackgroundStoreFailureIsReported(t *testing.T) {
	base, err := runtimesqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	want := errors.New("claim unavailable")
	reported := make(chan error, 1)
	var runnerCalls atomic.Int32
	manager, err := New(Config{
		Agent:          testAgent,
		AgentRevisions: testRevisions,
		Store:          claimErrorStore{Store: base, err: want},
		Runner: RunnerFunc(func(context.Context, RunRequest) (RunResult, error) {
			runnerCalls.Add(1)
			return RunResult{}, nil
		}),
		OnError: func(err error) { reported <- err },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	conversation, err := manager.CreateConversation(context.Background(), ponsruntime.ConversationOptions{Workspace: t.TempDir()})

	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Submit(context.Background(), conversation.ID, "key", []TextPart{{Type: "text", Text: "go"}}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-reported:
		if !errors.Is(err, want) {
			t.Fatalf("reported error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("background store failure was not reported")
	}
	if runnerCalls.Load() != 0 {
		t.Fatalf("runner calls = %d, want 0", runnerCalls.Load())
	}
}

func TestSnapshotThenEventsHasNoDurableGap(t *testing.T) {
	release := make(chan struct{})
	m := testManager(t, RunnerFunc(func(ctx context.Context, _ RunRequest) (RunResult, error) {
		select {
		case <-release:
			return RunResult{Answer: "done"}, nil
		case <-ctx.Done():
			return RunResult{}, ctx.Err()
		}
	}))
	conversation, _ := m.CreateConversation(context.Background(), ponsruntime.ConversationOptions{Workspace: t.TempDir()})

	view, err := m.View(context.Background(), conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	stream, err := m.Subscribe(ctx, conversation.ID, view.EventCursor)
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := m.Submit(context.Background(), conversation.ID, "key", []TextPart{{Type: "text", Text: "hello"}})
	if err != nil {
		t.Fatal(err)
	}
	close(release)
	deadline := time.After(3 * time.Second)
	foundUser, foundFinal := false, false
	for !foundFinal {
		select {
		case event := <-stream:
			if event.ID <= view.EventCursor {
				t.Fatalf("event cursor %d <= snapshot cursor %d", event.ID, view.EventCursor)
			}
			if event.Type == ponsruntime.EventInputAccepted && event.Input != nil && event.InboundMessageID == accepted.InboundMessageID {
				foundUser = true
			}
			if event.Type == EventAssistantCommitted && event.AssistantOutput != nil && event.AssistantOutput.Final {
				foundFinal = true
			}
		case <-deadline:
			t.Fatal("timed out waiting for catch-up/live events")
		}
	}
	if !foundUser {
		t.Fatal("user message upsert was missed")
	}
}

func TestEnvironmentProgressIsDurableWhileDeltaIsLiveOnly(t *testing.T) {
	release := make(chan struct{})
	m := testManager(t, RunnerFunc(func(ctx context.Context, request RunRequest) (RunResult, error) {
		if err := request.Emit(RunEvent{Type: ponsruntime.EventEnvironmentProgress, Step: "sandbox.prepare", Message: "Preparing sandbox…"}); err != nil {
			return RunResult{}, err
		}
		if err := request.Emit(RunEvent{Type: EventAssistantDelta, MessageID: "draft", PartID: "text", Text: "hel"}); err != nil {
			return RunResult{}, err
		}
		select {
		case <-release:
			return RunResult{Answer: "hello"}, nil
		case <-ctx.Done():
			return RunResult{}, ctx.Err()
		}
	}))
	conversation, _ := m.CreateConversation(context.Background(), ponsruntime.ConversationOptions{Workspace: t.TempDir()})

	ctx := t.Context()
	stream, err := m.Subscribe(ctx, conversation.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Submit(context.Background(), conversation.ID, "key", []TextPart{{Type: "text", Text: "go"}})
	if err != nil {
		t.Fatal(err)
	}
	seenProgress := false
	for {
		select {
		case event := <-stream:
			if event.Type == ponsruntime.EventEnvironmentProgress {
				if event.ID == 0 || event.EnvironmentProgress == nil || event.EnvironmentProgress.Step != "sandbox.prepare" || event.EnvironmentProgress.Message != "Preparing sandbox…" {
					t.Fatalf("progress = %+v", event)
				}
				seenProgress = true
			}
			if event.Type == EventAssistantDelta {
				if !seenProgress {
					t.Fatal("run progress was not delivered")
				}
				if event.ID != 0 || event.Delta == nil || event.Delta.Text != "hel" {
					t.Fatalf("delta = %+v", event)
				}
				persisted, loadErr := m.Events(context.Background(), conversation.ID, 0)
				if loadErr != nil {
					t.Fatal(loadErr)
				}
				for _, durable := range persisted {
					if durable.Type == EventAssistantDelta {
						t.Fatal("transient event was persisted")
					}
				}
				if !slices.ContainsFunc(persisted, func(event Event) bool {
					return event.Type == ponsruntime.EventEnvironmentProgress && event.EnvironmentProgress != nil && event.EnvironmentProgress.Step == "sandbox.prepare"
				}) {
					t.Fatal("environment progress was not persisted")
				}
				view, viewErr := m.View(context.Background(), conversation.ID)
				if viewErr != nil {
					t.Fatal(viewErr)
				}
				if len(view.EnvironmentEvents) != 1 || view.EnvironmentEvents[0].EnvironmentProgress.Message != "Preparing sandbox…" {
					t.Fatalf("environment progress snapshot = %+v", view.EnvironmentEvents)
				}
				for _, message := range view.Messages {
					if message.ID == "draft" {
						t.Fatal("draft leaked into snapshot")
					}
				}
				close(release)
				return
			}
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for delta")
		}
	}
}

func TestToolLifecycleUsesEntityUpserts(t *testing.T) {
	m := testManager(t, RunnerFunc(func(_ context.Context, request RunRequest) (RunResult, error) {
		action := protocol.Action{ID: "call-1", Kind: "fake", Args: protocol.MustArgsJSON(map[string]string{"value": request.Text})}
		if err := request.Emit(RunEvent{Type: RunEventAssistantTurn, Parts: []ponsruntime.MessagePart{{Type: "tool_call", ToolCallID: action.ID, ToolKind: string(action.Kind), Arguments: action.Args}}}); err != nil {
			return RunResult{}, err
		}
		result := protocol.ToolResult{ActionID: action.ID, Kind: "fake", OK: true, Output: "tool output"}
		if err := request.Emit(RunEvent{Type: RunEventToolCompleted, Result: &result}); err != nil {
			return RunResult{}, err
		}
		return RunResult{Answer: "hello " + request.Text}, nil
	}))
	conversation, err := m.CreateConversation(context.Background(), ponsruntime.ConversationOptions{Workspace: t.TempDir()})

	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Submit(context.Background(), conversation.ID, "stable", []TextPart{{Type: "text", Text: "world"}}); err != nil {
		t.Fatal(err)
	}
	observed := waitForEvent(t, m, conversation.ID, EventRunCompleted, 1)
	requested, completed := false, false
	for _, event := range observed {
		if event.Type == EventAssistantCommitted && event.AssistantOutput != nil {
			for _, part := range event.AssistantOutput.Parts {
				requested = requested || part.Type == "tool_call"
			}
		}
		if event.Type == ponsruntime.EventToolOutcomeRecorded && event.ToolOutcome != nil && event.ToolOutcome.Status == ToolCompleted {
			completed = true
		}
	}
	if !requested || !completed {
		t.Fatalf("tool lifecycle missing: %+v", observed)
	}
	view, err := m.View(context.Background(), conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Messages) != 4 || view.EventCursor == 0 {
		t.Fatalf("view = %+v", view)
	}
}

func TestDeniedToolPersistsDistinctStatusAndResult(t *testing.T) {
	m := testManager(t, RunnerFunc(func(_ context.Context, request RunRequest) (RunResult, error) {
		action := protocol.Action{ID: "call-1", Kind: "fake", Args: protocol.MustArgsJSON(map[string]string{"value": "blocked"})}
		if err := request.Emit(RunEvent{Type: RunEventAssistantTurn, Parts: []ponsruntime.MessagePart{{
			Type: "tool_call", ToolCallID: action.ID, ToolKind: string(action.Kind), Arguments: action.Args,
		}}}); err != nil {
			return RunResult{}, err
		}
		result := protocol.ToolResult{ActionID: action.ID, Kind: "fake", OK: false, Error: "action denied"}
		if err := request.Emit(RunEvent{Type: ponsruntime.RunEventToolDenied, Result: &result}); err != nil {
			return RunResult{}, err
		}
		return RunResult{Answer: "done"}, nil
	}))
	conversation, err := m.CreateConversation(context.Background(), ponsruntime.ConversationOptions{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Submit(context.Background(), conversation.ID, "deny", []TextPart{{Type: "text", Text: "go"}}); err != nil {
		t.Fatal(err)
	}
	_ = waitForEvent(t, m, conversation.ID, EventRunUpdated, 2)
	view, err := m.View(context.Background(), conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.ToolCalls) != 1 || view.ToolCalls[0].Status != ponsruntime.ToolDenied ||
		view.ToolCalls[0].Result == nil || view.ToolCalls[0].Result.Error != "action denied" {
		t.Fatalf("denied tool call: %+v", view.ToolCalls)
	}
	var resultFound bool
	for _, message := range view.Messages {
		for _, part := range message.Parts {
			if part.Type == "tool_result" && part.Result != nil && part.Result.Error == "action denied" {
				resultFound = true
			}
		}
	}
	if !resultFound {
		t.Fatalf("denied tool result missing from history: %+v", view.Messages)
	}
}

func TestFailedRunIsDurableAndNotRetried(t *testing.T) {
	var executions atomic.Int32
	m := testManager(t, RunnerFunc(func(_ context.Context, request RunRequest) (RunResult, error) {
		executions.Add(1)
		action := protocol.Action{ID: "started", Kind: "bash", Args: protocol.MustArgsJSON(map[string]string{"command": "side effect"})}
		if err := request.Emit(RunEvent{Type: RunEventAssistantTurn, Parts: []ponsruntime.MessagePart{{Type: "tool_call", ToolCallID: action.ID, ToolKind: string(action.Kind), Arguments: action.Args}}}); err != nil {
			return RunResult{}, err
		}
		return RunResult{}, errors.New("failed after intent")
	}))
	conversation, _ := m.CreateConversation(context.Background(), ponsruntime.ConversationOptions{Workspace: t.TempDir()})

	_, err := m.Submit(context.Background(), conversation.ID, "once", []TextPart{{Type: "text", Text: "go"}})
	if err != nil {
		t.Fatal(err)
	}
	events := waitForEvent(t, m, conversation.ID, ponsruntime.EventRunFailed, 1)
	last := events[len(events)-2]
	_ = last
	if executions.Load() != 1 {
		t.Fatalf("executions = %d", executions.Load())
	}
	view, err := m.View(context.Background(), conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if view.ActiveRun != nil || len(view.ToolCalls) != 1 || view.ToolCalls[0].Status != ToolInterrupted {
		t.Fatalf("view = %+v", view)
	}
}

func TestRestartMarksRequestedToolInterruptedWithoutRetry(t *testing.T) {
	stateDir, workspace := t.TempDir(), t.TempDir()
	store, err := runtimesqlite.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	conversation := ponsruntime.Conversation{ID: ponsruntime.NewID(), AgentID: testAgent.ID, Workspace: workspace, CreatedAt: time.Now().UTC()}
	if err := store.CreateConversation(context.Background(), conversation); err != nil {
		t.Fatal(err)
	}
	accepted, _, err := acceptInput(store, context.Background(), conversation.ID, "stable", []TextPart{{Type: "text", Text: "do it"}})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := store.ClaimRunnable(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	action := protocol.Action{ID: "dangerous", Kind: "bash", Args: protocol.MustArgsJSON(map[string]string{"command": "side effect"})}
	_, err = store.CommitAssistantTurn(context.Background(), claim.Run, []ponsruntime.MessagePart{{Type: "tool_call", ToolCallID: action.ID, ToolKind: string(action.Kind), Arguments: action.Args}})
	if err != nil {
		t.Fatal(err)
	}
	var executions atomic.Int32
	second, err := New(Config{Agent: testAgent, AgentRevisions: testRevisions, Store: store, Runner: RunnerFunc(func(context.Context, RunRequest) (RunResult, error) {
		executions.Add(1)
		return RunResult{}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	view, err := second.View(context.Background(), conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if executions.Load() != 0 || view.ActiveRun != nil || len(view.ToolCalls) != 1 || view.ToolCalls[0].Status != ToolInterrupted {
		t.Fatalf("recovered view = %+v; executions = %d", view, executions.Load())
	}
	if len(view.Submissions) != 1 || view.Submissions[0].Status != RunFailed {
		t.Fatalf("recovered submissions = %+v", view.Submissions)
	}
	duplicate, err := second.Submit(context.Background(), conversation.ID, "stable", []TextPart{{Type: "text", Text: "retry"}})
	if err != nil {
		t.Fatal(err)
	}
	if !duplicate.Duplicate || duplicate.InboundMessageID != accepted.InboundMessageID {
		t.Fatalf("duplicate = %+v", duplicate)
	}
}

func TestClaimHistoryExcludesLaterQueuedMessages(t *testing.T) {
	ctx := context.Background()
	store, err := runtimesqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	conversation := ponsruntime.Conversation{ID: ponsruntime.NewID(), AgentID: testAgent.ID, Workspace: t.TempDir(), CreatedAt: time.Now().UTC()}
	if err := store.CreateConversation(ctx, conversation); err != nil {
		t.Fatal(err)
	}
	first, _, err := acceptInput(store, ctx, conversation.ID, "first", []TextPart{{Type: "text", Text: "A"}})
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := acceptInput(store, ctx, conversation.ID, "second", []TextPart{{Type: "text", Text: "B"}})
	if err != nil {
		t.Fatal(err)
	}
	claimA, err := store.ClaimRunnable(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if claimA.Message.ID != first.InboundMessageID || len(claimA.Context) != 0 {
		t.Fatalf("first claim = %+v", claimA)
	}
	if _, err := store.FinishRun(ctx, claimA.Run, "answer A"); err != nil {
		t.Fatal(err)
	}
	claimB, err := store.ClaimRunnable(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if claimB.Message.ID != second.InboundMessageID || len(claimB.Context) != 2 {
		t.Fatalf("second claim = %+v", claimB)
	}
	if got := claimB.Context[0]; got.Role != "user" || got.Content[0].Text != "A" {
		t.Fatalf("history[0] = %+v", got)
	}
	if got := claimB.Context[1]; got.Role != "assistant" || got.Content[0].Text != "answer A" {
		t.Fatalf("history[1] = %+v", got)
	}
}

func TestAssistantToolTurnsRemainOrderedAndComplete(t *testing.T) {
	ctx := context.Background()
	store, err := runtimesqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	conversation := ponsruntime.Conversation{ID: ponsruntime.NewID(), AgentID: testAgent.ID, Workspace: t.TempDir(), CreatedAt: time.Now().UTC()}
	if err := store.CreateConversation(ctx, conversation); err != nil {
		t.Fatal(err)
	}
	if _, _, err := acceptInput(store, ctx, conversation.ID, "one", []TextPart{{Type: "text", Text: "go"}}); err != nil {
		t.Fatal(err)
	}
	claim, err := store.ClaimRunnable(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"call-1", "call-2"} {
		args := protocol.MustArgsJSON(map[string]any{"round": i + 1})
		if i == 0 {
			args = nil
		}
		parts := []ponsruntime.MessagePart{
			{Type: "text", Text: "before " + id},
			{Type: "tool_call", ToolCallID: id, ToolKind: "fake", Arguments: args},
			{Type: "text", Text: "after " + id},
		}
		if _, err := store.CommitAssistantTurn(ctx, claim.Run, parts); err != nil {
			t.Fatal(err)
		}
		if _, err := store.ToolCompleted(ctx, claim.Run, protocol.ToolResult{ActionID: id, Kind: "fake", OK: true, Output: id + " result"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.FinishRun(ctx, claim.Run, "done"); err != nil {
		t.Fatal(err)
	}
	view, err := store.View(ctx, conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Messages) != 6 {
		t.Fatalf("messages = %+v", view.Messages)
	}
	for _, index := range []int{1, 3} {
		message := view.Messages[index]
		if message.Role != "assistant" || !message.Complete || message.Final || len(message.Parts) != 3 || message.Parts[0].Type != "text" || message.Parts[1].Type != "tool_call" || message.Parts[2].Type != "text" {
			t.Fatalf("assistant turn %d = %+v", index, message)
		}
	}
	if view.Messages[1].ID == view.Messages[3].ID {
		t.Fatal("separate planning rounds were merged")
	}
	if got := string(view.Messages[1].Parts[1].Arguments); got != "{}" {
		t.Fatalf("normalized arguments = %q", got)
	}
}

func TestRestartRunsDurablyQueuedSubmission(t *testing.T) {
	stateDir, workspace := t.TempDir(), t.TempDir()
	store, err := runtimesqlite.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	conversation := ponsruntime.Conversation{ID: ponsruntime.NewID(), AgentID: testAgent.ID, Workspace: workspace, CreatedAt: time.Now().UTC()}
	if err := store.CreateConversation(context.Background(), conversation); err != nil {
		t.Fatal(err)
	}
	if _, _, err := acceptInput(store, context.Background(), conversation.ID, "queued", []TextPart{{Type: "text", Text: "resume me"}}); err != nil {
		t.Fatal(err)
	}
	ran := make(chan string, 1)
	second, err := New(Config{Agent: testAgent, AgentRevisions: testRevisions, Store: store, Runner: RunnerFunc(func(_ context.Context, request RunRequest) (RunResult, error) {
		ran <- request.Text
		return RunResult{Answer: "resumed"}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	select {
	case text := <-ran:
		if text != "resume me" {
			t.Fatalf("resumed text = %q", text)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("queued submission did not resume")
	}
	waitForEvent(t, second, conversation.ID, EventRunCompleted, 1)
}

// idleStore accepts work but never claims it, leaving submissions queued.
type idleStore struct{ ponsruntime.Store }

func (idleStore) ClaimRunnable(context.Context) (*ponsruntime.ClaimedRun, error) { return nil, nil }

func TestQueuedSubmissionKeepsAgentRevisionAcrossPersonaChange(t *testing.T) {
	ctx := context.Background()
	stateDir := t.TempDir()
	store, err := runtimesqlite.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	agents, err := agentdir.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}

	original := ponsruntime.AgentDefinition{ID: ponsruntime.DefaultAgentID, Name: "Ada", Persona: "Be brief.", MaxTurns: 3}
	if err := agents.Record(original); err != nil {
		t.Fatal(err)
	}
	first, err := New(Config{Agent: original, AgentRevisions: agents, Store: idleStore{store}, Runner: RunnerFunc(func(context.Context, RunRequest) (RunResult, error) {
		t.Error("idle manager ran work")
		return RunResult{}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	conversation, err := first.CreateConversation(ctx, ponsruntime.ConversationOptions{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if conversation.AgentID != original.ID {
		t.Fatalf("conversation agent = %q", conversation.AgentID)
	}
	if _, err := first.Submit(ctx, conversation.ID, "before", []TextPart{{Type: "text", Text: "queued"}}); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	edited := original
	edited.Name, edited.Persona = "Grace", "Be thorough."
	if err := agents.Record(edited); err != nil {
		t.Fatal(err)
	}
	ran := make(chan RunRequest, 2)
	second, err := New(Config{Agent: edited, AgentRevisions: agents, Store: store, Runner: RunnerFunc(func(_ context.Context, request RunRequest) (RunResult, error) {
		ran <- request
		return RunResult{Answer: "done"}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	queued := receiveRequest(t, ran)
	if queued.Text != "queued" || queued.Agent.Name != "Ada" || queued.Agent.Persona != "Be brief." || queued.Agent.Revision() != original.Revision() {
		t.Fatalf("queued submission ran under %+v", queued.Agent)
	}
	waitForEvent(t, second, conversation.ID, EventRunCompleted, 1)
	if _, err := second.Submit(ctx, conversation.ID, "after", []TextPart{{Type: "text", Text: "new"}}); err != nil {
		t.Fatal(err)
	}
	fresh := receiveRequest(t, ran)
	if fresh.Text != "new" || fresh.Agent.Revision() != edited.Revision() {
		t.Fatalf("new submission ran under %+v", fresh.Agent)
	}
	runs := waitForEvent(t, second, conversation.ID, EventRunCompleted, 2)

	var revisions []string
	for _, event := range runs {
		if event.Type == ponsruntime.EventRunStarted || event.Type == EventRunCompleted {
			revisions = append(revisions, event.Run.AgentRevision)
		}
	}
	want := []string{original.Revision(), original.Revision(), edited.Revision(), edited.Revision()}
	if !slices.Equal(revisions, want) {
		t.Fatalf("run revisions = %v, want %v", revisions, want)
	}
	view, err := second.View(ctx, conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Submissions) != 2 ||
		view.Submissions[0].AgentRevision != original.Revision() ||
		view.Submissions[1].AgentRevision != edited.Revision() {
		t.Fatalf("submission revisions = %+v", view.Submissions)
	}
	if view.Agent != (ponsruntime.AgentSummary{ID: "default", Name: "Grace"}) || second.Agent() != view.Agent {
		t.Fatalf("view agent = %+v, manager agent = %+v", view.Agent, second.Agent())
	}
}

func TestUnresolvedAgentRevisionFailsClosed(t *testing.T) {
	stateDir := t.TempDir()
	store, err := runtimesqlite.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	agents, err := agentdir.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := agents.Record(testAgent); err != nil {
		t.Fatal(err)
	}
	var runs atomic.Int32
	manager, err := New(Config{Agent: testAgent, AgentRevisions: agents, Store: idleStore{store}, Runner: RunnerFunc(func(context.Context, RunRequest) (RunResult, error) {
		return RunResult{}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	conversation, err := manager.CreateConversation(context.Background(), ponsruntime.ConversationOptions{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Submit(context.Background(), conversation.ID, "key", []TextPart{{Type: "text", Text: "go"}}); err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}

	// The queued submission's snapshot disappears; the next server's current
	// definition differs, so nothing may fall back to it.
	revisions := filepath.Join(agents.Dir(testAgent.ID), "revisions")
	if err := os.RemoveAll(revisions); err != nil {
		t.Fatal(err)
	}
	current := testAgent
	current.Persona = "Different authority."
	if err := agents.Record(current); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(Config{Agent: current, AgentRevisions: agents, Store: store, Runner: RunnerFunc(func(context.Context, RunRequest) (RunResult, error) {
		runs.Add(1)
		return RunResult{Answer: "should not run"}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.Close(); _ = store.Close() })

	events := waitForEvent(t, restarted, conversation.ID, ponsruntime.EventRunFailed, 1)
	var failed Event
	for _, event := range events {
		if event.Type == ponsruntime.EventRunFailed {
			failed = event
		}
	}
	if failed.Run.Status != RunFailed || !strings.Contains(failed.Run.Error, "unavailable") {
		t.Fatalf("run = %+v", failed.Run)
	}
	if runs.Load() != 0 {
		t.Fatal("runner executed an unresolved agent revision")
	}
}

func TestManagerRequiresAgent(t *testing.T) {
	store, err := runtimesqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	runner := RunnerFunc(func(context.Context, RunRequest) (RunResult, error) {
		return RunResult{}, nil
	})
	if _, err := New(Config{Store: store, Runner: runner}); !errors.Is(err, ponsruntime.ErrInvalidAgent) {
		t.Fatalf("err = %v, want ErrInvalidAgent", err)
	}
	if _, err := New(Config{Agent: testAgent, AgentRevisions: agentRevisions{}, Store: store, Runner: runner}); err == nil {
		t.Fatal("manager started with an unrecorded current revision")
	}
}

func receiveRequest(t *testing.T, requests <-chan RunRequest) RunRequest {
	t.Helper()
	select {
	case request := <-requests:
		return request
	case <-time.After(3 * time.Second):
		t.Fatal("run did not start")
		return RunRequest{}
	}
}
