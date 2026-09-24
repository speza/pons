package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadSettingsMerge(t *testing.T) {
	home, ws := t.TempDir(), t.TempDir()
	write(t, filepath.Join(home, ".pons", "config.json"),
		`{"provider":"codex","max_turns":9,"bash_timeout":0,"workspace_root":"/projects"}`)
	write(t, filepath.Join(ws, ".pons.json"), `{"max_turns":3,"model":"m2"}`)

	s, err := loadSettings(home, ws)
	if err != nil {
		t.Fatal(err)
	}
	if s.Provider == nil || *s.Provider != "codex" {
		t.Fatalf("provider: %v", s.Provider)
	}
	if s.WorkspaceRoot == nil || *s.WorkspaceRoot != "/projects" {
		t.Fatalf("workspace root: %v", s.WorkspaceRoot)
	}
	if s.MaxTurns == nil || *s.MaxTurns != 3 {
		t.Fatalf("project should win: %v", s.MaxTurns)
	}
	if s.BashTimeout == nil || *s.BashTimeout != 0 {
		t.Fatalf("zero is meaningful: %v", s.BashTimeout)
	}
	if s.Model == nil || *s.Model != "m2" {
		t.Fatalf("model: %v", s.Model)
	}
}

func TestLoadSettingsMissingFiles(t *testing.T) {
	s, err := loadSettings(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if s.Provider != nil || s.MaxTurns != nil {
		t.Fatalf("absent files = empty settings: %+v", s)
	}
}

func TestLoadSettingsStrict(t *testing.T) {
	ws := t.TempDir()

	write(t, filepath.Join(ws, ".pons.json"), `{"nope":1}`)
	if _, err := loadSettings(t.TempDir(), ws); err == nil {
		t.Fatal("unknown key should be rejected")
	}

	write(t, filepath.Join(ws, ".pons.json"), `{"max_turns":1} trailing`)
	if _, err := loadSettings(t.TempDir(), ws); err == nil {
		t.Fatal("trailing data should be rejected")
	}

	write(t, filepath.Join(ws, ".pons.json"), `{"max_turns":"no"}`)
	if _, err := loadSettings(t.TempDir(), ws); err == nil {
		t.Fatal("wrong type should be rejected")
	}
}

func TestLoadSettingsE2BAndGitHubApp(t *testing.T) {
	home, ws := t.TempDir(), t.TempDir()
	write(t, filepath.Join(home, ".pons", "config.json"), `{
		"environment": {
			"sandbox":"e2b",
			"e2b":{"template":"pons-hands","api_key":"test-key","idle_timeout":"5m"},
			"github_app":{"app_id":123,"installation_id":456,"private_key":"/secure/github.pem"}
		}
	}`)
	write(t, filepath.Join(ws, ".pons.json"), `{"environment":{"e2b":{"idle_timeout":"2m"}}}`)
	s, err := loadSettings(home, ws)
	if err != nil {
		t.Fatal(err)
	}
	if s.Environment == nil || *s.Environment.Sandbox != "e2b" || *s.Environment.E2B.Template != "pons-hands" ||
		*s.Environment.E2B.APIKey != "test-key" || *s.Environment.GitHubApp.AppID != 123 ||
		*s.Environment.GitHubApp.InstallationID != 456 || *s.Environment.GitHubApp.PrivateKey != "/secure/github.pem" ||
		*s.Environment.E2B.IdleTimeout != "2m" {
		t.Fatalf("E2B config = %+v", s)
	}
}

func TestLoadSettingsRejectsReadableE2BKey(t *testing.T) {
	home, ws := t.TempDir(), t.TempDir()
	path := filepath.Join(home, ".pons", "config.json")
	write(t, path, `{"environment":{"e2b":{"api_key":"test-key"}}}`)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadSettings(home, ws); err == nil {
		t.Fatal("expected insecure config permissions to be rejected")
	}
}

func TestLoadSettingsRejectsAgentBlock(t *testing.T) {
	ws := t.TempDir()
	write(t, filepath.Join(ws, ".pons.json"), `{"agent":{"name":"Ada"}}`)
	if _, err := loadSettings(t.TempDir(), ws); err == nil {
		t.Fatal("agent settings accepted in config; they belong in the agent directory")
	}
}

func TestLoadSettingsGlobalPlugins(t *testing.T) {
	home, ws := t.TempDir(), t.TempDir()
	write(t, filepath.Join(home, ".pons", "config.json"), `{
		"plugins":[
			{"id":"classifier/typesafe-jev","version":"1.0.0","enabled":true,"config":{"model":"jev-latest","timeout":"5s"}},
			{"id":"action_policy","version":"1.0.0","enabled":true,"config":{"classifier":"classifier/typesafe-jev","min_safe_confidence":0.95}},
			{"id":"external","version":"1.0.0","enabled":true,"config":{"manifests":["/opt/pons/example.json"]}}
		]
	}`)
	write(t, filepath.Join(ws, ".pons.json"), `{"max_turns":3}`)

	s, err := loadSettings(home, ws)
	if err != nil {
		t.Fatal(err)
	}
	plugins := s.Plugins
	if len(plugins) != 3 || plugins[0].ID != "classifier/typesafe-jev" ||
		plugins[1].ID != "action_policy" || plugins[2].ID != "external" {
		t.Fatalf("plugins: %+v", plugins)
	}
}

func TestLoadSettingsRejectsProjectPlugins(t *testing.T) {
	for _, key := range []string{"plugins", "action_policy"} {
		t.Run(key, func(t *testing.T) {
			ws := t.TempDir()
			write(t, filepath.Join(ws, ".pons.json"), `{"`+key+`":null}`)
			_, err := loadSettings(t.TempDir(), ws)
			if err == nil || (key == "plugins" && !strings.Contains(err.Error(), "allowed only")) {
				t.Fatalf("expected project config to reject %s, got %v", key, err)
			}
		})
	}
}

func TestLoadSettingsRejectsInvalidPlugins(t *testing.T) {
	tests := []string{
		`{"plugins":[{"id":"unknown","version":"1.0.0","enabled":true,"config":{}}]}`,
		`{"plugins":[{"id":"classifier/typesafe-jev","version":"1.0.0","config":{}}]}`,
		`{"plugins":[{"id":"action_policy","version":"1.0.0","enabled":true,"config":{}}]}`,
		`{"plugins":[{"id":"action_policy","version":"1.0.0","enabled":true,"config":{"classifier":"classifier/typesafe-jev"}}]}`,
		`{"plugins":[{"id":"classifier/typesafe-jev","version":"1.0.0","enabled":false,"config":{}},{"id":"action_policy","version":"1.0.0","enabled":true,"config":{"classifier":"classifier/typesafe-jev"}}]}`,
		`{"plugins":[{"id":"classifier/openai","version":"1.0.0","enabled":true,"config":{"timeout":"0s"}}]}`,
		`{"plugins":[{"id":"action_policy","version":"1.0.0","enabled":false,"config":{"min_safe_confidence":2}}]}`,
		`{"plugins":[{"id":"external","version":"1.0.0","enabled":true,"config":{"manifests":["relative.json"]}}]}`,
		`{"plugins":[{"id":"classifier/typesafe-jev","version":"1.0.0","enabled":true,"config":{"unexpected":1}}]}`,
		`{"plugins":[{"id":"classifier/typesafe-jev","version":"1.0.0","enabled":true,"config":{}},{"id":"classifier/typesafe-jev","version":"1.0.0","enabled":false,"config":{}}]}`,
		`{"plugins":[{"id":"classifier/typesafe-jev","version":"2.0.0","enabled":true,"config":{}}]}`,
		`{"plugins":[{"id":"classifier/typesafe-jev","enabled":true,"config":{}}]}`,
		`{"plugins":[{"id":"classifier/typesafe-jev","version":"1.0.0","enabled":true}]}`,
		`{"plugins":[{"id":"classifier/typesafe-jev","version":"1.0.0","enabled":true,"config":{},"typo":1}]}`,
		`{"plugins":null}`,
		`{"plugins":{"typesafe":{"enabled":true}}}`,
		`{"action_policy":{"enabled":true}}`,
	}
	for _, input := range tests {
		t.Run(input, func(t *testing.T) {
			home := t.TempDir()
			write(t, filepath.Join(home, ".pons", "config.json"), input)
			if _, err := loadSettings(home, ""); err == nil {
				t.Fatal("expected invalid config to fail")
			}
		})
	}
}
