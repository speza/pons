package llm

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samperrin/pons"
)

// TestAnthropicAdapter pins our Block-model translation and the request
// shape the SDK emits for us (tools schema, system, message roles).
func TestAnthropicAdapter(t *testing.T) {
	var gotBody []byte
	var gotPath, gotKey, gotVer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotKey, gotVer = r.URL.Path, r.Header.Get("x-api-key"), r.Header.Get("anthropic-version")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"m",
			"content":[{"type":"text","text":"looking..."},{"type":"tool_use","id":"tu_1","name":"bash","input":{"command":"ls"}}],
			"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer srv.Close()

	c := newAnthropicClient("sk-test", srv.URL, 1024)
	c.model = "claude-test"
	turn, err := c.Complete(context.Background(), "be brief",
		[]Turn{
			{Role: "user", Blocks: []Block{Text{Value: "go"}}},
			{Role: "assistant", Blocks: []Block{ToolUse{ID: "tu_0", Name: "bash", Input: map[string]any{"command": "echo"}}}},
			{Role: "user", Blocks: []Block{Result{ToolUseID: "tu_0", Content: "ok", IsError: true}}},
		},
		[]pons.ToolSpec{{Kind: "bash", Description: "run a command", Params: []pons.ToolParam{
			{Name: "command", Type: "string", Required: true},
		}}})
	if err != nil {
		t.Fatal(err)
	}

	if gotPath != "/v1/messages" || gotKey != "sk-test" || gotVer == "" {
		t.Fatalf("request shape: path=%s key=%s ver=%s", gotPath, gotKey, gotVer)
	}
	var req struct {
		Model     string           `json:"model"`
		MaxTokens int64            `json:"max_tokens"`
		System    []map[string]any `json:"system"`
		Messages  []struct {
			Role    string           `json:"role"`
			Content []map[string]any `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Name        string         `json:"name"`
			InputSchema map[string]any `json:"input_schema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(gotBody, &req); err != nil {
		t.Fatal(err)
	}
	if req.Model != "claude-test" || req.MaxTokens != 1024 || len(req.System) != 1 || req.System[0]["text"] != "be brief" {
		t.Fatalf("request fields: %+v", req)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("messages: %+v", req.Messages)
	}
	if req.Messages[1].Content[0]["type"] != "tool_use" || req.Messages[1].Content[0]["id"] != "tu_0" {
		t.Fatalf("assistant tool_use replay: %+v", req.Messages[1])
	}
	if req.Messages[2].Content[0]["is_error"] != true {
		t.Fatalf("tool_result is_error: %+v", req.Messages[2])
	}
	if len(req.Tools) != 1 || req.Tools[0].Name != "bash" {
		t.Fatalf("tools: %+v", req.Tools)
	}
	props := req.Tools[0].InputSchema["properties"].(map[string]any)
	if _, ok := props["command"]; !ok {
		t.Fatalf("tool schema properties: %+v", req.Tools[0].InputSchema)
	}

	if len(turn.Blocks) != 2 {
		t.Fatalf("blocks: %+v", turn.Blocks)
	}
	use, ok := turn.Blocks[1].(ToolUse)
	if !ok || use.ID != "tu_1" || use.Name != "bash" || use.Input["command"] != "ls" {
		t.Fatalf("tool_use: %+v", turn.Blocks[1])
	}
}

func TestOpenAIAdapter(t *testing.T) {
	var gotBody []byte
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("authorization")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, `{"id":"c1","object":"chat.completion","created":0,"model":"m","choices":[{"index":0,
			"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"bash","arguments":"{\"command\":\"ls\",\"timeout\":10}"}}],"refusal":null},
			"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer srv.Close()

	c := newOpenAIClient("sk-test", srv.URL+"/v1", 512)
	c.model = "gpt-test"
	turn, err := c.Complete(context.Background(), "be brief",
		[]Turn{
			{Role: "user", Blocks: []Block{Text{Value: "go"}}},
			{Role: "assistant", Blocks: []Block{ToolUse{ID: "call_0", Name: "bash", Input: map[string]any{"command": "echo"}}}},
			{Role: "user", Blocks: []Block{Result{ToolUseID: "call_0", Content: "ok"}}},
		},
		[]pons.ToolSpec{{Kind: "bash", Params: []pons.ToolParam{{Name: "command", Type: "string", Required: true}}}})
	if err != nil {
		t.Fatal(err)
	}

	if gotPath != "/v1/chat/completions" || gotAuth != "Bearer sk-test" {
		t.Fatalf("request shape: path=%s auth=%s", gotPath, gotAuth)
	}
	var req struct {
		Messages []struct {
			Role       string `json:"role"`
			ToolCallID string `json:"tool_call_id"`
		} `json:"messages"`
		Tools []struct {
			Function struct {
				Name       string         `json:"name"`
				Parameters map[string]any `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
		MaxCompletionTokens int64 `json:"max_completion_tokens"`
	}
	if err := json.Unmarshal(gotBody, &req); err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 4 { // system, user, assistant(tool_calls), tool
		t.Fatalf("messages: %+v", req.Messages)
	}
	if req.Messages[3].Role != "tool" || req.Messages[3].ToolCallID != "call_0" {
		t.Fatalf("tool message: %+v", req.Messages[3])
	}
	if req.MaxCompletionTokens != 512 {
		t.Fatalf("max_completion_tokens: %d", req.MaxCompletionTokens)
	}
	if len(req.Tools) != 1 {
		t.Fatalf("tools: %+v", req.Tools)
	}

	use, ok := turn.Blocks[0].(ToolUse)
	if !ok || use.ID != "call_1" || use.Name != "bash" || use.Input["command"] != "ls" {
		t.Fatalf("parsed tool_use: %+v", turn.Blocks[0])
	}
}

