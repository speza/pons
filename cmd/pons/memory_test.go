package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samperrin/pons/plugins/brain/llm"
)

// providerRequest is the part of an OpenAI chat request the scripted
// provider inspects.
type providerRequest struct {
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
	Tools []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tools"`
}

func (r providerRequest) system() string {
	if len(r.Messages) == 0 {
		return ""
	}
	return r.Messages[0].Content
}

func (r providerRequest) last() (string, string) {
	if len(r.Messages) == 0 {
		return "", ""
	}
	message := r.Messages[len(r.Messages)-1]
	return message.Role, message.Content
}

func (r providerRequest) hasTool(name string) bool {
	for _, tool := range r.Tools {
		if tool.Function.Name == name {
			return true
		}
	}
	return false
}

// providerReply is either a final text answer or one tool call.
type providerReply struct {
	Text      string
	Tool      string
	Arguments any
}

// scriptedProvider serves an OpenAI-compatible endpoint whose replies are
// chosen by respond, and records every request.
func scriptedProvider(t *testing.T, respond func(providerRequest) providerReply) (*httptest.Server, func() []providerRequest) {
	t.Helper()
	var mu sync.Mutex
	var requests []providerRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request providerRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		requests = append(requests, request)
		mu.Unlock()

		reply := respond(request)
		message := map[string]any{"role": "assistant", "content": reply.Text, "refusal": nil}
		finish := "stop"
		if reply.Tool != "" {
			arguments, _ := json.Marshal(reply.Arguments)
			message["content"] = nil
			message["tool_calls"] = []any{map[string]any{
				"id": "call-" + reply.Tool, "type": "function",
				"function": map[string]any{"name": reply.Tool, "arguments": string(arguments)},
			}}
			finish = "tool_calls"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "c", "object": "chat.completion", "created": 0, "model": "m",
			"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}},
		})
	}))
	t.Cleanup(server.Close)
	return server, func() []providerRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]providerRequest(nil), requests...)
	}
}

var memoryPathPattern = regexp.MustCompile(`<memory path="([^"]+)">`)

func bundledRun(t *testing.T, stateDir, providerURL, message string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := runBundled(ctx, newServerLogger(io.Discard, false), serverOptions{
		StateDir: stateDir, WorkspaceRoot: os.TempDir(), ClientWorkspace: t.TempDir(), MaxTurns: 4, MaxConcurrent: 1,
		Brain: llm.Config{Provider: "openai", Model: "test", APIKey: "test", BaseURL: providerURL + "/v1"},
	}, "", "", message, false)
	if err != nil {
		t.Fatal(err)
	}
}

func TestAgentMemorySurvivesRestartAndNewConversation(t *testing.T) {
	provider, requests := scriptedProvider(t, func(request providerRequest) providerReply {
		role, content := request.last()
		if role == "tool" {
			return providerReply{Text: "Noted."}
		}
		if strings.Contains(content, "remember that") {
			path := memoryPathPattern.FindStringSubmatch(content)
			if path == nil {
				return providerReply{Text: "no memory"}
			}
			return providerReply{Tool: "write_file", Arguments: map[string]string{
				"path": filepath.Join(path[1], "MEMORY.md"), "content": "- [Tea](MEMORY.md) — owner drinks green tea\n",
			}}
		}
		return providerReply{Text: "Green tea."}
	})

	stateDir := t.TempDir()
	bundledRun(t, stateDir, provider.URL, "remember that I drink green tea")
	bundledRun(t, stateDir, provider.URL, "what do I drink?")

	all := requests()
	if len(all) != 3 {
		t.Fatalf("provider requests = %d, want 3", len(all))
	}
	if !strings.Contains(all[0].system(), "<memory_rules>") {
		t.Fatalf("memory rules missing:\n%s", all[0].system())
	}
	_, recalled := all[2].last()
	if !strings.Contains(recalled, "<memory_index source=\"MEMORY.md\">\n- [Tea](MEMORY.md) — owner drinks green tea\n</memory_index>") {
		t.Fatalf("new conversation after restart did not hydrate memory:\n%s", recalled)
	}
	data, err := os.ReadFile(filepath.Join(stateDir, "agents", "default", "memory", "MEMORY.md"))
	if err != nil || !strings.Contains(string(data), "green tea") {
		t.Fatalf("canonical memory = %q, %v", data, err)
	}
	path := memoryPathPattern.FindStringSubmatch(recalled)
	if want, _ := filepath.EvalSymlinks(filepath.Join(stateDir, "agents", "default", "memory")); path == nil || path[1] != want {
		t.Fatalf("memory path = %v, want %s", path, want)
	}
}
