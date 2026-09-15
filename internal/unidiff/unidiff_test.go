package unidiff

import (
	"strings"
	"testing"
)

func TestUnifiedBasic(t *testing.T) {
	a := "alpha\nbeta\ngamma\n"
	b := "alpha\nbeta\nGAMMA\ndelta\n"
	got := Unified("notes.txt", a, b, 3)

	want := `--- a/notes.txt
+++ b/notes.txt
@@ -1,3 +1,4 @@
 alpha
 beta
-gamma
+GAMMA
+delta
`
	if got != want {
		t.Fatalf("diff mismatch:\n got: %q\nwant: %q", got, want)
	}
}

func TestEqualIsEmpty(t *testing.T) {
	if got := Unified("f", "a\nb\n", "a\nb\n", 3); got != "" {
		t.Fatalf("expected empty diff, got %q", got)
	}
}

func TestHunksSplitWithGap(t *testing.T) {
	// Two changes separated by 6 unchanged lines (> 2*context=6? context=1 → gap 6 > 2 → two hunks)
	a := "one\nX1\ntwo\nthree\nfour\nfive\nsix\nseven\neight\nY1\n"
	b := "one\nx1\ntwo\nthree\nfour\nfive\nsix\nseven\neight\ny1\n"
	got := Unified("f", a, b, 1)
	if n := strings.Count(got, "\n@@"); n != 2 { // each hunk header contains two "@@" markers
		t.Fatalf("expected 2 hunks with context=1 and gap of 6 unchanged lines, got %d:\n%s", n, got)
	}
}

func TestNoNewlineMarker(t *testing.T) {
	got := Unified("f", "a", "a\n", 3)
	want := `--- a/f
+++ b/f
@@ -1,1 +1,1 @@
-a
\ No newline at end of file
+a
`
	if got != want {
		t.Fatalf("diff mismatch:\n got: %q\nwant: %q", got, want)
	}
}

func TestAppendOnly(t *testing.T) {
	got := Unified("f", "a\nb\n", "a\nb\nc\n", 3)
	want := `--- a/f
+++ b/f
@@ -1,2 +1,3 @@
 a
 b
+c
`
	if got != want {
		t.Fatalf("diff mismatch:\n got: %q\nwant: %q", got, want)
	}
}
