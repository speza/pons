package httptransport

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	ponsruntime "github.com/samperrin/pons/runtime"
)

// The client and handler share routes and wire shapes, so exercise the
// client against the real handler rather than a hand-written server.
func TestClientRoundTripsThroughHandler(t *testing.T) {
	runtime := &fakeRuntime{
		created:       ponsruntime.Conversation{ID: "conversation-1", Environment: "seatbelt"},
		conversations: []ponsruntime.Conversation{{ID: "one", Workspace: "/workspace"}},
	}
	server := httptest.NewServer(HandlerWithOptions(runtime, HandlerOptions{
		Environments: []string{"seatbelt"}, DefaultEnvironment: "seatbelt",
	}))
	defer server.Close()
	client := Client{BaseURL: server.URL}
	ctx := context.Background()

	conversations, err := client.ListConversations(ctx)
	if err != nil || len(conversations) != 1 || conversations[0].ID != "one" || conversations[0].Workspace != "/workspace" {
		t.Fatalf("conversations = %+v, err = %v", conversations, err)
	}

	selection := ponsruntime.ConversationOptions{Environment: "seatbelt", Workspace: "/workspace"}
	conversation, err := client.CreateConversation(ctx, selection)
	if err != nil || conversation.ID != "conversation-1" || runtime.createdOptions != selection {
		t.Fatalf("conversation = %+v, options = %+v, err = %v", conversation, runtime.createdOptions, err)
	}

	options, err := client.RuntimeOptions(ctx)
	if err != nil || options.DefaultEnvironment != "seatbelt" || len(options.Environments) != 1 {
		t.Fatalf("options = %+v, err = %v", options, err)
	}
}

func TestSendHydratesExistingConversationBeforeEvents(t *testing.T) {
	var (
		mu    sync.Mutex
		order []string
		after string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		order = append(order, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/conversations/conversation":
			_ = json.NewEncoder(w).Encode(ponsruntime.ConversationView{
				Conversation: ponsruntime.Conversation{ID: "conversation"}, EventCursor: 7,
				Messages: []ponsruntime.Message{{
					ID: "answer", InboundMessageID: "message", Role: "assistant", Complete: true, Final: true,
					Parts: []ponsruntime.MessagePart{{Type: "text", Text: "already done"}},
				}},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/conversations/conversation/events":
			after = r.URL.Query().Get("after")
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case r.Method == http.MethodPost && r.URL.Path == "/v1/conversations/conversation/messages":
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(ponsruntime.AcceptedMessage{ConversationID: "conversation", InboundMessageID: "message", Duplicate: true})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var hydrated bool
	result, err := (Client{BaseURL: server.URL}).Send(ctx, "conversation", "key", "hello", ponsruntime.ConversationOptions{}, func(view ponsruntime.ConversationView) {
		hydrated = view.EventCursor == 7
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !hydrated || after != "7" || result.Answer != "already done" {
		t.Fatalf("hydrated=%v after=%q result=%+v", hydrated, after, result)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(order) < 3 || order[0] != "GET /v1/conversations/conversation" || order[1] != "GET /v1/conversations/conversation/events" || order[2] != "POST /v1/conversations/conversation/messages" {
		t.Fatalf("request order = %v", order)
	}
}

func TestSendCreatesConversationWithWorkspaceSelection(t *testing.T) {
	runtime := &fakeRuntime{
		created:  ponsruntime.Conversation{ID: "new-conversation"},
		accepted: ponsruntime.AcceptedMessage{ConversationID: "new-conversation", InboundMessageID: "first-message"},
		events: []ponsruntime.Event{{
			Type:             ponsruntime.EventAssistantCommitted,
			InboundMessageID: "first-message",
			AssistantOutput: &ponsruntime.AssistantOutput{
				ID: "answer", Final: true,
				Parts: []ponsruntime.MessagePart{{Type: "text", Text: "done"}},
			},
		}},
	}
	server := httptest.NewServer(Handler(runtime))
	defer server.Close()
	selection := ponsruntime.ConversationOptions{Workspace: "/selected/project"}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result, err := (Client{BaseURL: server.URL}).Send(ctx, "", "first", "hello", selection, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.createdOptions != selection || len(runtime.submitted) != 1 || runtime.submitted[0].Text != "hello" ||
		result.ConversationID != "new-conversation" || result.Answer != "done" {
		t.Fatalf("selection=%+v submitted=%+v result=%+v", runtime.createdOptions, runtime.submitted, result)
	}
}

func TestSendReportsStoppedRun(t *testing.T) {
	runtime := &fakeRuntime{
		created:  ponsruntime.Conversation{ID: "conversation"},
		accepted: ponsruntime.AcceptedMessage{ConversationID: "conversation", InboundMessageID: "input"},
		events: []ponsruntime.Event{{
			Type: ponsruntime.EventRunStopped, InboundMessageID: "input", RunID: "run",
			Run: &ponsruntime.Run{ID: "run", InboundMessageID: "input", Status: ponsruntime.RunStopped},
		}},
	}
	server := httptest.NewServer(Handler(runtime))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := (Client{BaseURL: server.URL}).Send(ctx, "", "key", "hello", ponsruntime.ConversationOptions{}, nil, nil); !errors.Is(err, ErrRunStopped) {
		t.Fatalf("send error = %v, want ErrRunStopped", err)
	}
}
