//go:build integration

package integration_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	ponsruntime "github.com/samperrin/pons/runtime"
)

const (
	fixtureName    = "fixture.txt"
	fixtureContent = "fixture-content-42"
)

// TestRuntimeBlackBox drives the compiled server with hands under Seatbelt.
// Hands only run in a sandbox, and Seatbelt is the local one, so the test
// needs macOS.
func TestRuntimeBlackBox(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("hands run only in a sandbox; the local Seatbelt sandbox requires macOS")
	}
	provider := newFakeOpenAI(t)
	workspace := t.TempDir()
	stateDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, fixtureName), []byte(fixtureContent), 0o600); err != nil {
		t.Fatal(err)
	}

	binaries := buildBinaries(t)
	server := startRuntimeServer(t, binaries.pons, binaries.hands, provider.URL()+"/v1", workspace, stateDir)
	defer server.Stop()

	if body := getHealth(t, server.URL()); body != `{"status":"ok"}` {
		t.Fatalf("health body = %q", body)
	}

	conversation := createConversation(t, server.URL(), workspace)
	stream := openSSE(t, server.URL(), conversation.ID, 0)
	accepted := submitMessage(t, server.URL(), conversation.ID, "first-attempt", "Read the fixture file and report what it contains.")
	events, answer := waitForFinal(t, stream, accepted.InboundMessageID)
	stream.Close()
	if answer != "BLACKBOX_TOOL_OK" {
		t.Fatalf("first answer = %q", answer)
	}
	assertLiveEvents(t, events, accepted.InboundMessageID)

	view := getConversation(t, server.URL(), conversation.ID)
	assertCompletedToolTranscript(t, view, accepted.InboundMessageID)
	if view.EventCursor == 0 {
		t.Fatal("completed conversation has no durable event cursor")
	}
	firstCursor := view.EventCursor
	firstProviderRequests := provider.RequestCount()
	if firstProviderRequests != 2 {
		t.Fatalf("provider requests for tool round-trip = %d, want 2", firstProviderRequests)
	}
	if !provider.RequestContains(fixtureContent) {
		t.Fatal("provider never received the real read_file result")
	}
	if !provider.RequestContains(`"read_file"`) {
		t.Fatal("provider request did not include the discovered read_file tool")
	}

	server.Stop()
	server = startRuntimeServer(t, binaries.pons, binaries.hands, provider.URL()+"/v1", workspace, stateDir)

	restartedView := getConversation(t, server.URL(), conversation.ID)
	if restartedView.EventCursor != firstCursor {
		t.Fatalf("event cursor after restart = %d, want %d", restartedView.EventCursor, firstCursor)
	}
	assertCompletedToolTranscript(t, restartedView, accepted.InboundMessageID)

	replay := openSSE(t, server.URL(), conversation.ID, 0)
	replayed := replayUntilCursor(t, replay, restartedView.EventCursor)
	replay.Close()
	if len(replayed) == 0 || replayed[len(replayed)-1].ID != restartedView.EventCursor {
		t.Fatalf("replayed events ended at %+v, want cursor %d", replayed, restartedView.EventCursor)
	}
	var previous uint64
	for i, event := range replayed {
		if event.ID <= previous || strings.HasPrefix(event.Type, "agent.") {
			t.Fatalf("replayed event %d has invalid client cursor/type: %+v", i, event)
		}
		previous = event.ID
	}

	duplicate := submitMessage(t, server.URL(), conversation.ID, "first-attempt", "This body must be ignored.")
	if !duplicate.Duplicate || duplicate.InboundMessageID != accepted.InboundMessageID {
		t.Fatalf("duplicate submission = %+v", duplicate)
	}
	if got := provider.RequestCount(); got != firstProviderRequests {
		t.Fatalf("duplicate submission triggered provider request; count = %d, want %d", got, firstProviderRequests)
	}

	stdout, stderr, err := runClient(t, binaries.pons, server.URL(), conversation.ID, "continue-after-restart", "continue after restart")
	if err != nil {
		t.Fatalf("compiled pons client failed: %v\nstderr:\n%s\nstdout:\n%s", err, stderr, stdout)
	}
	if !strings.Contains(stdout, "BLACKBOX_RESTART_OK") {
		t.Fatalf("compiled client output = %q", stdout)
	}
	if !strings.Contains(stdout, fixtureContent) {
		t.Fatalf("compiled client did not render the durable tool result: %q", stdout)
	}

	finalView := getConversation(t, server.URL(), conversation.ID)
	if len(finalView.Messages) != 6 {
		t.Fatalf("message count after continuation = %d, want 6", len(finalView.Messages))
	}
	if finalView.Messages[len(finalView.Messages)-1].Role != "assistant" || !finalView.Messages[len(finalView.Messages)-1].Final {
		t.Fatalf("last message after continuation = %+v", finalView.Messages[len(finalView.Messages)-1])
	}
	if provider.RequestCount() != firstProviderRequests+1 {
		t.Fatalf("provider requests after continuation = %d, want %d", provider.RequestCount(), firstProviderRequests+1)
	}
}

