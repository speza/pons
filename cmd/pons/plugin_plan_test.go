package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/samperrin/pons"
	"github.com/samperrin/pons/plugins/hostconfig"
)

type orderedTestPlugin string

func (orderedTestPlugin) Setup(*pons.Core) error { return nil }

func TestPluginPlanPreservesHostOrder(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	makeManifest := func(name, placement string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "plugin.json")
		data := fmt.Sprintf(`{"manifest_version":1,"name":%q,"entrypoint":%q,"runtime_protocol":1,"placement":%q}`, name, executable, placement)
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	firstHost := makeManifest("example.first", "host")
	handsManifest := makeManifest("example.hands", "hands")
	lastHost := makeManifest("example.last", "host")
	configured := hostconfig.Options{Plugins: []hostconfig.BuiltPlugin{
		{Host: orderedTestPlugin("before")},
		{Manifest: firstHost, Config: json.RawMessage(`{"one":1}`)},
		{Manifest: handsManifest, Config: json.RawMessage(`{"two":2}`)},
		{Host: orderedTestPlugin("after")},
	}}
	hands, hosts, configs, err := resolvePluginPlan(configured, []string{lastHost}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 4 || hosts[0].Plugin != orderedTestPlugin("before") ||
		hosts[1].Manifest != firstHost || hosts[2].Plugin != orderedTestPlugin("after") ||
		hosts[3].Manifest != lastHost {
		t.Fatalf("host order: %+v", hosts)
	}
	if len(hands) != 1 || hands[0] != handsManifest || string(configs[handsManifest]) != `{"two":2}` ||
		string(hosts[1].Config) != `{"one":1}` {
		t.Fatalf("manifest routing: hands=%v hosts=%+v configs=%v", hands, hosts, configs)
	}
}

func TestExplicitPluginFlagReplacesConfiguredManifests(t *testing.T) {
	configured := hostconfig.Options{Plugins: []hostconfig.BuiltPlugin{
		{Host: orderedTestPlugin("built-in")},
		{Manifest: "/missing/plugin.json"},
	}}
	hands, hosts, _, err := resolvePluginPlan(configured, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(hands) != 0 || len(hosts) != 1 || hosts[0].Plugin != orderedTestPlugin("built-in") {
		t.Fatalf("explicit override: hands=%v hosts=%+v", hands, hosts)
	}
}
