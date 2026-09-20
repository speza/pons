package httptransport

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	ponsruntime "github.com/samperrin/pons/runtime"
)

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
	result, err := (Client{BaseURL: server.URL}).Send(ctx, "conversation", "key", "hello", func(view ponsruntime.ConversationView) {
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