// TestResponsesAdapter pins the Codex/Responses streaming wire: SSE in,
// function_call + reasoning replay out, store:false + encrypted_content include.
func TestResponsesAdapter(t *testing.T) {
	var gotBody []byte
	var gotPath, gotAuth, gotAccount string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotAccount = r.URL.Path, r.Header.Get("authorization"), r.Header.Get("chatgpt-account-id")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, `event: response.output_item.done
data: {"type":"response.output_item.done","item":{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"enc"}}

event: response.output_item.done
data: {"type":"response.output_item.done","item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"bash","arguments":"{\"command\":\"ls\"}"}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed","output":[{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"enc"},{"type":"function_call","id":"fc_1","call_id":"call_1","name":"bash","arguments":"{\"command\":\"ls\"}"}],"usage":{"input_tokens":5,"output_tokens":2,"total_tokens":3}}}

`)
	}))
	defer srv.Close()

	c := &responsesClient{model: "gpt-test", maxTokens: 1024, baseURL: srv.URL, apiKey: "test-key"}
	turn, err := c.Complete(context.Background(), "be brief",
		[]Turn{
			{Role: "user", Blocks: []Block{Text{Value: "go"}}},
			{Role: "assistant", Blocks: []Block{ToolUse{ID: "call_0", Name: "bash", Input: map[string]any{"command": "echo"}}}},
			{Role: "user", Blocks: []Block{Result{ToolUseID: "call_0", Content: "ok"}}},
			{Role: "assistant", Blocks: []Block{Raw{Item: json.RawMessage(`{"type":"reasoning","id":"rs_0","summary":[],"encrypted_content":"old"}`)}}},
		},
		[]pons.ToolSpec{{Kind: "bash", Params: []pons.ToolParam{{Name: "command", Type: "string", Required: true}}}})
	if err != nil {
		t.Fatal(err)
	}

	if gotPath != "/responses" || gotAuth != "Bearer test-key" || gotAccount != "" {
		t.Fatalf("request shape (API-key mode): path=%s auth=%q account=%q", gotPath, gotAuth, gotAccount)
	}
	var req struct {
		Model        string           `json:"model"`
		Instructions string           `json:"instructions"`
		Stream       bool             `json:"stream"`
		Store        bool             `json:"store"`
		Include      []string         `json:"include"`
		Input        []map[string]any `json:"input"`
		Tools        []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(gotBody, &req); err != nil {
		t.Fatal(err)
	}
	if !req.Stream || req.Store {
		t.Fatalf("stream/store flags: stream=%v store=%v", req.Stream, req.Store)
	}
	if len(req.Include) != 1 || req.Include[0] != "reasoning.encrypted_content" {
		t.Fatalf("include: %v", req.Include)
	}
	// input items: user message, function_call, function_call_output, reasoning
	if len(req.Input) != 4 {
		t.Fatalf("input items: %d", len(req.Input))
	}
	if req.Input[2]["type"] != "function_call_output" || req.Input[2]["call_id"] != "call_0" {
		t.Fatalf("function_call_output: %+v", req.Input[2])
	}
	if req.Input[3]["type"] != "reasoning" || req.Input[3]["encrypted_content"] != "old" {
		t.Fatalf("reasoning replay: %+v", req.Input[3])
	}
	if len(req.Tools) != 1 || req.Tools[0]["type"] != "function" {
		t.Fatalf("tools: %+v", req.Tools)
	}

	// Blocks: reasoning replayed as Raw, function call as ToolUse.
	if len(turn.Blocks) != 2 {
		t.Fatalf("blocks: %+v", turn.Blocks)
	}
	if raw, ok := turn.Blocks[0].(Raw); !ok || !strings.Contains(string(raw.Item), "rs_1") {
		t.Fatalf("reasoning raw block: %+v", turn.Blocks[0])
	}
	use, ok := turn.Blocks[1].(ToolUse)
	if !ok || use.ID != "call_1" || use.Name != "bash" || use.Input["command"] != "ls" {
		t.Fatalf("tool_use block: %+v", turn.Blocks[1])
	}
}

