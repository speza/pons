package main

import (
	"context"
	"errors"
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
		RunID: "run-1", Environment: "seatbelt", Text: "hi",
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

func TestAgentWorkspaceIsNotYetSupportedOnE2B(t *testing.T) {
	runner, execution, _ := agentWorkspaceRunner(t, "e2b")
	request := agentWorkspaceRequest()
	request.Environment = "e2b"
	if _, err := runner.Run(context.Background(), request); !errors.Is(err, errAgentWorkspaceE2B) {
		t.Fatalf("run err = %v", err)
	}
	if len(execution.specs) != 0 {
		t.Fatal("E2B environment started for an agent workspace")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	started := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- runServerReady(ctx, newServerLogger(io.Discard, false), serverOptions{
			Address: "127.0.0.1:0", StateDir: t.TempDir(), WorkspaceRoot: t.TempDir(),
			MaxConcurrent: 1, Sandbox: "e2b", Environment: &recordingEnvironment{},
		}, started)
	}()
	client := httptransport.Client{BaseURL: <-started}
	options, err := client.RuntimeOptions(ctx)
	if err != nil || options.Agent.WorkspacePolicy != ponsruntime.WorkspaceAgent {
		t.Fatalf("runtime options = %+v, %v", options, err)
	}
	if _, err := client.CreateConversation(ctx, ponsruntime.ConversationOptions{}); err == nil ||
		!strings.Contains(err.Error(), "not yet supported on E2B") {
		t.Fatalf("create conversation err = %v", err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
