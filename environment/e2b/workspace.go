package e2b

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const maxWorkspaceArchiveEntries = 100_000

func archiveWorkspace(workspace string, limit int64) ([]byte, error) {
	var out bytes.Buffer
	entries := 0
	writer := tar.NewWriter(&limitedWriter{writer: &out, remaining: limit})
	err := filepath.Walk(workspace, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		entries++
		if entries > maxWorkspaceArchiveEntries {
			return fmt.Errorf("workspace archive exceeds %d entries", maxWorkspaceArchiveEntries)
		}
		rel, err := filepath.Rel(workspace, path)
		if err != nil {
			return err
		}
		if !info.IsDir() && !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("unsupported workspace entry %q", rel)
		}
		link := ""
		if info.Mode()&os.ModeSymlink != 0 {
			link, err = os.Readlink(path)
			if err != nil {
				return err
			}
		}
		header, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(rel)
		if err := writer.WriteHeader(header); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			file, openErr := os.Open(path)
			if openErr != nil {
				return openErr
			}
			_, copyErr := io.Copy(writer, file)
			closeErr := file.Close()
			return errors.Join(copyErr, closeErr)
		}
		return nil
	})
	if closeErr := writer.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, fmt.Errorf("environment: archive workspace: %w", err)
	}
	return out.Bytes(), nil
}

type limitedWriter struct {
	writer    io.Writer
	remaining int64
}

func (w *limitedWriter) Write(data []byte) (int, error) {
	if int64(len(data)) > w.remaining {
		return 0, errors.New("workspace archive exceeds configured limit")
	}
	n, err := w.writer.Write(data)
	w.remaining -= int64(n)
	return n, err
}

func restoreWorkspace(workspace string, archive []byte, limit int64) error {
	if limit < 0 {
		return errors.New("environment: E2B checkpoint limit must not be negative")
	}
	parent := filepath.Dir(workspace)
	staging, err := os.MkdirTemp(parent, ".pons-e2b-checkpoint-")
	if err != nil {
		return fmt.Errorf("environment: stage E2B checkpoint: %w", err)
	}
	defer removeWorkspaceTree(staging)
	rootInfo, err := os.Stat(workspace)
	if err != nil {
		return fmt.Errorf("environment: inspect existing workspace: %w", err)
	}
	rootMode := rootInfo.Mode().Perm()
	directoryModes := make(map[string]os.FileMode)
	reader := tar.NewReader(bytes.NewReader(archive))
	var total int64
	entries := 0
	for {
		header, nextErr := reader.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			return fmt.Errorf("environment: read E2B checkpoint: %w", nextErr)
		}
		entries++
		if entries > maxWorkspaceArchiveEntries {
			return fmt.Errorf("environment: E2B checkpoint exceeds %d entries", maxWorkspaceArchiveEntries)
		}
		name := filepath.Clean(filepath.FromSlash(header.Name))
		if name == "." {
			if header.Typeflag != tar.TypeDir {
				return fmt.Errorf("environment: unsupported checkpoint root entry %q", header.Name)
			}
			rootMode = os.FileMode(header.Mode).Perm()
			continue
		}
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return fmt.Errorf("environment: unsafe E2B checkpoint path %q", header.Name)
		}
		target := filepath.Join(staging, name)
		if err := safeCheckpointParent(staging, filepath.Dir(target)); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
			directoryModes[target] = os.FileMode(header.Mode).Perm()
		case tar.TypeReg, 0:
			if header.Size < 0 || total > limit || header.Size > limit-total {
				return fmt.Errorf("environment: E2B checkpoint exceeds %d bytes", limit)
			}
			total += header.Size
			mode := os.FileMode(header.Mode).Perm()
			file, openErr := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if openErr != nil {
				return openErr
			}
			_, copyErr := io.Copy(file, reader)
			chmodErr := file.Chmod(mode)
			closeErr := file.Close()
			if err := errors.Join(copyErr, chmodErr, closeErr); err != nil {
				return err
			}
		case tar.TypeSymlink:
			linkTarget := filepath.Clean(filepath.Join(filepath.Dir(name), filepath.FromSlash(header.Linkname)))
			if filepath.IsAbs(header.Linkname) || linkTarget == ".." || strings.HasPrefix(linkTarget, ".."+string(filepath.Separator)) {
				return fmt.Errorf("environment: unsafe symlink %q", header.Linkname)
			}
			if err := os.Symlink(header.Linkname, target); err != nil {
				return err
			}
		default:
			return fmt.Errorf("environment: unsupported checkpoint entry %q", header.Name)
		}
	}
	paths := make([]string, 0, len(directoryModes))
	for path := range directoryModes {
		paths = append(paths, path)
	}
	sort.Slice(paths, func(i, j int) bool { return len(paths[i]) > len(paths[j]) })
	for _, path := range paths {
		if err := os.Chmod(path, directoryModes[path]); err != nil {
			return fmt.Errorf("environment: restore directory mode: %w", err)
		}
	}
	if err := os.Chmod(staging, rootMode); err != nil {
		return fmt.Errorf("environment: restore workspace mode: %w", err)
	}
	backup := workspace + ".pons-e2b-backup-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	if err := os.Rename(workspace, backup); err != nil {
		return fmt.Errorf("environment: checkpoint existing workspace: %w", err)
	}
	if err := os.Rename(staging, workspace); err != nil {
		installErr := fmt.Errorf("environment: install E2B checkpoint: %w", err)
		if rollbackErr := os.Rename(backup, workspace); rollbackErr != nil {
			return errors.Join(installErr, fmt.Errorf("environment: restore workspace backup %q: %w", backup, rollbackErr))
		}
		return installErr
	}
	if err := removeWorkspaceTree(backup); err != nil {
		return fmt.Errorf("environment: remove workspace backup: %w", err)
	}
	return nil
}

func removeWorkspaceTree(root string) error {
	_ = filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr == nil && info.IsDir() {
			_ = os.Chmod(path, info.Mode().Perm()|0o700)
		}
		return nil
	})
	return os.RemoveAll(root)
}

func validateWorkspaceArchive(archive []byte, limit int64) error {
	parent, err := os.MkdirTemp("", "pons-e2b-checkpoint-validation-")
	if err != nil {
		return fmt.Errorf("environment: validate E2B checkpoint: %w", err)
	}
	workspace := filepath.Join(parent, "workspace")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		_ = os.RemoveAll(parent)
		return fmt.Errorf("environment: validate E2B checkpoint: %w", err)
	}
	defer func() { _ = removeWorkspaceTree(parent) }()
	return restoreWorkspace(workspace, archive, limit)
}

func safeCheckpointParent(root, parent string) error {
	rel, err := filepath.Rel(root, parent)
	if err != nil || rel == "." {
		return err
	}
	current := root
	for part := range strings.SplitSeq(rel, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, statErr := os.Lstat(current)
		if os.IsNotExist(statErr) {
			return nil
		}
		if statErr != nil {
			return statErr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("environment: checkpoint path traverses symlink %q", current)
		}
	}
	return nil
}
