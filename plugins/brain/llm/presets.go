// Providers and APIs, split the way pi-ai splits them: an API is one wire
// format (an adapter in anthropic.go, openai.go, or responses.go); a
// provider is data — where to send requests, which key to use, and which
// API each model speaks. Gateways such as OpenCode Go or OpenRouter are
// presets over the same adapters, not new code paths. Models that differ
// from their preset's API are listed in catalog_gen.go.
package llm

//go:generate go run gen_catalog.go

import (
	"crypto/rand"
	"fmt"
	"os"
	"runtime/debug"
	"slices"
	"strings"

	aopt "github.com/anthropics/anthropic-sdk-go/option"
	oopt "github.com/openai/openai-go/option"
)

// Wire formats a provider slot can speak.
const (
	APIAnthropicMessages = "anthropic-messages"
	APIOpenAICompletions = "openai-completions"
	APIOpenAIResponses   = "openai-responses"
)

// preset describes one named provider.
type preset struct {
	baseURL     string
	keyEnv      string
	keyOptional bool // e.g. local OpenAI-compatible servers
	model       string
	// modelPrefix is dropped from model IDs copied from other tools'
	// configs (e.g. OpenCode's "opencode-go/kimi-k3").
	modelPrefix string
	// api is the default wire API; catalog_gen.go overrides it per model
	// and a slot's API field overrides both.
	api string
	// headers are sent with every request; session is a stable
	// conversation ID (random when the caller has none).
	headers func(session string) map[string]string
	// codex slots authenticate with a ChatGPT subscription from the pons
	// auth store instead of an API key.
	codex bool
}

var presets = map[string]preset{
	"anthropic": {
		baseURL: "https://api.anthropic.com",
		keyEnv:  "ANTHROPIC_API_KEY",
		model:   "claude-sonnet-4-5",
		api:     APIAnthropicMessages,
	},
	"openai": {
		baseURL:     "https://api.openai.com/v1",
		keyEnv:      "OPENAI_API_KEY",
		keyOptional: true,
		model:       "gpt-4o-mini",
		api:         APIOpenAICompletions,
	},
	"codex": {
		baseURL: codexBaseURL,
		model:   "gpt-5.6-terra",
		api:     APIOpenAIResponses,
		codex:   true,
	},
	"opencode-go": {
		baseURL:     "https://opencode.ai/zen/go/v1",
		keyEnv:      "OPENCODE_API_KEY",
		model:       "kimi-k3",
		modelPrefix: "opencode-go/",
		api:         APIOpenAICompletions,
		// OpenCode Go asks clients to name themselves and to send a stable
		// session per conversation for routing and prompt caching.
		headers: func(session string) map[string]string {
			return map[string]string{
				"User-Agent":         userAgent(),
				"x-opencode-session": session,
			}
		},
	},
	"openrouter": {
		baseURL: "https://openrouter.ai/api/v1",
		keyEnv:  "OPENROUTER_API_KEY",
		model:   "openrouter/auto",
		api:     APIOpenAICompletions,
	},
}

// providerClient builds one provider slot: the preset's defaults, key
// resolution from env, API selection, and the adapter constructor. Codex
// slots resolve credentials from the pons auth store, keyed by slot id.
func providerClient(slot Fallback, maxTokens int, sessionID string) (Client, string, error) {
	name := slot.Provider
	if name == "" {
		name = "anthropic"
	}
	p, ok := presets[name]
	if !ok {
		return nil, "", fmt.Errorf("llm: unknown provider %q (want %s)", name, strings.Join(providerNames(), ", "))
	}

	model := slot.Model
	if p.modelPrefix != "" {
		model = strings.TrimPrefix(model, p.modelPrefix)
	}
	if model == "" {
		model = p.model
	}
	baseURL := strings.TrimSuffix(slot.BaseURL, "/")
	if baseURL == "" {
		baseURL = p.baseURL
	}
	api := slot.API
	if api == "" {
		api = catalog[name][model]
	}
	if api == "" {
		api = p.api
	}

	var headers map[string]string
	if p.headers != nil {
		if sessionID == "" {
			sessionID = rand.Text()
		}
		headers = p.headers(sessionID)
	}

	if p.codex {
		if api != APIOpenAIResponses {
			return nil, "", fmt.Errorf("llm: provider %q only speaks %s", name, APIOpenAIResponses)
		}
		auth, err := codexSlotAuth(slot.ID)
		if err != nil {
			return nil, "", err
		}
		rc := &responsesClient{auth: auth, baseURL: baseURL, model: model, maxTokens: maxTokens}
		return rc, model, nil
	}

	key := slot.APIKey
	if key == "" && p.keyEnv != "" {
		key = os.Getenv(p.keyEnv)
	}
	if key == "" && !p.keyOptional {
		return nil, "", fmt.Errorf("llm: no API key — set %s (or Config.APIKey)", p.keyEnv)
	}

	// pons resolves credentials itself. The SDKs would otherwise add their
	// own environment credentials and account headers to every request,
	// whichever host it goes to.
	switch api {
	case APIAnthropicMessages:
		opts := []aopt.RequestOption{aopt.WithoutEnvironmentDefaults()}
		for name, value := range headers {
			opts = append(opts, aopt.WithHeader(name, value))
		}
		// The Anthropic SDK appends v1/messages itself.
		ac := newAnthropicClient(key, strings.TrimSuffix(baseURL, "/v1"), maxTokens, opts...)
		ac.model = model
		return ac, model, nil
	case APIOpenAICompletions, APIOpenAIResponses:
		opts := []oopt.RequestOption{
			oopt.WithHeaderDel("OpenAI-Organization"),
			oopt.WithHeaderDel("OpenAI-Project"),
		}
		for name, value := range headers {
			opts = append(opts, oopt.WithHeader(name, value))
		}
		if api == APIOpenAIResponses {
			rc := &responsesClient{
				apiKey:    key,
				baseURL:   baseURL,
				model:     model,
				maxTokens: maxTokens,
				extra:     opts,
			}
			return rc, model, nil
		}
		oc := newOpenAIClient(key, baseURL, maxTokens, opts...)
		oc.model = model
		return oc, model, nil
	default:
		return nil, "", fmt.Errorf("llm: unknown api %q (want %q, %q, or %q)",
			api, APIAnthropicMessages, APIOpenAICompletions, APIOpenAIResponses)
	}
}

// codexSlotAuth loads a codex slot's ChatGPT-subscription credentials from
// pons's auth store (~/.pons/auth.json, created by --login -as <id>;
// refreshed tokens are written back to it). The slot's own id names the
// store entry. Synthetic slot ids (unnamed primary, ad-hoc flag) resolve
// to the default "codex" entry, which is what a bare --login writes.
func codexSlotAuth(slotID string) (*codexAuth, error) {
	authPath, err := DefaultCodexAuthPath()
	if err != nil {
		return nil, err
	}
	authID := slotID
	switch slotID {
	case "", "primary", "flag":
		authID = LegacyAuthID
	}
	return loadCodexAuth(authPath, authID)
}

func providerNames() []string {
	names := make([]string, 0, len(presets))
	for name := range presets {
		names = append(names, fmt.Sprintf("%q", name))
	}
	slices.Sort(names)
	return names
}

// userAgent names pons and its module version ("dev" for local builds).
func userAgent() string {
	version := "dev"
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		version = info.Main.Version
	}
	return "pons/" + version
}
