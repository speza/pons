package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestProvidersRouteModelsToTheirAPI pins the endpoint, credential, and
// client identity each provider and model reaches, and that the SDKs' own
// environment credentials never go with them.
func TestProvidersRouteModelsToTheirAPI(t *testing.T) {
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
		provider, model, api, base, path string
	}{
		{"opencode-go", "opencode-go/kimi-k3", "", "/v1", "/v1/chat/completions"},
		{"opencode-go", "qwen3.8-max", "", "/v1", "/v1/messages"},
		{"opencode-go", "MiniMax-M3", "", "/v1/", "/v1/messages"},
		{"opencode-go", "grok-4.7", "", "/v1", "/v1/responses"},
		{"opencode-go", "union-alpha", APIAnthropicMessages, "/v1", "/v1/messages"},
		{"openrouter", "openrouter/auto", "", "/v1", "/v1/chat/completions"},
		{"openai", "gpt-x", APIOpenAIResponses, "/v1", "/v1/responses"},
		{"anthropic", "claude-x", "", "", "/v1/messages"},
	}
	for _, tc := range cases {
		name := tc.provider + " " + tc.model
		gotPath, gotAuth, gotKey, gotSession, gotAgent, leaked = "", "", "", "", "", nil
		client, model, err := providerClient(Fallback{
			Provider: tc.provider,
			Model:    tc.model,
			BaseURL:  srv.URL + tc.base,
			APIKey:   "key-test",
			API:      tc.api,
		}, 32, "conv-1")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}

		_, err = client.Complete(context.Background(), "sys", []Turn{{Role: "user", Blocks: []Block{Text{Value: "go"}}}}, nil)
		if err == nil {
			t.Fatalf("%s: expected the stub's error", name)
		}
		if gotPath != tc.path {
			t.Errorf("%s: path = %q, want %q", name, gotPath, tc.path)
		}
		if gotAuth != "Bearer key-test" && gotKey != "key-test" {
			t.Errorf("%s: key not sent (Authorization %q, X-Api-Key %q)", name, gotAuth, gotKey)
		}
		if len(leaked) > 0 {
			t.Errorf("%s: environment credentials sent in %v", name, leaked)
		}
		wantSession := tc.provider == "opencode-go"
		if (gotSession == "conv-1") != wantSession || strings.HasPrefix(gotAgent, "pons/") != wantSession {
			t.Errorf("%s: session %q, user agent %q", name, gotSession, gotAgent)
		}
		if strings.HasPrefix(model, "opencode-go/") || tc.provider == "openrouter" && model != tc.model {
			t.Errorf("%s: model = %q", name, model)
		}
	}
}

func TestProviderClientRejectsBadSlots(t *testing.T) {
	t.Setenv("OPENCODE_API_KEY", "")
	for name, slot := range map[string]Fallback{
		"unknown provider":    {Provider: "nope", APIKey: "k"},
		"missing key":         {Provider: "opencode-go"},
		"unknown api":         {Provider: "opencode-go", APIKey: "k", API: "grpc"},
		"codex off Responses": {ID: "x", Provider: "codex", API: APIOpenAICompletions},
	} {
		if _, _, err := providerClient(slot, 32, ""); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