type builtBinaries struct {
	pons  string
	hands string
}

func buildBinaries(t *testing.T) builtBinaries {
	t.Helper()
	repoRoot := repositoryRoot(t)
	binDir := t.TempDir()
	result := builtBinaries{pons: filepath.Join(binDir, "pons")}
	build := func(output, packagePath string) {
		cmd := exec.Command("go", "build", "-o", output, packagePath)
		cmd.Dir = repoRoot
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go build %s: %v\n%s", packagePath, err, output)
		}
	}
	build(result.pons, "./cmd/pons")
	result.hands = filepath.Join(binDir, "pons-hands")
	build(result.hands, "./cmd/pons-hands")
	return result
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Dir(filepath.Dir(file))
}

type runtimeServer struct {
	cmd       *exec.Cmd
	url       string
	done      <-chan error
	stderrMu  sync.Mutex
	stderrBuf bytes.Buffer
}

func startRuntimeServer(t *testing.T, ponsBinary, handsBinary, providerURL, workspace, stateDir string) *runtimeServer {
	t.Helper()
	args := []string{
		"serve",
		"-provider", "openai",
		"-model", "blackbox-test",
		"-base-url", providerURL,
		"-workspace-root", workspace,
		"-state-dir", stateDir,
		"-addr", "127.0.0.1:0",
		"-max-turns", "4",
		"-runtime-concurrency", "1",
		"-sandbox", "seatbelt",
		"-hands-command", handsBinary,
	}
	cmd := exec.Command(ponsBinary, args...)
	cmd.Dir = workspace
	cmd.Stdout = io.Discard
	cmd.Env = isolatedEnvironment(t.TempDir())
	pipe, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start compiled pons server: %v", err)
	}

	server := &runtimeServer{cmd: cmd}
	t.Cleanup(server.Stop)
	ready := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(pipe)
		for scanner.Scan() {
			line := scanner.Text()
			server.stderrMu.Lock()
			server.stderrBuf.WriteString(line)
			server.stderrBuf.WriteByte('\n')
			server.stderrMu.Unlock()
			var entry struct {
				Message string `json:"msg"`
				Address string `json:"address"`
			}
			if json.Unmarshal([]byte(line), &entry) == nil && entry.Message == "runtime listening" {
				select {
				case ready <- "http://" + entry.Address:
				default:
				}
			}
		}
	}()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	server.done = done

	select {
	case server.url = <-ready:
	case err := <-done:
		server.cmd = nil
		t.Fatalf("compiled pons server exited before ready: %v\n%s", err, server.stderr())
	case <-time.After(10 * time.Second):
		server.Stop()
		t.Fatalf("timed out waiting for compiled pons server\n%s", server.stderr())
	}
	waitForHealth(t, server.URL())
	return server
}

func (s *runtimeServer) URL() string { return s.url }

func (s *runtimeServer) stderr() string {
	s.stderrMu.Lock()
	defer s.stderrMu.Unlock()
	return s.stderrBuf.String()
}

func (s *runtimeServer) Stop() {
	if s == nil || s.cmd == nil || s.cmd.Process == nil {
		return
	}
	_ = s.cmd.Process.Kill()
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		_ = s.cmd.Process.Kill()
		<-s.done
	}
	s.cmd = nil
}

