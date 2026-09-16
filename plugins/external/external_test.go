package external_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samperrin/pons"
	"github.com/samperrin/pons/plugins/external"
	"github.com/samperrin/pons/plugins/external/sdk"
	"github.com/samperrin/pons/protocol"
)

const helperSchema = `{
  "type": "object",
  "properties": {
    "text": {"type": "string", "description": "text to echo"},
    "delay_ms": {"type": "integer"},
    "wait": {"type": "boolean"},
    "size": {"type": "integer"},
    "meta": {"type": "object", "properties": {"ok": {"type": "boolean"}}, "required": ["ok"], "additionalProperties": false},
    "values": {"type": "array", "items": {"type": "number"}}
  },
  "required": ["text"],
  "additionalProperties": false
}`

// TestExternalPluginHelper is the real child process used by the hermetic
// host tests. The exact -test.run selector means no parent tests run in the
// child. It uses the public serving SDK, just as an independently built Go
// plugin would.
func TestExternalPluginHelper(t *testing.T) {
	if os.Getenv("PONS_EXTERNAL_HELPER") != "1" {
		return
	}
	server := sdk.Server{
		Name:           "example.test",
		Version:        "1.0.0",
		MaxConcurrency: 4,
		Tools: []sdk.Tool{{
			Kind:        "example_echo",
			Description: "Echo typed JSON arguments.",
			InputSchema: json.RawMessage(helperSchema),
			Handler: func(ctx context.Context, action protocol.Action) (protocol.ToolResult, error) {
				var args map[string]json.RawMessage
				if err := json.Unmarshal(action.Args, &args); err != nil {
					return protocol.ToolResult{}, err
				}
				var text string
				_ = json.Unmarshal(args["text"], &text)
				var delay int
				_ = json.Unmarshal(args["delay_ms"], &delay)
				var wait bool
				_ = json.Unmarshal(args["wait"], &wait)
				var size int
				_ = json.Unmarshal(args["size"], &size)
				if text == "env" {
					return protocol.ToolResult{OK: true, Output: os.Getenv("PONS_TEST_SECRET") + "|" + os.Getenv("PATH")}, nil
				}
				if text == "die" {
					os.Exit(0)
				}
				if wait {
					<-ctx.Done()
					return protocol.ToolResult{OK: false, Error: "handler canceled"}, nil
				}
				if delay > 0 {
					select {
					case <-time.After(time.Duration(delay) * time.Millisecond):
					case <-ctx.Done():
						return protocol.ToolResult{OK: false, Error: "handler canceled"}, nil
					}
				}
				output := text
				if size > 0 {
					output = strings.Repeat("x", size)
				}
				// Deliberately use the wrong action id. The host must replace it
				// with the id of the request it routed.
				return protocol.ToolResult{ActionID: "plugin-chosen-id", Kind: "forged.payload.namespace", OK: true, Output: output}, nil
			},
		}},
	}
	if err := sdk.Serve(context.Background(), os.Stdin, os.Stdout, server); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}

func TestExternalRawPluginHelper(t *testing.T) {
	if os.Getenv("PONS_EXTERNAL_RAW") != "1" {
		return
	}
	encoder := json.NewEncoder(os.Stdout)
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var request external.RPCRequest
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
			return
		}
		switch request.Method {
		case external.MethodInitialize:
			configuration, _ := json.Marshal(external.ToolProviderConfiguration{
				MaxConcurrency: 1,
				Tools:          []external.ToolDescription{{Kind: "raw_echo", Description: "raw echo", InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"],"additionalProperties":false}`)}},
			})
			result, _ := json.Marshal(external.InitializeResult{
				Plugin:       external.PluginInfo{Name: "raw.test", Version: "1.0.0"},
				Capabilities: []external.Capability{{Type: external.CapabilityToolProvider, Version: external.ToolProviderVersion, Configuration: configuration}},
			})
			_ = encoder.Encode(external.RPCResponse{JSONRPC: "2.0", ID: request.ID, Result: result})
		case external.MethodExecute:
			result, _ := json.Marshal(protocol.ToolResult{ActionID: "forged-action", Kind: "forged-kind", OK: true, Output: "raw"})
			_ = encoder.Encode(external.RPCResponse{JSONRPC: "2.0", ID: request.ID, Result: result})
		case external.MethodShutdown:
			_ = encoder.Encode(external.RPCResponse{JSONRPC: "2.0", ID: request.ID, Result: json.RawMessage(`{}`)})
			return
		}
	}
}

