package seatbelt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samperrin/pons/environment"
)

func TestProfileDefaultsToNoNetwork(t *testing.T) {
	profile, err := seatbeltProfile(`/tmp/work "quoted"`, "/tmp/scratch", "/tmp/pons-hands", nil, environment.NetworkDisabled)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"(deny default)", "process-exec", "vnode-type REGULAR-FILE", `work \"quoted\"`, `(literal "/")`, "/tmp/scratch", "/tmp/pons-hands"} {
		if !strings.Contains(profile, want) {
			t.Fatalf("profile does not contain %q:\n%s", want, profile)
		}
	}
	if strings.Contains(profile, "allow network") {
		t.Fatalf("network unexpectedly enabled:\n%s", profile)
	}
	enabled, err := seatbeltProfile("/tmp/work", "/tmp/scratch", "/tmp/pons-hands", nil, environment.NetworkEnabled)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(enabled, "(allow network*)") {
		t.Fatalf("network policy missing:\n%s", enabled)
	}
}

func TestProfileAllowsExplicitReadOnlyPluginFiles(t *testing.T) {
	dir := t.TempDir()
	manifest := filepath.Join(dir, "plugin.json")
	executable := filepath.Join(dir, "plugin")
	if err := os.WriteFile(manifest, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	profile, err := seatbeltProfile("/tmp/work", "/tmp/scratch", "/tmp/pons-hands", []string{manifest, executable}, environment.NetworkDisabled)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{manifest, executable} {
		quoted, err := seatbeltString(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(profile, "(allow file-read* (literal "+quoted+"))") {
			t.Fatalf("profile does not allow %q:\n%s", path, profile)
		}
	}
}

func TestCleanEnvironmentIsExplicit(t *testing.T) {
	env, err := cleanEnvironment([]string{"LANG=C", "PATH=/custom/bin"}, "/scratch")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"PATH=/custom/bin", "TMPDIR=/scratch", "LANG=C"}
	if strings.Join(env, "|") != strings.Join(want, "|") {
		t.Fatalf("env = %v, want %v", env, want)
	}
	if _, err := cleanEnvironment([]string{"TMPDIR=/host/tmp"}, "/scratch"); err == nil {
		t.Fatal("provider-owned TMPDIR override succeeded")
	}
}
