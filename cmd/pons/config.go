// Durable CLI configuration: ~/.pons/config.json (global defaults) and
// .pons.json in the workspace (project overrides), with command-line flags
// winning over both. Same decoding rules as external plugin manifests:
// strict, stdlib-only, fail loudly on malformed files.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"unicode/utf8"
)

// fallbackSettings is one fallback provider slot in a config file.
type fallbackSettings struct {
	Provider string `json:"provider"`
	Model    string `json:"model,omitempty"`
	BaseURL  string `json:"base_url,omitempty"`
	APIKey   string `json:"api_key,omitempty"`
}

// providerSettings is one named provider slot in a config file. Multiple
// entries of the same provider type are allowed (e.g. two ChatGPT
// subscriptions): each entry's id names its auth-store entry, so
// `--login -as <id>` is all the wiring a credential needs. The auth-store
// file itself is pons-wide (~/.pons/auth.json; -auth-file relocates it).
type providerSettings struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
	Model    string `json:"model,omitempty"`
	BaseURL  string `json:"base_url,omitempty"`
	APIKey   string `json:"api_key,omitempty"`
}

type e2bSettings struct {
	Template    *string `json:"template,omitempty"`
	APIKey      *string `json:"api_key,omitempty"`
	IdleTimeout *string `json:"idle_timeout,omitempty"`
}

type githubAppSettings struct {
	AppID          *int64  `json:"app_id,omitempty"`
	InstallationID *int64  `json:"installation_id,omitempty"`
	PrivateKey     *string `json:"private_key,omitempty"`
}

type environmentSettings struct {
	Sandbox   *string            `json:"sandbox,omitempty"`
	E2B       *e2bSettings       `json:"e2b,omitempty"`
	GitHubApp *githubAppSettings `json:"github_app,omitempty"`
}

// settings carries only the durable knobs. Pointer fields distinguish
// "absent" from zero, because zero is meaningful (e.g. bash_timeout 0 =
// no default timeout, fs_read_bytes negative = unlimited).
type settings struct {
	Provider             *string              `json:"provider,omitempty"`
	Model                *string              `json:"model,omitempty"`
	BaseURL              *string              `json:"base_url,omitempty"`
	StateDir             *string              `json:"state_dir,omitempty"`
	Environment          *environmentSettings `json:"environment,omitempty"`
	MaxTurns             *int                 `json:"max_turns,omitempty"`
	CompactChars         *int                 `json:"compact_chars,omitempty"`
	FsReadBytes          *int                 `json:"fs_read_bytes,omitempty"`
	BashTimeout          *int                 `json:"bash_timeout,omitempty"`
	BashMaxLines         *int                 `json:"bash_max_lines,omitempty"`
	BashMaxBytes         *int                 `json:"bash_max_bytes,omitempty"`
	PluginMaxResultBytes *int                 `json:"plugin_max_result_bytes,omitempty"`
	Fallbacks            []fallbackSettings   `json:"fallbacks,omitempty"`
	DefaultProviderID    *string              `json:"default_provider_id,omitempty"`
	Providers            []providerSettings   `json:"providers,omitempty"`
}

// orderedProviders validates the providers list and returns it
// default-first (the failover chain order). Strict and loud: missing ids,
// duplicate ids, an unknown default_provider_id, or several providers
// without one are all errors.
func (s settings) orderedProviders() ([]providerSettings, error) {
	if len(s.Providers) == 0 {
		return nil, nil
	}
	ids := make(map[string]int, len(s.Providers))
	for i, p := range s.Providers {
		if p.ID == "" {
			return nil, fmt.Errorf("providers[%d]: id is required", i)
		}
		if p.Provider == "" {
			return nil, fmt.Errorf("provider %q: provider is required", p.ID)
		}
		if _, dup := ids[p.ID]; dup {
			return nil, fmt.Errorf("duplicate provider id %q", p.ID)
		}
		ids[p.ID] = i
	}
	def := 0
	if s.DefaultProviderID != nil && *s.DefaultProviderID != "" {
		idx, ok := ids[*s.DefaultProviderID]
		if !ok {
			return nil, fmt.Errorf("default_provider_id %q does not match any provider", *s.DefaultProviderID)
		}
		def = idx
	} else if len(s.Providers) > 1 {
		return nil, fmt.Errorf("%d providers declared but default_provider_id is not set", len(s.Providers))
	}
	ordered := []providerSettings{s.Providers[def]}
	for i, p := range s.Providers {
		if i != def {
			ordered = append(ordered, p)
		}
	}
	return ordered, nil
}

