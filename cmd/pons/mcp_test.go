package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samperrin/pons"
	"github.com/samperrin/pons/plugins/actionpolicy"
	"github.com/samperrin/pons/plugins/brain/llm"
	"github.com/samperrin/pons/plugins/mcp"
	"github.com/samperrin/pons/plugins/mcp/mcptest"
	ponsruntime "github.com/samperrin/pons/runtime"
	"github.com/samperrin/pons/runtime/agentdir"
)

const testMCPSecret = "fake-github-token-value"

func TestLoadSettingsMCPServers(t *testing.T) {
	home := t.TempDir()
	global := filepath.Join(home, ".pons", "config.json")
	write(t, global, `{"mcpServers":{"github":{"type":"http","url":"https://mcp.example.com/mcp",
		"headers":{"Authorization":"Bearer ${secret:github_token}"},"approvalMode":"writes","disabledTools":["delete_*"]}}}`)
	cfg, err := loadSettings(home, "")
	if err != nil {
		t.Fatal(err)
	}
	if server := cfg.MCPServers["github"]; server.ApprovalMode != mcp.ApprovalWrites || len(server.DisabledTools) != 1 {
		t.Fatalf("mcpServers = %+v", cfg.MCPServers)
	}

	for name, content := range map[string]string{
		"remote http":   `{"mcpServers":{"github":{"type":"http","url":"http://mcp.example.com/mcp"}}}`,
		"unknown field": `{"mcpServers":{"github":{"type":"http","url":"https://mcp.example.com/mcp","token":"x"}}}`,
	} {
		write(t, global, content)
		if _, err := loadSettings(home, ""); err == nil {
			t.Errorf("%s: config accepted", name)
		}
	}
}

// mcpHome writes a host configuration with one server and stores its secret
// through the CLI, as an owner would.
func mcpHome(t *testing.T, serverConfig string) string {
	t.Helper()
	home := t.TempDir()
	write(t, filepath.Join(home, ".pons", "config.json"), `{"mcpServers":{"github":`+serverConfig+`}}`)
	var stdout bytes.Buffer
	if err := runMCP(t.Context(), []string{"set-secret", "github_token"}, strings.NewReader(testMCPSecret+"\n"), &stdout, io.Discard, home); err != nil {
		t.Fatal(err)
	}
	if got := stdout.String(); got != "stored secret github_token\n" {
		t.Fatalf("set-secret printed %q", got)
	}
	return home
}

func TestMCPToolsListsAServersToolsWithStoredSecrets(t *testing.T) {
	server := mcptest.New(t)
	server.SetTools(
		mcptest.Tool{Name: "get_issue", ReadOnly: true},
		mcptest.Tool{Name: "delete_repo"},
		mcptest.Tool{Name: "bad name"},
	)
	// A disabled server can still be checked before it is enabled.
	home := mcpHome(t, fmt.Sprintf(`{"type":"http","url":%q,"enabled":false,
		"headers":{"Authorization":"Bearer ${secret:github_token}"},"disabledTools":["delete_*"]}`, server.URL))

	var stdout bytes.Buffer
	if err := runMCP(t.Context(), []string{"tools", "github"}, strings.NewReader(""), &stdout, io.Discard, home); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"TOOL READ-ONLY STATUS",
		"get_issue yes available",
		"delete_repo no filtered",
		"bad name no skipped: tool name has unsupported characters",
	}
	var got []string
	for line := range strings.Lines(stdout.String()) {
		got = append(got, strings.Join(strings.Fields(line), " "))
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("tools output:\n%s\nwant rows %q", stdout.String(), want)
	}
	if strings.Contains(stdout.String(), testMCPSecret) {
		t.Fatalf("tools output contains the secret:\n%s", stdout.String())
	}
	if got := server.Requests()[0].Header.Get("Authorization"); got != "Bearer "+testMCPSecret {
		t.Fatalf("server received Authorization %q, want the stored secret", got)
	}
}

func TestMCPToolsFailsWhenARunWouldGetNoTools(t *testing.T) {
	server := mcptest.New(t)
	tools := make([]mcptest.Tool, mcp.MaxToolsPerServer+1)
	for i := range tools {
		tools[i].Name = fmt.Sprintf("tool_%d", i)
	}
	server.SetTools(tools...)
	home := mcpHome(t, fmt.Sprintf(`{"type":"http","url":%q}`, server.URL))

	var stdout bytes.Buffer
	err := runMCP(t.Context(), []string{"tools", "github"}, strings.NewReader(""), &stdout, io.Discard, home)
	if err == nil || !strings.Contains(err.Error(), "more than the limit of 128") {
		t.Fatalf("error = %v, want the tool limit", err)
	}
	if !strings.Contains(stdout.String(), "tool_128") {
		t.Fatalf("the listing was not printed before the error:\n%s", stdout.String())
	}
}

