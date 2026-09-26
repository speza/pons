package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestOpenCodeGoRoutesModelsToTheirWire pins the endpoint, credential, and
// client identity each model family reaches under one OpenCode Go base URL,
// and that the SDKs' environment credentials never reach it.
func TestOpenCodeGoRoutesModelsToTheirWire(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "anthropic-secret")
	t.Setenv("ANTHROPIC_CUSTOM_HEADERS", "X-Leak: anthropic-secret")
	t.Setenv("OPENAI_ORG_ID", "org-secret")
	t.Setenv("OPENAI_PROJECT_ID", "proj-secret")

	var gotPath, gotAuth, gotKey, gotSession, gotAgent string
	var leaked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotKey = r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("X-Api-Key")
		gotSession, gotAgent = r.Header.Get("X-Opencode-Session"), r.Header.Get("User-Agent")
		for name, values := range r.Header {
			for _, v := range values {
				if strings.Contains(v, "secret") {
					leaked = append(leaked, name)
				}
			}
		}
		http.Error(w, `{"error":{"message":"stop"}}`, http.StatusBadRequest)
	}))
	defer srv.Close()

	cases := []struct {
		model, api, base, path string
	}{
		{"opencode-go/kimi-k3", "", "/v1", "/v1/chat/completions"},
		{"glm-5.3", "", "", "/v1/chat/completions"},
		{"qwen3.8-max", "", "/v1", "/v1/messages"},
		{"minimax-m3", "", "", "/v1/messages"},
		{"grok-4.7", "", "/v1/", "/v1/responses"},
		{"gpt-6-luna", "", "", "/v1/responses"},
		{"union-alpha", "messages", "/v1", "/v1/messages"},
		{"qwen3.8-max", "chat", "/v1", "/v1/chat/completions"},
	}
	for _, tc := range cases {
		gotPath, gotAuth, gotKey, gotSession, gotAgent, leaked = "", "", "", "", "", nil
		client, model, err := providerClient(Fallback{
			Provider: "opencode-go",
			Model:    tc.model,
			BaseURL:  srv.URL + tc.base,
			APIKey:   "oc-test",
			API:      tc.api,
		}, 32, "conv-1")
		if err != nil {
			t.Fatalf("%s: %v", tc.model, err)
		}

		_, err = client.Complete(context.Background(), "sys", []Turn{{Role: "user", Blocks: []Block{Text{Value: "go"}}}}, nil)
		if err == nil {
			t.Fatalf("%s: expected the stub's error", tc.model)
		}
		if gotPath != tc.path {
			t.Errorf("%s: path = %q, want %q", tc.model, gotPath, tc.path)
		}
		if gotAuth != "Bearer oc-test" && gotKey != "oc-test" {
			t.Errorf("%s: key not sent (Authorization %q, X-Api-Key %q)", tc.model, gotAuth, gotKey)
		}
		if len(leaked) > 0 {
			t.Errorf("%s: environment credentials sent in %v", tc.model, leaked)
		}
		if gotSession != "conv-1" || !strings.HasPrefix(gotAgent, "pons/") {
			t.Errorf("%s: session %q, user agent %q", tc.model, gotSession, gotAgent)
		}
		if strings.HasPrefix(model, "opencode-go/") {
			t.Errorf("%s: model = %q, want the prefix stripped", tc.model, model)
		}
	}
}

func TestOpenCodeGoRejectsBadSlots(t *testing.T) {
	t.Setenv("OPENCODE_API_KEY", "")
	for name, slot := range map[string]Fallback{
		"missing key":         {Provider: "opencode-go"},
		"unknown api":         {Provider: "opencode-go", APIKey: "k", API: "grpc"},
		"api on another type": {Provider: "openai", APIKey: "k", API: "chat"},
	} {
		if _, _, err := providerClient(slot, 32, ""); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
