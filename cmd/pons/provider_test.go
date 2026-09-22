package main

import (
	"strings"
	"testing"
)

func TestProviderSlotsFromConfigList(t *testing.T) {
	cfg := settings{
		DefaultProviderID: new("codex-work"),
		Providers: []providerSettings{
			{ID: "codex-personal", Provider: "codex"},
			{ID: "codex-work", Provider: "codex", Model: "m-w"},
			{ID: "anthropic", Provider: "anthropic", Model: "claude-x"},
		},
	}
	slots, err := providerSlots(cfg, map[string]bool{}, "anthropic", "", "", nil)
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
	slots, err := providerSlots(cfg, map[string]bool{"provider": true}, "openai", "", "", nil)
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

func TestProviderSlotsModelFlagOverridesEntry(t *testing.T) {
	cfg := settings{
		DefaultProviderID: new("a"),
		Providers:         []providerSettings{{ID: "a", Provider: "anthropic", Model: "entry-model"}},
	}
	slots, err := providerSlots(cfg, map[string]bool{"model": true}, "anthropic", "flag-model", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if slots[0].Model != "flag-model" {
		t.Fatalf("--model should override the entry: %+v", slots[0])
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
	slots, err := providerSlots(cfg, map[string]bool{}, "anthropic", "", "", []string{"codex"})
	if err != nil {
		t.Fatal(err)
	}
	if len(slots) != 2 || slots[0].Provider != "anthropic" || slots[1].Provider != "codex" {
		t.Fatalf("-fallback should replace the list: %+v", slots)
	}
}

func TestProviderSlotsLegacyFlat(t *testing.T) {
	cfg := settings{Provider: new("openai")}
	slots, err := providerSlots(cfg, map[string]bool{}, "openai", "gpt-x", "", nil)
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
			if _, err := providerSlots(tc.cfg, map[string]bool{}, "anthropic", "", "", flagFallbacks); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
}
