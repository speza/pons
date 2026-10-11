// Package mcptest provides an in-process fake MCP server for tests. It speaks
// just enough Streamable HTTP for tools, is written without the MCP SDK so it
// checks the client's wire behavior independently, and records what it
// receives.
package mcptest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// Tool is one tool the fake server lists.
type Tool struct {
	Name        string
	Description string
	// InputSchema defaults to an empty object schema.
	InputSchema json.RawMessage
	ReadOnly    bool
}

// Result is what a tool call returns. Content, when set, replaces the single
// text block built from Text.
type Result struct {
	Text       string
	IsError    bool
	Structured any
	Content    []map[string]any
}

// Request is one JSON-RPC message the server received.
type Request struct {
	Method string
	Params json.RawMessage
	Header http.Header
}

// Handler answers a tool call. ctx ends when the client gives up.
type Handler func(ctx context.Context, tool string, args json.RawMessage) Result

// Server is a fake MCP server listening on loopback HTTP.
type Server struct {
	URL string

	mu              sync.Mutex
	tools           []Tool
	handler         Handler
	requests        []Request
	sessions        map[string]bool
	nextSession     int
	requireSampling bool
	redirect        string
	status          int
}

// New starts a fake server that stops when the test ends.
func New(t testing.TB) *Server {
	t.Helper()
	server := &Server{sessions: map[string]bool{}}
	listener := httptest.NewServer(http.HandlerFunc(server.serve))
	t.Cleanup(listener.Close)
	server.URL = listener.URL
	return server
}

// SetTools replaces the tool list.
func (s *Server) SetTools(tools ...Tool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tools = tools
}

// Handle sets the tool call handler. Without one, a call echoes its tool name.
func (s *Server) Handle(handler Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handler = handler
}

// ExpireSessions makes the server forget every open session, as a restarted
// or load-balanced server would.
func (s *Server) ExpireSessions() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions = map[string]bool{}
}

// RequireSampling makes initialize fail unless the client offers sampling.
func (s *Server) RequireSampling() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requireSampling = true
}

// RedirectTo makes every request answer with a redirect to target.
func (s *Server) RedirectTo(target string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.redirect = target
}

// FailWith makes every request answer with an HTTP status and no body.
func (s *Server) FailWith(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = status
}

// Requests returns the JSON-RPC messages received so far.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

// Calls counts the received messages with the given method.
func (s *Server) Calls(method string) int {
	count := 0
	for _, request := range s.Requests() {
		if request.Method == method {
			count++
		}
	}
	return count
}

type message struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	redirect, status := s.redirect, s.status
	s.mu.Unlock()
	switch {
	case redirect != "":
		http.Redirect(w, r, redirect, http.StatusTemporaryRedirect)
		return
	case status != 0:
		s.record(Request{Header: r.Header.Clone()})
		w.WriteHeader(status)
		return
	}

	switch r.Method {
	case http.MethodDelete:
		s.mu.Lock()
		delete(s.sessions, r.Header.Get("Mcp-Session-Id"))
		s.mu.Unlock()
		return
	case http.MethodPost:
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	var request message
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	s.record(Request{Method: request.Method, Params: request.Params, Header: r.Header.Clone()})
	if len(request.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	switch request.Method {
	case "initialize":
		s.initialize(w, request)
		return
	case "tools/list", "tools/call":
	default:
		respondError(w, request.ID, -32601, "method not found")
		return
	}

	s.mu.Lock()
	known := s.sessions[r.Header.Get("Mcp-Session-Id")]
	tools, handler := s.tools, s.handler
	s.mu.Unlock()
	if !known {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	if request.Method == "tools/list" {
		respond(w, request.ID, map[string]any{"tools": listedTools(tools)})
		return
	}

	var call struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(request.Params, &call); err != nil {
		respondError(w, request.ID, -32602, "invalid params")
		return
	}
	result := Result{Text: "called " + call.Name}
	if handler != nil {
		result = handler(r.Context(), call.Name, call.Arguments)
	}
	if r.Context().Err() != nil {
		return
	}
	content := result.Content
	if content == nil {
		content = []map[string]any{{"type": "text", "text": result.Text}}
	}
	body := map[string]any{"content": content, "isError": result.IsError}
	if result.Structured != nil {
		body["structuredContent"] = result.Structured
	}
	respond(w, request.ID, body)
}

func (s *Server) initialize(w http.ResponseWriter, request message) {
	var params struct {
		Capabilities map[string]json.RawMessage `json:"capabilities"`
	}
	_ = json.Unmarshal(request.Params, &params)

	s.mu.Lock()
	if _, offered := params.Capabilities["sampling"]; s.requireSampling && !offered {
		s.mu.Unlock()
		respondError(w, request.ID, -32600, "this server requires the sampling capability")
		return
	}
	s.nextSession++
	id := fmt.Sprintf("session-%d", s.nextSession)
	s.sessions[id] = true
	s.mu.Unlock()

	w.Header().Set("Mcp-Session-Id", id)
	respond(w, request.ID, map[string]any{
		"protocolVersion": "2025-11-25",
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": "mcptest", "version": "1"},
	})
}

func (s *Server) record(request Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, request)
}

func listedTools(tools []Tool) []map[string]any {
	listed := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		schema := tool.InputSchema
		if schema == nil {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		listed = append(listed, map[string]any{
			"name": tool.Name, "description": tool.Description, "inputSchema": schema,
			"annotations": map[string]any{"readOnlyHint": tool.ReadOnly},
		})
	}
	return listed
}

func respond(w http.ResponseWriter, id json.RawMessage, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func respondError(w http.ResponseWriter, id json.RawMessage, code int, text string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": text},
	})
}
