package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
)

type externalPluginSettings struct {
	Manifests []string `json:"manifests,omitempty"`

	id string
}

func decodeExternalPlugin(id string, enabled bool, raw json.RawMessage) (configuredPlugin, error) {
	config, err := decodePluginObject[externalPluginSettings](id, raw)
	if err != nil {
		return nil, err
	}
	if enabled && len(config.Manifests) == 0 {
		return nil, fmt.Errorf("config plugins.%s.manifests must not be empty when enabled", id)
	}
	for i, path := range config.Manifests {
		if !filepath.IsAbs(path) {
			return nil, fmt.Errorf("config plugins.%s.manifests[%d]: path must be absolute", id, i)
		}
	}
	config.id = id
	return config, nil
}

func (p externalPluginSettings) ID() string         { return p.id }
func (p externalPluginSettings) Provides() []string { return nil }
func (p externalPluginSettings) Requires() []string { return nil }

func (p externalPluginSettings) Build(build *pluginBuildContext) error {
	build.manifests = append(build.manifests, p.Manifests...)
	return nil
}
