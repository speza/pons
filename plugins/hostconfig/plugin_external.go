package hostconfig

import (
	"encoding/json"
	"fmt"
	"path/filepath"
)

type externalPluginSettings struct {
	Manifests []string `json:"manifests,omitempty"`

	id string
}

func decodeExternalPlugin(id string, enabled bool, raw json.RawMessage) (ConfiguredPlugin, error) {
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

func (p externalPluginSettings) Build(build *BuildContext) error {
	for _, manifest := range p.Manifests {
		build.AddManifest(manifest)
	}
	return nil
}