func TestMCPCommandFailures(t *testing.T) {
	server := mcptest.New(t)
	server.FailWith(http.StatusUnauthorized)
	home := mcpHome(t, fmt.Sprintf(`{"type":"http","url":%q,"headers":{"Authorization":"Bearer ${secret:github_token}"}}`, server.URL))
	noSecret := t.TempDir()
	write(t, filepath.Join(noSecret, ".pons", "config.json"),
		`{"mcpServers":{"github":{"type":"http","url":"https://mcp.example.com/mcp","headers":{"Authorization":"Bearer ${secret:github_token}"}}}}`)

	tests := []struct {
		name    string
		home    string
		args    []string
		stdin   string
		wantErr string
	}{
		{name: "rejected token", home: home, args: []string{"tools", "github"},
			wantErr: "rejected the configured Authorization header"},
		{name: "unknown server", home: home, args: []string{"tools", "gmail"}, wantErr: `"gmail" is not in mcpServers`},
		{name: "missing secret", home: noSecret, args: []string{"tools", "github"}, wantErr: "pons mcp set-secret github_token"},
		{name: "value as an argument", home: home, args: []string{"set-secret", "github_token", "value"}, wantErr: "exactly one name"},
		{name: "empty value", home: home, args: []string{"set-secret", "github_token"}, stdin: "\n", wantErr: "must not be empty"},
		{name: "bad secret name", home: home, args: []string{"set-secret", "git hub"}, stdin: "v\n", wantErr: "secret name"},
		{name: "no command", home: home, args: nil, wantErr: "set-secret or tools"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := runMCP(t.Context(), test.args, strings.NewReader(test.stdin), io.Discard, io.Discard, test.home)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want one containing %q", err, test.wantErr)
			}
		})
	}
}

// mcpRun runs one bundled conversation whose agent is granted the github
// server, and returns what the model was offered and told.
func mcpRun(
	t *testing.T, server *mcptest.Server, approvalMode, grant string, hostPlugins ...pons.Plugin,
) (offered []string, toolResult string) {
	t.Helper()
	offered, toolResult, _ = mcpRunLogged(t, server, approvalMode, grant, hostPlugins...)
	return offered, toolResult
}

// mcpRunLogged is mcpRun that also returns the server's log.
func mcpRunLogged(
	t *testing.T, server *mcptest.Server, approvalMode, grant string, hostPlugins ...pons.Plugin,
) (offered []string, toolResult, logs string) {
	t.Helper()
	config := mcp.ServerConfig{
		Type: "http", URL: server.URL, ApprovalMode: approvalMode,
		Headers: map[string]string{"Authorization": "Bearer ${secret:github_token}"},
	}
	manager, err := mcp.NewManager(mcp.Options{
		Servers: map[string]mcp.ServerConfig{"github": config},
		Secrets: map[string]string{"github_token": testMCPSecret},
	})
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	write(t, filepath.Join(stateDir, "agents", "default", agentdir.SettingsFile), `{"mcpServers":{"github":`+grant+`}}`)
	var hostSources []hostPluginSource
	for _, plugin := range hostPlugins {
		hostSources = append(hostSources, hostPluginSource{Plugin: plugin})
	}

	provider, requests := scriptedProvider(t, func(request providerRequest) providerReply {
		if role, content := request.last(); role == "tool" {
			toolResult = content
			return providerReply{Text: "done"}
		}
		for _, tool := range request.Tools {
			if strings.HasPrefix(tool.Function.Name, "mcp__") {
				offered = append(offered, tool.Function.Name)
			}
		}
		return providerReply{Tool: "mcp__github__get_issue", Arguments: map[string]int{"number": 7}}
	})

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var logged bytes.Buffer
	err = runBundled(ctx, newServerLogger(&logged, false), testServerOptions(serverOptions{
		StateDir: stateDir, WorkspaceRoot: os.TempDir(), MaxTurns: 4, MaxConcurrent: 1, MCP: manager,
		HostSources: hostSources,
		Brain:       llm.Config{Provider: "openai", Model: "test", APIKey: "test", BaseURL: provider.URL + "/v1"},
	}), "", "", "look at issue 7", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(requests()) != 2 {
		t.Fatalf("provider saw %d requests, want the tool call and its result", len(requests()))
	}
	return offered, toolResult, logged.String()
}

func TestGrantedAgentCallsAnMCPTool(t *testing.T) {
	server := mcptest.New(t)
	server.SetTools(mcptest.Tool{Name: "get_issue", ReadOnly: true}, mcptest.Tool{Name: "create_issue"})
	server.Handle(func(_ context.Context, tool string, args json.RawMessage) mcptest.Result {
		return mcptest.Result{Text: tool + " " + string(args)}
	})

	offered, result := mcpRun(t, server, mcp.ApprovalWrites, `{"enabledTools":["get_*"]}`)

	if fmt.Sprint(offered) != "[mcp__github__get_issue]" {
		t.Fatalf("model was offered %v, want only the granted tool", offered)
	}
	if result != `get_issue {"number":7}` {
		t.Fatalf("model saw tool result %q", result)
	}
}

func TestAskDeniesAsApprovalUnavailableWhenNoOneCanAnswer(t *testing.T) {
	server := mcptest.New(t)
	server.SetTools(mcptest.Tool{Name: "get_issue", ReadOnly: true})

	_, result := mcpRun(t, server, mcp.ApprovalAsk, `{}`)

	if !strings.Contains(result, "approval_unavailable") {
		t.Fatalf("model saw %q, want an approval_unavailable denial", result)
	}
	if calls := server.Calls("tools/call"); calls != 0 {
		t.Fatalf("server received %d tool calls, want none for a denied call", calls)
	}
}

func TestAutoLeavesMCPCallsToTheActionPolicy(t *testing.T) {
	classifier := func(risk string) pons.Plugin {
		return actionpolicy.Policy{Classifiers: []actionpolicy.Classifier{actionpolicy.ClassifierFunc(
			func(context.Context, pons.ToolCallStartInput) (pons.ActionAssessment, error) {
				return pons.ActionAssessment{Risk: risk, Confidence: 0.99, ProbabilityConfidence: true}, nil
			},
		)}}
	}
	// The model is told only that no one could approve, so the reason the
	// call needed approval is read from the server's log.
	tests := []struct {
		name    string
		plugins []pons.Plugin
		want    string
		reason  string
		calls   int
	}{
		{name: "safe call runs", plugins: []pons.Plugin{classifier("safe")}, want: "get_issue", calls: 1},
		{name: "reviewed call is denied", plugins: []pons.Plugin{classifier("review")},
			want: "approval_unavailable", reason: "classifier_review"},
		{name: "no policy denies", want: "approval_unavailable", reason: "mcp_no_decision"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := mcptest.New(t)
			server.SetTools(mcptest.Tool{Name: "get_issue"})
			server.Handle(func(_ context.Context, tool string, _ json.RawMessage) mcptest.Result {
				return mcptest.Result{Text: tool}
			})

			// No approvalMode is set, so the server is in the default mode.
			_, result, logs := mcpRunLogged(t, server, "", `{}`, test.plugins...)

			if !strings.Contains(result, test.want) {
				t.Fatalf("model saw %q, want %q", result, test.want)
			}
			if asked := strings.Contains(logs, `"reason_code":"`+test.reason+`"`); test.reason != "" && !asked {
				t.Fatalf("log does not give the reason %q:\n%s", test.reason, logs)
			}
			if calls := server.Calls("tools/call"); calls != test.calls {
				t.Fatalf("server received %d tool calls, want %d", calls, test.calls)
			}
		})
	}
}

