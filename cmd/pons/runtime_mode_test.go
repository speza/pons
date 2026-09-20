package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samperrin/pons/environment"
	"github.com/samperrin/pons/plugins/brain/llm"
	"github.com/samperrin/pons/plugins/external"
	"github.com/samperrin/pons/protocol"
	ponsruntime "github.com/samperrin/pons/runtime"
)

type recordingEnvironment struct {
	starts atomic.Int32
	closes atomic.Int32
	specs  chan environment.Spec
}

func (p *recordingEnvironment) Start(_ context.Context, spec environment.Spec) (environment.HandsSession, error) {
	p.starts.Add(1)
	p.specs <- spec
	return recordingSession{closes: &p.closes}, nil
}

type recordingSession struct{ closes *atomic.Int32 }

func (s recordingSession) Catalog() []external.ToolDescription { return nil }
func (s recordingSession) Metadata() environment.Metadata {
	return environment.Metadata{Provider: "recording"}
}
func (s recordingSession) Execute(_ context.Context, action protocol.Action) (protocol.ToolResult, error) {
	return protocol.ToolResult{ActionID: action.ID, Kind: string(action.Kind), OK: true}, nil
}
func (s recordingSession) Close() error { s.closes.Add(1); return nil }

func TestAgentRunnerHydratesFreshBrainFromConversation(t *testing.T) {
	var (
		mu     sync.Mutex
		bodies []string
	)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(body))
		n := len(bodies)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c","object":"chat.completion","created":0,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"answer `+string(rune('0'+n))+`","refusal":null},"finish_reason":"stop"}]}`)
	}))
	defer provider.Close()
	workspace := t.TempDir()
	execution := &recordingEnvironment{specs: make(chan environment.Spec, 2)}
	var debugLog bytes.Buffer
	runner := &agentRunner{opts: serverOptions{
		Workspace:   workspace,
		MaxTurns:    3,
		Debug:       true,
		Environment: execution,
		EnvironmentSpec: environment.Spec{
			Command: []string{"/test/pons-hands"},
		},
		Brain: llm.Config{
			Provider: "openai", Model: "test", APIKey: "test", BaseURL: provider.URL + "/v1",
		},
	}, logger: log.New(&debugLog, "", 0)}
	requestNumber := 0
	var history []ponsruntime.Message
	request := func(text string) ponsruntime.RunRequest {
		requestNumber++
		inboundID := fmt.Sprintf("inbound-%d", requestNumber)
		return ponsruntime.RunRequest{
			ConversationID: "conversation-1", InboundMessageID: inboundID, Workspace: workspace, Text: text,
			Messages: append([]ponsruntime.Message(nil), history...),
			Emit:     func(ponsruntime.RunEvent) error { return nil },
		}
	}
	first, err := runner.Run(context.Background(), request("first question"))
	if err != nil || first.Answer != "answer 1" {
		t.Fatalf("first run = %+v, err = %v", first, err)
	}
	history = append(history,
		ponsruntime.Message{ID: "user-1", Role: "user", Complete: true, Parts: []ponsruntime.MessagePart{{Type: "text", Text: "first question"}}},
		ponsruntime.Message{ID: "assistant-1", Role: "assistant", Complete: true, Final: true, Parts: []ponsruntime.MessagePart{{Type: "text", Text: first.Answer}}},
	)
	second, err := runner.Run(context.Background(), request("second question"))
	if err != nil || second.Answer != "answer 2" {
		t.Fatalf("second run = %+v, err = %v", second, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 || !strings.Contains(bodies[1], "answer 1") || !strings.Contains(bodies[1], "second question") {
		t.Fatalf("second provider request was not hydrated: %v", bodies)
	}
	if execution.starts.Load() != 2 || execution.closes.Load() != 2 {
		t.Fatalf("environment lifecycle: starts=%d closes=%d", execution.starts.Load(), execution.closes.Load())
	}
	if got := debugLog.String(); !strings.Contains(got, "sandbox=recording") || !strings.Contains(got, "workspace="+fmt.Sprintf("%q", workspace)) {
		t.Fatalf("debug log omits effective environment: %s", got)
	}
	for range 2 {
		if spec := <-execution.specs; spec.Workspace != workspace {
			t.Fatalf("environment workspace = %q, want %q", spec.Workspace, workspace)
		}
	}
}

func TestDebugConfigurationIncludesSandboxPolicy(t *testing.T) {
	var output bytes.Buffer
	logDebugConfiguration(log.New(&output, "", 0), serverOptions{
		Debug: true, Sandbox: "seatbelt", Workspace: "/workspace", MaxTurns: 12, MaxConcurrent: 4,
		Brain:       llm.Config{Provider: "codex", Model: "gpt-5.6-luna"},
		Environment: &recordingEnvironment{},
		EnvironmentSpec: environment.Spec{
			Command:  []string{"/usr/local/bin/pons-hands"},
			Network:  environment.NetworkDisabled,
			ReadOnly: []string{"/plugins"},
		},
		PluginPaths: []string{"plugin.json"},
	})
	got := output.String()
	for _, want := range []string{
		"provider=codex", "model=gpt-5.6-luna", `workspace="/workspace"`,
		"sandbox=seatbelt", "network=disabled", `hands="/usr/local/bin/pons-hands"`,
		"external_plugins=1", "read_only_paths=1",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("debug configuration missing %q: %s", want, got)
		}
	}
}

