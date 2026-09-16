package external_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samperrin/pons/plugins/external"
	"github.com/samperrin/pons/protocol"
)

func scriptManifest(t *testing.T, body string, limits external.Limits) (*external.Plugin, error) {
	t.Helper()
	return scriptManifestConfig(t, body, external.HostConfig{Limits: limits})
}

func scriptManifestConfig(t *testing.T, body string, cfg external.HostConfig) (*external.Plugin, error) {
	t.Helper()
	dir := t.TempDir()
	exe := filepath.Join(dir, "plugin.sh")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(dir, "plugin.json")
	contents := `{"manifest_version":1,"name":"script.test","entrypoint":"./plugin.sh","runtime_protocol":1}`
	if err := os.WriteFile(manifest, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	return external.NewHands(manifest, cfg)
}

func TestBlockedProtocolWriteHonorsCallTimeout(t *testing.T) {
	body := `read request
printf '%s\n' '{"jsonrpc":"2.0","id":"pons-1","result":{"plugin":{"name":"script.test","version":"1.0.0"},"capabilities":[{"type":"tool_provider","version":1,"configuration":{"max_concurrency":1,"tools":[{"kind":"blocked","description":"block","input_schema":{"type":"object","properties":{"data":{"type":"string"}},"additionalProperties":false}}]}}]}}'
while :; do :; done`
	plugin, err := scriptManifestConfig(t, body, external.HostConfig{
		CallTimeout:     50 * time.Millisecond,
		ShutdownTimeout: 50 * time.Millisecond,
		CancelGrace:     50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer plugin.Close()
	if err := plugin.Host().Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	result, err := plugin.Host().Execute(context.Background(), protocol.Action{
		ID: "blocked", Kind: "blocked",
		Args: protocol.MustArgsJSON(map[string]string{"data": strings.Repeat("x", 512<<10)}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.OK || !strings.Contains(result.Error, "deadline exceeded") {
		t.Fatalf("blocked write result: %+v", result)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("blocked write ignored call timeout: %s", elapsed)
	}
}

func TestCloseReleasesPendingExecute(t *testing.T) {
	body := `read request
printf '%s\n' '{"jsonrpc":"2.0","id":"pons-1","result":{"plugin":{"name":"script.test","version":"1.0.0"},"capabilities":[{"type":"tool_provider","version":1,"configuration":{"max_concurrency":1,"tools":[{"kind":"pending","description":"pending","input_schema":{"type":"object","properties":{},"additionalProperties":false}}]}}]}}'
while :; do :; done`
	plugin, err := scriptManifestConfig(t, body, external.HostConfig{
		CallTimeout: 10 * time.Second, ShutdownTimeout: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := plugin.Host().Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	done := make(chan protocol.ToolResult, 1)
	go func() {
		result, _ := plugin.Host().Execute(context.Background(), protocol.Action{
			ID: "pending", Kind: "pending", Args: protocol.MustArgsJSON(map[string]any{}),
		})
		done <- result
	}()
	time.Sleep(20 * time.Millisecond)
	_ = plugin.Close()
	select {
	case result := <-done:
		if result.OK || !strings.Contains(result.Error, "shutdown") {
			t.Fatalf("pending result: %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("pending Execute was not released by Close")
	}
}

func TestMalformedAndOversizedFramesFailStartup(t *testing.T) {
	malformed, err := scriptManifest(t, `printf 'not-json\n'`, external.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if err := malformed.Host().Start(context.Background()); err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("malformed frame should fail startup: %v", err)
	}
	_ = malformed.Close()

	oversized, err := scriptManifest(t, `awk 'BEGIN { for (i = 0; i < 2048; i++) printf "x"; printf "\n" }'`, external.Limits{MaxFrameBytes: 128})
	if err != nil {
		t.Fatal(err)
	}
	if err := oversized.Host().Start(context.Background()); err == nil || !strings.Contains(err.Error(), "frame") {
		t.Fatalf("oversized frame should fail startup: %v", err)
	}
	_ = oversized.Close()
}