func TestMCPDeciderOf(t *testing.T) {
	judged := actionpolicy.Policy{ClassifierID: "classifier/typesafe-jev"}
	rulesOnly := actionpolicy.Policy{Rules: []actionpolicy.Rule{actionpolicy.ToolRule(pons.PermissionAsk, "", "bash")}}
	for _, test := range []struct {
		name    string
		sources []hostPluginSource
		want    mcpDecider
	}{
		{"nothing", nil, mcpDeciderNone},
		{"a classifier alone", []hostPluginSource{{Plugin: actionpolicy.ClassifierPlugin{ID: "classifier/typesafe-jev"}}}, mcpDeciderNone},
		{"rules only", []hostPluginSource{{Plugin: rulesOnly}}, mcpDeciderRules},
		{"policy with a classifier", []hostPluginSource{{Plugin: rulesOnly}, {Plugin: judged}}, mcpDeciderJudged},
		{"external hook plugin", []hostPluginSource{{Manifest: "/plugins/policy.json"}}, mcpDeciderJudged},
	} {
		if got := mcpDeciderOf(test.sources); got != test.want {
			t.Errorf("%s: decider = %d, want %d", test.name, got, test.want)
		}
	}
}

func TestAgentGrantMustNameAConfiguredServer(t *testing.T) {
	agents, err := agentdir.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	settings := filepath.Join(agents.Dir(ponsruntime.DefaultAgentID), agentdir.SettingsFile)
	write(t, settings, `{"mcpServers":{"gmail":{}}}`)

	_, err = defaultAgent(serverOptions{}, agents)
	if err == nil || !strings.Contains(err.Error(), settings) || !strings.Contains(err.Error(), `"gmail"`) {
		t.Fatalf("error = %v, want the file and the unconfigured server", err)
	}
}

func TestRunFailsBeforeItsSandboxWhenAGrantedServerWasRemoved(t *testing.T) {
	runner, execution, _ := agentWorkspaceRunner(t, "seatbelt")
	request := agentWorkspaceRequest()
	request.Agent.MCPServers = map[string]ponsruntime.MCPGrant{"github": {}}

	_, err := runner.Run(t.Context(), request)
	if err == nil || !strings.Contains(err.Error(), `MCP server "github", which is no longer configured`) {
		t.Fatalf("error = %v, want the removed server named", err)
	}
	if len(execution.specs) != 0 {
		t.Fatal("environment started for a rejected run")
	}
}
