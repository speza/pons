package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"golang.org/x/term"

	"github.com/samperrin/pons/plugins/mcp"
)

const mcpUsage = `usage:
  pons mcp set-secret NAME    store a secret read from the terminal or standard input
  pons mcp tools SERVER       connect to a configured server and list its tools`

// secretsPath is the named-secret store, beside the provider auth store.
func secretsPath(home string) string {
	return filepath.Join(home, ".pons", "secrets.json")
}

// newMCPManager builds the server's MCP connections from global settings and
// the secret store. With no servers configured there is no manager, and the
// secret store is never opened.
func newMCPManager(home string, servers map[string]mcp.ServerConfig, maxResultBytes int) (*mcp.Manager, error) {
	if len(servers) == 0 {
		return nil, nil
	}
	secrets, err := mcp.LoadSecrets(secretsPath(home))
	if err != nil {
		return nil, err
	}
	return mcp.NewManager(mcp.Options{Servers: servers, Secrets: secrets, MaxResultBytes: maxResultBytes})
}

// runMCP implements `pons mcp set-secret|tools`. Both work on the host's
// configuration and secret store directly and need no running server.
func runMCP(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, home string) error {
	if len(args) == 0 || (args[0] != "set-secret" && args[0] != "tools") {
		fmt.Fprintln(stderr, mcpUsage)
		return errors.New("an mcp command, set-secret or tools, is required")
	}
	if len(args) != 2 || strings.HasPrefix(args[1], "-") {
		fmt.Fprintln(stderr, mcpUsage)
		if len(args) > 1 && (args[1] == "-h" || args[1] == "-help" || args[1] == "--help") {
			return flag.ErrHelp
		}
		// A secret value is never an argument, so there is nothing else to pass.
		return fmt.Errorf("%s takes exactly one name", args[0])
	}
	if home == "" {
		return errors.New("home directory is unknown")
	}

	if args[0] == "set-secret" {
		return setMCPSecret(ctx, args[1], stdin, stdout, stderr, home)
	}
	return listMCPTools(ctx, args[1], stdout, home)
}

func setMCPSecret(ctx context.Context, name string, stdin io.Reader, stdout, stderr io.Writer, home string) error {
	if !mcp.ValidSecretName(name) {
		return fmt.Errorf("secret name %q must be 1 to 64 letters, digits, underscores, or hyphens", name)
	}
	value, err := readSecret(ctx, name, stdin, stderr)
	if err != nil {
		return err
	}
	if err := mcp.SetSecret(secretsPath(home), name, value); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "stored secret %s\n", name)
	return nil
}

// readSecret reads a value without echo from a terminal, or one line from
// any other input so a password manager can pipe it in.
func readSecret(ctx context.Context, name string, stdin io.Reader, stderr io.Writer) (string, error) {
	if file, ok := stdin.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
		return promptSecret(ctx, name, int(file.Fd()), stderr)
	}

	line, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read secret: %w", err)
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// promptSecret reads a value from a terminal without echo. The read cannot
// be interrupted, so an interrupt signal restores the terminal and returns
// while the read is abandoned with the process.
func promptSecret(ctx context.Context, name string, fd int, stderr io.Writer) (string, error) {
	state, err := term.GetState(fd)
	if err != nil {
		return "", fmt.Errorf("read secret: %w", err)
	}
	fmt.Fprintf(stderr, "Value for %s: ", name)

	type answer struct {
		value []byte
		err   error
	}
	read := make(chan answer, 1)
	go func() {
		value, err := term.ReadPassword(fd)
		read <- answer{value, err}
	}()
	select {
	case got := <-read:
		fmt.Fprintln(stderr)
		if got.err != nil {
			return "", fmt.Errorf("read secret: %w", got.err)
		}
		return string(got.value), nil
	case <-ctx.Done():
		_ = term.Restore(fd, state)
		fmt.Fprintln(stderr)
		return "", ctx.Err()
	}
}

func listMCPTools(ctx context.Context, name string, stdout io.Writer, home string) error {
	cfg, err := loadSettings(home, "")
	if err != nil {
		return err
	}
	config, ok := cfg.MCPServers[name]
	if !ok {
		return fmt.Errorf("MCP server %q is not in mcpServers in ~/.pons/config.json", name)
	}
	// A disabled server is still worth checking before it is enabled.
	config.Enabled = nil

	// The same result limit as the server, so both see the same listing.
	maxResultBytes := 0
	if cfg.PluginMaxResultBytes != nil {
		maxResultBytes = *cfg.PluginMaxResultBytes
	}
	manager, err := newMCPManager(home, map[string]mcp.ServerConfig{name: config}, maxResultBytes)
	if err != nil {
		return err
	}
	defer manager.Close()
	tools, err := manager.Tools(ctx, name)
	if err != nil {
		return err
	}

	writer := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(writer, "TOOL\tREAD-ONLY\tSTATUS")
	available := 0
	for _, tool := range tools {
		readOnly := "no"
		if tool.ReadOnly {
			readOnly = "yes"
		}
		if tool.Status == mcp.StatusAvailable {
			available++
		}
		fmt.Fprintf(writer, "%s\t%s\t%s\n", tool.Name, readOnly, tool.Status)
	}
	if err := writer.Flush(); err != nil {
		return err
	}
	if available > mcp.MaxToolsPerServer {
		return fmt.Errorf("MCP server %q offers %d tools, more than the limit of %d, so a run would get none; narrow enabledTools in its configuration or in the agent's grant",
			name, available, mcp.MaxToolsPerServer)
	}
	return nil
}
