package checkpoint

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samperrin/pons/environment"
)

func TestCheckpointStreamingRoundTripRepairAndPrune(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "workspaces")
	store := New(dir)
	ctx := context.Background()
	refs := make([]string, 3)
	contents := []string{"base checkpoint", "intermediate checkpoint", "latest checkpoint"}
	for i, body := range contents {
		ref, err := store.PutWorkspaceCheckpoint(ctx, "workspace", strings.NewReader(body), int64(len(body)))
		if err != nil {
			t.Fatal(err)
		}
		refs[i] = ref
	}

	// A new instance recovers archives without SQLite or process-local state.
	store = New(dir)
	for i, ref := range refs {
		if got := readCheckpoint(t, store, ref, int64(len(contents[i]))); got != contents[i] {
			t.Fatalf("checkpoint = %q, want %q", got, contents[i])
		}
	}
	if reader, err := store.WorkspaceCheckpoint(ctx, "workspace", refs[0], int64(len(contents[0])-1)); err == nil {
		_ = reader.Close()
		t.Fatal("accepted checkpoint exceeding read limit")
	}
	if reader, err := store.WorkspaceCheckpoint(ctx, "workspace", refs[0], math.MaxInt64); err == nil {
		_ = reader.Close()
		t.Fatal("accepted overflowing read limit")
	}
	if reader, err := store.WorkspaceCheckpoint(ctx, "other", refs[0], 1<<20); !errors.Is(err, environment.ErrStateNotFound) {
		if reader != nil {
			_ = reader.Close()
		}
		t.Fatalf("other workspace read = %v", err)
	}

	path, _, err := store.workspaceCheckpointPath("workspace", refs[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Verification is complete before any reader is returned.
	if reader, err := store.WorkspaceCheckpoint(ctx, "workspace", refs[0], 1<<20); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		if reader != nil {
			_ = reader.Close()
		}
		t.Fatalf("corrupt checkpoint open = %v", err)
	}
	// Put always replaces the digest path, repairing corrupt content.
	repaired, err := store.PutWorkspaceCheckpoint(
		ctx,
		"workspace",
		strings.NewReader(contents[0]),
		int64(len(contents[0])),
	)
	if err != nil || repaired != refs[0] {
		t.Fatalf("repair = %q, %v; want %q", repaired, err, refs[0])
	}
	if got := readCheckpoint(t, store, refs[0], 1<<20); got != contents[0] {
		t.Fatalf("repaired checkpoint = %q, want %q", got, contents[0])
	}

	if err := store.PruneWorkspaceCheckpoints(ctx, "workspace", []string{refs[0], refs[2]}); err != nil {
		t.Fatal(err)
	}
	if reader, err := store.WorkspaceCheckpoint(ctx, "workspace", refs[1], 1<<20); !errors.Is(err, environment.ErrStateNotFound) {
		if reader != nil {
			_ = reader.Close()
		}
		t.Fatalf("pruned checkpoint read = %v", err)
	}
	for _, i := range []int{0, 2} {
		if got := readCheckpoint(t, store, refs[i], 1<<20); got != contents[i] {
			t.Fatalf("retained checkpoint = %q, want %q", got, contents[i])
		}
	}
}

func TestPutWorkspaceCheckpointLimitsAndCleanup(t *testing.T) {
	tests := []struct {
		name  string
		body  string
		limit int64
		ok    bool
	}{
		{name: "exact limit", body: "1234", limit: 4, ok: true},
		{name: "overflow", body: "12345", limit: 4},
		{name: "empty", limit: 0, ok: true},
		{name: "zero limit overflow", body: "1", limit: 0},
		{name: "negative limit", limit: -1},
		{name: "unsafe limit", limit: math.MaxInt64},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "workspaces")
			store := New(dir)
			ref, err := store.PutWorkspaceCheckpoint(
				context.Background(),
				"workspace",
				strings.NewReader(test.body),
				test.limit,
			)
			if test.ok && (err != nil || ref == "") {
				t.Fatalf("put = %q, %v", ref, err)
			}
			if !test.ok && err == nil {
				t.Fatalf("put unexpectedly succeeded with ref %q", ref)
			}
			assertNoTemporaryCheckpoints(t, store.workspaceCheckpointDir("workspace"))
		})
	}
}

