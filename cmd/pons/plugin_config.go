package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"github.com/samperrin/pons"
)

// pluginEntry separates shared plugin metadata from implementation settings.
type pluginEntry struct {
	ID      string          `json:"id"`
	Version string          `json:"version"`
	Enabled *bool           `json:"enabled"`
	Config  json.RawMessage `json:"config"`
}

type pluginSettings []pluginEntry

func (settings *pluginSettings) UnmarshalJSON(data []byte) error {
	if trimmed := bytes.TrimSpace(data); len(trimmed) == 0 || trimmed[0] != '[' {
		return fmt.Errorf("config plugins must be a list")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var result []pluginEntry
	if err := decoder.Decode(&result); err != nil {
		return fmt.Errorf("config plugins: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("config plugins: trailing data")
	}
	*settings = result
	return nil
}

// Configured plugins declare capability dependencies. Their implementation
// decides what to register; the loader only validates and orders them.
type configuredPlugin interface {
	ID() string
	Provides() []string
	Requires() []string
	Build(*pluginBuildContext) error
}

type pluginFactory func(string, bool, json.RawMessage) (configuredPlugin, error)

type pluginRegistration struct {
	version string
	decode  pluginFactory
}

var pluginRegistry = map[string]pluginRegistration{
	"classifier/typesafe-jev": {version: "1.0.0", decode: decodeTypeSafePlugin},
	"classifier/openai":       {version: "1.0.0", decode: decodeOpenAIPlugin},
	"action_policy":           {version: "1.0.0", decode: decodeActionPolicyPlugin},
	"external":                {version: "1.0.0", decode: decodeExternalPlugin},
}

type pluginBuildContext struct {
	getenv      func(string) string
	hostPlugins []pons.Plugin
	manifests   []string
}

type pluginOptions struct {
	hostPlugins []pons.Plugin
	manifests   []string
}

func decodePluginObject[T any](id string, raw json.RawMessage) (T, error) {
	var config T
	if trimmed := bytes.TrimSpace(raw); len(trimmed) == 0 || trimmed[0] != '{' {
		return config, fmt.Errorf("config plugins.%s must be an object", id)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return config, fmt.Errorf("config plugins.%s: %w", id, err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return config, fmt.Errorf("config plugins.%s: trailing data", id)
	}
	return config, nil
}

// decodePluginConfigs validates plugin-owned config and resolves capability
// dependencies before any provider keys or executable manifests are used.
func decodePluginConfigs(raw pluginSettings) ([]configuredPlugin, error) {
	plugins := make(map[string]configuredPlugin, len(raw))
	active := make(map[string]bool, len(raw))
	ids := make([]string, 0, len(raw))
	for i, entry := range raw {
		id := entry.ID
		if id == "" {
			return nil, fmt.Errorf("config plugins[%d].id is required", i)
		}
		if _, exists := plugins[id]; exists {
			return nil, fmt.Errorf("config plugins.%s: duplicate plugin ID", id)
		}
		registration, ok := pluginRegistry[id]
		if !ok {
			return nil, fmt.Errorf("config plugins.%s: unknown plugin ID", id)
		}
		if entry.Version != registration.version {
			return nil, fmt.Errorf("config plugins.%s: unsupported version %q (supported: %s)", id, entry.Version, registration.version)
		}
		if entry.Enabled == nil {
			return nil, fmt.Errorf("config plugins.%s.enabled is required", id)
		}
		plugin, err := registration.decode(id, *entry.Enabled, entry.Config)
		if err != nil {
			return nil, err
		}
		plugins[id] = plugin
		active[id] = *entry.Enabled
		ids = append(ids, id)
	}

	providers := make(map[string]string)
	for _, id := range ids {
		plugin := plugins[id]
		if !active[id] {
			continue
		}
		for _, capability := range plugin.Provides() {
			if other, exists := providers[capability]; exists {
				return nil, fmt.Errorf("config plugins.%s: capability %q already provided by %q", id, capability, other)
			}
			providers[capability] = id
		}
	}

	state := make(map[string]uint8)
	ordered := make([]configuredPlugin, 0, len(ids))
	var visit func(string) error
	visit = func(id string) error {
		if state[id] == 2 {
			return nil
		}
		if state[id] == 1 {
			return fmt.Errorf("config plugins.%s: cyclic capability dependency", id)
		}
		state[id] = 1
		plugin := plugins[id]
		for _, capability := range plugin.Requires() {
			provider, ok := providers[capability]
			if !ok {
				return fmt.Errorf("config plugins.%s: required capability %q is missing or disabled", id, capability)
			}
			if err := visit(provider); err != nil {
				return err
			}
		}
		state[id] = 2
		ordered = append(ordered, plugin)
		return nil
	}
	for _, id := range ids {
		if active[id] {
			if err := visit(id); err != nil {
				return nil, err
			}
		}
	}
	return ordered, nil
}

// buildPluginOptions resolves trusted host plugins once at startup. Core.Use
// applies their capabilities afresh for each run.
func buildPluginOptions(raw pluginSettings, getenv func(string) string) (pluginOptions, error) {
	plugins, err := decodePluginConfigs(raw)
	if err != nil {
		return pluginOptions{}, err
	}
	build := &pluginBuildContext{getenv: getenv}
	for _, plugin := range plugins {
		if err := plugin.Build(build); err != nil {
			return pluginOptions{}, fmt.Errorf("config plugins.%s: %w", plugin.ID(), err)
		}
	}
	return pluginOptions{hostPlugins: build.hostPlugins, manifests: build.manifests}, nil
}