func waitForHealth(t *testing.T, baseURL string) {
	t.Helper()
	client := &http.Client{Timeout: 500 * time.Millisecond}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		request, err := http.NewRequest(http.MethodGet, baseURL+"/healthz", nil)
		if err == nil {
			response, requestErr := client.Do(request)
			if requestErr == nil {
				body, readErr := io.ReadAll(response.Body)
				_ = response.Body.Close()
				if readErr == nil && response.StatusCode == http.StatusOK && string(body) == `{"status":"ok"}`+"\n" {
					return
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("compiled pons server did not become healthy at %s", baseURL)
}

func isolatedEnvironment(home string) []string {
	blocked := map[string]bool{
		"HOME":              true,
		"OPENAI_API_KEY":    true,
		"ANTHROPIC_API_KEY": true,
	}
	env := make([]string, 0, len(os.Environ())+1)
	for _, entry := range os.Environ() {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || blocked[key] {
			continue
		}
		env = append(env, entry)
	}
	return append(env, "HOME="+home)
}

type fakeOpenAI struct {
	server *httptest.Server
	mu     sync.Mutex
	bodies [][]byte
}

func newFakeOpenAI(t *testing.T) *fakeOpenAI {
	t.Helper()
	fake := &fakeOpenAI{}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
			writeProviderError(w, http.StatusNotFound, "expected POST /v1/chat/completions")
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			writeProviderError(w, http.StatusBadRequest, err.Error())
			return
		}
		fake.mu.Lock()
		fake.bodies = append(fake.bodies, append([]byte(nil), body...))
		requestNumber := len(fake.bodies)
		fake.mu.Unlock()

		var request struct {
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(body, &request); err != nil {
			writeProviderError(w, http.StatusBadRequest, "invalid request: "+err.Error())
			return
		}
		lastUser := ""
		for _, message := range request.Messages {
			if message.Role != "user" {
				continue
			}
			var content string
			if json.Unmarshal(message.Content, &content) == nil {
				lastUser = content
			}
		}

		if strings.Contains(lastUser, "continue after restart") {
			writeChatCompletion(w, requestNumber, "BLACKBOX_RESTART_OK", nil)
			return
		}
		if bytes.Contains(body, []byte(fixtureContent)) {
			writeChatCompletion(w, requestNumber, "BLACKBOX_TOOL_OK", nil)
			return
		}
		writeChatCompletion(w, requestNumber, "", map[string]string{
			"id":        "call-read-fixture",
			"name":      "read_file",
			"arguments": `{"path":"fixture.txt"}`,
		})
	}))
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *fakeOpenAI) URL() string { return f.server.URL }

func (f *fakeOpenAI) RequestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

func (f *fakeOpenAI) RequestContains(value string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, body := range f.bodies {
		if bytes.Contains(body, []byte(value)) {
			return true
		}
	}
	return false
}

func writeProviderError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": message}})
}

func writeChatCompletion(w http.ResponseWriter, requestNumber int, answer string, toolCall map[string]string) {
	message := map[string]any{
		"role":    "assistant",
		"content": nil,
		"refusal": nil,
	}
	finishReason := "stop"
	if toolCall != nil {
		message["tool_calls"] = []any{map[string]any{
			"id":   toolCall["id"],
			"type": "function",
			"function": map[string]string{
				"name":      toolCall["name"],
				"arguments": toolCall["arguments"],
			},
		}}
		finishReason = "tool_calls"
	} else {
		message["content"] = answer
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":      fmt.Sprintf("blackbox-completion-%d", requestNumber),
		"object":  "chat.completion",
		"created": 0,
		"model":   "blackbox-test",
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": finishReason,
		}},
	})
}

func getHealth(t *testing.T, baseURL string) string {
	t.Helper()
	response, err := (&http.Client{Timeout: 5 * time.Second}).Get(baseURL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d, body = %q", response.StatusCode, body)
	}
	return strings.TrimSpace(string(body))
}

func createConversation(t *testing.T, baseURL, workspace string) ponsruntime.Conversation {
	t.Helper()
	var conversation ponsruntime.Conversation
	doJSON(t, http.MethodPost, baseURL+"/v1/conversations", []byte(fmt.Sprintf(`{"workspace":%q}`, workspace)), "", http.StatusCreated, &conversation)
	canonical, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if conversation.ID == "" || conversation.Workspace != canonical {
		t.Fatalf("created conversation = %+v", conversation)
	}
	return conversation
}