// TestCodexAuthRefresh covers the subscription flow: 401 → refresh → retry.
func TestCodexAuthRefreshOn401(t *testing.T) {
	refreshSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["client_id"] != oauthClientID || body["grant_type"] != "refresh_token" {
			t.Errorf("refresh request: %+v", body)
		}
		io.WriteString(w, `{"access_token":"new-token","expires_in":3600}`)
	}))
	defer refreshSrv.Close()

	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		want := map[int]string{1: "old-token", 2: "new-token"}[calls]
		if got := r.Header.Get("authorization"); got != "Bearer "+want {
			t.Errorf("call %d auth header: %s (want %s)", calls, got, want)
		}
		if calls == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"error":{"message":"expired"}}`)
			return
		}
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, `event: response.completed
data: {"type":"response.completed","response":{"id":"r","object":"response","output":[{"type":"message","content":[{"type":"output_text","text":"done"}]}],"usage":{}}}

`)
	}))
	defer srv.Close()

	authFile := filepath.Join(t.TempDir(), "auth.json")
	os.WriteFile(authFile, []byte(fmt.Sprintf(`{"access":"old-token","accountId":"acct","refresh":"refresh-token","expires":%d}`, time.Now().Add(time.Hour).UnixMilli())), 0o600)
	auth, err := loadCodexAuth(authFile)
	if err != nil {
		t.Fatal(err)
	}
	auth.refreshURL = refreshSrv.URL
	auth.http = refreshSrv.Client()

	c := &responsesClient{model: "gpt-test", maxTokens: 128, baseURL: srv.URL, auth: auth}
	turn, err := c.Complete(context.Background(), "sys",
		[]Turn{{Role: "user", Blocks: []Block{Text{Value: "go"}}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || auth.Access != "new-token" {
		t.Fatalf("refresh+retry: calls=%d token=%q", calls, auth.Access)
	}
	var ok bool
	for _, b := range turn.Blocks {
		if t, isText := b.(Text); isText && t.Value == "done" {
			ok = true
		}
	}
	if !ok {
		t.Fatalf("assistant turn: %+v", turn.Blocks)
	}
}

func TestLoadCodexAuth(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	if _, err := loadCodexAuth(path); err == nil {
		t.Fatal("missing file should error")
	}
	os.WriteFile(path, []byte(`{"other":{}}`), 0o644)
	if _, err := loadCodexAuth(path); err == nil || !strings.Contains(err.Error(), "has no access token") {
		t.Fatalf("missing token: %v", err)
	}
}

func TestPKCEChallenge(t *testing.T) {
	p := newPKCE()
	if len(p.verifier) < 43 || len(p.challenge) < 43 {
		t.Fatalf("pkce lengths: verifier=%d challenge=%d", len(p.verifier), len(p.challenge))
	}
	sum := sha256.Sum256([]byte(p.verifier))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if p.challenge != want {
		t.Fatalf("challenge is not S256(verifier)")
	}
	if p.state == p.verifier {
		t.Fatal("state should be independent")
	}
}

func TestAuthorizeURLMatchesCodexShape(t *testing.T) {
	p := pkce{verifier: strings.Repeat("v", 43), challenge: strings.Repeat("c", 43), state: "state123"}
	u, err := url.Parse(authorizeURL(p))
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "https" || u.Host != "auth.openai.com" || u.Path != "/oauth/authorize" {
		t.Fatalf("authorize endpoint: %s", u)
	}
	q := u.Query()
	if q.Get("response_type") != "code" ||
		q.Get("client_id") != oauthClientID ||
		q.Get("redirect_uri") != "http://localhost:1455/auth/callback" ||
		q.Get("code_challenge_method") != "S256" ||
		q.Get("codex_cli_simplified_flow") != "true" ||
		q.Get("id_token_add_organizations") != "true" ||
		q.Get("originator") != "codex_cli_rs" {
		t.Fatalf("authorize params: %+v", q)
	}
	if !strings.Contains(q.Get("scope"), "offline_access") ||
		!strings.Contains(q.Get("scope"), "api.connectors.read") {
		t.Fatalf("scope: %q", q.Get("scope"))
	}
	if q.Get("code_challenge") != strings.Repeat("c", 43) || q.Get("state") != "state123" {
		t.Fatalf("pkce/state wiring: %+v", q)
	}
}

func TestAccountIDFromIDToken(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(
		`{"https://api.openai.com/auth":{"chatgpt_account_id":"acct-123"}}`))
	id := accountIDFromIDToken("header." + payload + ".sig")
	if id != "acct-123" {
		t.Fatalf("account id = %q", id)
	}
	if accountIDFromIDToken("header.aW52YWxpZA.sig") != "" {
		t.Fatal("garbage id_token should yield empty account id")
	}
}

func TestExchangeCodeRequestShape(t *testing.T) {
	var gotBody []byte
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = b
		w.Write([]byte(`{"access_token":"at","refresh_token":"rt","expires_in":3600,"id_token":"a.b.c"}`))
	}))
	defer srv.Close()

	prev := refreshTokenURL
	refreshTokenURL = srv.URL + "/oauth/token"
	defer func() { refreshTokenURL = prev }()

	tokens, err := exchangeCode(context.Background(), "auth-code", "verifier-xyz")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/oauth/token" {
		t.Fatalf("token path: %s", gotPath)
	}
	if !strings.Contains(string(gotBody), "grant_type=authorization_code") ||
		!strings.Contains(string(gotBody), "code=auth-code") ||
		!strings.Contains(string(gotBody), "code_verifier=verifier-xyz") {
		t.Fatalf("exchange body: %s", gotBody)
	}
	if tokens.Access != "at" || tokens.Refresh != "rt" || tokens.AccountID != "" {
		t.Fatalf("tokens: %+v", tokens)
	}
}

func TestSaveLoadRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".pons", "auth.json")
	a := &codexAuth{
		authPath:  path,
		Access:    "tok",
		AccountID: "acct",
		Refresh:   "ref",
		ExpiresAt: time.UnixMilli(1700000000000),
	}
	if err := a.save(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("auth file perms: %v", info.Mode().Perm())
	}
	loaded, err := loadCodexAuth(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Access != "tok" || loaded.AccountID != "acct" || loaded.Refresh != "ref" {
		t.Fatalf("roundtrip: %+v", loaded)
	}
}
