package seatbelt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samperrin/pons/environment"
)

func TestProfileDefaultsToNoNetwork(t *testing.T) {
	profile, err := seatbeltProfile(`/tmp/work "quoted"`, "/tmp/scratch", "/tmp/pons-hands", nil, nil, environment.NetworkDisabled)
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
	enabled, err := seatbeltProfile("/tmp/work", "/tmp/scratch", "/tmp/pons-hands", nil, nil, environment.NetworkEnabled)
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
	profile, err := seatbeltProfile("/tmp/work", "/tmp/scratch", "/tmp/pons-hands", []string{manifest, executable}, nil, environment.NetworkDisabled)
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

func TestProfileGrantsOnlyReadWriteDirectories(t *testing.T) {
	profile, err := seatbeltProfile("/tmp/work", "/tmp/scratch", "/tmp/pons-hands", nil,
		[]string{"/state/runs/r1/memory"}, environment.NetworkDisabled)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(profile, `(allow file-read* file-write* (subpath "/state/runs/r1/memory"))`) {
		t.Fatalf("memory copy is not granted:\n%s", profile)
	}
	if strings.Contains(profile, `(subpath "/state")`) || strings.Contains(profile, "agents") {
		t.Fatalf("grant widened beyond the memory copy:\n%s", profile)
	}
}

func TestValidateSpecKeepsReadWriteAlignedAndRejectsOverlap(t *testing.T) {
	workspace, memory := t.TempDir(), t.TempDir()
	command := filepath.Join(t.TempDir(), "pons-hands")
	if err := os.WriteFile(command, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	spec := environment.Spec{WorkspacePath: workspace, Command: []string{command}, ReadWrite: []string{memory, memory}}
	if valid, err := validateSpec(spec); err != nil || len(valid.readWrite) != 2 {
		t.Fatalf("read-write = %v, %v", valid.readWrite, err)
	}
	spec.ReadOnly = []string{memory}
	if _, err := validateSpec(spec); err == nil || !strings.Contains(err.Error(), "both read-only and read-write") {
		t.Fatalf("overlap error = %v", err)
	}
}
