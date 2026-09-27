package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/samperrin/pons/plugins/brain/llm"
)

func TestPolicyProvidersIgnoreProjectOverrides(t *testing.T) {
	home, workspace := t.TempDir(), t.TempDir()
	write(t, filepath.Join(home, ".pons", "config.json"), `{
		"providers":[{"id":"policy-codex","provider":"codex","base_url":"https://trusted.example"}],
		"plugins":[{"id":"classifier/codex","version":"1.0.0","enabled":true,"config":{"provider_id":"policy-codex"}}]
	}`)
	write(t, filepath.Join(workspace, ".pons.json"), `{
		"providers":[{"id":"policy-codex","provider":"codex","base_url":"https://project.example"}]
	}`)
	merged, err := loadSettings(home, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if merged.Providers[0].BaseURL != "https://project.example" {
		t.Fatal("test did not establish a project override")
	}
	global, err := loadSettings(home, "")
	if err != nil {
		t.Fatal(err)
	}
	providers, err := trustedPluginProviders(global, map[string]bool{}, llm.Fallback{Provider: "anthropic"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := providers["policy-codex"].BaseURL; got != "https://trusted.example" {
		t.Fatalf("policy provider endpoint = %q", got)
	}
}

func TestPluginProvidersResolveNamedAndEffectivePrimary(t *testing.T) {
	cfg := settings{Providers: []providerSettings{
		{ID: "codex-personal", Provider: "codex", Model: "configured-model"},
		{ID: "codex-work", Provider: "codex", Model: "work-model"},
	}}
	primary := toFallback(cfg.Providers[0])
	primary.Model = "flag-model"
	providers := pluginProviders(cfg, primary)
	if providers["primary"].Model != "flag-model" ||
		providers["codex-personal"].Model != "flag-model" ||
		providers["codex-work"].Model != "work-model" {
		t.Fatalf("resolved plugin providers: %+v", providers)
	}
}

func TestProviderSlotsFromConfigList(t *testing.T) {
	cfg := settings{
		DefaultProviderID: new("codex-work"),
		Providers: []providerSettings{
			{ID: "codex-personal", Provider: "codex"},
			{ID: "codex-work", Provider: "codex", Model: "m-w"},
			{ID: "anthropic", Provider: "anthropic", Model: "claude-x"},
		},
	}
	slots, err := providerSlots(cfg, map[string]bool{}, llm.Fallback{Provider: "anthropic"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(slots) != 3 {
		t.Fatalf("slots: %+v", slots)
	}
	// default first, remaining entries in listed order
	if slots[0].ID != "codex-work" || slots[0].Model != "m-w" {
		t.Fatalf("primary: %+v", slots[0])
	}
	if slots[1].ID != "codex-personal" || slots[2].ID != "anthropic" {
		t.Fatalf("backups: %+v %+v", slots[1], slots[2])
	}
}

func TestProviderSlotsFlagOverridesDefault(t *testing.T) {
	cfg := settings{
		DefaultProviderID: new("a"),
		Providers: []providerSettings{
			{ID: "a", Provider: "codex"},
			{ID: "b", Provider: "openai"},
		},
	}
	// Explicit -provider is ad-hoc: flags-only primary, all declared
	// providers back it up.
	slots, err := providerSlots(cfg, map[string]bool{"provider": true}, llm.Fallback{Provider: "openai"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if slots[0].Provider != "openai" || slots[0].Model != "" || slots[0].APIKey != "" {
		t.Fatalf("flag primary should be flags-only: %+v", slots[0])
	}
	if len(slots) != 3 || slots[1].ID != "a" || slots[2].ID != "b" {
		t.Fatalf("declared providers should back up: %+v", slots)
	}
}

func TestProviderSlotsFlagsOverrideEntry(t *testing.T) {
	cfg := settings{
		DefaultProviderID: new("a"),
		Providers: []providerSettings{
			{ID: "a", Provider: "opencode-go", Model: "entry-model", API: llm.APIOpenAICompletions},
		},
	}
	flags := llm.Fallback{Provider: "anthropic", Model: "flag-model", API: llm.APIAnthropicMessages}
	slots, err := providerSlots(cfg, map[string]bool{"model": true, "api": true}, flags, nil)
	if err != nil {
		t.Fatal(err)
	}
	if slots[0].Provider != "opencode-go" || slots[0].Model != "flag-model" || slots[0].API != llm.APIAnthropicMessages {
		t.Fatalf("--model and --api should override the entry: %+v", slots[0])
	}
}

func TestProviderSlotsFallbackFlagReplaces(t *testing.T) {
	cfg := settings{
		DefaultProviderID: new("a"),
		Providers: []providerSettings{
			{ID: "a", Provider: "anthropic"},
			{ID: "b", Provider: "openai"},
		},
	}
	slots, err := providerSlots(cfg, map[string]bool{}, llm.Fallback{Provider: "anthropic"}, []string{"codex"})
	if err != nil {
		t.Fatal(err)
	}
	if len(slots) != 2 || slots[0].Provider != "anthropic" || slots[1].Provider != "codex" {
		t.Fatalf("-fallback should replace the list: %+v", slots)
	}
}

func TestProviderSlotsLegacyFlat(t *testing.T) {
	cfg := settings{Provider: new("openai")}
	slots, err := providerSlots(cfg, map[string]bool{}, llm.Fallback{Provider: "openai", Model: "gpt-x"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(slots) != 1 || slots[0].Provider != "openai" || slots[0].Model != "gpt-x" {
		t.Fatalf("flat config: %+v", slots)
	}
}

func TestProviderSlotsValidation(t *testing.T) {
	cases := []struct {
		name string
		cfg  settings
		want string
	}{
		{"duplicate id", settings{Providers: []providerSettings{{ID: "a", Provider: "openai"}, {ID: "a", Provider: "codex"}}}, "duplicate"},
		{"missing id", settings{Providers: []providerSettings{{Provider: "openai"}}}, "id is required"},
		{"reserved primary id", settings{Providers: []providerSettings{{ID: "primary", Provider: "codex"}}}, "reserved"},
		{"missing provider type", settings{Providers: []providerSettings{{ID: "a"}}}, "provider is required"},
		{"no default with several", settings{Providers: []providerSettings{{ID: "a", Provider: "openai"}, {ID: "b", Provider: "codex"}}}, "default_provider_id"},
		{"unknown default", settings{DefaultProviderID: new("z"), Providers: []providerSettings{{ID: "a", Provider: "openai"}}}, "does not match"},
		{"flat and list", settings{Provider: new("openai"), Providers: []providerSettings{{ID: "a", Provider: "openai"}}}, "both"},
		{"default without list", settings{DefaultProviderID: new("a")}, "no providers"},
		{"empty fallback spec", settings{}, "provider is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var flagFallbacks []string
			if tc.name == "empty fallback spec" {
				flagFallbacks = []string{":model"}
			}
			if _, err := providerSlots(tc.cfg, map[string]bool{}, llm.Fallback{Provider: "anthropic"}, flagFallbacks); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
}
