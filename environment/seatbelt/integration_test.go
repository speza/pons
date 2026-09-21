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
		host, err := toolhost.New(toolhost.Config{Workspace: workspace, BashTimeout: 10})
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
		Workspace:   workspace,
		Command:     []string{helper},
		Environment: []string{"PONS_HANDS_TEST_HELPER=1"},
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
