package httptransport

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	ponsruntime "github.com/samperrin/pons/runtime"
)

type fakeRuntime struct {
	view           ponsruntime.ConversationView
	events         []ponsruntime.Event
	after          uint64
	created        ponsruntime.Conversation
	accepted       ponsruntime.AcceptedMessage
	submitted      []ponsruntime.TextPart
	createdOptions ponsruntime.ConversationOptions
}

func (f *fakeRuntime) CreateConversation(_ context.Context, options ponsruntime.ConversationOptions) (ponsruntime.Conversation, error) {
	f.createdOptions = options
	return f.created, nil
}

func (f *fakeRuntime) Submit(_ context.Context, _, _ string, parts []ponsruntime.TextPart) (ponsruntime.AcceptedMessage, error) {
	f.submitted = append([]ponsruntime.TextPart(nil), parts...)
	return f.accepted, nil
}

func (f *fakeRuntime) View(context.Context, string) (ponsruntime.ConversationView, error) {
	return f.view, nil
}

func (f *fakeRuntime) Subscribe(_ context.Context, _ string, after uint64) (<-chan ponsruntime.Event, error) {
	f.after = after
	stream := make(chan ponsruntime.Event, len(f.events))
	for _, event := range f.events {
		stream <- event
	}
	close(stream)
	return stream, nil
}

func TestHandlerReturnsConversationSnapshot(t *testing.T) {
	runtime := &fakeRuntime{view: ponsruntime.ConversationView{
		Conversation: ponsruntime.Conversation{ID: "conversation-1", Workspace: "/workspace"},
		Messages:     []ponsruntime.Message{{ID: "message-1", Role: "user", Parts: []ponsruntime.MessagePart{{Type: "text", Text: "hello"}}}},
		EventCursor:  9,
	}}
	req := httptest.NewRequest(http.MethodGet, "/v1/conversations/conversation-1", nil)
	recorder := httptest.NewRecorder()
	Handler(runtime).ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"event_cursor":9`) || !strings.Contains(body, `"id":"message-1"`) {
		t.Fatalf("snapshot body = %s", body)
	}
}

func TestCreateConversationPassesGitSelection(t *testing.T) {
	runtime := &fakeRuntime{created: ponsruntime.Conversation{ID: "session-1"}}
	req := httptest.NewRequest(http.MethodPost, "/v1/conversations", strings.NewReader(`{
		"git_repository":"https://github.com/acme/a.git",
		"git_revision":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"git_all_repositories":true
	}`))
	recorder := httptest.NewRecorder()
	Handler(runtime).ServeHTTP(recorder, req)
	if recorder.Code != http.StatusCreated ||
		runtime.createdOptions.GitRepository != "https://github.com/acme/a.git" ||
		runtime.createdOptions.GitRevision != strings.Repeat("a", 40) ||
		!runtime.createdOptions.GitAllRepositories {
		t.Fatalf("status = %d, options = %+v", recorder.Code, runtime.createdOptions)
	}
}

func TestCreateConversationPassesHostWorkspace(t *testing.T) {
	runtime := &fakeRuntime{created: ponsruntime.Conversation{ID: "session-1"}}
	req := httptest.NewRequest(http.MethodPost, "/v1/conversations", strings.NewReader(`{"workspace":"/projects/repo-a"}`))
	recorder := httptest.NewRecorder()
	Handler(runtime).ServeHTTP(recorder, req)
	if recorder.Code != http.StatusCreated || runtime.createdOptions.Workspace != "/projects/repo-a" {
		t.Fatalf("status = %d, options = %+v", recorder.Code, runtime.createdOptions)
	}
}

func TestHandlerUsesIDsOnlyForDurableSSEEvents(t *testing.T) {
	runtime := &fakeRuntime{events: []ponsruntime.Event{
		{ID: 7, Type: ponsruntime.EventRunUpdated, ConversationID: "conversation-1", CreatedAt: time.Now()},
		{Type: ponsruntime.EventAssistantDelta, ConversationID: "conversation-1", Delta: &ponsruntime.TextDelta{MessageID: "draft", PartID: "text", Text: "hi"}, CreatedAt: time.Now()},
	}}
	req := httptest.NewRequest(http.MethodGet, "/v1/conversations/conversation-1/events?after=6", nil)
	req.Header.Set("Last-Event-ID", "4")
	recorder := httptest.NewRecorder()
	Handler(runtime).ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if runtime.after != 6 {
		t.Fatalf("subscription cursor = %d", runtime.after)
	}
	body := recorder.Body.String()
	if strings.Count(body, "id: ") != 1 || !strings.Contains(body, "id: 7\n") {
		t.Fatalf("SSE IDs = %q", body)
	}
	if !strings.Contains(body, "event: assistant.delta") {
		t.Fatalf("transient event missing: %q", body)
	}
}

func TestScanSSEIgnoresTransportFieldsAndDecodesData(t *testing.T) {
	input := "id: 3\nevent: run.updated\ndata: {\"cursor\":3,\"type\":\"run.updated\",\"conversation_id\":\"c\",\"created_at\":\"2026-09-19T00:00:00Z\"}\n\n"
	var events []ponsruntime.Event
	err := scanSSE(strings.NewReader(input), func(event ponsruntime.Event) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].ID != 3 || events[0].Type != ponsruntime.EventRunUpdated {
		t.Fatalf("events = %+v", events)
	}
}

func TestHealth(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	recorder := httptest.NewRecorder()
	Handler(&fakeRuntime{}).ServeHTTP(recorder, req)
	data, err := io.ReadAll(recorder.Result().Body)
	if err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusOK || string(data) != "{\"status\":\"ok\"}\n" {
		t.Fatalf("status = %d, body = %q", recorder.Code, data)
	}
}