func submitMessage(t *testing.T, baseURL, conversationID, key, text string) ponsruntime.AcceptedMessage {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"parts": []map[string]string{{"type": "text", "text": text}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var accepted ponsruntime.AcceptedMessage
	doJSON(t, http.MethodPost, baseURL+"/v1/conversations/"+conversationID+"/messages", body, key, http.StatusAccepted, &accepted)
	if accepted.InboundMessageID == "" {
		t.Fatalf("accepted message = %+v", accepted)
	}
	return accepted
}

func getConversation(t *testing.T, baseURL, conversationID string) ponsruntime.ConversationView {
	t.Helper()
	var view ponsruntime.ConversationView
	doJSON(t, http.MethodGet, baseURL+"/v1/conversations/"+conversationID, nil, "", http.StatusOK, &view)
	return view
}

func doJSON(t *testing.T, method, endpoint string, body []byte, idempotencyKey string, status int, value any) {
	t.Helper()
	request, err := http.NewRequest(method, endpoint, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != status {
		t.Fatalf("%s %s status = %d, want %d: %s", method, endpoint, response.StatusCode, status, responseBody)
	}
	if value != nil && json.Unmarshal(responseBody, value) != nil {
		t.Fatalf("decode %s %s response %q", method, endpoint, responseBody)
	}
}

type sseStream struct {
	response *http.Response
	cancel   context.CancelFunc
	events   chan ponsruntime.Event
	errors   chan error
}

func openSSE(t *testing.T, baseURL, conversationID string, after uint64) *sseStream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	request, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/v1/conversations/%s/events?after=%d", baseURL, conversationID, after), nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	request.Header.Set("Accept", "text/event-stream")
	response, err := (&http.Client{Timeout: 0}).Do(request)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		cancel()
		t.Fatalf("SSE status = %d: %s", response.StatusCode, body)
	}
	if got := response.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		_ = response.Body.Close()
		cancel()
		t.Fatalf("SSE content type = %q", got)
	}
	stream := &sseStream{response: response, cancel: cancel, events: make(chan ponsruntime.Event, 64), errors: make(chan error, 1)}
	go stream.scan()
	return stream
}

func (s *sseStream) scan() {
	defer close(s.events)
	defer close(s.errors)
	scanner := bufio.NewScanner(s.response.Body)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	var data string
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if data != "" {
				var event ponsruntime.Event
				if err := json.Unmarshal([]byte(data), &event); err != nil {
					s.errors <- err
					return
				}
				select {
				case s.events <- event:
				case <-s.response.Request.Context().Done():
					return
				}
			}
			data = ""
			continue
		}
		if value, ok := strings.CutPrefix(line, "data:"); ok {
			value = strings.TrimPrefix(value, " ")
			if data == "" {
				data = value
			} else {
				data += "\n" + value
			}
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, context.Canceled) {
		s.errors <- err
	}
}

func (s *sseStream) Close() {
	if s == nil {
		return
	}
	s.cancel()
	_ = s.response.Body.Close()
}

func waitForFinal(t *testing.T, stream *sseStream, inboundMessageID string) ([]ponsruntime.Event, string) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	var events []ponsruntime.Event
	for {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for final assistant SSE event")
		case err := <-stream.errors:
			if err != nil {
				t.Fatalf("SSE reader: %v", err)
			}
			stream.errors = nil
		case event, ok := <-stream.events:
			if !ok {
				t.Fatal("SSE stream closed before final assistant event")
			}
			if event.InboundMessageID != inboundMessageID {
				continue
			}
			events = append(events, event)
			if event.Type == ponsruntime.EventRunFailed && event.Run != nil {
				t.Fatalf("runtime run failed: %s; events = %+v", event.Run.Error, events)
			}
			if event.Type == ponsruntime.EventAssistantCommitted && event.AssistantOutput != nil && event.AssistantOutput.Final {
				message, err := event.ProjectMessage()
				if err != nil {
					t.Fatal(err)
				}
				return events, messageText(message)
			}
		}
	}
}

