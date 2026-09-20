package environment

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samperrin/pons"
	"github.com/samperrin/pons/plugins/external"
	"github.com/samperrin/pons/protocol"
)

type fakeSession struct{}

func (fakeSession) Catalog() []external.ToolDescription {
	return []external.ToolDescription{{
		Kind: "remote", Description: "remote tool",
		InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
	}}
}
func (fakeSession) Metadata() Metadata { return Metadata{Provider: "fake"} }
func (fakeSession) Close() error       { return nil }
func (fakeSession) Execute(_ context.Context, action protocol.Action) (protocol.ToolResult, error) {
	return protocol.ToolResult{OK: true, Output: "ok", ActionID: action.ID, Kind: string(action.Kind)}, nil
}

func TestProxyRegistersSessionCatalog(t *testing.T) {
	core := pons.New()
	if err := core.Use(Proxy(fakeSession{})); err != nil {
		t.Fatal(err)
	}
	specs := core.ToolSpecs()
	if len(specs) != 1 || specs[0].Kind != "remote" || specs[0].Source.Executable != "fake" {
		t.Fatalf("specs = %+v", specs)
	}
	result, err := core.Execute(context.Background(), protocol.Action{ID: "one", Kind: "remote", Args: json.RawMessage(`{}`)})
	if err != nil || !result.OK || result.Output != "ok" {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
}

func TestProxyPreflightsConflicts(t *testing.T) {
	core := pons.New()
	if err := core.AddTool("remote", pons.ToolDef{Handler: fakeSession{}.Execute}); err != nil {
		t.Fatal(err)
	}
	if err := core.Use(Proxy(fakeSession{})); err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("conflict error = %v", err)
	}
}

func TestSeatbeltProfileDefaultsToNoNetwork(t *testing.T) {
	profile, err := seatbeltProfile(`/tmp/work "quoted"`, "/tmp/scratch", "/tmp/pons-hands", nil, NetworkDisabled)
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
	enabled, err := seatbeltProfile("/tmp/work", "/tmp/scratch", "/tmp/pons-hands", nil, NetworkEnabled)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(enabled, "(allow network*)") {
		t.Fatalf("network policy missing:\n%s", enabled)
	}
}

func TestSeatbeltProfileAllowsExplicitReadOnlyPluginFiles(t *testing.T) {
	dir := t.TempDir()
	manifest := filepath.Join(dir, "plugin.json")
	executable := filepath.Join(dir, "plugin")
	if err := os.WriteFile(manifest, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	profile, err := seatbeltProfile("/tmp/work", "/tmp/scratch", "/tmp/pons-hands", []string{manifest, executable}, NetworkDisabled)
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
