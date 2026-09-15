// Package bash adds the bash action: run a shell command in the workspace,
// pi-style (packages/coding-agent/src/core/tools/bash.ts): optional per-call
// timeout in seconds (none by default), combined stdout/stderr, output
// truncated to the LAST N lines / bytes, full output saved to a temp file
// when truncated.
//
// Trust model: this plugin is unrestricted by design — policy comes from
// composition. Compose it only when the hands run inside an OS/container
// boundary (or when the user is the boundary, as in pi's interactive TUI).
// For allowlisted command execution, use the shell plugin instead.
package bash

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/samperrin/pons"
	"github.com/samperrin/pons/protocol"
)

// KindBash is the kind owned by this plugin.
const KindBash protocol.ActionKind = "bash"

// maxTimeoutSeconds caps per-call timeouts (pi caps too).
const maxTimeoutSeconds = 600

// Run is the action constructor. timeoutSecs = 0 means no per-call timeout.
func Run(command string, timeoutSecs int) protocol.Action {
	args := map[string]string{"command": command}
	if timeoutSecs > 0 {
		args["timeout"] = strconv.Itoa(timeoutSecs)
	}
	return protocol.Action{Kind: KindBash, Args: args}
}

// Bash is the unrestricted command-execution plugin.
type Bash struct{ cfg Config }

// Config tunes the plugin.
type Config struct {
	Root     string // working directory for commands
	Timeout  int    // default timeout seconds; 0 = no default (pi semantics)
	MaxLines int    // tail-truncation lines (default 200)
	MaxBytes int    // tail-truncation bytes (default 50KiB)
}

// New creates the plugin (installed with pons.Core.Use).
func New(cfg Config) *Bash { return &Bash{cfg: cfg} }

// Setup registers the bash tool.
func (p *Bash) Setup(c *pons.Core) error {
	return c.AddTool(KindBash, pons.ToolDef{
		Handler:     p.run,
		Description: "Execute a shell command. Returns combined stdout/stderr and the exit code. Output is truncated to the last lines; non-zero exit codes are observations, not failures.",
		Params: []pons.ToolParam{
			{Name: "command", Type: "string", Description: "The shell command to execute", Required: true},
			{Name: "timeout", Type: "integer", Description: "Timeout in seconds (optional; no default timeout)"},
		},
	})
}

func (p *Bash) run(ctx context.Context, a protocol.Action) (protocol.ToolResult, error) {
	command := a.Args["command"]
	if command == "" {
		return protocol.ToolResult{ActionID: a.ID, OK: false, Error: "empty command"}, nil
	}

	timeout := p.cfg.Timeout
	if t := a.Args["timeout"]; t != "" {
		n, err := strconv.Atoi(t)
		if err != nil || n <= 0 {
			return protocol.ToolResult{ActionID: a.ID, OK: false, Error: "invalid timeout: must be a positive number of seconds"}, nil
		}
		if n > maxTimeoutSeconds {
			return protocol.ToolResult{ActionID: a.ID, OK: false, Error: fmt.Sprintf("invalid timeout: maximum is %d seconds", maxTimeoutSeconds)}, nil
		}
		timeout = n
	}

	cctx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		cctx, cancel = context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
		defer cancel()
	}

	cmd := exec.CommandContext(cctx, "sh", "-c", command)
	if p.cfg.Root != "" {
		cmd.Dir = p.cfg.Root
	}
	out, err := cmd.CombinedOutput()

	text, extras := truncat(string(out), p.maxLines(), p.maxBytes())
	res := protocol.ToolResult{ActionID: a.ID, OK: true, Kind: string(KindBash), Output: text, ExitCode: 0}
	if extras != nil {
		res.Payload, _ = json.Marshal(extras)
	}

	switch {
	case err == nil:
		// success
	case ctx.Err() != nil:
		// caller canceled
		res.OK = false
		res.Error = "canceled"
	case cctx.Err() != nil:
		// our per-call timeout fired (may surface as ExitError from the kill)
		res.OK = false
		res.Error = fmt.Sprintf("command timed out after %ds", timeout)
	case errors.As(err, new(*exec.ExitError)):
		// Non-zero exit is an observation for the brain, not a crash.
		res.ExitCode = exitCodeOf(err)
		res.Error = fmt.Sprintf("exit %d", res.ExitCode)
	default:
		res.OK = false
		res.Error = err.Error()
	}
	return res, nil
}

func exitCodeOf(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return 0
}

func (p *Bash) maxLines() int {
	if p.cfg.MaxLines > 0 {
		return p.cfg.MaxLines
	}
	return 200
}

func (p *Bash) maxBytes() int {
	if p.cfg.MaxBytes > 0 {
		return p.cfg.MaxBytes
	}
	return 50 * 1024
}

// truncat keeps the LAST maxLines lines and last maxBytes bytes of output.
// The model-facing note stays in Output; machine-readable facts land in
// Details (truncated, full_output — pi's truncation/fullOutputPath).
// ExecExtras are the structured facts of a command run; consumers decode
// via AsExecResult. The model-facing text stays in Output.
type ExecExtras struct {
	Truncated    bool   `json:"truncated,omitempty"`
	DroppedLines int    `json:"dropped_lines,omitempty"`
	FullOutput   string `json:"full_output,omitempty"`
}

// AsExecResult decodes the structured exec payload from a ToolResult.
func AsExecResult(tr protocol.ToolResult) (ExecExtras, bool) {
	if tr.Kind != string(KindBash) {
		return ExecExtras{}, false
	}
	var r ExecExtras
	if err := json.Unmarshal(tr.Payload, &r); err != nil {
		return ExecExtras{}, false
	}
	return r, true
}

func truncat(s string, maxLines, maxBytes int) (string, *ExecExtras) {
	totalLines := strings.Count(s, "\n")
	if s != "" && !strings.HasSuffix(s, "\n") {
		totalLines++
	}
	result, dropped, truncated := s, 0, false

	if totalLines > maxLines {
		lines := strings.Split(s, "\n")
		dropped = len(lines) - maxLines
		result = strings.Join(lines[dropped:], "\n")
		truncated = true
	}
	if len(result) > maxBytes {
		cut := len(result) - maxBytes
		if nl := strings.IndexByte(result[cut:], '\n'); nl >= 0 {
			cut += nl + 1
		}
		result = result[cut:]
		truncated = true
	}
	if !truncated {
		return s, nil
	}
	extras := &ExecExtras{Truncated: true, DroppedLines: dropped}
	if full, err := saveFull(s); err == nil {
		extras.FullOutput = full
		result = fmt.Sprintf("…[%d earlier lines truncated; full output: %s]\n%s", dropped, full, result)
	} else {
		result = fmt.Sprintf("…[%d earlier lines truncated]\n%s", dropped, result)
	}
	return result, extras
}

func saveFull(s string) (string, error) {
	f, err := os.CreateTemp("", "pons-bash-*.log")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return f.Name(), nil
}
