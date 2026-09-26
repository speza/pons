package hostconfig

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/samperrin/pons/plugins/external"
)

// installedPlugin resolves an explicitly enabled external plugin by its ID.
// Installation is a manifest at ~/.pons/plugins/<id>/plugin.json.
type installedPlugin struct {
	id     string
	path   string
	config json.RawMessage
}

func installedRegistration(id string) (pluginRegistration, error) {
	if !external.ValidPluginName(id) {
		return pluginRegistration{}, fmt.Errorf("config plugins.%s: unknown plugin ID", id)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return pluginRegistration{}, err
	}
	path := filepath.Join(home, ".pons", "plugins", id, "plugin.json")
	manifest, err := external.LoadManifest(path)
	if err != nil || manifest.Name != id {
		return pluginRegistration{}, fmt.Errorf("config plugins.%s: no installed plugin manifest with this ID", id)
	}
	version := manifest.ConfigVersion
	if version == "" {
		version = "1.0.0"
	}
	return pluginRegistration{version: version, decode: func(id string, _ bool, raw json.RawMessage) (ConfiguredPlugin, error) {
		if _, err := decodePluginObject[map[string]any](id, raw); err != nil {
			return nil, err
		}
		return installedPlugin{id: id, path: path, config: append(json.RawMessage(nil), raw...)}, nil
	}}, nil
}

func (p installedPlugin) ID() string         { return p.id }
func (p installedPlugin) Provides() []string { return nil }
func (p installedPlugin) Requires() []string { return nil }
func (p installedPlugin) Build(build *BuildContext) error {
	build.AddManifestConfig(p.path, p.config)
	return nil
}
