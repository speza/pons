// Package fs adds jailed filesystem actions: read_file, write_file, list_dir.
// Every path is symlink-resiliently jailed to a root directory.
package fs

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/samperrin/pons"
	"github.com/samperrin/pons/internal/filelock"
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
	return protocol.Action{Kind: KindRead, Args: protocol.MustArgsJSON(map[string]string{"path": path})}
}

func Write(path, content string) protocol.Action {
	return protocol.Action{Kind: KindWrite, Args: protocol.MustArgsJSON(map[string]string{"path": path, "content": content})}
}

func List(path string) protocol.Action {
	return protocol.Action{Kind: KindList, Args: protocol.MustArgsJSON(map[string]string{"path": path})}
}

// FS is the filesystem tool plugin.
type FS struct {
	roots []string // resolved jail roots; the first is the workspace
	cfg   Config
}

// Config tunes the plugin.
type Config struct {
	Root string // jail root; "" = process cwd
	// ExtraRoots are further directories absolute paths may address, such
	// as a host-granted memory directory. Relative paths stay under Root.
	ExtraRoots []string
	// MaxReadBytes caps read_file output so a huge or binary file cannot
	// flood the model context. 0 = default (256 KiB); negative = unlimited.
	MaxReadBytes int
}

const defaultMaxReadBytes = 256 << 10

func (p *FS) maxReadBytes() int {
	if p.cfg.MaxReadBytes > 0 {
		return p.cfg.MaxReadBytes
	}
	if p.cfg.MaxReadBytes < 0 {
		return -1
	}
	return defaultMaxReadBytes
}

// New creates the plugin (it is installed with pons.Core.Use).
func New(cfg Config) (*FS, error) {
	roots, err := jail.ResolveRoots(cfg.Root, cfg.ExtraRoots)
	if err != nil {
		return nil, fmt.Errorf("fs: %w", err)
	}
	return &FS{roots: roots, cfg: cfg}, nil
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
	return jail.ResolvePathWithin(p.roots, path)
}

func (p *FS) readFile(ctx context.Context, a protocol.Action) (protocol.ToolResult, error) {
	pathArg, err := protocol.StringArg(a.Args, "path")
	if err != nil {
		return invalidArgs(a, err), nil
	}

	path, err := p.resolvePath(pathArg)
	if err != nil {
		return denied(a, err), nil
	}

	f, err := os.Open(path)
	if err != nil {
		return protocol.ToolResult{ActionID: a.ID, OK: false, Error: err.Error()}, nil
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return protocol.ToolResult{ActionID: a.ID, OK: false, Error: err.Error()}, nil
	}

	limit := p.maxReadBytes()
	data, truncated, rerr := readBounded(f, limit)
	if rerr != nil {
		return protocol.ToolResult{ActionID: a.ID, OK: false, Error: rerr.Error()}, nil
	}
	if !utf8.Valid(data) {
		return protocol.ToolResult{ActionID: a.ID, OK: false,
			Error: fmt.Sprintf("%s is not UTF-8 text (binary file); use bash to inspect it", path)}, nil
	}
	if truncated {
		return protocol.ToolResult{ActionID: a.ID, OK: true, Output: string(data) + fmt.Sprintf(
			"\n…[truncated: file is %d bytes, showing first %d — use bash to read the rest]", info.Size(), len(data))}, nil
	}
	return protocol.ToolResult{ActionID: a.ID, OK: true, Output: string(data)}, nil
}

// readBounded reads at most limit bytes (limit < 0 = unlimited) and reports
// whether content was cut off. Reading beyond the limit by one chunk keeps
// the truncation signal reliable even if the stat size is stale.
// Read errors are returned so a partial read is never reported as success.
func readBounded(f *os.File, limit int) (data []byte, truncated bool, err error) {
	if limit < 0 {
		data, err = io.ReadAll(f)
		return data, false, err
	}
	data, err = io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err != nil {
		return nil, false, err
	}
	if len(data) > limit {
		cut := data[:limit]
		// Trim a trailing partial rune so valid text is not mistaken for
		// binary content by the UTF-8 check.
		for len(cut) > 0 && !utf8.Valid(cut) {
			cut = cut[:len(cut)-1]
		}
		return cut, true, nil
	}
	return data, false, nil
}

func (p *FS) writeFile(ctx context.Context, a protocol.Action) (protocol.ToolResult, error) {
	pathArg, err := protocol.StringArg(a.Args, "path")
	if err != nil {
		return invalidArgs(a, err), nil
	}
	content, err := protocol.StringArg(a.Args, "content")
	if err != nil {
		return invalidArgs(a, err), nil
	}

	path, err := p.resolvePath(pathArg)
	if err != nil {
		return denied(a, err), nil
	}

	// Coordinate with edit_file's read-modify-write on the same path so a
	// concurrent edit cannot overwrite this write with stale content.
	unlock := filelock.Lock(path)
	defer unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return protocol.ToolResult{ActionID: a.ID, OK: false, Error: err.Error()}, nil
	}
	// Atomic replace like edit_file, preserving the existing mode so a
	// write does not silently drop executable bits (new files: 0644).
	mode := os.FileMode(0o644)
	if info, serr := os.Stat(path); serr == nil {
		mode = info.Mode().Perm()
	}
	if err := writeAtomic(path, []byte(content), mode); err != nil {
		return protocol.ToolResult{ActionID: a.ID, OK: false, Error: err.Error()}, nil
	}
	return protocol.ToolResult{ActionID: a.ID, OK: true, Output: "wrote " + path}, nil
}

func (p *FS) listDir(ctx context.Context, a protocol.Action) (protocol.ToolResult, error) {
	pathArg, err := protocol.StringArg(a.Args, "path")
	if err != nil {
		return invalidArgs(a, err), nil
	}

	path, err := p.resolvePath(pathArg)
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

// writeAtomic replaces path via a temp file in the same directory plus a
// rename, so concurrent readers never see a half-written file. Mirrors
// the edit plugin's writer (same-dir temp is required: the directory must
// already exist).
func writeAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".pons-fs-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func invalidArgs(a protocol.Action, err error) protocol.ToolResult {
	return protocol.ToolResult{ActionID: a.ID, OK: false, Error: "invalid arguments: " + err.Error()}
}
