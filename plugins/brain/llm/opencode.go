// OpenCode Go adapter: the OpenCode Go subscription serves each model over
// one of three wire formats (Chat Completions, Anthropic Messages, or
// Responses) behind a single API key. This file only picks the format and
// endpoint; the existing adapters do the translation.
package llm

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"runtime/debug"
	"strings"

	aopt "github.com/anthropics/anthropic-sdk-go/option"
	oopt "github.com/openai/openai-go/option"
)

const (
	openCodeGoBaseURL      = "https://opencode.ai/zen/go"
	openCodeGoDefaultModel = "kimi-k3"
)

// openCodeGoClient builds a client for the OpenCode Go subscription. The
// service asks clients to name themselves in User-Agent and to send a
// stable x-opencode-session per conversation; without a conversation ID
// (e.g. a classifier call) each client gets its own random session.
func openCodeGoClient(slot Fallback, maxTokens int, sessionID string) (Client, string, error) {
	key := slot.APIKey
	if key == "" {
		key = os.Getenv("OPENCODE_API_KEY")
	}
	if key == "" {
		return nil, "", errors.New("llm: no API key — set OPENCODE_API_KEY (or Config.APIKey)")
	}

	// Accept the "opencode-go/<model>" form from OpenCode configs.
	model := strings.TrimPrefix(slot.Model, "opencode-go/")
	if model == "" {
		model = openCodeGoDefaultModel
	}
	// Accept base URLs with or without /v1: each SDK wants a different root.
	baseURL := strings.TrimSuffix(strings.TrimSuffix(slot.BaseURL, "/"), "/v1")
	if baseURL == "" {
		baseURL = openCodeGoBaseURL
	}

	if sessionID == "" {
		sessionID = rand.Text()
	}
	headers := map[string]string{
		"User-Agent":         userAgent(),
		"x-opencode-session": sessionID,
	}

	api := slot.API
	if api == "" {
		api = openCodeGoWire(model)
	}
	// Both SDKs add credentials and account headers from their own
	// environment variables; none of those may reach a third party.
	switch api {
	case "messages":
		opts := []aopt.RequestOption{aopt.WithoutEnvironmentDefaults()}
		for name, value := range headers {
			opts = append(opts, aopt.WithHeader(name, value))
		}
		// The Anthropic SDK appends v1/messages itself.
		ac := newAnthropicClient(key, baseURL, maxTokens, opts...)
		ac.model = model
		return ac, model, nil
	case "chat", "responses":
		opts := []oopt.RequestOption{
			oopt.WithHeaderDel("OpenAI-Organization"),
			oopt.WithHeaderDel("OpenAI-Project"),
		}
		for name, value := range headers {
			opts = append(opts, oopt.WithHeader(name, value))
		}
		if api == "responses" {
			rc := &responsesClient{
				apiKey:    key,
				baseURL:   baseURL + "/v1",
				model:     model,
				maxTokens: maxTokens,
				extra:     opts,
			}
			return rc, model, nil
		}
		oc := newOpenAIClient(key, baseURL+"/v1", maxTokens, opts...)
		oc.model = model
		return oc, model, nil
	default:
		return nil, "", fmt.Errorf("llm: unknown api %q (want \"chat\", \"messages\", or \"responses\")", api)
	}
}

// openCodeGoWire maps a model to the wire format OpenCode Go serves it
// over, by model family. Unknown families use Chat Completions, which
// covers most of the catalog; a slot's API field overrides the guess.
func openCodeGoWire(model string) string {
	for _, prefix := range []string{"minimax-", "qwen"} {
		if strings.HasPrefix(model, prefix) {
			return "messages"
		}
	}
	for _, prefix := range []string{"grok-", "gpt-", "muse-"} {
		if strings.HasPrefix(model, prefix) {
			return "responses"
		}
	}
	return "chat"
}

// userAgent names pons and its module version ("dev" for local builds).
func userAgent() string {
	version := "dev"
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		version = info.Main.Version
	}
	return "pons/" + version
}
