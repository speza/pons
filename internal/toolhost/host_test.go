package toolhost

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/samperrin/pons"
	"github.com/samperrin/pons/plugins/external"
	"github.com/samperrin/pons/protocol"
)

func TestHostCatalogAndExecution(t *testing.T) {
	workspace := t.TempDir()
	host, err := New(Config{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Close() })

	wantKinds := []string{"bash", "edit_file", "list_dir", "read_file", "write_file"}
	if len(host.server.Tools) != len(wantKinds) {
		t.Fatalf("tools = %d, want %d", len(host.server.Tools), len(wantKinds))
	}
	if !host.server.Unbounded {
		t.Fatal("default tool host unexpectedly serializes tool calls")
	}
	for i, want := range wantKinds {
		tool := host.server.Tools[i]
		if tool.Kind != want {
			t.Fatalf("tool %d = %q, want %q", i, tool.Kind, want)
		}
		if len(tool.InputSchema) == 0 {
			t.Fatalf("tool %q has no schema", tool.Kind)
		}
	}

	write := host.server.Tools[4]
	result, err := write.Handler(context.Background(), protocol.Action{
		ID:   "write-1",
		Kind: "write_file",
		Args: protocol.MustArgsJSON(map[string]string{"path": "hello.txt", "content": "hello"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK || result.ActionID != "write-1" || result.Kind != "write_file" {
		t.Fatalf("result = %+v", result)
	}
	data, err := os.ReadFile(filepath.Join(workspace, "hello.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello" {
		t.Fatalf("file = %q", data)
	}
}

func TestToolSchemaRejectsInvalidLegacyType(t *testing.T) {
	raw, err := toolSchema(pons.ToolSpec{Params: []pons.ToolParam{{Name: "value", Type: "invalid"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := external.ValidateToolSchema(json.RawMessage(raw)); err == nil {
		t.Fatal("invalid parameter type passed schema validation")
	}
}

// The tool host runs inside an execution environment, which alone decides
// what is reachable; file tools accept absolute paths outside the workspace
// while relative paths stay in it.
func TestHostFileToolsLeaveAbsolutePathsToTheEnvironment(t *testing.T) {
	workspace, memory := t.TempDir(), t.TempDir()
	host, err := New(Config{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Close() })

	write := host.server.Tools[4]
	for _, path := range []string{filepath.Join(memory, "MEMORY.md"), "MEMORY.md"} {
		result, err := write.Handler(context.Background(), protocol.Action{
			ID: "write", Kind: "write_file",
			Args: protocol.MustArgsJSON(map[string]string{"path": path, "content": "- tea"}),
		})
		if err != nil || !result.OK {
			t.Fatalf("write %s = %+v, %v", path, result, err)
		}
	}
	for _, path := range []string{filepath.Join(memory, "MEMORY.md"), filepath.Join(workspace, "MEMORY.md")} {
		if data, err := os.ReadFile(path); err != nil || string(data) != "- tea" {
			t.Fatalf("%s = %q, %v", path, data, err)
		}
	}
}
