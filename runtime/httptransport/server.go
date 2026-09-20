// Package httptransport exposes the native pons runtime protocol over HTTP
// and Server-Sent Events. It is a transport adapter over runtime.Manager, not
// part of the runtime's persistence or orchestration model.
package httptransport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	ponsruntime "github.com/samperrin/pons/runtime"
)

// Runtime is the transport-independent conversation surface required by the
// native HTTP adapter.
type Runtime interface {
	CreateConversation(context.Context) (ponsruntime.Conversation, error)
	Submit(context.Context, string, string, []ponsruntime.TextPart) (ponsruntime.AcceptedMessage, error)
	View(context.Context, string) (ponsruntime.ConversationView, error)
	Subscribe(context.Context, string, uint64) (<-chan ponsruntime.Event, error)
}

// Handler exposes the runtime v1 loopback HTTP API.
func Handler(runtime Runtime) http.Handler {
	server := server{runtime: runtime}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/conversations", server.createConversation)
	mux.HandleFunc("GET /v1/conversations/{id}", server.getConversation)
	mux.HandleFunc("POST /v1/conversations/{id}/messages", server.submitMessage)
	mux.HandleFunc("GET /v1/conversations/{id}/events", server.events)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ok"}`+"\n")
	})
	return securityHeaders(mux)
}

type server struct{ runtime Runtime }

type messageBody struct {
	Parts []ponsruntime.TextPart `json:"parts"`
}

func (s server) getConversation(w http.ResponseWriter, r *http.Request) {
	view, err := s.runtime.View(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, ponsruntime.ErrNotFound) {
			writeAPIError(w, http.StatusNotFound, "conversation not found")
			return
		}
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

func (s server) createConversation(w http.ResponseWriter, r *http.Request) {
	if r.Body != nil {
		defer r.Body.Close()
		var extra any
		dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
		if err := dec.Decode(&extra); err != nil && !errors.Is(err, io.EOF) {
			writeAPIError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
	}
	conversation, err := s.runtime.CreateConversation(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, conversation)
}

func (s server) submitMessage(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	var body messageBody
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid message body: "+err.Error())
		return
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		writeAPIError(w, http.StatusBadRequest, "message body contains trailing data")
		return
	}
	accepted, err := s.runtime.Submit(r.Context(), r.PathValue("id"), r.Header.Get("Idempotency-Key"), body.Parts)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, ponsruntime.ErrNotFound) {
			status = http.StatusNotFound
		} else if errors.Is(err, ponsruntime.ErrClosed) {
			status = http.StatusServiceUnavailable
		}
		writeAPIError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, accepted)
}

func (s server) events(w http.ResponseWriter, r *http.Request) {
	after := parseEventID(r.Header.Get("Last-Event-ID"))
	if query := r.URL.Query().Get("after"); query != "" {
		parsed, err := strconv.ParseUint(query, 10, 64)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid event cursor")
			return
		}
		after = parsed
	}
	events, err := s.runtime.Subscribe(r.Context(), r.PathValue("id"), after)
	if err != nil {
		if errors.Is(err, ponsruntime.ErrNotFound) {
			writeAPIError(w, http.StatusNotFound, "conversation not found")
			return
		}
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAPIError(w, http.StatusInternalServerError, "streaming is unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	heartbeats := time.NewTicker(15 * time.Second)
	defer heartbeats.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case now := <-heartbeats.C:
			if !writeEvent(w, flusher, ponsruntime.Event{Type: ponsruntime.EventHeartbeat, ConversationID: r.PathValue("id"), CreatedAt: now.UTC()}) {
				return
			}
		case event, ok := <-events:
			if !ok || !writeEvent(w, flusher, event) {
				return
			}
		}
	}
}

func writeEvent(w io.Writer, flusher http.Flusher, event ponsruntime.Event) bool {
	data, err := json.Marshal(event)
	if err != nil {
		return false
	}
	prefix := ""
	if event.ID > 0 {
		prefix = fmt.Sprintf("id: %d\n", event.ID)
	}
	if _, err := fmt.Fprintf(w, "%sevent: %s\ndata: %s\n\n", prefix, event.Type, data); err != nil {
		return false
	}
	flusher.Flush()
	return true
}

func parseEventID(value string) uint64 {
	id, _ := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
	return id
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeAPIError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": strings.TrimSpace(message)})
}
