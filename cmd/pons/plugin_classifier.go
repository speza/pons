package main

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/samperrin/pons/plugins/actionpolicy"
)

type classifierPluginSettings struct {
	Model     string `json:"model,omitempty"`
	APIKeyEnv string `json:"api_key_env,omitempty"`
	Timeout   string `json:"timeout,omitempty"`

	id            string
	defaultKey    string
	newClassifier func(actionpolicy.RemoteConfig) (actionpolicy.Classifier, error)
}

func decodeTypeSafePlugin(id string, _ bool, raw json.RawMessage) (configuredPlugin, error) {
	return decodeClassifierPlugin(id, raw, "TYPESAFE_API_KEY", func(config actionpolicy.RemoteConfig) (actionpolicy.Classifier, error) {
		return actionpolicy.NewTypeSafeClassifier(config)
	})
}

func decodeOpenAIPlugin(id string, _ bool, raw json.RawMessage) (configuredPlugin, error) {
	return decodeClassifierPlugin(id, raw, "OPENAI_API_KEY", func(config actionpolicy.RemoteConfig) (actionpolicy.Classifier, error) {
		return actionpolicy.NewOpenAIClassifier(config)
	})
}

func decodeClassifierPlugin(
	id string,
	raw json.RawMessage,
	defaultKey string,
	newClassifier func(actionpolicy.RemoteConfig) (actionpolicy.Classifier, error),
) (configuredPlugin, error) {
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

func (c classifierPluginSettings) Build(build *pluginBuildContext) error {
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
