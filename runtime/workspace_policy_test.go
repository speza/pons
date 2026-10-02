package runtime_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	ponsruntime "github.com/samperrin/pons/runtime"
	runtimesqlite "github.com/samperrin/pons/runtime/sqlite"
)

func agentPolicy(policy string) ponsruntime.AgentDefinition {
	agent := testAgent
	agent.WorkspacePolicy = policy
	return agent
}

// policyRevisions records the test agent under both policies, so work
// accepted under one resolves after a switch to the other.
var policyRevisions = agentRevisions{
	agentPolicy(ponsruntime.WorkspaceAgent).Revision():           agentPolicy(ponsruntime.WorkspaceAgent),
	agentPolicy(ponsruntime.WorkspacePerConversation).Revision(): agentPolicy(ponsruntime.WorkspacePerConversation),
}

func policyManager(t *testing.T, store ponsruntime.Store, agent ponsruntime.AgentDefinition, runner Runner) *Manager {
	t.Helper()
	m, err := New(Config{
		Agent: agent, AgentRevisions: policyRevisions,
		Store: store, Runner: runner, MaxConcurrent: 2,
		EnvironmentOptions: []string{"seatbelt", "e2b"}, DefaultEnvironment: "seatbelt",
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func openStore(t *testing.T) ponsruntime.Store {
	t.Helper()
	store, err := runtimesqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestAgentPolicyConversationsShareOneWorkspaceAndRunInTurn(t *testing.T) {
	ctx := context.Background()
	var running atomic.Bool
	release := make(chan struct{})
	ran := make(chan RunRequest, 2)
	m := policyManager(t, openStore(t), agentPolicy(ponsruntime.WorkspaceAgent), RunnerFunc(func(ctx context.Context, request RunRequest) (RunResult, error) {
		if !running.CompareAndSwap(false, true) {
			t.Error("two runs used the agent workspace at once")
		}
		defer running.Store(false)
		ran <- request
		// Watching ctx lets Close end a run held here when the test fails.
		select {
		case <-release:
			return RunResult{Answer: "done"}, nil
		case <-ctx.Done():
			return RunResult{}, ctx.Err()
		}
	}))
	defer m.Close()

	var ids []string
	for range 2 {
		conversation, err := m.CreateConversation(ctx, ponsruntime.ConversationOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if !conversation.AgentWorkspace || conversation.WorkspaceID != "agent-default" ||
			conversation.WorkspaceLock != "agent-default" || conversation.Workspace != "" ||
			conversation.Environment != "seatbelt" {
			t.Fatalf("conversation = %+v", conversation)
		}
		if _, err := m.Submit(ctx, conversation.ID, "go", []TextPart{{Type: "text", Text: "go"}}); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, conversation.ID)
	}

	for range 2 {
		request := receiveRequest(t, ran)
		if !request.AgentWorkspace || request.WorkspaceID != "agent-default" || request.Workspace != "" {
			t.Fatalf("request workspace = %q %q", request.WorkspaceID, request.Workspace)
		}
		release <- struct{}{}
	}
	for _, id := range ids {
		waitForEvent(t, m, id, EventRunCompleted, 1)
	}
}

func TestAgentPolicyRejectsWorkspaceChoices(t *testing.T) {
	m := policyManager(t, openStore(t), agentPolicy(ponsruntime.WorkspaceAgent), RunnerFunc(func(context.Context, RunRequest) (RunResult, error) {
		return RunResult{}, nil
	}))
	defer m.Close()

	for name, options := range map[string]ponsruntime.ConversationOptions{
		"workspace":        {Workspace: t.TempDir()},
		"git repository":   {GitRepository: "https://github.com/example/repo.git"},
		"git revision":     {GitRevision: "0123456789abcdef0123456789abcdef01234567"},
		"all repositories": {GitAllRepositories: true},
		"environment":      {Environment: "e2b"},
	} {
		if _, err := m.CreateConversation(context.Background(), options); !errors.Is(err, ponsruntime.ErrInvalidConversation) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if _, err := m.CreateConversation(context.Background(), ponsruntime.ConversationOptions{Environment: "seatbelt"}); err != nil {
		t.Fatalf("default environment rejected: %v", err)
	}
}

// A conversation keeps the workspace it was created with when the owner
// changes the agent's policy.
func TestConversationWorkspaceSurvivesPolicyChange(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	idle := RunnerFunc(func(context.Context, RunRequest) (RunResult, error) { return RunResult{}, nil })

	hostPath := t.TempDir()
	perConversation := policyManager(t, idleStore{store}, agentPolicy(ponsruntime.WorkspacePerConversation), idle)
	hosted, err := perConversation.CreateConversation(ctx, ponsruntime.ConversationOptions{Workspace: hostPath})
	if err != nil {
		t.Fatal(err)
	}
	if hosted.AgentWorkspace || hosted.WorkspaceID != hosted.ID || hosted.WorkspaceLock != hostPath {
		t.Fatalf("per-conversation workspace = %+v", hosted)
	}
	if _, err := perConversation.Submit(ctx, hosted.ID, "hosted", []TextPart{{Type: "text", Text: "hosted"}}); err != nil {
		t.Fatal(err)
	}
	if err := perConversation.Close(); err != nil {
		t.Fatal(err)
	}

	agentWorkspace := policyManager(t, idleStore{store}, agentPolicy(ponsruntime.WorkspaceAgent), idle)
	owned, err := agentWorkspace.CreateConversation(ctx, ponsruntime.ConversationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agentWorkspace.Submit(ctx, owned.ID, "owned", []TextPart{{Type: "text", Text: "owned"}}); err != nil {
		t.Fatal(err)
	}
	if err := agentWorkspace.Close(); err != nil {
		t.Fatal(err)
	}

	// Back under per_conversation, both queued runs keep their workspaces.
	ran := make(chan RunRequest, 2)
	final := policyManager(t, store, agentPolicy(ponsruntime.WorkspacePerConversation), RunnerFunc(func(_ context.Context, request RunRequest) (RunResult, error) {
		ran <- request
		return RunResult{Answer: "done"}, nil
	}))
	defer final.Close()
	got := map[string]RunRequest{}
	for range 2 {
		request := receiveRequest(t, ran)
		got[request.ConversationID] = request
	}
	if request := got[hosted.ID]; request.AgentWorkspace || request.WorkspaceID != hosted.ID || request.Workspace != hostPath {
		t.Fatalf("hosted conversation ran in %q %q", request.WorkspaceID, request.Workspace)
	}
	if request := got[owned.ID]; !request.AgentWorkspace || request.WorkspaceID != "agent-default" || request.Workspace != "" {
		t.Fatalf("agent conversation ran in %q %q", request.WorkspaceID, request.Workspace)
	}
}
