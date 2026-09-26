package hostconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/samperrin/pons"
	"github.com/samperrin/pons/plugins/actionpolicy"
	"github.com/samperrin/pons/plugins/brain/llm"
)

func TestBuildPluginOptionsSelectsClassifier(t *testing.T) {
	plugins := Settings{
		pluginEntryForTest("classifier/typesafe-jev", true, `{"api_key_env":"JEV_KEY"}`),
		pluginEntryForTest("action_policy", true, `{"classifier":"classifier/typesafe-jev","min_safe_confidence":0.95}`),
		pluginEntryForTest("classifier/openai", true, `{"model":"custom-model","timeout":"5s"}`),
		pluginEntryForTest("external", true, `{"manifests":["/opt/plugin.json"]}`),
	}
	options, err := Build(plugins, func(name string) string {
		if name == "JEV_KEY" || name == "OPENAI_API_KEY" {
			return "test-key"
		}
		return ""
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(options.Plugins) != 4 || options.Plugins[3].Manifest != "/opt/plugin.json" {
		t.Fatalf("unexpected plugin options: %+v", options)
	}
	if got := reflect.TypeOf(options.Plugins[0].Host).String(); got != "actionpolicy.ClassifierPlugin" {
		t.Fatalf("first host plugin: %s", got)
	}
	policy, ok := options.Plugins[1].Host.(actionpolicy.Policy)
	if !ok || policy.ClassifierID != "classifier/typesafe-jev" || policy.MinSafeConfidence != 0.95 {
		t.Fatalf("policy plugin: %+v", options.Plugins[1].Host)
	}
	core := pons.New()
	if err := core.Use(options.Plugins[0].Host, options.Plugins[1].Host, options.Plugins[2].Host); err != nil {
		t.Fatalf("register configured plugins: %v", err)
	}
	if _, ok := core.Capability(actionpolicy.ClassifierCapability("classifier/typesafe-jev")); !ok {
		t.Fatal("classifier capability was not registered")
	}
}

func TestBuildPluginOptionsDisabledAndMissingKey(t *testing.T) {
	plugins := Settings{
		pluginEntryForTest("classifier/openai", false, `{}`),
		pluginEntryForTest("action_policy", false, `{"classifier":"classifier/openai"}`),
	}
	options, err := Build(plugins, func(string) string { return "" }, nil)
	if err != nil || len(options.Plugins) != 0 {
		t.Fatalf("disabled policy = %+v, %v", options, err)
	}

	plugins[0] = pluginEntryForTest("classifier/openai", true, `{}`)
	plugins[1] = pluginEntryForTest("action_policy", true, `{"classifier":"classifier/openai"}`)
	_, err = Build(plugins, func(string) string { return "" }, nil)
	if err == nil || !strings.Contains(err.Error(), "OPENAI_API_KEY is not set") {
		t.Fatalf("missing key error: %v", err)
	}
}

func TestBuildCodexClassifierFromProvider(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".pons"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".pons", "auth.json"),
		[]byte(`{"codex":{"access":"test-token","accountId":"test-account","refresh":"test-refresh","expires":9999999999999}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	plugins := Settings{
		pluginEntryForTest("classifier/codex", true, `{"provider_id":"primary","model":"gpt-6-luna"}`),
		pluginEntryForTest("action_policy", true, `{"classifier":"classifier/codex"}`),
	}
	providers := map[string]llm.Fallback{"primary": {ID: "primary", Provider: "codex"}}
	options, err := Build(plugins, func(string) string { return "" }, providers)
	if err != nil {
		t.Fatal(err)
	}
	core := pons.New()
	if err := core.Use(options.Plugins[0].Host, options.Plugins[1].Host); err != nil {
		t.Fatal(err)
	}
	if _, ok := core.Capability(actionpolicy.ClassifierCapability("classifier/codex")); !ok {
		t.Fatal("Codex classifier capability was not registered")
	}

	providers["primary"] = llm.Fallback{ID: "primary", Provider: "openai"}
	if _, err := Build(plugins, func(string) string { return "" }, providers); err == nil || !strings.Contains(err.Error(), "must use the codex provider") {
		t.Fatalf("wrong provider error: %v", err)
	}
	if _, err := Build(plugins, func(string) string { return "" }, nil); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("missing provider error: %v", err)
	}
}

func pluginEntryForTest(id string, enabled bool, config string) Entry {
	return Entry{ID: id, Version: "1.0.0", Enabled: &enabled, Config: json.RawMessage(config)}
}

type customPlugin struct{}

func (customPlugin) ID() string                { return "example/custom" }
func (customPlugin) Provides() []string        { return nil }
func (customPlugin) Requires() []string        { return nil }
func (customPlugin) Build(*BuildContext) error { return nil }

func TestRegistryAcceptsCustomInProcessPlugin(t *testing.T) {
	registry := NewRegistry()
	factory := func(string, bool, json.RawMessage) (ConfiguredPlugin, error) {
		return customPlugin{}, nil
	}
	if err := registry.Register("example/custom", "1.0.0", factory); err != nil {
		t.Fatal(err)
	}
	if err := registry.Decode(Settings{pluginEntryForTest("example/custom", true, `{}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Build(Settings{pluginEntryForTest("example/custom", true, `{}`)}, func(string) string { return "" }, nil); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register("example/custom", "1.0.0", factory); err == nil {
		t.Fatal("duplicate registration accepted")
	}
}

func TestInstalledExternalPluginResolvesByID(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".pons", "plugins", "example.policy")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := `{"manifest_version":1,"name":"example.policy","entrypoint":"/bin/sh","runtime_protocol":1,"placement":"host"}`
	path := filepath.Join(dir, "plugin.json")
	if err := os.WriteFile(path, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	configured := Settings{pluginEntryForTest("example.policy", true, `{"threshold":0.9}`)}
	options, err := Build(configured, func(string) string { return "" }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(options.Plugins) != 1 || options.Plugins[0].Manifest != path {
		t.Fatalf("installed plugin paths: %+v", options.Plugins)
	}
	if string(options.Plugins[0].Config) != `{"threshold":0.9}` {
		t.Fatalf("installed plugin config: %s", options.Plugins[0].Config)
	}
}

func TestActionPolicyRulesAndGeneratedConfidence(t *testing.T) {
	options, err := Build(Settings{
		pluginEntryForTest("action_policy", true, `{"allow_generated_confidence":true,"rules":[{"action":"allow","tools":["*"]},{"action":"ask","tools":["bash"],"reason_code":"shell_review"}]}`),
	}, func(string) string { return "" }, nil)
	if err != nil {
		t.Fatal(err)
	}
	policy, ok := options.Plugins[0].Host.(actionpolicy.Policy)
	if !ok || policy.ClassifierID != "" || !policy.AllowGeneratedConfidence || len(policy.Rules) != 2 ||
		policy.Rules[1].ReasonCode != "shell_review" || policy.Rules[0].ReasonCode != "rule_allow" {
		t.Fatalf("policy plugin: %+v", options.Plugins[0].Host)
	}

	for _, config := range []string{
		`{}`,
		`{"rules":[{"action":"maybe","tools":["bash"]}]}`,
		`{"rules":[{"action":"ask","tools":[]}]}`,
		`{"rules":[{"action":"ask","tools":[""]}]}`,
	} {
		if err := Decode(Settings{pluginEntryForTest("action_policy", true, config)}); err == nil {
			t.Fatalf("accepted invalid action_policy config %s", config)
		}
	}
}

func TestBuildPluginIncludesOnlyItsDependencies(t *testing.T) {
	settings := Settings{
		pluginEntryForTest("classifier/typesafe-jev", false, `{"api_key_env":"JEV_KEY"}`),
		pluginEntryForTest("classifier/openai", true, `{}`),
		pluginEntryForTest("action_policy", false, `{"classifier":"classifier/typesafe-jev"}`),
		pluginEntryForTest("external", false, `{}`), // incomplete, but unrelated
	}
	getenv := func(name string) string {
		if name == "JEV_KEY" {
			return "test-key"
		}
		return ""
	}
	options, err := NewRegistry().BuildPlugin(settings, "action_policy", getenv, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(options.Plugins) != 2 || options.Plugins[0].ID != "classifier/typesafe-jev" || options.Plugins[1].ID != "action_policy" {
		t.Fatalf("plugins = %+v", options.Plugins)
	}
	if _, err := NewRegistry().BuildPlugin(settings, "missing", getenv, nil); err == nil {
		t.Fatal("built an unconfigured plugin")
	}
}

func TestBuildPluginNamesAnInvalidProvider(t *testing.T) {
	settings := Settings{
		pluginEntryForTest("classifier/codex", false, `{}`), // provider_id is required when enabled
		pluginEntryForTest("action_policy", false, `{"classifier":"classifier/codex"}`),
	}
	_, err := NewRegistry().BuildPlugin(settings, "action_policy", func(string) string { return "" }, nil)
	if err == nil || !strings.Contains(err.Error(), "plugins.classifier/codex is invalid") ||
		!strings.Contains(err.Error(), "provider_id") {
		t.Fatalf("err = %v", err)
	}
}
