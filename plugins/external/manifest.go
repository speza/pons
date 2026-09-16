// Package external implements pons's language-neutral external plugin
// runtime.  It is intentionally a hands-side adapter: the v1 capability it
// exposes to Core is tool_provider/v1, and the child process receives no
// brain, history, or Core handle.
package external

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	// RuntimeProtocol is the common JSON-RPC/NDJSON runtime major version.
	RuntimeProtocol = 1
	// ManifestVersion is the installation manifest schema version.
	ManifestVersion = 1
)

// Placement is the deployment side in which an external plugin is launched.
// Version 1 only has a normative hands capability.
type Placement string

const PlacementHands Placement = "hands"

// Manifest describes one explicitly activated external executable.  The
// unexported fields are populated by LoadManifest and are not part of the
// on-disk schema.
type Manifest struct {
	ManifestVersion int      `json:"manifest_version"`
	Name            string   `json:"name"`
	Entrypoint      string   `json:"entrypoint"`
	Args            []string `json:"args,omitempty"`
	RuntimeProtocol int      `json:"runtime_protocol"`

	path string
	dir  string
}

var pluginNamePattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]*[a-z0-9])?)*$`)

// LoadManifest reads and validates a UTF-8 JSON manifest.  Relative
// entrypoints are resolved relative to the manifest directory, never the
// caller's current directory.  It does not execute anything.
func LoadManifest(path string) (Manifest, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("external: manifest path: %w", err)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return Manifest{}, fmt.Errorf("external: read manifest %q: %w", path, err)
	}
	if !utf8.Valid(data) {
		return Manifest{}, fmt.Errorf("external: manifest %q is not valid UTF-8", path)
	}
	var m Manifest
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("external: decode manifest %q: %w", path, err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return Manifest{}, fmt.Errorf("external: manifest %q contains trailing JSON", path)
		}
		return Manifest{}, fmt.Errorf("external: manifest %q has trailing data: %w", path, err)
	}
	m.path = abs
	m.dir = filepath.Dir(abs)
	if err := m.Validate(); err != nil {
		return Manifest{}, fmt.Errorf("external: manifest %q: %w", path, err)
	}
	entrypoint := m.ResolvedEntrypoint()
	info, err := os.Stat(entrypoint)
	if err != nil {
		return Manifest{}, fmt.Errorf("external: manifest %q entrypoint: %w", path, err)
	}
	if info.IsDir() {
		return Manifest{}, fmt.Errorf("external: manifest %q entrypoint %q is a directory", path, entrypoint)
	}
	if info.Mode()&0o111 == 0 {
		return Manifest{}, fmt.Errorf("external: manifest %q entrypoint %q is not executable", path, entrypoint)
	}
	return m, nil
}

// Validate checks the manifest fields independent of the filesystem.
func (m Manifest) Validate() error {
	if m.ManifestVersion != ManifestVersion {
		return fmt.Errorf("unsupported manifest_version %d (want %d)", m.ManifestVersion, ManifestVersion)
	}
	if !pluginNamePattern.MatchString(m.Name) {
		return errors.New("name must be a lower-case reverse-DNS-style identifier (for example acme.github)")
	}
	if strings.TrimSpace(m.Entrypoint) == "" {
		return errors.New("entrypoint is required")
	}
	if m.RuntimeProtocol != RuntimeProtocol {
		return fmt.Errorf("unsupported runtime_protocol %d (want %d)", m.RuntimeProtocol, RuntimeProtocol)
	}
	return nil
}

// Path returns the absolute manifest path when loaded from disk.
func (m Manifest) Path() string { return m.path }

// Dir returns the directory from which relative entrypoints and the child
// working directory are resolved.
func (m Manifest) Dir() string {
	if m.dir != "" {
		return m.dir
	}
	if m.path != "" {
		return filepath.Dir(m.path)
	}
	return ""
}

// ResolvedEntrypoint returns the direct executable path.  A caller-supplied
// Manifest without a path resolves relative paths against its current cwd;
// LoadManifest always resolves against the manifest directory.
func (m Manifest) ResolvedEntrypoint() string {
	if filepath.IsAbs(m.Entrypoint) {
		return m.Entrypoint
	}
	return filepath.Join(m.Dir(), m.Entrypoint)
}