func TestPutWorkspaceCheckpointReaderFailureAndCancellationCleanup(t *testing.T) {
	t.Run("reader failure", func(t *testing.T) {
		store := New(filepath.Join(t.TempDir(), "workspaces"))
		want := errors.New("read failed")
		_, err := store.PutWorkspaceCheckpoint(
			context.Background(),
			"workspace",
			io.MultiReader(strings.NewReader("partial"), failingReader{err: want}),
			1<<20,
		)
		if !errors.Is(err, want) {
			t.Fatalf("put error = %v, want %v", err, want)
		}
		assertNoTemporaryCheckpoints(t, store.workspaceCheckpointDir("workspace"))
	})

	t.Run("cancellation", func(t *testing.T) {
		store := New(filepath.Join(t.TempDir(), "workspaces"))
		ctx, cancel := context.WithCancel(context.Background())
		reader := &cancelingReader{cancel: cancel}
		_, err := store.PutWorkspaceCheckpoint(ctx, "workspace", reader, 1<<20)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("put error = %v, want context cancellation", err)
		}
		if reader.reads != 1 {
			t.Fatalf("underlying reader called %d times after cancellation, want 1", reader.reads)
		}
		assertNoTemporaryCheckpoints(t, store.workspaceCheckpointDir("workspace"))
	})
}

func TestPutWorkspaceCheckpointRejectsInvalidDirectoryHierarchy(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "workspaces")
	store := New(dir)
	if err := os.WriteFile(dir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	ref, err := store.PutWorkspaceCheckpoint(
		context.Background(),
		"workspace",
		strings.NewReader("checkpoint"),
		1<<20,
	)
	if err == nil {
		t.Fatal("PutWorkspaceCheckpoint succeeded with a file in its directory hierarchy")
	}
	if ref != "" {
		t.Fatalf("reference = %q, want empty on directory installation failure", ref)
	}
}

func readCheckpoint(t *testing.T, store *Store, ref string, limit int64) string {
	t.Helper()
	reader, err := store.WorkspaceCheckpoint(context.Background(), "workspace", ref, limit)
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func assertNoTemporaryCheckpoints(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".checkpoint-") {
			t.Fatalf("temporary checkpoint %q was not cleaned up", entry.Name())
		}
	}
}

type failingReader struct{ err error }

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }

func TestLargeCheckpointUsesBoundedReads(t *testing.T) {
	store := New(t.TempDir())
	const size = 8 << 20
	ref, err := store.PutWorkspaceCheckpoint(context.Background(), "workspace", &chunkReader{remaining: size}, size)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := store.WorkspaceCheckpoint(context.Background(), "workspace", ref, size)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	n, err := io.Copy(io.Discard, reader)
	if err != nil || n != size {
		t.Fatalf("read = %d, %v; want %d bytes", n, err, size)
	}
}

type chunkReader struct{ remaining int }

func (r *chunkReader) Read(buffer []byte) (int, error) {
	if len(buffer) > 32<<10 {
		return 0, errors.New("checkpoint requested an archive-sized buffer")
	}
	if r.remaining == 0 {
		return 0, io.EOF
	}
	n := min(len(buffer), r.remaining)
	clear(buffer[:n])
	r.remaining -= n
	return n, nil
}

type cancelingReader struct {
	cancel context.CancelFunc
	reads  int
}

func (r *cancelingReader) Read(buffer []byte) (int, error) {
	r.reads++
	if r.reads > 1 {
		return 0, errors.New("reader called after cancellation")
	}
	r.cancel()
	return copy(buffer, bytes.Repeat([]byte{'x'}, len(buffer))), nil
}
