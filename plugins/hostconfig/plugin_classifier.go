package hostconfig

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/samperrin/pons/plugins/actionpolicy"
	"github.com/samperrin/pons/plugins/brain/llm"
)

type codexClassifierSettings struct {
	ProviderID string `json:"provider_id"`
	Model      string `json:"model,omitempty"`
	Timeout    string `json:"timeout,omitempty"`
	id         string
}

func decodeCodexPlugin(id string, enabled bool, raw json.RawMessage) (ConfiguredPlugin, error) {
	config, err := decodePluginObject[codexClassifierSettings](id, raw)
	if err != nil {
		return nil, err
	}
	if enabled && config.ProviderID == "" {
		return nil, fmt.Errorf("config plugins.%s.provider_id is required when enabled", id)
	}
	if config.Timeout != "" {
		duration, err := time.ParseDuration(config.Timeout)
		if err != nil || duration <= 0 {
			return nil, fmt.Errorf("config plugins.%s.timeout must be a positive duration", id)
		}
	}
	config.id = id
	return config, nil
}

func (c codexClassifierSettings) ID() string { return c.id }
func (c codexClassifierSettings) Provides() []string {
	return []string{actionpolicy.ClassifierCapability(c.id)}
}
func (c codexClassifierSettings) Requires() []string { return nil }

func (c codexClassifierSettings) Build(build *BuildContext) error {
	slot, ok := build.providers[c.ProviderID]
	if !ok {
		return fmt.Errorf("provider_id %q does not match a configured provider", c.ProviderID)
	}
	if slot.Provider != "codex" {
		return fmt.Errorf("provider_id %q must use the codex provider", c.ProviderID)
	}
	if c.Model != "" {
		slot.Model = c.Model
	}
	if slot.Model == "" {
		slot.Model = "gpt-5.6-terra"
	}
	client, err := llm.NewProviderClient(slot)
	if err != nil {
		return err
	}
	var timeout time.Duration
	if c.Timeout != "" {
		timeout, _ = time.ParseDuration(c.Timeout) // validated during decoding
	}
	classifier, err := actionpolicy.NewCodexClassifier(client, slot.Model, timeout)
	if err != nil {
		return err
	}
	build.hostPlugins = append(build.hostPlugins, actionpolicy.ClassifierPlugin{
		ID: c.id, Classifier: classifier,
	})
	return nil
}

type classifierPluginSettings struct {
	Model     string `json:"model,omitempty"`
	APIKeyEnv string `json:"api_key_env,omitempty"`
	Timeout   string `json:"timeout,omitempty"`

	id            string
	defaultKey    string
	newClassifier func(actionpolicy.RemoteConfig) (actionpolicy.Classifier, error)
}

func decodeTypeSafePlugin(id string, _ bool, raw json.RawMessage) (ConfiguredPlugin, error) {
	return decodeClassifierPlugin(id, raw, "TYPESAFE_API_KEY", func(config actionpolicy.RemoteConfig) (actionpolicy.Classifier, error) {
		return actionpolicy.NewTypeSafeClassifier(config)
	})
}

func decodeOpenAIPlugin(id string, _ bool, raw json.RawMessage) (ConfiguredPlugin, error) {
	return decodeClassifierPlugin(id, raw, "OPENAI_API_KEY", func(config actionpolicy.RemoteConfig) (actionpolicy.Classifier, error) {
		return actionpolicy.NewOpenAIClassifier(config)
	})
}

func decodeClassifierPlugin(
	id string,
	raw json.RawMessage,
	defaultKey string,
	newClassifier func(actionpolicy.RemoteConfig) (actionpolicy.Classifier, error),
) (ConfiguredPlugin, error) {
	config, err := decodePluginObject[classifierPluginSettings](id, raw)
	if err != nil {
		return nil, err
	}
	if config.Timeout != "" {
		duration, err := time.ParseDuration(config.Timeout)
		if err != nil || duration <= 0 {
			return nil, fmt.Errorf("config plugins.%s.timeout must be a positive duration", id)
		}
	}
	config.id = id
	config.defaultKey = defaultKey
	config.newClassifier = newClassifier
	return config, nil
}

func (c classifierPluginSettings) ID() string { return c.id }
func (c classifierPluginSettings) Provides() []string {
	return []string{actionpolicy.ClassifierCapability(c.id)}
}
func (c classifierPluginSettings) Requires() []string { return nil }

func (c classifierPluginSettings) Build(build *BuildContext) error {
	keyName := c.APIKeyEnv
	if keyName == "" {
		keyName = c.defaultKey
	}
	key := build.getenv(keyName)
	if key == "" {
		return fmt.Errorf("%s is not set", keyName)
	}
	remote := actionpolicy.RemoteConfig{APIKey: key, Model: c.Model}
	if c.Timeout != "" {
		var err error
		remote.Timeout, err = time.ParseDuration(c.Timeout)
		if err != nil {
			return fmt.Errorf("timeout: %w", err)
		}
	}
	classifier, err := c.newClassifier(remote)
	if err != nil {
		return err
	}
	build.hostPlugins = append(build.hostPlugins, actionpolicy.ClassifierPlugin{
		ID: c.id, Classifier: classifier,
	})
	return nil
}
