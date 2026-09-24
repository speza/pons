package main

import (
	"encoding/json"
	"fmt"

	"github.com/samperrin/pons"
	"github.com/samperrin/pons/plugins/external"
	"github.com/samperrin/pons/plugins/hostconfig"
)

// hostPluginSource preserves config order until each run creates its own Core.
// External hook processes are started per run; trusted Go plugins are reused.
type hostPluginSource struct {
	Plugin   pons.Plugin
	Manifest string
	Config   json.RawMessage
}

func resolvePluginPlan(configured hostconfig.Options, explicit []string, replaceManifests bool) (
	hands []string,
	host []hostPluginSource,
	configs map[string]json.RawMessage,
	err error,
) {
	configs = make(map[string]json.RawMessage)
	addManifest := func(path string, config json.RawMessage) error {
		manifest, loadErr := external.LoadManifest(path)
		if loadErr != nil {
			return loadErr
		}
		path = manifest.Path()
		if manifest.Placement == external.PlacementHost {
			host = append(host, hostPluginSource{Manifest: path, Config: config})
		} else {
			hands = append(hands, path)
			configs[path] = config
		}
		return nil
	}
	for _, plugin := range configured.Plugins {
		if plugin.Host != nil {
			host = append(host, hostPluginSource{Plugin: plugin.Host})
			continue
		}
		if replaceManifests {
			continue
		}
		if plugin.Manifest == "" {
			return nil, nil, nil, fmt.Errorf("configured plugin has no host or manifest")
		}
		if err := addManifest(plugin.Manifest, plugin.Config); err != nil {
			return nil, nil, nil, err
		}
	}
	for _, path := range explicit {
		if err := addManifest(path, nil); err != nil {
			return nil, nil, nil, err
		}
	}
	return hands, host, configs, nil
}
