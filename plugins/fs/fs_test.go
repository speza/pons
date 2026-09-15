package fs

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func newFS(t *testing.T, root string) *FS {
	t.Helper()
	p, err := New(Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestJailRejectsEscape(t *testing.T) {
	p := newFS(t, t.TempDir())
	res, err := p.readFile(context.Background(), Read(filepath.Join(p.root, "..", "etc", "passwd")))
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
