package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samperrin/pons"
	"github.com/samperrin/pons/plugins/hostconfig"
)

type orderedTestPlugin string

func (orderedTestPlugin) Setup(*pons.Core) error { return nil }

func writeTestManifest(t *testing.T, name, placement string) string {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "plugin.json")
	data := fmt.Sprintf(`{"manifest_version":1,"name":%q,"entrypoint":%q,"runtime_protocol":1,"placement":%q}`, name, executable, placement)
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPluginPlanPreservesHostOrder(t *testing.T) {
	firstHost := writeTestManifest(t, "example.first", "host")
	handsManifest := writeTestManifest(t, "example.hands", "hands")
	lastHost := writeTestManifest(t, "example.last", "host")
	configured := hostconfig.Options{Plugins: []hostconfig.BuiltPlugin{
		{Host: orderedTestPlugin("before")},
		{Manifest: firstHost, Config: json.RawMessage(`{"one":1}`)},
		{Manifest: handsManifest, Config: json.RawMessage(`{}`)},
		{Host: orderedTestPlugin("after")},
	}}
	hands, hosts, err := resolvePluginPlan(configured, []string{lastHost}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 4 || hosts[0].Plugin != orderedTestPlugin("before") ||
		hosts[1].Manifest != firstHost || hosts[2].Plugin != orderedTestPlugin("after") ||
		hosts[3].Manifest != lastHost {
		t.Fatalf("host order: %+v", hosts)
	}
	if len(hands) != 1 || hands[0] != handsManifest || string(hosts[1].Config) != `{"one":1}` {
		t.Fatalf("manifest routing: hands=%v hosts=%+v", hands, hosts)
	}
}

func TestExplicitPluginFlagReplacesOnlyConfiguredHands(t *testing.T) {
	hook := writeTestManifest(t, "example.policy", "host")
	configuredHands := writeTestManifest(t, "example.configured", "hands")
	explicitHands := writeTestManifest(t, "example.explicit", "hands")
	configured := hostconfig.Options{Plugins: []hostconfig.BuiltPlugin{
		{Host: orderedTestPlugin("built-in")},
		{Manifest: hook},
		{Manifest: configuredHands},
	}}
	hands, hosts, err := resolvePluginPlan(configured, []string{explicitHands}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(hands) != 1 || hands[0] != explicitHands {
		t.Fatalf("hands = %v", hands)
	}
	if len(hosts) != 2 || hosts[0].Plugin != orderedTestPlugin("built-in") || hosts[1].Manifest != hook {
		t.Fatalf("installed hook was dropped: %+v", hosts)
	}
}

func TestHandsPluginRejectsConfig(t *testing.T) {
	handsManifest := writeTestManifest(t, "example.hands", "hands")
	configured := hostconfig.Options{Plugins: []hostconfig.BuiltPlugin{
		{Manifest: handsManifest, Config: json.RawMessage(`{"token":"x"}`)},
	}}
	if _, _, err := resolvePluginPlan(configured, nil, false); err == nil ||
		!strings.Contains(err.Error(), "only for host hook plugins") {
		t.Fatalf("err = %v", err)
	}
}
