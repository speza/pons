// Package edit adds the edit_file action: applies a patch to a single file
// using exact text replacement, modeled on pi's edit tool
// (packages/coding-agent/src/core/tools/edit.ts):
//
//   - the patch is one or more SEARCH/REPLACE blocks
//   - each SEARCH text must match a UNIQUE region of the file (exact match
//     first, then a whitespace/unicode-normalized fallback, like pi's
//     fuzzyFindText)
//   - on success the result carries a standard unified diff (EditResult.Diff,
//     like pi's details.patch) for review and SDK consumption
//
// Patch format (marker-based, robust for LLM emission):
//
//	<<<<<<< SEARCH
//	exact old text
//	=======
//	replacement text
//	>>>>>>> REPLACE
//
// Multiple blocks are applied in order (block N+1 sees block N's result).
// Known limitation, shared by all marker formats: a SEARCH/REPLACE payload
// cannot itself contain a bare "=======" line.
package edit

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/samperrin/pons"
	"github.com/samperrin/pons/internal/filelock"
	"github.com/samperrin/pons/internal/jail"
	"github.com/samperrin/pons/internal/unidiff"
	"github.com/samperrin/pons/protocol"
)

// KindEdit is the kind owned by this plugin.
const KindEdit protocol.ActionKind = "edit_file"

const (
	searchMarker  = "<<<<<<< SEARCH"
	splitMarker   = "======="
	replacePrefix = ">>>>>>>"
	bomUTF8       = "\uFEFF"
)

// Patch is the action constructor.
func Patch(path, patch string) protocol.Action {
	return protocol.Action{
		Kind: KindEdit,
		Args: protocol.MustArgsJSON(map[string]string{"path": path, "patch": patch}),
	}
}

// Edit is the edit_file tool plugin.
type Edit struct {
	roots []string // resolved jail roots; the first is the workspace
	// The operation is a read-modify-write. Serialize edits so concurrent
	// actions cannot lose one another's changes (ADR-0006).
	mu sync.Mutex
}

// Config tunes the plugin.
type Config struct {
	Root string // jail root; "" = process cwd
	// ExtraRoots are further directories absolute paths may address, such
	// as a host-granted memory directory. Relative paths stay under Root.
	ExtraRoots []string
}

// New creates the plugin (installed with pons.Core.Use).
func New(cfg Config) (*Edit, error) {
	roots, err := jail.ResolveRoots(cfg.Root, cfg.ExtraRoots)
	if err != nil {
		return nil, fmt.Errorf("edit: %w", err)
	}
	return &Edit{roots: roots}, nil
}

// Setup registers the edit tool.
func (p *Edit) Setup(c *pons.Core) error {
	return c.AddTool(KindEdit, pons.ToolDef{
		Handler:     p.apply,
		Description: "Edit a single file by applying SEARCH/REPLACE patch blocks. Every SEARCH text must match a unique region of the file (exact match; trailing whitespace and unicode quotes are normalized as a fallback). Do not include large unchanged regions.",
		Params: []pons.ToolParam{
			{Name: "path", Type: "string", Description: "File to edit (relative to the workspace root, or an absolute path you have access to)", Required: true},
			{Name: "patch", Type: "string", Description: "One or more blocks: '<<<<<<< SEARCH' / old text / '=======' / new text / '>>>>>>> REPLACE'", Required: true},
		},
	})
}

// EditResult is the structured result of one edit_file action.
type EditResult struct {
	Blocks int    `json:"blocks"`
	Path   string `json:"path"`
	Diff   string `json:"diff"` // unified diff of the change
}

// AsEditResult decodes the structured edit payload from a ToolResult.
// UIs and audit tools opt in here; brains use Output/Observation.
func AsEditResult(tr protocol.ToolResult) (EditResult, bool) {
	if tr.Kind != string(KindEdit) {
		return EditResult{}, false
	}
	var r EditResult
	if err := json.Unmarshal(tr.Payload, &r); err != nil {
		return EditResult{}, false
	}
	return r, true
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".pons-edit-*")
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
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	return nil
}

type block struct{ search, replace string }

