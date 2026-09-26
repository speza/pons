package actionpolicy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/samperrin/pons"
	"github.com/samperrin/pons/protocol"
)

func testRequest() pons.ToolCallStartInput {
	return pons.ToolCallStartInput{
		Message:       "push it to github.com/speza/frontier",
		Action:        protocol.Action{ID: "a1", Kind: "run", Args: json.RawMessage(`{"command":"git push origin dev"}`)},
		RecentContext: []pons.ActionContextItem{{Source: pons.ContextUser, Text: "push it"}},
	}
}

func TestRemoteClassifiers(t *testing.T) {
	for _, tt := range []struct {
		name, response, model string
		newClassifier         func(RemoteConfig) (Classifier, error)
		probability           bool
	}{
		{"openai", `{"status":"completed","output":[{"content":[{"type":"output_text","text":"{\"risk\":\"safe\",\"confidence\":0.99,\"reason_code\":\"approved\"}"}]}]}`, "gpt-6-astra", func(c RemoteConfig) (Classifier, error) { return NewOpenAIClassifier(c) }, false},
		{"typesafe", `{"model":"jev-test","answers":{"action_risk":{"type":"choice","choice":"safe","confidence":0.99,"probabilities":{"safe":0.99,"review":0.01}}}}`, "jev-test", func(c RemoteConfig) (Classifier, error) { return NewTypeSafeClassifier(c) }, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer secret" {
					t.Errorf("unexpected request method or authentication")
				}
				if tt.name == "openai" && r.URL.Path != "/responses" {
					t.Errorf("OpenAI request path = %q", r.URL.Path)
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode request: %v", err)
				}
				if body["model"] != tt.model {
					t.Errorf("model=%v, want %q", body["model"], tt.model)
				}
				if tt.name == "openai" {
					text, ok := body["text"].(map[string]any)
					format, valid := text["format"].(map[string]any)
					if !ok || !valid || format["type"] != "json_schema" || format["strict"] != true || body["store"] != false {
						t.Errorf("OpenAI structured output settings missing: %+v", body)
					}
				}
				encoded, _ := json.Marshal(body)
				for _, want := range []string{"github.com/speza/frontier", "git push origin dev", "push it"} {
					if !strings.Contains(string(encoded), want) {
						t.Errorf("request missing %q", want)
					}
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tt.response))
			}))
			defer server.Close()
			classifier, err := tt.newClassifier(RemoteConfig{APIKey: "secret", Endpoint: server.URL, Model: tt.model})
			if err != nil {
				t.Fatal(err)
			}
			a, err := classifier.Assess(context.Background(), testRequest())
			if err != nil || a.Risk != "safe" || a.Confidence != 0.99 || a.ProbabilityConfidence != tt.probability {
				t.Fatalf("assessment=%+v error=%v", a, err)
			}
			if tt.name == "openai" && a.Classifier != "openai/"+tt.model {
				t.Fatalf("classifier=%q", a.Classifier)
			}
			if tt.name == "typesafe" && a.Classifier != "typesafe/"+tt.model {
				t.Fatalf("classifier=%q", a.Classifier)
			}
		})
	}
}

func TestOpenAIDefaultModel(t *testing.T) {
	classifier, err := NewOpenAIClassifier(RemoteConfig{APIKey: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if classifier.model != "gpt-6-luna" {
		t.Fatalf("model=%q", classifier.model)
	}
}

func TestOpenAIClassifierLimitsAndSanitizesResponses(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{name: "HTTP error", status: http.StatusInternalServerError, body: "sensitive body", want: "HTTP status 500"},
		{name: "oversized response", status: http.StatusOK, body: strings.Repeat("x", maxClassifierResponse+1), want: "response too large"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()
			classifier, err := NewOpenAIClassifier(RemoteConfig{APIKey: "secret", Endpoint: server.URL})
			if err != nil {
				t.Fatal(err)
			}
			_, err = classifier.Assess(context.Background(), testRequest())
			if err == nil || !strings.Contains(err.Error(), tt.want) || strings.Contains(err.Error(), "sensitive body") {
				t.Fatalf("classifier error = %v", err)
			}
		})
	}
}

func TestTypeSafeDefaultModel(t *testing.T) {
	classifier, err := NewTypeSafeClassifier(RemoteConfig{APIKey: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if classifier.model != "jev-latest" {
		t.Fatalf("model=%q", classifier.model)
	}
}

func TestRemoteClassifierFailureAsks(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("sensitive body"))
	}))
	defer server.Close()
	classifier, err := NewTypeSafeClassifier(RemoteConfig{APIKey: "secret", Endpoint: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	decision, err := (Policy{Classifiers: []Classifier{classifier}}).decide(context.Background(), testRequest())
	if err != nil || decision.Permission != pons.PermissionAsk {
		t.Fatalf("decision=%+v error=%v", decision, err)
	}
}
