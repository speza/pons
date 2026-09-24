package main

import (
	"os"
	"path/filepath"
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
