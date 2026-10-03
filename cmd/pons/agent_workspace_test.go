package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samperrin/pons/environment"
	"github.com/samperrin/pons/plugins/brain/llm"
	ponsruntime "github.com/samperrin/pons/runtime"
	"github.com/samperrin/pons/runtime/agentdir"
	"github.com/samperrin/pons/runtime/httptransport"
)

func TestAgentWorkspacePersistsAcrossConversations(t *testing.T) {
	provider, requests := scriptedProvider(t, func(request providerRequest) providerReply {
		role, content := request.last()
		switch {
		case role == "tool":
			return providerReply{Text: "done"}
		case strings.Contains(content, "WRITE-NOTE"):
			return providerReply{Tool: "write_file", Arguments: map[string]string{"path": "notes.txt", "content": "kept"}}
		default:
			return providerReply{Tool: "read_file", Arguments: map[string]string{"path": "notes.txt"}}
		}
	})

	stateDir := t.TempDir()
	bundledRun(t, stateDir, provider.URL, "WRITE-NOTE")
	bundledRun(t, stateDir, provider.URL, "READ-NOTE")

	data, err := os.ReadFile(filepath.Join(stateDir, "agents", "default", "workspace", "notes.txt"))
	if err != nil || string(data) != "kept" {
		t.Fatalf("agent workspace file = %q, %v", data, err)
	}
	all := requests()
	if len(all) != 4 {
		t.Fatalf("provider requests = %d, want 4", len(all))
	}
	if role, content := all[3].last(); role != "tool" || !strings.Contains(content, "kept") {
		t.Fatalf("second conversation read %s %q", role, content)
	}
}

func agentWorkspaceRunner(t *testing.T, sandbox string) (*agentRunner, remoteMemoryEnvironment, *agentdir.Store) {
	t.Helper()
	provider, _ := scriptedProvider(t, func(providerRequest) providerReply { return providerReply{Text: "ok"} })
	agents, err := agentdir.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := agents.Load(ponsruntime.DefaultAgentID); err != nil {
		t.Fatal(err)
	}
	execution := remoteMemoryEnvironment{specs: make(chan environment.Spec, 1)}
	runner := &agentRunner{opts: serverOptions{
		MaxTurns: 2, Sandbox: sandbox, Environment: execution,
		EnvironmentSpec: environment.Spec{Command: []string{"/test/pons-hands"}},
		Brain:           llm.Config{Provider: "openai", Model: "test", APIKey: "test", BaseURL: provider.URL + "/v1"},
	}, logger: newServerLogger(io.Discard, false), agents: agents}
	return runner, execution, agents
}

func agentWorkspaceRequest() ponsruntime.RunRequest {
	return ponsruntime.RunRequest{
		Agent: ponsruntime.AgentDefinition{
			ID: ponsruntime.DefaultAgentID, Model: "test", MaxTurns: 2,
			WorkspacePolicy: ponsruntime.WorkspaceAgent,
		},
		ConversationID: "conversation-1", WorkspaceID: ponsruntime.AgentWorkspaceID(ponsruntime.DefaultAgentID),
		AgentWorkspace: true, RunID: "run-1", Environment: "seatbelt", Text: "hi",
		Emit: func(ponsruntime.RunEvent) error { return nil },
	}
}

func TestAgentWorkspaceRunGrantsOnlyWorkspaceAndMemory(t *testing.T) {
	runner, execution, agents := agentWorkspaceRunner(t, "seatbelt")
	if _, err := runner.Run(context.Background(), agentWorkspaceRequest()); err != nil {
		t.Fatal(err)
	}

	spec := <-execution.specs
	dir, _ := filepath.EvalSymlinks(agents.Dir(ponsruntime.DefaultAgentID))
	if spec.WorkspaceID != "agent-default" || spec.WorkspacePath != filepath.Join(dir, agentdir.WorkspaceDir) {
		t.Fatalf("workspace = %q at %q", spec.WorkspaceID, spec.WorkspacePath)
	}
	if len(spec.ReadWrite) != 1 || spec.ReadWrite[0] != filepath.Join(dir, agentdir.MemoryDir) {
		t.Fatalf("read-write grants = %v", spec.ReadWrite)
	}
	if spec.WorkspacePlan != (environment.WorkspacePlan{}) {
		t.Fatalf("workspace plan = %+v", spec.WorkspacePlan)
	}
}

func TestEmptyWorkspacePolicyResolvesToAgent(t *testing.T) {
	for _, test := range []struct {
		sandbox, setting, want string
	}{
		{"seatbelt", "", ponsruntime.WorkspaceAgent},
		{"e2b", "", ponsruntime.WorkspaceAgent},
		{"e2b", "per_conversation", ponsruntime.WorkspacePerConversation},
	} {
		var provider environment.Provider = &recordingEnvironment{}
		if test.sandbox == "e2b" {
			provider = &lifecycleEnvironment{}
		}
		agents, err := agentdir.Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(agents.Dir(ponsruntime.DefaultAgentID), agentdir.SettingsFile), `{"workspace":"`+test.setting+`"}`)
		agent, err := defaultAgent(serverOptions{Sandbox: test.sandbox, Environment: provider}, agents)
		if err != nil || agent.WorkspacePolicy != test.want {
			t.Errorf("%s with %q = %q, %v; want %q", test.sandbox, test.setting, agent.WorkspacePolicy, err, test.want)
		}
	}
}

// durableMemoryEnvironment stands in for E2B: a provider that keeps its own
// remote copy of the workspace.
type durableMemoryEnvironment struct{ remoteMemoryEnvironment }