// validate rejects ambiguous configs: the flat provider keys and the
// providers list are exclusive, and default_provider_id needs the list.
func (s settings) validate() error {
	if len(s.Providers) > 0 && (s.Provider != nil || s.Model != nil || s.BaseURL != nil) {
		return fmt.Errorf("config cannot set both \"providers\" and the flat provider/model/base_url keys")
	}
	if s.DefaultProviderID != nil && len(s.Providers) == 0 {
		return fmt.Errorf("config sets default_provider_id but no providers")
	}
	return nil
}

// loadSettings merges the global config and the workspace's .pons.json,
// project file winning per key. Missing files are fine; a malformed file
// is an error naming the file.
func loadSettings(home, workspace string) (settings, error) {
	var paths []string
	if home != "" {
		paths = append(paths, filepath.Join(home, ".pons", "config.json"))
	}
	paths = append(paths, filepath.Join(workspace, ".pons.json"))

	merged := map[string]json.RawMessage{}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return settings{}, fmt.Errorf("read config %q: %w", path, err)
		}
		m, err := decodeSettingsMap(data, path)
		if err != nil {
			return settings{}, err
		}
		if hasE2BAPIKey(m["environment"]) {
			info, err := os.Stat(path)
			if err != nil {
				return settings{}, fmt.Errorf("stat config %q: %w", path, err)
			}
			if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
				return settings{}, fmt.Errorf("config %q with environment.e2b.api_key must be a regular file readable only by its owner", path)
			}
		}
		if err := mergeSettingsMap(merged, m); err != nil {
			return settings{}, fmt.Errorf("merge config %q: %w", path, err)
		}
	}
	if len(merged) == 0 {
		return settings{}, nil
	}
	b, err := json.Marshal(merged)
	if err != nil {
		return settings{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var s settings
	if err := dec.Decode(&s); err != nil {
		return settings{}, fmt.Errorf("config: %w", err)
	}
	if err := s.validate(); err != nil {
		return settings{}, err
	}
	if _, err := s.orderedProviders(); err != nil {
		return settings{}, err
	}
	return s, nil
}

func hasE2BAPIKey(raw json.RawMessage) bool {
	var environment map[string]json.RawMessage
	if json.Unmarshal(raw, &environment) != nil {
		return false
	}
	var e2b map[string]json.RawMessage
	if json.Unmarshal(environment["e2b"], &e2b) != nil {
		return false
	}
	_, ok := e2b["api_key"]
	return ok
}

func mergeSettingsMap(dst, src map[string]json.RawMessage) error {
	for key, value := range src {
		previous, exists := dst[key]
		oldValue, newValue := bytes.TrimSpace(previous), bytes.TrimSpace(value)
		if !exists || len(oldValue) == 0 || len(newValue) == 0 || oldValue[0] != '{' || newValue[0] != '{' {
			dst[key] = value
			continue
		}
		var oldObject, newObject map[string]json.RawMessage
		if err := json.Unmarshal(previous, &oldObject); err != nil {
			return err
		}
		if err := json.Unmarshal(value, &newObject); err != nil {
			return err
		}
		if err := mergeSettingsMap(oldObject, newObject); err != nil {
			return err
		}
		merged, err := json.Marshal(oldObject)
		if err != nil {
			return err
		}
		dst[key] = merged
	}
	return nil
}

func decodeSettingsMap(data []byte, path string) (map[string]json.RawMessage, error) {
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("config %q is not valid UTF-8", path)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	var m map[string]json.RawMessage
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("config %q: %w", path, err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("config %q contains trailing JSON", path)
		}
		return nil, fmt.Errorf("config %q has trailing data: %w", path, err)
	}
	return m, nil
}