func (p *Edit) apply(ctx context.Context, a protocol.Action) (protocol.ToolResult, error) {
	pathArg, err := protocol.StringArg(a.Args, "path")
	if err != nil {
		return protocol.ToolResult{ActionID: a.ID, OK: false, Error: "invalid arguments: " + err.Error()}, nil
	}
	patch, err := protocol.StringArg(a.Args, "patch")
	if err != nil {
		return protocol.ToolResult{ActionID: a.ID, OK: false, Error: "invalid arguments: " + err.Error()}, nil
	}

	path, err := jail.ResolvePathWithin(p.roots, pathArg)
	if err != nil {
		return protocol.ToolResult{ActionID: a.ID, OK: false, Error: err.Error()}, nil
	}

	blocks, err := parsePatch(patch)
	if err != nil {
		return protocol.ToolResult{ActionID: a.ID, OK: false, Error: err.Error()}, nil
	}
	if len(blocks) == 0 {
		return protocol.ToolResult{ActionID: a.ID, OK: false, Error: "patch contains no SEARCH/REPLACE blocks"}, nil
	}

	// The operation is a read-modify-write. Serialize edits so concurrent
	// actions cannot lose one another's changes (ADR-0006). The per-path
	// lock additionally coordinates with fs.writeFile on the same file.
	unlock := filelock.Lock(path)
	defer unlock()
	p.mu.Lock()
	defer p.mu.Unlock()

	raw, err := os.ReadFile(path)
	if err != nil {
		return protocol.ToolResult{ActionID: a.ID, OK: false,
			Error: fmt.Sprintf("could not edit file: %s. %v.", path, err)}, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return protocol.ToolResult{ActionID: a.ID, OK: false,
			Error: fmt.Sprintf("could not stat file: %s. %v.", path, err)}, nil
	}

	// Normalize CRLF/BOM for matching; restore on write (pi does the same).
	bom := ""
	content := string(raw)
	if after, ok := strings.CutPrefix(content, bomUTF8); ok {
		bom, content = bomUTF8, after
	}
	ending := "\n"
	if strings.Contains(content, "\r\n") {
		ending = "\r\n"
	}
	original := strings.ReplaceAll(content, "\r\n", "\n")

	current := original
	for i, b := range blocks {
		current, err = applyOne(current, b, path, i, len(blocks))
		if err != nil {
			return protocol.ToolResult{ActionID: a.ID, OK: false, Error: err.Error()}, nil
		}
	}

	if current != original {
		out := bom + strings.ReplaceAll(current, "\n", ending)
		if err := writeAtomic(path, []byte(out), info.Mode().Perm()); err != nil {
			return protocol.ToolResult{ActionID: a.ID, OK: false, Error: err.Error()}, nil
		}
	}
	return protocol.ToolResult{
		ActionID: a.ID,
		OK:       true,
		Kind:     string(KindEdit),
		Output:   fmt.Sprintf("Successfully replaced %d block(s) in %s.", len(blocks), path),
		// Review artifact for UIs/harness; the model doesn't need the patch
		// body — it sees the confirmation in Output.
		Payload: mustJSON(EditResult{Blocks: len(blocks), Path: filepath.Base(path), Diff: unidiff.Unified(filepath.Base(path), original, current, 3)}),
	}, nil
}

