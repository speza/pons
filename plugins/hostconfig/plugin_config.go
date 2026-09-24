package hostconfig

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"github.com/samperrin/pons"
	"github.com/samperrin/pons/plugins/brain/llm"
)

// Entry separates shared plugin metadata from implementation settings.
type Entry struct {
	ID      string          `json:"id"`
	Version string          `json:"version"`
	Enabled *bool           `json:"enabled"`
	Config  json.RawMessage `json:"config"`
}

// Settings is the versioned plugin list accepted by the host configuration.
type Settings []Entry

type pluginEntry = Entry
type pluginSettings = Settings

func (settings *Settings) UnmarshalJSON(data []byte) error {
	if trimmed := bytes.TrimSpace(data); len(trimmed) == 0 || trimmed[0] != '[' {
		return fmt.Errorf("config plugins must be a list")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var result []Entry
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
type ConfiguredPlugin interface {
	ID() string
	Provides() []string
	Requires() []string
	Build(*BuildContext) error
}

type Factory func(string, bool, json.RawMessage) (ConfiguredPlugin, error)

type pluginRegistration struct {
	version string
	decode  Factory
}

// Registry maps implementation IDs to trusted in-process plugin factories.
// A custom host can register additional factories before loading settings.
type Registry struct {
	entries map[string]pluginRegistration
}

func NewRegistry() *Registry {
	return &Registry{entries: map[string]pluginRegistration{
		"classifier/typesafe-jev": {version: "1.0.0", decode: decodeTypeSafePlugin},
		"classifier/openai":       {version: "1.0.0", decode: decodeOpenAIPlugin},
		"classifier/codex":        {version: "1.0.0", decode: decodeCodexPlugin},
		"action_policy":           {version: "1.0.0", decode: decodeActionPolicyPlugin},
		"external":                {version: "1.0.0", decode: decodeExternalPlugin},
	}}
}

func (r *Registry) Register(id, version string, factory Factory) error {
	if id == "" || version == "" || factory == nil {
		return fmt.Errorf("plugin registration requires ID, version, and factory")
	}
	if _, exists := r.entries[id]; exists {
		return fmt.Errorf("plugin %q is already registered", id)
	}
	r.entries[id] = pluginRegistration{version: version, decode: factory}
	return nil
}

type BuildContext struct {
	getenv    func(string) string
	providers map[string]llm.Fallback
	plugins   []BuiltPlugin
}

func (b *BuildContext) Getenv(name string) string { return b.getenv(name) }
func (b *BuildContext) Provider(id string) (llm.Fallback, bool) {
	provider, ok := b.providers[id]
	return provider, ok
}
func (b *BuildContext) AddHostPlugin(plugin pons.Plugin) {
	b.plugins = append(b.plugins, BuiltPlugin{Host: plugin})
}
func (b *BuildContext) AddManifest(path string) {
	b.plugins = append(b.plugins, BuiltPlugin{Manifest: path})
}
func (b *BuildContext) AddManifestConfig(path string, config json.RawMessage) {
	b.plugins = append(b.plugins, BuiltPlugin{Manifest: path, Config: append(json.RawMessage(nil), config...)})
}

// BuiltPlugin is one enabled registration in dependency-resolved config order.
// Exactly one of Host and Manifest is populated.
type BuiltPlugin struct {
	Host     pons.Plugin
	Manifest string
	Config   json.RawMessage
}

type Options struct {
	Plugins []BuiltPlugin
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

// DecodeObject applies the same strict JSON decoding used by built-in plugins.
func DecodeObject[T any](id string, raw json.RawMessage) (T, error) {
	return decodePluginObject[T](id, raw)
}

// decodePluginConfigs validates plugin-owned config and resolves capability
// dependencies before any provider keys or executable manifests are used.
func (r *Registry) decodePluginConfigs(raw Settings) ([]ConfiguredPlugin, error) {
	plugins := make(map[string]ConfiguredPlugin, len(raw))
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
		registration, ok := r.entries[id]
		if !ok {
			var err error
			registration, err = installedRegistration(id)
			if err != nil {
				return nil, err
			}
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
	ordered := make([]ConfiguredPlugin, 0, len(ids))
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

// Decode validates plugin entries and their capability dependencies.
func Decode(raw Settings) error {
	_, err := NewRegistry().decodePluginConfigs(raw)
	return err
}

func (r *Registry) Decode(raw Settings) error {
	_, err := r.decodePluginConfigs(raw)
	return err
}

// Build resolves plugin registrations once at startup in dependency order.
// Runtime applies their capabilities to each new Core in that order.
func (r *Registry) Build(raw Settings, getenv func(string) string, providers map[string]llm.Fallback) (Options, error) {
	plugins, err := r.decodePluginConfigs(raw)
	if err != nil {
		return Options{}, err
	}
	build := &BuildContext{getenv: getenv, providers: providers}
	for _, plugin := range plugins {
		if err := plugin.Build(build); err != nil {
			return Options{}, fmt.Errorf("config plugins.%s: %w", plugin.ID(), err)
		}
	}
	return Options{Plugins: build.plugins}, nil
}

// Build resolves enabled plugins against the trusted host's providers and environment.
func Build(raw Settings, getenv func(string) string, providers map[string]llm.Fallback) (Options, error) {
	return NewRegistry().Build(raw, getenv, providers)
}

func buildPluginOptions(raw Settings, getenv func(string) string, providers map[string]llm.Fallback) (Options, error) {
	return Build(raw, getenv, providers)
}
