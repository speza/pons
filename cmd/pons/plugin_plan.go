package main

import (
	"bytes"
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

// resolvePluginPlan routes configured and explicit manifests by placement.
// Explicit --plugin manifests replace configured hands plugins only; host
// hook plugins, such as an installed policy, stay enabled.
func resolvePluginPlan(configured hostconfig.Options, explicit []string, replaceHands bool) (
	hands []string,
	host []hostPluginSource,
	err error,
) {
	addManifest := func(path string, config json.RawMessage, configured bool) error {
		manifest, loadErr := external.LoadManifest(path)
		if loadErr != nil {
			return loadErr
		}
		path = manifest.Path()
		if manifest.Placement == external.PlacementHost {
			host = append(host, hostPluginSource{Manifest: path, Config: config})
			return nil
		}
		// Hands plugins run inside the execution environment, which has no
		// channel for plugin config yet; refuse config instead of dropping it.
		if trimmed := bytes.TrimSpace(config); len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("{}")) {
			return fmt.Errorf("plugin %q: config is supported only for host hook plugins", manifest.Name)
		}
		if configured && replaceHands {
			return nil
		}
		hands = append(hands, path)
		return nil
	}
	for _, plugin := range configured.Plugins {
		if plugin.Host != nil {
			host = append(host, hostPluginSource{Plugin: plugin.Host})
			continue
		}
		if plugin.Manifest == "" {
			return nil, nil, fmt.Errorf("configured plugin has no host or manifest")
		}
		if err := addManifest(plugin.Manifest, plugin.Config, true); err != nil {
			return nil, nil, err
		}
	}
	for _, path := range explicit {
		if err := addManifest(path, nil, false); err != nil {
			return nil, nil, err
		}
	}
	return hands, host, nil
}
