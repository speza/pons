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
	"unicode/utf8"

	"github.com/samperrin/pons"
	"github.com/samperrin/pons/protocol"
)

// KindBash is the kind owned by this plugin.
const KindBash protocol.ActionKind = "bash"

// maxTimeoutSeconds caps per-call timeouts (pi caps too).
const maxTimeoutSeconds = 600

// maxFullFileBytes bounds the full-output spill file so one truncated call
// cannot fill /tmp; larger outputs keep the tail (see saveFull).
const maxFullFileBytes = 8 << 20

// Run is the action constructor. timeoutSecs = 0 means no per-call timeout.
func Run(command string, timeoutSecs int) protocol.Action {
	args := map[string]any{"command": command}
	if timeoutSecs > 0 {
		args["timeout"] = timeoutSecs
	}
	return protocol.Action{Kind: KindBash, Args: protocol.MustArgsJSON(args)}
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
	command, err := protocol.StringArg(a.Args, "command")
	if err != nil {
		return protocol.ToolResult{ActionID: a.ID, OK: false, Error: "invalid arguments: " + err.Error()}, nil
	}
	if command == "" {
		return protocol.ToolResult{ActionID: a.ID, OK: false, Error: "empty command"}, nil
	}

	timeout := p.cfg.Timeout
	if raw, err := protocol.ObjectArgs(a.Args); err != nil {
		return protocol.ToolResult{ActionID: a.ID, OK: false, Error: "invalid arguments: " + err.Error()}, nil
	} else if value, ok := raw["timeout"]; ok && string(value) != "null" {
		var n int
		if err := json.Unmarshal(value, &n); err != nil {
			var legacy string
			if err := json.Unmarshal(value, &legacy); err != nil {
				return protocol.ToolResult{ActionID: a.ID, OK: false, Error: "invalid timeout: must be a positive number of seconds"}, nil
			}
			n, err = strconv.Atoi(legacy)
			if err != nil {
				return protocol.ToolResult{ActionID: a.ID, OK: false, Error: "invalid timeout: must be a positive number of seconds"}, nil
			}
		}
		if n <= 0 {
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

	text, extras := truncateOutput(string(out), p.maxLines(), p.maxBytes())
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

// truncateOutput keeps the LAST maxLines lines and last maxBytes bytes of
// output. The model-facing note stays in Output; machine-readable facts land
// in the typed Payload (truncation and full-output path). ExecExtras are the
// structured facts of a command run; consumers decode via AsExecResult.
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

func truncateOutput(s string, maxLines, maxBytes int) (string, *ExecExtras) {
	result, droppedLines, truncated := s, 0, false

	lines := strings.Split(s, "\n")
	trailingNewline := s != "" && strings.HasSuffix(s, "\n")
	if trailingNewline {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > maxLines {
		droppedLines = len(lines) - maxLines
		result = strings.Join(lines[droppedLines:], "\n")
		if trailingNewline {
			result += "\n"
		}
		truncated = true
	}
	bytesDropped := 0
	if len(result) > maxBytes {
		pre := result
		cut := len(result) - maxBytes
		if nl := strings.IndexByte(result[cut:], '\n'); nl >= 0 {
			cut += nl + 1
		} else {
			// No line boundary in range: keep the cut inside a rune so the
			// tail stays valid UTF-8.
			for cut < len(result) && !utf8.RuneStart(result[cut]) {
				cut++
			}
		}
		droppedLines += strings.Count(pre[:cut], "\n")
		bytesDropped = len(pre) - len(pre[cut:])
		result = result[cut:]
		truncated = true
	}
	if !truncated {
		return s, nil
	}
	extras := &ExecExtras{Truncated: true, DroppedLines: droppedLines}
	note := ""
	switch {
	case droppedLines > 0:
		note = fmt.Sprintf("%d earlier lines truncated", droppedLines)
	case bytesDropped > 0:
		// Single huge line with no newline in range: lines dropped is
		// zero but bytes were still cut.
		note = fmt.Sprintf("%d earlier bytes truncated", bytesDropped)
	default:
		note = "output truncated"
	}
	if full, err := saveFull(s); err == nil {
		extras.FullOutput = full
		result = fmt.Sprintf("…[%s; full output: %s]\n%s", note, full, result)
	} else {
		result = fmt.Sprintf("…[%s]\n%s", note, result)
	}
	return result, extras
}

func saveFull(s string) (string, error) {
	// Bound the spill so one truncated call cannot fill /tmp; larger
	// outputs keep the tail, matching what the model sees.
	if len(s) > maxFullFileBytes {
		s = s[len(s)-maxFullFileBytes:]
		if nl := strings.IndexByte(s, '\n'); nl >= 0 {
			s = s[nl+1:]
		} else {
			for len(s) > 0 && !utf8.RuneStart(s[0]) {
				s = s[1:]
			}
		}
	}
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
