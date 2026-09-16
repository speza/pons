// Package shell adds the run_command action.
//
// Guard: a first-token allowlist. This is a POLICY gate, not a security
// boundary — "ls; rm -rf /" passes a first-token check. Real isolation
// must come from running the hands layer (this process) inside an OS /
// container / VM boundary.
package shell

import (
	"context"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/samperrin/pons"
	"github.com/samperrin/pons/protocol"
)

// KindRun is the kind owned by this plugin.
const KindRun protocol.ActionKind = "run_command"

// Run is the action constructor.
func Run(command string) protocol.Action {
	return protocol.Action{Kind: KindRun, Args: map[string]string{"command": command}}
}

// Shell is the command-execution plugin.
type Shell struct {
	cfg Config
}

// Config tunes the plugin.
type Config struct {
	Root      string        // working directory for commands
	Allow     []string      // allowed first tokens, e.g. "ls", "go", "cat"
	Timeout   time.Duration // per command; 0 = 10s
	MaxOutput int           // stdout truncation, bytes
}

// New creates the plugin (installed with pons.Core.Use).
func New(cfg Config) *Shell { return &Shell{cfg: cfg} }

// Setup registers the run_command tool.
func (p *Shell) Setup(c *pons.Core) error {
	return c.AddTool(KindRun, pons.ToolDef{
		Handler:     p.run,
		Description: "Run a shell command (first token must be on the plugin allowlist). Non-zero exit codes are observations, not failures.",
		Params: []pons.ToolParam{
			{Name: "command", Type: "string", Description: "The shell command to run", Required: true},
		},
	})
}

func (p *Shell) run(ctx context.Context, a protocol.Action) (protocol.ToolResult, error) {
	fields := strings.Fields(a.Args["command"])
	if len(fields) == 0 {
		return protocol.ToolResult{ActionID: a.ID, OK: false, Error: "empty command"}, nil
	}
	if !p.allowed(fields[0]) {
		return protocol.ToolResult{ActionID: a.ID, OK: false,
			Error: fmt.Sprintf("command %q not in allowlist", fields[0])}, nil
	}

	timeout := p.cfg.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, "sh", "-c", a.Args["command"])
	if p.cfg.Root != "" {
		cmd.Dir = p.cfg.Root
	}
	out, err := cmd.CombinedOutput()
	res := protocol.ToolResult{ActionID: a.ID, OK: err == nil, Output: truncate(string(out), p.cfg.MaxOutput)}
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			// Non-zero exit is an observation for the brain, not a crash.
			res.OK = true
			res.ExitCode = ee.ExitCode()
			res.Error = fmt.Sprintf("exit %d", ee.ExitCode())
		} else {
			res.Error = err.Error()
		}
	}
	return res, nil
}

func (p *Shell) allowed(first string) bool {
	return slices.Contains(p.cfg.Allow, first)
}

func truncate(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n] + "\n…[truncated]"
}