// parsePatch splits patch text into SEARCH/REPLACE blocks.
func parsePatch(patch string) ([]block, error) {
	var blocks []block
	var search, replace strings.Builder
	mode := 0 // 0: outside, 1: in search, 2: in replace

	flush := func() error {
		switch mode {
		case 0:
			return nil
		case 1:
			return fmt.Errorf("patch block %d: missing ======= separator", len(blocks)+1)
		case 2:
			if search.Len() == 0 {
				return fmt.Errorf("patch block %d: SEARCH text must not be empty", len(blocks)+1)
			}
			blocks = append(blocks, block{
				search:  strings.TrimSuffix(search.String(), "\n"),
				replace: strings.TrimSuffix(replace.String(), "\n"),
			})
			search.Reset()
			replace.Reset()
			mode = 0
			return nil
		}
		return nil
	}

	for line := range strings.SplitSeq(patch, "\n") {
		t := strings.TrimRight(line, " \t")
		switch {
		case t == searchMarker:
			if err := flush(); err != nil {
				return nil, err
			}
			mode = 1
		case t == splitMarker && mode == 1:
			mode = 2
		case strings.HasPrefix(t, replacePrefix) && mode == 2:
			if err := flush(); err != nil {
				return nil, err
			}
		case mode == 1:
			search.WriteString(line + "\n")
		case mode == 2:
			replace.WriteString(line + "\n")
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return blocks, nil
}

// applyOne applies one SEARCH/REPLACE block: exact match first (must be
// unique), then the normalized fallback. pi-style error messages.
func applyOne(content string, b block, path string, idx, total int) (string, error) {
	if b.search == "" {
		return "", emptyError(path, idx, total)
	}
	switch n := strings.Count(content, b.search); {
	case n == 1:
		return strings.Replace(content, b.search, b.replace, 1), nil
	case n > 1:
		return "", duplicateError(path, idx, total, n)
	}

	start, occ, ok := fuzzyFind(content, b.search)
	if !ok {
		return "", notFoundError(path, idx, total)
	}
	if occ > 1 {
		return "", duplicateError(path, idx, total, occ)
	}
	return replaceLineRange(content, start, len(strings.Split(strings.TrimSuffix(b.search, "\n"), "\n")), b.replace), nil
}

func notFoundError(path string, idx, total int) error {
	if total == 1 {
		return fmt.Errorf("could not find the exact text in %s. The old text must match exactly including all whitespace and newlines.", path)
	}
	return fmt.Errorf("could not find patch block %d in %s. The SEARCH text must match exactly including all whitespace and newlines.", idx+1, path)
}

func duplicateError(path string, idx, total, occurrences int) error {
	if total == 1 {
		return fmt.Errorf("found %d occurrences of the text in %s. The text must be unique. Please provide more context to make it unique.", occurrences, path)
	}
	return fmt.Errorf("found %d occurrences of patch block %d in %s. Each SEARCH text must be unique. Please provide more context to make it unique.", occurrences, idx+1, path)
}

func emptyError(path string, idx, total int) error {
	if total == 1 {
		return fmt.Errorf("SEARCH text must not be empty in %s.", path)
	}
	return fmt.Errorf("patch block %d: SEARCH text must not be empty in %s.", idx+1, path)
}

// fuzzyFind locates the search text with per-line normalization (trailing
// whitespace stripped, unicode quotes/dashes → ASCII). Line-boundary
// matching only; returns the first match's line index and total occurrences.
func fuzzyFind(content, old string) (startLine int, occurrences int, ok bool) {
	lines := strings.Split(content, "\n")
	norm := make([]string, len(lines))
	for i, l := range lines {
		norm[i] = normalizeLine(l)
	}
	needle := strings.Split(strings.TrimSuffix(old, "\n"), "\n")
	for i := range needle {
		needle[i] = normalizeLine(needle[i])
	}

	var hits []int
	for i := 0; i+len(needle) <= len(norm); i++ {
		match := true
		for k := range needle {
			if norm[i+k] != needle[k] {
				match = false
				break
			}
		}
		if match {
			hits = append(hits, i)
		}
	}
	if len(hits) == 0 {
		return -1, 0, false
	}
	return hits[0], len(hits), true
}

// replaceLineRange replaces n lines starting at start with replacement text.
func replaceLineRange(content string, start, n int, replacement string) string {
	lines := strings.Split(content, "\n")
	out := make([]string, 0, len(lines)+8)
	out = append(out, lines[:start]...)
	if replacement != "" {
		out = append(out, strings.Split(replacement, "\n")...)
	}
	out = append(out, lines[start+n:]...)
	return strings.Join(out, "\n")
}

var asciiNormalize = strings.NewReplacer(
	"“", `"`, "”", `"`, "„", `"`,
	"‘", "'", "’", "'",
	"–", "-", "—", "-", "−", "-",
	"…", "...",
)

func normalizeLine(s string) string {
	return asciiNormalize.Replace(strings.TrimRight(s, " \t"))
}
