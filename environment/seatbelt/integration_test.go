package seatbelt

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/samperrin/pons/environment"
	"github.com/samperrin/pons/internal/toolhost"
	"github.com/samperrin/pons/protocol"
)

// TestMain doubles as a pons-hands process for the opt-in Seatbelt integration
// test. Intercepting before the testing harness keeps stdout protocol-only.
func TestMain(m *testing.M) {
	if os.Getenv("PONS_HANDS_TEST_HELPER") == "1" {
		workspace := argumentValue(os.Args[1:], "--workspace")
		host, err := toolhost.New(toolhost.Config{
			Workspace:   workspace,
			ReadWrite:   argumentValues(os.Args[1:], "--read-write"),
			BashTimeout: 10,
		})
		if err == nil {
			err = host.Serve(context.Background(), os.Stdin, os.Stdout)
		}
		if closeErr := hostClose(host); err == nil {
			err = closeErr
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func hostClose(host *toolhost.Host) error {
	if host == nil {
		return nil
	}
	return host.Close()
}

func argumentValue(args []string, name string) string {
	for i := range len(args) - 1 {
		if args[i] == name {
			return args[i+1]
		}
	}
	return ""
}

func argumentValues(args []string, name string) []string {
	var values []string
	for i := range len(args) - 1 {
		if args[i] == name {
			values = append(values, args[i+1])
		}
	}
	return values
}

func TestSeatbeltIntegration(t *testing.T) {
	if os.Getenv("PONS_SEATBELT_TEST") != "1" {
		t.Skip("set PONS_SEATBELT_TEST=1 to run the macOS Seatbelt integration test")
	}
	workspace, err := os.MkdirTemp("/private/tmp", "pons-seatbelt-integration-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(workspace) })
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	helper := workspace + "/pons-hands-test"
	binary, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(helper, binary, 0o755); err != nil {
		t.Fatal(err)
	}
	session, err := (Provider{}).Start(context.Background(), environment.Spec{
		WorkspacePath: workspace,
		Command:       []string{helper},
		Environment:   []string{"PONS_HANDS_TEST_HELPER=1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })

	result, err := session.Execute(context.Background(), protocol.Action{
		ID:   "write",
		Kind: "write_file",
		Args: protocol.MustArgsJSON(map[string]string{"path": "sandbox.txt", "content": "inside"}),
	})
	if err != nil || !result.OK {
		t.Fatalf("sandboxed write = %+v, err = %v", result, err)
	}
	data, err := os.ReadFile(workspace + "/sandbox.txt")
	if err != nil || string(data) != "inside" {
		t.Fatalf("workspace file = %q, err = %v", data, err)
	}

	outside, err := session.Execute(context.Background(), protocol.Action{
		ID:   "outside",
		Kind: "bash",
		Args: protocol.MustArgsJSON(map[string]any{"command": "cat /etc/passwd", "timeout": 5}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if outside.ExitCode == 0 || !strings.Contains(outside.Output, "Operation not permitted") {
		t.Fatalf("outside read was not denied: %+v", outside)
	}
}

// TestSeatbeltMemoryGrant proves the plan's phase 2 boundary: hands may write
// the agent's memory directory, but never its siblings PERSONA.md,
// agent.json, and revisions.
func TestSeatbeltMemoryGrant(t *testing.T) {
	if os.Getenv("PONS_SEATBELT_TEST") != "1" {
		t.Skip("set PONS_SEATBELT_TEST=1 to run the macOS Seatbelt integration test")
	}
	root, err := os.MkdirTemp("/private/tmp", "pons-seatbelt-memory-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	workspace := root + "/workspace"
	agentDir := root + "/state/agents/default"
	memoryDir := agentDir + "/memory"
	for _, dir := range []string{workspace, agentDir + "/revisions", memoryDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	protected := []string{agentDir + "/PERSONA.md", agentDir + "/agent.json", agentDir + "/revisions/r.json"}
	for _, path := range protected {
		if err := os.WriteFile(path, []byte("owner"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	helper := root + "/pons-hands-test"
	if err := os.WriteFile(helper, binary, 0o755); err != nil {
		t.Fatal(err)
	}
	session, err := (Provider{}).Start(context.Background(), environment.Spec{
		WorkspacePath: workspace,
		ReadWrite:     []string{memoryDir},
		Command:       []string{helper},
		Environment:   []string{"PONS_HANDS_TEST_HELPER=1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	execute := func(kind string, args any) protocol.ToolResult {
		t.Helper()
		result, err := session.Execute(context.Background(), protocol.Action{
			ID: kind, Kind: protocol.ActionKind(kind), Args: protocol.MustArgsJSON(args),
		})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}

	if result := execute("write_file", map[string]string{"path": memoryDir + "/MEMORY.md", "content": "- note"}); !result.OK {
		t.Fatalf("memory write = %+v", result)
	}
	if result := execute("bash", map[string]any{"command": "echo more >> " + memoryDir + "/MEMORY.md", "timeout": 5}); result.ExitCode != 0 {
		t.Fatalf("memory append = %+v", result)
	}
	if data, err := os.ReadFile(memoryDir + "/MEMORY.md"); err != nil || string(data) != "- notemore\n" {
		t.Fatalf("memory copy = %q, %v", data, err)
	}
	for _, path := range protected {
		if result := execute("write_file", map[string]string{"path": path, "content": "agent"}); result.OK {
			t.Fatalf("write_file reached %s", path)
		}
		if result := execute("bash", map[string]any{"command": "echo agent > " + path, "timeout": 5}); result.ExitCode == 0 {
			t.Fatalf("bash reached %s: %+v", path, result)
		}
		if data, err := os.ReadFile(path); err != nil || string(data) != "owner" {
			t.Fatalf("%s changed to %q, %v", path, data, err)
		}
	}
}
