package fs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func newFS(t *testing.T, root string) *FS {
	t.Helper()
	p, err := New(Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReadTruncatesLargeFile(t *testing.T) {
	root := t.TempDir()
	p, err := New(Config{Root: root, MaxReadBytes: 100})
	if err != nil {
		t.Fatal(err)
	}
	if werr := os.WriteFile(filepath.Join(root, "big.txt"), []byte(strings.Repeat("x", 1000)), 0o644); werr != nil {
		t.Fatal(werr)
	}
	res, err := p.readFile(context.Background(), Read("big.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || !strings.Contains(res.Output, "truncated") || len(res.Output) >= 1000 {
		t.Fatalf("large read not truncated: ok=%v len=%d out=%q", res.OK, len(res.Output), res.Output)
	}
}

func TestReadTruncationKeepsValidUTF8(t *testing.T) {
	root := t.TempDir()
	p, err := New(Config{Root: root, MaxReadBytes: 11})
	if err != nil {
		t.Fatal(err)
	}
	if werr := os.WriteFile(filepath.Join(root, "u.txt"), []byte(strings.Repeat("é", 20)), 0o644); werr != nil {
		t.Fatal(werr)
	}
	res, err := p.readFile(context.Background(), Read("u.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || !strings.Contains(res.Output, "truncated") {
		t.Fatalf("expected truncated ok read: %+v", res)
	}
	if body, _, _ := strings.Cut(res.Output, "\n…["); !utf8.ValidString(body) {
		t.Fatalf("truncated body is not valid UTF-8: %q", body)
	}
}

func TestReadRejectsBinary(t *testing.T) {
	root := t.TempDir()
	p := newFS(t, root)
	if werr := os.WriteFile(filepath.Join(root, "bin.dat"), []byte{0x00, 0xff, 0xfe, 0x01}, 0o644); werr != nil {
		t.Fatal(werr)
	}
	res, err := p.readFile(context.Background(), Read("bin.dat"))
	if err != nil {
		t.Fatal(err)
	}
	if res.OK || !strings.Contains(res.Error, "not UTF-8") {
		t.Fatalf("binary read should be rejected: %+v", res)
	}
}

func TestJailRejectsEscape(t *testing.T) {
	p := newFS(t, t.TempDir())
	res, err := p.readFile(context.Background(), Read(filepath.Join(p.roots[0], "..", "etc", "passwd")))
	if err != nil {
		t.Fatal(err)
	}
	if res.OK {
		t.Fatal("escape path should be denied")
	}
	if !strings.Contains(res.Error, "escapes root") {
		t.Fatalf("unexpected error: %s", res.Error)
	}
}

func TestReadWriteList(t *testing.T) {
	root := t.TempDir()
	p := newFS(t, root)
	ctx := context.Background()

	res, _ := p.writeFile(ctx, Write(filepath.Join(root, "sub", "a.txt"), "hello"))
	if !res.OK {
		t.Fatalf("write: %s", res.Error)
	}
	res, _ = p.readFile(ctx, Read(filepath.Join(root, "sub", "a.txt")))
	if res.OK == false || res.Output != "hello" {
		t.Fatalf("read: %+v", res)
	}
	res, _ = p.listDir(ctx, List(root))
	if res.OK == false || !strings.Contains(res.Output, "sub/") {
		t.Fatalf("list: %+v", res)
	}
}

func TestRelativePathsUseWorkspaceRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "relative.txt"), []byte("from root"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := newFS(t, root)
	res, err := p.readFile(context.Background(), Read("relative.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.Output != "from root" {
		t.Fatalf("relative read: %+v", res)
	}
	res, err = p.writeFile(context.Background(), Write("new/relative.txt", "written"))
	if err != nil || !res.OK {
		t.Fatalf("relative write: %+v err=%v", res, err)
	}
	if got, err := os.ReadFile(filepath.Join(root, "new", "relative.txt")); err != nil || string(got) != "written" {
		t.Fatalf("relative write landed outside workspace: %q err=%v", got, err)
	}
}

func TestWritePreservesMode(t *testing.T) {
	root := t.TempDir()
	p := newFS(t, root)
	ctx := context.Background()
	path := filepath.Join(root, "run.sh")
	if err := os.WriteFile(path, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	res, _ := p.writeFile(ctx, Write(path, "new"))
	if !res.OK {
		t.Fatalf("write: %s", res.Error)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("write clobbered mode: %o", info.Mode().Perm())
	}
	if got, _ := os.ReadFile(path); string(got) != "new" {
		t.Fatalf("write content: %q", got)
	}
}

func TestExtraRootsAcceptAbsolutePathsOnly(t *testing.T) {
	workspace, memory := t.TempDir(), t.TempDir()
	p, err := New(Config{Root: workspace, ExtraRoots: []string{memory}})
	if err != nil {
		t.Fatal(err)
	}
	res, err := p.writeFile(context.Background(), Write(filepath.Join(memory, "MEMORY.md"), "- note"))
	if err != nil || !res.OK {
		t.Fatalf("write to extra root = %+v, %v", res, err)
	}
	if data, err := os.ReadFile(filepath.Join(memory, "MEMORY.md")); err != nil || string(data) != "- note" {
		t.Fatalf("extra root file = %q, %v", data, err)
	}
	res, err = p.writeFile(context.Background(), Write("MEMORY.md", "workspace"))
	if err != nil || !res.OK {
		t.Fatalf("relative write = %+v, %v", res, err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "MEMORY.md")); err != nil {
		t.Fatalf("relative path did not stay in the workspace: %v", err)
	}
	res, _ = p.writeFile(context.Background(), Write(filepath.Join(filepath.Dir(memory), "outside.md"), "x"))
	if res.OK {
		t.Fatal("path beside the extra root was allowed")
	}
}