func rawHelperManifest(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	entrypoint, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	manifest := fmt.Sprintf(`{"manifest_version":1,"name":"raw.test","entrypoint":%q,"args":["-test.run=^TestExternalRawPluginHelper$"],"runtime_protocol":1}`, entrypoint)
	path := filepath.Join(dir, "plugin.json")
	if err := os.WriteFile(path, []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestHostNormalizesUntrustedResultKind(t *testing.T) {
	plugin, err := external.NewHands(rawHelperManifest(t), external.HostConfig{
		Env:         []string{"PONS_EXTERNAL_RAW=1"},
		CallTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer plugin.Close()
	if err := plugin.Host().Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	result, err := plugin.Host().Execute(context.Background(), protocol.Action{
		ID: "host-action", Kind: "raw_echo", Args: protocol.MustArgsJSON(map[string]any{"text": "x"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ActionID != "host-action" || result.Kind != "raw_echo" {
		t.Fatalf("host trusted plugin result identity: %+v", result)
	}
}

func helperManifest(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	entrypoint, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	manifest := fmt.Sprintf(`{"manifest_version":1,"name":"example.test","entrypoint":%q,"args":["-test.run=^TestExternalPluginHelper$"],"runtime_protocol":1}`, entrypoint)
	path := filepath.Join(dir, "plugin.json")
	if err := os.WriteFile(path, []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func helperHost(t *testing.T, manifestPath string, limits external.Limits) *external.Plugin {
	t.Helper()
	p, err := external.NewHands(manifestPath, external.HostConfig{
		Workspace:   t.TempDir(),
		InheritEnv:  true,
		Env:         []string{"PONS_EXTERNAL_HELPER=1"},
		Limits:      limits,
		CancelGrace: 500 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func echoAction(id, text string, extra map[string]any) protocol.Action {
	args := map[string]any{"text": text}
	maps.Copy(args, extra)
	return protocol.Action{ID: id, Kind: "example_echo", Args: protocol.MustArgsJSON(args)}
}

func TestManifestResolutionAndSchemaValidation(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "plugin")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "plugin.json")
	if err := os.WriteFile(path, []byte(`{"manifest_version":1,"name":"acme.test","entrypoint":"./plugin","runtime_protocol":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest, err := external.LoadManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	unknownPath := filepath.Join(dir, "unknown.json")
	unknown := `{"manifest_version":1,"name":"acme.test","entrypoint":"./plugin","runtime_protocol":1,"unexpected":true}`
	if err := os.WriteFile(unknownPath, []byte(unknown), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := external.LoadManifest(unknownPath); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown manifest field should be rejected: %v", err)
	}
	if manifest.Dir() != dir || manifest.ResolvedEntrypoint() != exe {
		t.Fatalf("manifest resolution: dir=%q entrypoint=%q", manifest.Dir(), manifest.ResolvedEntrypoint())
	}
	for name := range map[string]bool{"Bad.Name": true, "has space": true, "": true} {
		bad := manifest
		bad.Name = name
		if err := bad.Validate(); err == nil {
			t.Errorf("name %q should be rejected", name)
		}
	}
	valid := []string{
		helperSchema,
		`{"type":"object","properties":{"x":{"type":"string"}},"required":["x"]}`,
		`{"type":"object","properties":{"x":{"type":"array","items":{"type":"object","properties":{"n":{"type":"integer"}}}}}}`,
	}
	for _, schema := range valid {
		if err := external.ValidateToolSchema(json.RawMessage(schema)); err != nil {
			t.Errorf("valid schema rejected: %v", err)
		}
	}
	invalid := []string{
		`{"type":"string"}`,
		`{"type":"object","unknown":true}`,
		`{"type":"object","properties":{"x":{"type":"array"}}}`,
		`{"type":"object","required":["missing"]}`,
		`{"type":"object","additionalProperties":true}`,
	}
	for _, schema := range invalid {
		if err := external.ValidateToolSchema(json.RawMessage(schema)); err == nil {
			t.Errorf("invalid schema accepted: %s", schema)
		}
	}
}

func TestCleanEnvironmentAllowsExplicitPathWithoutCredentials(t *testing.T) {
	t.Setenv("PONS_TEST_SECRET", "host-secret")
	manifest := helperManifest(t)
	plugin, err := external.NewHands(manifest, external.HostConfig{
		Env:         []string{"PONS_EXTERNAL_HELPER=1"},
		Path:        "/bin",
		InheritEnv:  false,
		CallTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer plugin.Close()
	if err := plugin.Host().Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	result, err := plugin.Host().Execute(context.Background(), echoAction("env", "env", nil))
	if err != nil {
		t.Fatal(err)
	}
	if result.Output != "|/bin" {
		t.Fatalf("child environment leaked or lost explicit PATH: %q", result.Output)
	}
}

func TestPersistentToolProviderTypedArgsAndDiscovery(t *testing.T) {
	plugin := helperHost(t, helperManifest(t), external.Limits{})
	if err := plugin.Host().Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	tools := plugin.Tools()
	if len(tools) != 1 || tools[0].Kind != "example_echo" {
		t.Fatalf("discovery: %+v", tools)
	}
	if err := external.ValidateToolSchema(tools[0].InputSchema); err != nil {
		t.Fatalf("discovered schema: %v", err)
	}
	health, err := plugin.Host().Health(context.Background())
	if err != nil || health.Status != "ok" {
		t.Fatalf("health: %+v %v", health, err)
	}
	result, err := plugin.Host().Execute(context.Background(), echoAction("call-typed", "typed", map[string]any{
		"delay_ms": 2,
		"wait":     false,
		"meta":     map[string]any{"ok": true},
		"values":   []any{1.5, 2},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK || result.ActionID != "call-typed" || result.Kind != "example_echo" || result.Output != "typed" {
		t.Fatalf("result correlation/namespacing/domain: %+v", result)
	}
	if err := plugin.Close(); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestConcurrentCorrelationAndProviderLimit(t *testing.T) {
	plugin := helperHost(t, helperManifest(t), external.Limits{MaxConcurrency: 2})
	if err := plugin.Host().Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	const calls = 6
	results := make([]protocol.ToolResult, calls)
	errs := make([]error, calls)
	var wg sync.WaitGroup
	for i := range calls {
		wg.Go(func() {
			results[i], errs[i] = plugin.Host().Execute(context.Background(), echoAction(fmt.Sprintf("call-%d", i), fmt.Sprintf("value-%d", i), map[string]any{
				"delay_ms": (calls - i) * 10,
			}))
		})
	}
	wg.Wait()
	for i := range results {
		if errs[i] != nil || !results[i].OK || results[i].ActionID != fmt.Sprintf("call-%d", i) || results[i].Output != fmt.Sprintf("value-%d", i) {
			t.Fatalf("correlation %d: result=%+v err=%v", i, results[i], errs[i])
		}
	}
}

func TestCancellationAndFailureBecomeToolObservations(t *testing.T) {
	plugin := helperHost(t, helperManifest(t), external.Limits{})
	if err := plugin.Host().Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	result, err := plugin.Host().Execute(ctx, echoAction("cancel", "wait", map[string]any{"wait": true}))
	if err != nil {
		t.Fatal(err)
	}
	if result.OK || !strings.Contains(result.Error, "context deadline exceeded") {
		t.Fatalf("cancellation result: %+v", result)
	}
	// The canceled request's late response is consumed and does not poison
	// the persistent connection.
	result, err = plugin.Host().Execute(context.Background(), echoAction("after-cancel", "alive", nil))
	if err != nil || !result.OK {
		t.Fatalf("connection after cancellation: %+v %v", result, err)
	}

	result, err = plugin.Host().Execute(context.Background(), echoAction("die", "die", nil))
	if err != nil {
		t.Fatal(err)
	}
	if result.OK || !strings.Contains(result.Error, "plugin") {
		t.Fatalf("process death result: %+v", result)
	}
}

func TestOversizedResultIsCheckedFailure(t *testing.T) {
	plugin := helperHost(t, helperManifest(t), external.Limits{MaxResultBytes: 128})
	if err := plugin.Host().Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	result, err := plugin.Host().Execute(context.Background(), echoAction("big", "big", map[string]any{"size": 2048}))
	if err != nil {
		t.Fatal(err)
	}
	if result.OK || !strings.Contains(result.Error, "exceeds") {
		t.Fatalf("oversized result: %+v", result)
	}
}

func TestExternalSetupPublishesPluginIdentity(t *testing.T) {
	plugin := helperHost(t, helperManifest(t), external.Limits{})
	core := pons.New()
	if err := plugin.Setup(core); err != nil {
		t.Fatal(err)
	}
	specs := core.ToolSpecs()
	if len(specs) != 1 {
		t.Fatalf("tool specs: %+v", specs)
	}
	source := specs[0].Source
	if !source.External || source.PluginName != "example.test" || source.PluginVersion != "1.0.0" ||
		source.Capability != external.CapabilityToolProvider || source.CapabilityVersion != external.ToolProviderVersion || source.Executable == "" {
		t.Fatalf("external plugin identity was not published: %+v", source)
	}
}

func TestExternalSetupPreservesAdditiveConflictRules(t *testing.T) {
	plugin := helperHost(t, helperManifest(t), external.Limits{})
	defer plugin.Close()
	core := pons.New()
	if err := core.AddTool("example_echo", pons.ToolDef{Handler: func(context.Context, protocol.Action) (protocol.ToolResult, error) {
		return protocol.ToolResult{OK: true}, nil
	}}); err != nil {
		t.Fatal(err)
	}
	if err := plugin.Setup(core); err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("expected additive conflict, got %v", err)
	}
}
