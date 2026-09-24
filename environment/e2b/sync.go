package e2b

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"github.com/samperrin/pons/internal/dirsync"
)

// syncRoot holds host directories copied into the sandbox, such as the
// agent's memory. It is outside the workspace, so workspace checkpoints never
// include them.
const syncRoot = "/home/user/.pons"

var unsafeSyncName = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// syncedDirectory is a host directory copied into the sandbox for one run,
// with the snapshot it was copied from.
type syncedDirectory struct {
	host   string
	remote string
	base   dirsync.Tree
}

// syncDirectoriesIn copies each host directory into a fresh sandbox
// directory. A warm sandbox's earlier copy is replaced, so each run starts
// from the host's current files.
func syncDirectoriesIn(ctx context.Context, client *e2bClient, sandbox e2bSandbox, dirs []string) ([]syncedDirectory, error) {
	synced := make([]syncedDirectory, 0, len(dirs))
	used := map[string]bool{}
	for i, host := range dirs {
		name := unsafeSyncName.ReplaceAllString(filepath.Base(host), "_")
		if name == "" || name == "." || name == ".." || used[name] {
			name += "-" + strconv.Itoa(i)
		}
		used[name] = true
		remote := syncRoot + "/" + name

		base, err := dirsync.Read(host)
		if err != nil {
			return nil, err
		}
		archive, err := dirsync.Archive(base)
		if err != nil {
			return nil, err
		}
		upload := "/tmp/pons-sync-" + strconv.Itoa(i) + ".tar"
		if err := client.upload(ctx, sandbox, upload, bytes.NewReader(archive)); err != nil {
			return nil, err
		}
		if _, _, err := client.run(ctx, sandbox, "/bin/sh", []string{
			"-c", "rm -rf " + remote + " && mkdir -p " + remote + " && tar -xf " + upload + " -C " + remote + " && rm -f " + upload,
		}, "/home/user", nil); err != nil {
			return nil, fmt.Errorf("environment: copy %s into E2B: %w", host, err)
		}
		synced = append(synced, syncedDirectory{host: host, remote: remote, base: base})
	}
	return synced, nil
}

func syncedRemotePaths(synced []syncedDirectory) []string {
	paths := make([]string, len(synced))
	for i, dir := range synced {
		paths[i] = dir.remote
	}
	return paths
}

// syncDirectoriesOut applies each copy's changes back to its host directory,
// whatever the run's outcome, as a local grant would have kept them. A failed
// sync loses only that run's edits, so it is reported rather than retaining
// the sandbox for recovery.
func (s *e2bSession) syncDirectoriesOut() {
	if len(s.synced) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for i, dir := range s.synced {
		if err := s.syncDirectoryOut(ctx, i, dir); err != nil && s.onError != nil {
			s.onError(fmt.Errorf("environment: copy %s back from E2B: %w", dir.host, err))
		}
	}
}

func (s *e2bSession) syncDirectoryOut(ctx context.Context, i int, dir syncedDirectory) error {
	// Links are never synced: drop them from the discarded sandbox copy so a
	// link pointing outside it cannot reject the whole archive, and flatten
	// hard links as workspace checkpoints do.
	out := "/tmp/pons-sync-out-" + strconv.Itoa(i) + ".tar"
	if _, _, err := s.client.run(ctx, s.sandbox, "/bin/sh", []string{
		"-c", "find " + dir.remote + " -type l -delete && tar --hard-dereference -cf " + out + " -C " + dir.remote + " .",
	}, "/home/user", nil); err != nil {
		return err
	}
	archive, err := stageWorkspaceArchive(func(w io.Writer) error {
		return s.client.download(ctx, s.sandbox, out, w, s.maxWorkspaceBytes)
	})
	if err != nil {
		return err
	}
	defer os.Remove(archive.Name())
	defer archive.Close()

	// Extract with the same validation and limits as workspace checkpoints,
	// then read back only regular files.
	parent, err := os.MkdirTemp("", "pons-e2b-sync-")
	if err != nil {
		return err
	}
	defer func() { _ = removeWorkspaceTree(parent) }()
	extracted := filepath.Join(parent, "copy")
	if err := os.Mkdir(extracted, 0o700); err != nil {
		return err
	}
	if err := restoreWorkspaceArchive(extracted, &contextReader{ctx: ctx, reader: archive}, s.maxWorkspaceBytes); err != nil {
		return err
	}
	result, err := dirsync.Read(extracted)
	if err != nil {
		return err
	}

	changed, err := dirsync.Apply(dir.host, dir.base, result)
	if len(changed) != 0 {
		s.debugf("synced %d changed file(s) back to %s", len(changed), dir.host)
	}
	return err
}
