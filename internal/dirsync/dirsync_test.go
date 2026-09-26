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

	changed, err := Apply(host, base.Digests(), remote)
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
	if _, err := Apply(host, nil, Tree{"up/PERSONA.md": []byte("agent")}); err == nil {
		t.Fatal("write through a symlink escaped the directory")
	}
	if _, err := os.Stat(filepath.Join(parent, "PERSONA.md")); !os.IsNotExist(err) {
		t.Fatalf("file written outside the directory: %v", err)
	}
}

func TestApplyKeepsDeletedFileChangedSinceBase(t *testing.T) {
	host := t.TempDir()
	writeTestFile(t, filepath.Join(host, "tea.md"), "green\n")
	writeTestFile(t, filepath.Join(host, ".dirsync-notes.md"), "keep me\n")
	base, err := Read(host)
	if err != nil {
		t.Fatal(err)
	}
	// Another run edits tea.md while this run deletes it and edits notes.
	writeTestFile(t, filepath.Join(host, "tea.md"), "oolong\n")
	result := Tree{".dirsync-notes.md": []byte("keep me\n"), "notes.md": []byte("new\n")}
	if _, err := Apply(host, base.Digests(), result); err != nil {
		t.Fatal(err)
	}
	after, err := Read(host)
	if err != nil {
		t.Fatal(err)
	}
	if string(after["tea.md"]) != "oolong\n" {
		t.Fatalf("concurrent edit lost to a delete: %q", after["tea.md"])
	}
	if string(after[".dirsync-notes.md"]) != "keep me\n" || string(after["notes.md"]) != "new\n" || len(after) != 3 {
		t.Fatalf("tree after apply = %q", after)
	}
}

func TestApplyReplacesFileWithDirectory(t *testing.T) {
	checkApplyReplacement(t,
		Tree{"topic": []byte("old")},
		Tree{"topic/notes/new.md": []byte("new")},
	)
}

func checkApplyReplacement(t *testing.T, before, after Tree) {
	t.Helper()
	host := t.TempDir()
	for name, data := range before {
		writeTestFile(t, filepath.Join(host, name), string(data))
	}
	if _, err := Apply(host, before.Digests(), after); err != nil {
		t.Fatal(err)
	}
	got, err := Read(host)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(after) {
		t.Fatalf("tree = %q, want %q", got, after)
	}
	for name, want := range after {
		if !bytes.Equal(got[name], want) {
			t.Fatalf("%s = %q, want %q", name, got[name], want)
		}
	}
}

func TestApplyPathReplacementPreservesConcurrentFiles(t *testing.T) {
	for _, test := range []struct {
		name       string
		before     Tree
		concurrent string
		after      Tree
	}{
		{
			name:       "changed file blocks new directory",
			before:     Tree{"topic": []byte("old")},
			concurrent: "topic",
			after:      Tree{"topic/new.md": []byte("new")},
		},
		{
			name:       "changed child blocks replacement file",
			before:     Tree{"topic/old.md": []byte("old")},
			concurrent: "topic/old.md",
			after:      Tree{"topic": []byte("new")},
		},
		{
			name:       "new child blocks replacement file",
			before:     Tree{"topic/old.md": []byte("old")},
			concurrent: "topic/new.md",
			after:      Tree{"topic": []byte("new")},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			host := t.TempDir()
			for name, data := range test.before {
				writeTestFile(t, filepath.Join(host, name), string(data))
			}
			writeTestFile(t, filepath.Join(host, test.concurrent), "concurrent edit")
			if _, err := Apply(host, test.before.Digests(), test.after); err == nil {
				t.Fatal("path replacement unexpectedly succeeded")
			}
			got, err := os.ReadFile(filepath.Join(host, test.concurrent))
			if err != nil || string(got) != "concurrent edit" {
				t.Fatalf("concurrent file = %q, %v", got, err)
			}
		})
	}
}
