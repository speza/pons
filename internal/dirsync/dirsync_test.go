package dirsync

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRoundTripAppliesOnlyTheRunsChanges(t *testing.T) {
	host := t.TempDir()
	writeTestFile(t, filepath.Join(host, "MEMORY.md"), "- index\n")
	writeTestFile(t, filepath.Join(host, "tea.md"), "green\n")
	writeTestFile(t, filepath.Join(host, "old.md"), "stale\n")
	if err := os.Symlink("/etc/passwd", filepath.Join(host, "link.md")); err != nil {
		t.Fatal(err)
	}
	base, err := Read(host)
	if err != nil {
		t.Fatal(err)
	}
	if _, linked := base["link.md"]; linked || len(base) != 3 {
		t.Fatalf("snapshot = %v", base)
	}
	if archive, err := Archive(base); err != nil || len(archive) == 0 {
		t.Fatalf("archive = %d bytes, %v", len(archive), err)
	}

	// The remote run edits its copy while another run changes tea.md here.
	remote := Tree{}
	for name, data := range base {
		remote[name] = bytes.Clone(data)
	}
	remote["MEMORY.md"] = []byte("- index\n- coffee\n")
	remote["notes/coffee.md"] = []byte("flat white\n")
	delete(remote, "old.md")
	writeTestFile(t, filepath.Join(host, "tea.md"), "oolong\n")

	changed, err := Apply(host, base, remote)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(changed, []string{"MEMORY.md", "notes/coffee.md", "old.md"}) {
		t.Fatalf("changed = %v", changed)
	}
	after, err := Read(host)
	if err != nil {
		t.Fatal(err)
	}
	if string(after["MEMORY.md"]) != "- index\n- coffee\n" || string(after["notes/coffee.md"]) != "flat white\n" {
		t.Fatalf("applied tree = %v", after)
	}
	if string(after["tea.md"]) != "oolong\n" {
		t.Fatalf("a file the run did not touch was reverted: %q", after["tea.md"])
	}
	if _, ok := after["old.md"]; ok {
		t.Fatal("deleted file survived")
	}
}

func TestApplyStaysInsideDirectory(t *testing.T) {
	parent := t.TempDir()
	host := filepath.Join(parent, "memory")
	if err := os.Mkdir(host, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(parent, filepath.Join(host, "up")); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(host, Tree{}, Tree{"up/PERSONA.md": []byte("agent")}); err == nil {
		t.Fatal("write through a symlink escaped the directory")
	}
	if _, err := os.Stat(filepath.Join(parent, "PERSONA.md")); !os.IsNotExist(err) {
		t.Fatalf("file written outside the directory: %v", err)
	}
}
