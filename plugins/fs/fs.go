// Package fs adds jailed filesystem actions: read_file, write_file, list_dir.
// Every path is symlink-resiliently jailed to a root directory.
package fs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/samperrin/pons"
	"github.com/samperrin/pons/internal/jail"
	"github.com/samperrin/pons/protocol"
)

// Kinds owned by this plugin.
const (
	KindRead  protocol.ActionKind = "read_file"
	KindWrite protocol.ActionKind = "write_file"
	KindList  protocol.ActionKind = "list_dir"
)

// Action constructors — the vocabulary brains use to speak to this plugin.
func Read(path string) protocol.Action {
	return protocol.Action{Kind: KindRead, Args: map[string]string{"path": path}}
}

func Write(path, content string) protocol.Action {
	return protocol.Action{Kind: KindWrite, Args: map[string]string{"path": path, "content": content}}
}

func List(path string) protocol.Action {
	return protocol.Action{Kind: KindList, Args: map[string]string{"path": path}}
}

// FS is the filesystem tool plugin.
type FS struct {
	root string // resolved jail root
}

// Config tunes the plugin.
type Config struct {
	Root string // jail root; "" = process cwd
}

// New creates the plugin (it is installed with pons.Core.Use).
func New(cfg Config) (*FS, error) {
	root, err := jail.Resolve(cfg.Root)
	if err != nil {
		return nil, fmt.Errorf("fs: %w", err)
	}
	return &FS{root: root}, nil
}

// Setup registers the three filesystem tools.
func (p *FS) Setup(c *pons.Core) error {
	path := pons.ToolParam{Name: "path", Type: "string", Description: "File path (relative to the workspace root, or absolute within it)", Required: true}
	if err := c.AddTool(KindRead, pons.ToolDef{
		Handler:     p.readFile,
		Description: "Read a file inside the workspace and return its full content.",
		Params:      []pons.ToolParam{path},
	}); err != nil {
		return err
	}
	if err := c.AddTool(KindWrite, pons.ToolDef{
		Handler:     p.writeFile,
		Description: "Create or overwrite a file inside the workspace. Parent dirs are created as needed.",
		Params: []pons.ToolParam{path,
			{Name: "content", Type: "string", Description: "Full file content to write", Required: true}},
	}); err != nil {
		return err
	}
	return c.AddTool(KindList, pons.ToolDef{
		Handler:     p.listDir,
		Description: "List a directory inside the workspace. Directories have a trailing slash.",
		Params:      []pons.ToolParam{path},
	})
}

// resolvePath is the jail: it returns the canonical, root-relative path
// that the operation must use after the containment check.
func (p *FS) resolvePath(path string) (string, error) {
	return jail.ResolvePath(p.root, path)
}

func (p *FS) readFile(ctx context.Context, a protocol.Action) (protocol.ToolResult, error) {
	path, err := p.resolvePath(a.Args["path"])
	if err != nil {
		return denied(a, err), nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return protocol.ToolResult{ActionID: a.ID, OK: false, Error: err.Error()}, nil
	}
	return protocol.ToolResult{ActionID: a.ID, OK: true, Output: string(b)}, nil
}

func (p *FS) writeFile(ctx context.Context, a protocol.Action) (protocol.ToolResult, error) {
	path, err := p.resolvePath(a.Args["path"])
	if err != nil {
		return denied(a, err), nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return protocol.ToolResult{ActionID: a.ID, OK: false, Error: err.Error()}, nil
	}
	if err := os.WriteFile(path, []byte(a.Args["content"]), 0o644); err != nil {
		return protocol.ToolResult{ActionID: a.ID, OK: false, Error: err.Error()}, nil
	}
	return protocol.ToolResult{ActionID: a.ID, OK: true, Output: "wrote " + path}, nil
}

func (p *FS) listDir(ctx context.Context, a protocol.Action) (protocol.ToolResult, error) {
	path, err := p.resolvePath(a.Args["path"])
	if err != nil {
		return denied(a, err), nil
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return protocol.ToolResult{ActionID: a.ID, OK: false, Error: err.Error()}, nil
	}
	var sb strings.Builder
	for _, en := range entries {
		suffix := ""
		if en.IsDir() {
			suffix = "/"
		}
		sb.WriteString(en.Name() + suffix + "\n")
	}
	return protocol.ToolResult{ActionID: a.ID, OK: true, Output: sb.String()}, nil
}

func denied(a protocol.Action, err error) protocol.ToolResult {
	return protocol.ToolResult{ActionID: a.ID, OK: false, Error: err.Error()}
}
