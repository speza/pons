// Provider-chain resolution: config files and flags turn into an ordered
// list of provider slots for the brain. Slot 0 is the primary; the rest
// are failover slots. See llm.Fallback.
package main

import (
	"fmt"
	"strings"

	"github.com/samperrin/pons/plugins/brain/llm"
)

// providerSlots resolves the brain's provider chain:
//
//   - With a config providers list and no -provider flag: the
//     defaultProviderId entry is primary and the remaining entries back it
//     up in listed order. --model/--base-url flags still override the
//     primary entry's own fields.
//   - With -provider set (or no providers list): the flags define the
//     primary; the config providers all back it up.
//   - -fallback flags replace the backup list in both cases.
func providerSlots(cfg settings, flagSet map[string]bool, flagProvider, flagModel, flagBaseURL string, flagFallbacks []string) ([]llm.Fallback, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	var primary llm.Fallback
	var backups []llm.Fallback

	if len(cfg.Providers) > 0 {
		ordered, err := cfg.orderedProviders()
		if err != nil {
			return nil, err
		}
		if flagSet["provider"] {
			// Explicit -provider is ad-hoc: it defines the primary from the
			// flags alone (fresh credentials from env), and the declared
			// providers back it up.
			primary = llm.Fallback{ID: "flag", Provider: flagProvider, Model: flagModel, BaseURL: flagBaseURL}
			for _, p := range ordered {
				backups = append(backups, toFallback(p))
			}
		} else {
			p := ordered[0]
			primary = toFallback(p)
			if flagSet["model"] {
				primary.Model = flagModel
			}
			if flagSet["base-url"] {
				primary.BaseURL = flagBaseURL
			}
			for _, q := range ordered[1:] {
				backups = append(backups, toFallback(q))
			}
		}
	} else {
		primary = llm.Fallback{ID: "primary", Provider: flagProvider, Model: flagModel, BaseURL: flagBaseURL}
		for _, f := range cfg.Fallbacks {
			backups = append(backups, llm.Fallback{
				ID: f.Provider, Provider: f.Provider, Model: f.Model, BaseURL: f.BaseURL,
				APIKey: f.APIKey,
			})
		}
	}

	if len(flagFallbacks) > 0 {
		backups = backups[:0]
		for _, f := range flagFallbacks {
			provider, fbModel, err := parseFallbackFlag(f)
			if err != nil {
				return nil, err
			}
			backups = append(backups, llm.Fallback{ID: f, Provider: provider, Model: fbModel})
		}
	}

	slots := make([]llm.Fallback, 0, 1+len(backups))
	slots = append(slots, primary)
	return append(slots, backups...), nil
}

func toFallback(p providerSettings) llm.Fallback {
	return llm.Fallback{
		ID: p.ID, Provider: p.Provider, Model: p.Model, BaseURL: p.BaseURL,
		APIKey: p.APIKey,
	}
}

// pluginProviders exposes resolved provider slots to trusted host plugins.
// The primary alias follows CLI overrides; named entries remain addressable
// even when a temporary fallback chain excludes them.
func pluginProviders(cfg settings, primary llm.Fallback) map[string]llm.Fallback {
	providers := make(map[string]llm.Fallback, len(cfg.Providers)+1)
	for _, provider := range cfg.Providers {
		providers[provider.ID] = toFallback(provider)
	}
	if primary.ID != "" {
		providers[primary.ID] = primary
	}
	providers["primary"] = primary
	return providers
}

// parseFallbackFlag is the -fallback provider[:model] syntax.
func parseFallbackFlag(spec string) (string, string, error) {
	provider, model, _ := strings.Cut(spec, ":")
	if strings.TrimSpace(provider) == "" {
		return "", "", fmt.Errorf("fallback %q: provider is required", spec)
	}
	return provider, model, nil
}
