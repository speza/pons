package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/samperrin/pons"
	"github.com/samperrin/pons/plugins/actionpolicy"
)

func TestBuildPluginOptionsSelectsClassifier(t *testing.T) {
	plugins := pluginSettings{
		pluginEntryForTest("classifier/typesafe-jev", true, `{"api_key_env":"JEV_KEY"}`),
		pluginEntryForTest("action_policy", true, `{"classifier":"classifier/typesafe-jev","min_safe_confidence":0.95,"allow_generated_confidence":true}`),
		pluginEntryForTest("classifier/openai", true, `{"model":"custom-model","timeout":"5s"}`),
		pluginEntryForTest("external", true, `{"manifests":["/opt/plugin.json"]}`),
	}
	options, err := buildPluginOptions(plugins, func(name string) string {
		if name == "JEV_KEY" || name == "OPENAI_API_KEY" {
			return "test-key"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(options.hostPlugins) != 3 || len(options.manifests) != 1 ||
		options.manifests[0] != "/opt/plugin.json" {
		t.Fatalf("unexpected plugin options: %+v", options)
	}
	if got := reflect.TypeOf(options.hostPlugins[0]).String(); got != "actionpolicy.ClassifierPlugin" {
		t.Fatalf("first host plugin: %s", got)
	}
	policy, ok := options.hostPlugins[1].(actionpolicy.Policy)
	if !ok || policy.ClassifierID != "classifier/typesafe-jev" || policy.MinSafeConfidence != 0.95 ||
		!policy.AllowGeneratedConfidence {
		t.Fatalf("policy plugin: %+v", options.hostPlugins[1])
	}
	core := pons.New()
	if err := core.Use(options.hostPlugins...); err != nil {
		t.Fatalf("register configured plugins: %v", err)
	}
	if _, ok := core.Capability(actionpolicy.ClassifierCapability("classifier/typesafe-jev")); !ok {
		t.Fatal("classifier capability was not registered")
	}
}

func TestBuildPluginOptionsDisabledAndMissingKey(t *testing.T) {
	plugins := pluginSettings{
		pluginEntryForTest("classifier/openai", false, `{}`),
		pluginEntryForTest("action_policy", false, `{"classifier":"classifier/openai"}`),
	}
	options, err := buildPluginOptions(plugins, func(string) string { return "" })
	if err != nil || len(options.hostPlugins) != 0 {
		t.Fatalf("disabled policy = %+v, %v", options, err)
	}

	plugins[0] = pluginEntryForTest("classifier/openai", true, `{}`)
	plugins[1] = pluginEntryForTest("action_policy", true, `{"classifier":"classifier/openai"}`)
	_, err = buildPluginOptions(plugins, func(string) string { return "" })
	if err == nil || !strings.Contains(err.Error(), "OPENAI_API_KEY is not set") {
		t.Fatalf("missing key error: %v", err)
	}
}

func pluginEntryForTest(id string, enabled bool, config string) pluginEntry {
	return pluginEntry{ID: id, Version: "1.0.0", Enabled: &enabled, Config: json.RawMessage(config)}
}