func (durableMemoryEnvironment) SetStores(environment.StateStore, environment.CheckpointStore) error {
	return nil
}

func TestDurableAgentWorkspaceRunSeedsEmptyWorkspace(t *testing.T) {
	runner, execution, agents := agentWorkspaceRunner(t, "e2b")
	runner.opts.Environment = durableMemoryEnvironment{execution}
	request := agentWorkspaceRequest()
	request.Environment = "e2b"
	if _, err := runner.Run(context.Background(), request); err != nil {
		t.Fatal(err)
	}

	spec := <-execution.specs
	if spec.WorkspaceID != "agent-default" || spec.WorkspacePath != "" ||
		spec.WorkspacePlan != (environment.WorkspacePlan{Strategy: environment.WorkspaceStrategyEmpty}) {
		t.Fatalf("workspace = %q at %q with plan %+v", spec.WorkspaceID, spec.WorkspacePath, spec.WorkspacePlan)
	}
	dir, _ := filepath.EvalSymlinks(agents.Dir(ponsruntime.DefaultAgentID))
	if len(spec.ReadWrite) != 1 || spec.ReadWrite[0] != filepath.Join(dir, agentdir.MemoryDir) {
		t.Fatalf("read-write grants = %v", spec.ReadWrite)
	}
}

func TestAgentRunnerRejectsInconsistentAgentWorkspace(t *testing.T) {
	runner, execution, _ := agentWorkspaceRunner(t, "seatbelt")
	for name, change := range map[string]func(*ponsruntime.RunRequest){
		"another agent's workspace": func(r *ponsruntime.RunRequest) { r.WorkspaceID = "agent-other" },
		"host path":                 func(r *ponsruntime.RunRequest) { r.Workspace = t.TempDir() },
		"unmarked agent workspace":  func(r *ponsruntime.RunRequest) { r.AgentWorkspace = false },
		"missing workspace":         func(r *ponsruntime.RunRequest) { r.AgentWorkspace, r.WorkspaceID = false, "" },
	} {
		request := agentWorkspaceRequest()
		change(&request)
		if _, err := runner.Run(context.Background(), request); err == nil || !strings.Contains(err.Error(), "inconsistent workspace") {
			t.Errorf("%s: err = %v", name, err)
		}
	}

	if len(execution.specs) != 0 {
		t.Fatal("environment started for a rejected run")
	}
}

func writePerConversationAgent(t *testing.T, stateDir string) {
	t.Helper()
	write(t, filepath.Join(stateDir, "agents", "default", agentdir.SettingsFile), `{"workspace":"per_conversation"}`)
}

func TestPerConversationServerCanonicalizesHostWorkspace(t *testing.T) {
	stateDir, root := t.TempDir(), t.TempDir()
	writePerConversationAgent(t, stateDir)
	project := filepath.Join(root, "project")
	alias := filepath.Join(root, "alias")
	if err := os.Mkdir(project, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(project, alias); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- runServerReady(ctx, newServerLogger(io.Discard, false), testServerOptions(serverOptions{
			Address: "127.0.0.1:0", StateDir: stateDir, WorkspaceRoot: root, MaxConcurrent: 1,
		}), started)
	}()
	var serverURL string
	select {
	case serverURL = <-started:
	case err := <-done:
		t.Fatalf("server stopped: %v", err)
	}
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()

	client := httptransport.Client{BaseURL: serverURL}
	conversation, err := client.CreateConversation(ctx, ponsruntime.ConversationOptions{Workspace: alias})
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(project)
	if err != nil {
		t.Fatal(err)
	}
	if conversation.AgentWorkspace || conversation.WorkspaceID != conversation.ID || conversation.Workspace != canonical {
		t.Fatalf("conversation = %+v, want workspace %s", conversation, canonical)
	}
	if _, err := client.CreateConversation(ctx, ponsruntime.ConversationOptions{Workspace: filepath.Join(stateDir, "agents")}); err == nil {
		t.Fatal("workspace inside the state directory accepted over HTTP")
	}
}

func TestClientDefaultsToCurrentDirectoryUnderPerConversation(t *testing.T) {
	provider, _ := scriptedProvider(t, func(request providerRequest) providerReply {
		if role, _ := request.last(); role == "tool" {
			return providerReply{Text: "done"}
		}
		return providerReply{Tool: "write_file", Arguments: map[string]string{"path": "marker.txt", "content": "here"}}
	})
	stateDir, project := t.TempDir(), t.TempDir()
	writePerConversationAgent(t, stateDir)
	t.Chdir(project)

	bundledRun(t, stateDir, provider.URL, "mark the project")
	if data, err := os.ReadFile(filepath.Join(project, "marker.txt")); err != nil || string(data) != "here" {
		t.Fatalf("marker in current directory = %q, %v", data, err)
	}
}

func TestClientRejectsWorkspaceOptionsUnderAgentPolicy(t *testing.T) {
	provider, requests := scriptedProvider(t, func(providerRequest) providerReply { return providerReply{Text: "ok"} })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := runBundled(ctx, newServerLogger(io.Discard, false), testServerOptions(serverOptions{
		StateDir: t.TempDir(), WorkspaceRoot: os.TempDir(), ClientWorkspace: t.TempDir(), MaxTurns: 2, MaxConcurrent: 1,
		Brain: llm.Config{Provider: "openai", Model: "test", APIKey: "test", BaseURL: provider.URL + "/v1"},
	}), "", "", "hi", false)
	if err == nil || !strings.Contains(err.Error(), "workspace and Git options do not apply") {
		t.Fatalf("err = %v", err)
	}
	if len(requests()) != 0 {
		t.Fatal("rejected client still ran the agent")
	}
}
