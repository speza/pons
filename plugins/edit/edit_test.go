package edit

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newEdit(t *testing.T, root string) *Edit {
	t.Helper()
	p, err := New(Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestExactUniqueReplacement(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "a.txt")
	os.WriteFile(file, []byte("alpha\nbeta\ngamma\n"), 0o644)
	p := newEdit(t, root)

	res, _ := p.apply(context.Background(), Patch(file, "<<<<<<< SEARCH\ngamma\n=======\nGAMMA\n>>>>>>> REPLACE"))
	if !res.OK {
		t.Fatalf("apply failed: %s", res.Error)
	}
	er, ok := AsEditResult(res)
	if !ok || !strings.Contains(er.Diff, "-gamma") || !strings.Contains(er.Diff, "+GAMMA") {
		t.Fatalf("structured edit result: ok=%v diff=%q", ok, er.Diff)
	}
	if er.Blocks != 1 || er.Path != "a.txt" {
		t.Fatalf("edit payload fields: %+v", er)
	}
	b, _ := os.ReadFile(file)
	if string(b) != "alpha\nbeta\nGAMMA\n" {
		t.Fatalf("file content: %q", b)
	}
}

func TestNotFoundAndDuplicateErrors(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "a.txt")
	os.WriteFile(file, []byte("x\ny\n"), 0o644)
	p := newEdit(t, root)
	ctx := context.Background()

	res, _ := p.apply(ctx, Patch(file, "<<<<<<< SEARCH\nzzz\n=======\nq\n>>>>>>> REPLACE"))
	if res.OK || !strings.Contains(res.Error, "could not find the exact text") {
		t.Fatalf("not-found: %+v", res)
	}

	os.WriteFile(file, []byte("dup\ndup\n"), 0o644)
	res, _ = p.apply(ctx, Patch(file, "<<<<<<< SEARCH\ndup\n=======\none\n>>>>>>> REPLACE"))
	if res.OK || !strings.Contains(res.Error, "found 2 occurrences") {
		t.Fatalf("duplicate: %+v", res)
	}
}

func TestFuzzyFallbackMatches(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "a.txt")
	// Trailing whitespace + curly quote + CRLF in the file.
	os.WriteFile(file, []byte("name: “Ada”   \r\nrole: pilot\r\n"), 0o644)
	p := newEdit(t, root)

	res, _ := p.apply(context.Background(), Patch(file, "<<<<<<< SEARCH\nname: \"Ada\"\nrole: pilot\n=======\nname: \"Ada\"\nrole: captain\n>>>>>>> REPLACE"))
	if !res.OK {
		t.Fatalf("fuzzy apply failed: %s", res.Error)
	}
	b, _ := os.ReadFile(file)
	// CRLF must be preserved; the matched lines (incl. trailing ws) replaced.
	if !strings.Contains(string(b), "role: captain\r\n") {
		t.Fatalf("content after fuzzy edit: %q", b)
	}
}

func TestMultipleBlocksAppliedSequentially(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "a.txt")
	os.WriteFile(file, []byte("one\ntwo\nthree\n"), 0o644)
	p := newEdit(t, root)

	patch := "<<<<<<< SEARCH\none\n=======\nONE\n>>>>>>> REPLACE\n<<<<<<< SEARCH\nthree\n=======\nTHREE\n>>>>>>> REPLACE"
	res, _ := p.apply(context.Background(), Patch(file, patch))
	if !res.OK {
		t.Fatalf("multi-block failed: %s", res.Error)
	}
	b, _ := os.ReadFile(file)
	if string(b) != "ONE\ntwo\nTHREE\n" {
		t.Fatalf("content: %q", b)
	}
	if !strings.Contains(res.Output, "replaced 2 block(s)") {
		t.Fatalf("summary: %s", res.Output)
	}
}

func TestJailRejectsEscape(t *testing.T) {
	p := newEdit(t, t.TempDir())
	res, _ := p.apply(context.Background(), Patch(filepath.Join(p.root, "..", "x"), "<<<<<<< SEARCH\na\n=======\nb\n>>>>>>> REPLACE"))
	if res.OK || !strings.Contains(res.Error, "escapes root") {
		t.Fatalf("escape: %+v", res)
	}
}

func TestRelativePathUsesWorkspaceRoot(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "note.txt")
	if err := os.WriteFile(path, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := newEdit(t, root)
	res, err := p.apply(context.Background(), Patch("note.txt", "<<<<<<< SEARCH\nold\n=======\nnew\n>>>>>>> REPLACE"))
	if err != nil || !res.OK {
		t.Fatalf("relative edit: %+v err=%v", res, err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "new\n" {
		t.Fatalf("relative edit landed outside workspace: %q err=%v", got, err)
	}
}
