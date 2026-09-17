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
		`{"provider":"codex","max_turns":9,"bash_timeout":0}`)
	write(t, filepath.Join(ws, ".pons.json"), `{"max_turns":3,"model":"m2"}`)

	s, err := loadSettings(home, ws)
	if err != nil {
		t.Fatal(err)
	}
	if s.Provider == nil || *s.Provider != "codex" {
		t.Fatalf("provider: %v", s.Provider)
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