func replayUntilCursor(t *testing.T, stream *sseStream, cursor uint64) []ponsruntime.Event {
	t.Helper()
	deadline := time.After(10 * time.Second)
	var events []ponsruntime.Event
	for {
		select {
		case <-deadline:
			t.Fatalf("timed out replaying SSE through cursor %d", cursor)
		case err := <-stream.errors:
			if err != nil {
				t.Fatalf("SSE replay reader: %v", err)
			}
			stream.errors = nil
		case event, ok := <-stream.events:
			if !ok {
				t.Fatalf("SSE replay closed before cursor %d", cursor)
			}
			if event.ID == 0 {
				continue
			}
			events = append(events, event)
			if event.ID >= cursor {
				return events
			}
		}
	}
}

func assertLiveEvents(t *testing.T, events []ponsruntime.Event, inboundMessageID string) {
	t.Helper()
	wanted := map[string]bool{
		ponsruntime.EventInputAccepted:       false,
		ponsruntime.EventAssistantCommitted:  false,
		ponsruntime.EventToolOutcomeRecorded: false,
		ponsruntime.EventRunStarted:          false,
	}
	toolStatuses := map[string]bool{}
	for _, event := range events {
		if event.InboundMessageID != inboundMessageID {
			continue
		}
		if _, ok := wanted[event.Type]; ok {
			wanted[event.Type] = true
		}
		if event.Type == ponsruntime.EventAssistantCommitted && event.AssistantOutput != nil {
			for _, part := range event.AssistantOutput.Parts {
				if part.Type == "tool_call" {
					toolStatuses[ponsruntime.ToolRequested] = true
				}
			}
		}
		if event.Type == ponsruntime.EventToolOutcomeRecorded && event.ToolOutcome != nil {
			toolStatuses[event.ToolOutcome.Status] = true
		}
	}
	for eventType, seen := range wanted {
		if !seen {
			t.Errorf("live SSE did not include %s: %+v", eventType, events)
		}
	}
	for _, status := range []string{ponsruntime.ToolRequested, ponsruntime.ToolCompleted} {
		if !toolStatuses[status] {
			t.Errorf("live SSE did not include tool status %s: %+v", status, events)
		}
	}
}

func assertCompletedToolTranscript(t *testing.T, view ponsruntime.ConversationView, inboundMessageID string) {
	t.Helper()
	if len(view.Messages) != 4 {
		t.Fatalf("message count for tool round-trip = %d, want 4", len(view.Messages))
	}
	if view.Submissions[0].Status != ponsruntime.RunCompleted {
		t.Fatalf("submission = %+v", view.Submissions)
	}
	roles := []string{"user", "assistant", "tool", "assistant"}
	for i, role := range roles {
		if view.Messages[i].Role != role || view.Messages[i].InboundMessageID != inboundMessageID || !view.Messages[i].Complete {
			t.Fatalf("message[%d] = %+v", i, view.Messages[i])
		}
	}
	toolMessage := view.Messages[2]
	if len(toolMessage.Parts) != 1 || toolMessage.Parts[0].Result == nil || !toolMessage.Parts[0].Result.OK || !strings.Contains(toolMessage.Parts[0].Result.Output, fixtureContent) {
		t.Fatalf("tool message = %+v", toolMessage)
	}
	if len(view.ToolCalls) != 1 || view.ToolCalls[0].Status != ponsruntime.ToolCompleted || view.ToolCalls[0].Kind != "read_file" {
		t.Fatalf("tool calls = %+v", view.ToolCalls)
	}
	if got := messageText(view.Messages[3]); got != "BLACKBOX_TOOL_OK" {
		t.Fatalf("final message = %q", got)
	}
}

func messageText(message ponsruntime.Message) string {
	var text string
	for _, part := range message.Parts {
		if part.Type == "text" {
			text += part.Text
		}
	}
	return text
}

func runClient(t *testing.T, ponsBinary, serverURL, conversationID, idempotencyKey, message string) (string, string, error) {
	t.Helper()
	home := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, ponsBinary,
		"client", "-server", serverURL, "-conversation", conversationID,
		"-idempotency-key", idempotencyKey, "-message", message)
	cmd.Env = isolatedEnvironment(home)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return stdout.String(), stderr.String(), ctx.Err()
	}
	return stdout.String(), stderr.String(), err
}
