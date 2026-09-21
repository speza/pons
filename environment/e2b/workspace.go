package e2b

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func archiveWorkspace(workspace string, limit int64) ([]byte, error) {
	var out bytes.Buffer
	writer := tar.NewWriter(&limitedWriter{writer: &out, remaining: limit})
	err := filepath.Walk(workspace, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(workspace, path)
		if err != nil || rel == "." {
			return err
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
	parent := filepath.Dir(workspace)
	staging, err := os.MkdirTemp(parent, ".pons-e2b-checkpoint-")
	if err != nil {
		return fmt.Errorf("environment: stage E2B checkpoint: %w", err)
	}
	defer os.RemoveAll(staging)
	reader := tar.NewReader(bytes.NewReader(archive))
	var total int64
	for {
		header, nextErr := reader.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			return fmt.Errorf("environment: read E2B checkpoint: %w", nextErr)
		}
		name := filepath.Clean(filepath.FromSlash(header.Name))
		if name == "." {
			continue
		}
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return fmt.Errorf("environment: unsafe E2B checkpoint path %q", header.Name)
		}
		target := filepath.Join(staging, name)
		if err := safeCheckpointParent(staging, filepath.Dir(target)); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(header.Mode).Perm()); err != nil {
				return err
			}
		case tar.TypeReg:
			total += header.Size
			if total > limit {
				return fmt.Errorf("environment: E2B checkpoint exceeds %d bytes", limit)
			}
			file, openErr := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(header.Mode).Perm())
			if openErr != nil {
				return openErr
			}
			_, copyErr := io.Copy(file, reader)
			closeErr := file.Close()
			if err := errors.Join(copyErr, closeErr); err != nil {
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
	backup := workspace + ".pons-e2b-backup-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	if err := os.Rename(workspace, backup); err != nil {
		return fmt.Errorf("environment: checkpoint existing workspace: %w", err)
	}
	if err := os.Rename(staging, workspace); err != nil {
		_ = os.Rename(backup, workspace)
		return fmt.Errorf("environment: install E2B checkpoint: %w", err)
	}
	if err := os.RemoveAll(backup); err != nil {
		return fmt.Errorf("environment: remove workspace backup: %w", err)
	}
	return nil
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
