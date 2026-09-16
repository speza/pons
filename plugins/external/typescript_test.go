package external_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/samperrin/pons/plugins/external"
	"github.com/samperrin/pons/protocol"
)

// This is a black-box smoke test of the TypeScript SDK. It remains optional
// on minimal Go-only builders, but runs wherever Bun is available.
func TestTypeScriptSDKSmoke(t *testing.T) {
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Skip("bun is not installed")
	}
	example, err := filepath.Abs(filepath.Join("..", "..", "examples", "external-echo-ts", "index.ts"))
	if err != nil {
		t.Fatal(err)
	}
	manifestData, err := json.Marshal(map[string]any{
		"manifest_version": 1,
		"name":             "example.echo.ts",
		"entrypoint":       bun,
		"args":             []string{"run", example},
		"runtime_protocol": 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(t.TempDir(), "plugin.json")
	if err := os.WriteFile(manifest, manifestData, 0o600); err != nil {
		t.Fatal(err)
	}
	plugin, err := external.NewHands(manifest, external.HostConfig{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = plugin.Close() })
	if err := plugin.Host().Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	result, err := plugin.Host().Execute(context.Background(), protocol.Action{
		ID: "ts-smoke", Kind: "echo_text_ts",
		Args: protocol.MustArgsJSON(map[string]any{"text": "hello", "loud": true, "repeat": 2}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK || result.Output != "HELLOHELLO" {
		t.Fatalf("TypeScript tool result: %+v", result)
	}
}
