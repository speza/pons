package dirsync

import (
	"archive/tar"
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
	archive, err := Archive(base)
	if err != nil {
		t.Fatal(err)
	}

	// The remote run edits its copy while another run changes tea.md here.
	files, content, _ := base.Limits()
	remote, err := Unarchive(bytes.NewReader(archive), files, content)
	if err != nil {
		t.Fatal(err)
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

func TestUnarchiveRejectsUnsafeEntries(t *testing.T) {
	for name, header := range map[string]tar.Header{
		"escape":    {Typeflag: tar.TypeReg, Name: "../PERSONA.md", Size: 1},
		"absolute":  {Typeflag: tar.TypeReg, Name: "/etc/passwd", Size: 1},
		"oversized": {Typeflag: tar.TypeReg, Name: "big.md", Size: 2},
	} {
		t.Run(name, func(t *testing.T) {
			var buffer bytes.Buffer
			writer := tar.NewWriter(&buffer)
			if err := writer.WriteHeader(&header); err != nil {
				t.Fatal(err)
			}
			_, _ = writer.Write(bytes.Repeat([]byte("x"), int(header.Size)))
			_ = writer.Close()
			if _, err := Unarchive(&buffer, 10, 1); err == nil {
				t.Fatal("unsafe entry accepted")
			}
		})
	}
}

func TestUnarchiveSkipsLinksAndKeepsOtherFiles(t *testing.T) {
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	for _, header := range []tar.Header{
		{Typeflag: tar.TypeSymlink, Name: "./drinks.md", Linkname: "../PERSONA.md"},
		{Typeflag: tar.TypeLink, Name: "./copy.md", Linkname: "./coffee.md"},
		{Typeflag: tar.TypeReg, Name: "./coffee.md", Size: 5},
	} {
		if err := writer.WriteHeader(&header); err != nil {
			t.Fatal(err)
		}
		if header.Size > 0 {
			_, _ = writer.Write([]byte("flat\n"))
		}
	}
	_ = writer.Close()
	tree, err := Unarchive(&buffer, 10, 100)
	if err != nil || len(tree) != 1 || string(tree["coffee.md"]) != "flat\n" {
		t.Fatalf("tree = %v, %v", tree, err)
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