func TestRuntimeServerRejectsPublicBind(t *testing.T) {
	if err := validateLoopbackAddress("0.0.0.0:7337"); err == nil {
		t.Fatal("public bind unexpectedly accepted")
	}
	for _, address := range []string{"127.0.0.1:7337", "[::1]:7337", "localhost:7337"} {
		if err := validateLoopbackAddress(address); err != nil {
			t.Fatalf("loopback %q rejected: %v", address, err)
		}
	}
}

func TestRuntimeServerShutdownClosesActiveSSE(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan string, 1)
	done := make(chan error, 1)
	stateDir, workspace := t.TempDir(), t.TempDir()
	go func() {
		done <- runServerReady(ctx, log.New(io.Discard, "", 0), serverOptions{
			Address: "127.0.0.1:0", StateDir: stateDir, Workspace: workspace, MaxConcurrent: 1,
		}, started)
	}()
	serverURL := <-started
	response, err := http.Post(serverURL+"/v1/conversations", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %s", response.Status)
	}
	var conversation ponsruntime.Conversation
	if err := json.NewDecoder(response.Body).Decode(&conversation); err != nil {
		t.Fatal(err)
	}
	stream, err := http.Get(serverURL + "/v1/conversations/" + conversation.ID + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	if stream.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %s", stream.Status)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("server shutdown waited on active SSE subscription")
	}
}

func TestRuntimeTurnsPreserveJSONNumberPrecision(t *testing.T) {
	turns, err := runtimeTurns([]ponsruntime.Message{{
		ID: "assistant", Role: "assistant", Complete: true,
		Parts: []ponsruntime.MessagePart{{Type: "tool_call", ToolCallID: "call", ToolKind: "tool", Arguments: json.RawMessage(`{"value":9007199254740993}`)}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	tool, ok := turns[0].Blocks[0].(llm.ToolUse)
	if !ok {
		t.Fatalf("block = %#v", turns[0].Blocks[0])
	}
	value, ok := tool.Input["value"].(json.Number)
	if !ok || value.String() != "9007199254740993" {
		t.Fatalf("value = %#v", tool.Input["value"])
	}
}

func TestRuntimeTurnsSkipIncompleteAssistantMessages(t *testing.T) {
	turns, err := runtimeTurns([]ponsruntime.Message{
		{ID: "draft", Role: "assistant", Parts: []ponsruntime.MessagePart{{Type: "text", Text: "partial"}}},
		{ID: "complete", Role: "assistant", Complete: true, Parts: []ponsruntime.MessagePart{{Type: "text", Text: "kept"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 || turns[0].Blocks[0].(llm.Text).Value != "kept" {
		t.Fatalf("turns = %+v", turns)
	}
}

func TestRuntimeTurnsNormalizeEmptyToolArguments(t *testing.T) {
	for _, arguments := range []json.RawMessage{nil, json.RawMessage(`null`)} {
		turns, err := runtimeTurns([]ponsruntime.Message{{
			ID: "assistant", Role: "assistant", Complete: true,
			Parts: []ponsruntime.MessagePart{{Type: "tool_call", ToolCallID: "call", ToolKind: "tool", Arguments: arguments}},
		}})
		if err != nil {
			t.Fatal(err)
		}
		tool := turns[0].Blocks[0].(llm.ToolUse)
		if tool.Input == nil || len(tool.Input) != 0 {
			t.Fatalf("input = %#v", tool.Input)
		}
	}
}

func TestExecutionEnvironmentAllowsConfiguredPluginFiles(t *testing.T) {
	dir := t.TempDir()
	executable := filepath.Join(dir, "plugin")
	resource := filepath.Join(dir, "resource.js")
	manifest := filepath.Join(dir, "plugin.json")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resource, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	manifestBody := fmt.Sprintf(`{"manifest_version":1,"name":"test.plugin","entrypoint":%q,"args":[%q],"runtime_protocol":1}`, executable, resource)
	if err := os.WriteFile(manifest, []byte(manifestBody), 0o600); err != nil {
		t.Fatal(err)
	}
	_, spec, err := executionEnvironment("seatbelt", "/bin/sh", false, serverOptions{PluginPaths: []string{manifest}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{manifest, executable, resource} {
		if !slices.Contains(spec.ReadOnly, want) {
			t.Fatalf("read-only paths %v do not contain %q", spec.ReadOnly, want)
		}
	}
}

func TestBundledCLIUsesRuntimeServerPath(t *testing.T) {
	var requests atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c","object":"chat.completion","created":0,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"bundled answer","refusal":null},"finish_reason":"stop"}]}`)
	}))
	defer provider.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := runBundled(ctx, log.New(&bytes.Buffer{}, "", 0), serverOptions{
		StateDir: t.TempDir(), Workspace: t.TempDir(), MaxTurns: 3, MaxConcurrent: 1,
		Brain: llm.Config{Provider: "openai", Model: "test", APIKey: "test", BaseURL: provider.URL + "/v1"},
	}, "", "stable", "hello", false)
	if err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 {
		t.Fatalf("provider requests = %d, want 1", requests.Load())
	}
}
