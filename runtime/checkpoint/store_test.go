package checkpoint

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samperrin/pons/environment"
)

func TestCheckpointRoundTripRepairAndPrune(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "workspaces")
	store := New(dir)
	ctx := context.Background()
	refs := make([]string, 3)
	contents := []string{"base checkpoint", "intermediate checkpoint", "latest checkpoint"}
	for i, body := range contents {
		ref, err := store.PutWorkspaceCheckpoint(ctx, "workspace", []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		refs[i] = ref
	}
	// A new instance must recover archives without SQLite or process-local state.
	store = New(dir)
	for i, ref := range refs {
		body, err := store.WorkspaceCheckpoint(ctx, "workspace", ref, int64(len(contents[i])))
		if err != nil || string(body) != contents[i] {
			t.Fatalf("checkpoint = %q, %v; want %q", body, err, contents[i])
		}
	}
	if _, err := store.WorkspaceCheckpoint(ctx, "workspace", refs[0], int64(len(contents[0])-1)); err == nil {
		t.Fatal("accepted checkpoint exceeding read limit")
	}
	if _, err := store.WorkspaceCheckpoint(ctx, "workspace", refs[0], math.MaxInt64); err == nil {
		t.Fatal("accepted overflowing read limit")
	}
	if _, err := store.WorkspaceCheckpoint(ctx, "other", refs[0], 1<<20); !errors.Is(err, environment.ErrStateNotFound) {
		t.Fatalf("other workspace read = %v", err)
	}
	path, _, err := store.workspaceCheckpointPath("workspace", refs[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.WorkspaceCheckpoint(ctx, "workspace", refs[0], 1<<20); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("corrupt checkpoint read = %v", err)
	}
	// First put repairs the corruption; the second exercises verified reuse.
	for range 2 {
		ref, err := store.PutWorkspaceCheckpoint(ctx, "workspace", []byte(contents[0]))
		if err != nil || ref != refs[0] {
			t.Fatalf("repair/reuse = %q, %v; want %q", ref, err, refs[0])
		}
	}
	if err := store.PruneWorkspaceCheckpoints(ctx, "workspace", []string{refs[0], refs[2]}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.WorkspaceCheckpoint(ctx, "workspace", refs[1], 1<<20); !errors.Is(err, environment.ErrStateNotFound) {
		t.Fatalf("pruned checkpoint read = %v", err)
	}
	for _, i := range []int{0, 2} {
		body, err := store.WorkspaceCheckpoint(ctx, "workspace", refs[i], 1<<20)
		if err != nil || string(body) != contents[i] {
			t.Fatalf("retained checkpoint = %q, %v; want %q", body, err, contents[i])
		}
	}
}

func TestPutWorkspaceCheckpointRejectsInvalidDirectoryHierarchy(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "workspaces")
	store := New(dir)
	if err := os.WriteFile(dir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	ref, err := store.PutWorkspaceCheckpoint(context.Background(), "workspace", []byte("checkpoint"))
	if err == nil {
		t.Fatal("PutWorkspaceCheckpoint succeeded with a file in its directory hierarchy")
	}
	if ref != "" {
		t.Fatalf("reference = %q, want empty on directory installation failure", ref)
	}
}
