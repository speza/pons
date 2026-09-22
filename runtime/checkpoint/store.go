// Package checkpoint persists workspace archives on the local filesystem.
package checkpoint

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/samperrin/pons/environment"
)

// Store keeps content-addressed archives outside source checkouts. Metadata
// stores persist its references, not its filesystem paths.
type Store struct {
	dir string
}

var _ environment.CheckpointStore = (*Store)(nil)

// New configures an archive directory. Directories are created lazily on write.
func New(dir string) *Store { return &Store{dir: dir} }

func (s *Store) PutWorkspaceCheckpoint(ctx context.Context, workspaceID string, archive []byte) (string, error) {
	if workspaceID == "" {
		return "", errors.New("runtime: workspace ID is required")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	digest := sha256.Sum256(archive)
	ref := "sha256:" + hex.EncodeToString(digest[:])
	dir := s.workspaceCheckpointDir(workspaceID)
	if err := mkdirAllDurable(dir, 0o700); err != nil {
		return "", fmt.Errorf("runtime: create workspace checkpoint directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", fmt.Errorf("runtime: secure workspace checkpoint directory: %w", err)
	}
	path := filepath.Join(dir, strings.TrimPrefix(ref, "sha256:")+".tar")
	if _, err := os.Stat(path); err == nil {
		matches, matchErr := checkpointFileMatches(path, digest)
		if matchErr != nil {
			return "", matchErr
		}
		if matches {
			if err := os.Chmod(path, 0o600); err != nil {
				return "", fmt.Errorf("runtime: secure workspace checkpoint: %w", err)
			}
			if err := syncDirectory(dir); err != nil {
				return "", fmt.Errorf("runtime: sync workspace checkpoint directory: %w", err)
			}
			return ref, nil
		}
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("runtime: inspect workspace checkpoint: %w", err)
	}
	file, err := os.CreateTemp(dir, ".checkpoint-*")
	if err != nil {
		return "", fmt.Errorf("runtime: create workspace checkpoint: %w", err)
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return "", fmt.Errorf("runtime: secure workspace checkpoint: %w", err)
	}
	_, writeErr := file.Write(archive)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return "", fmt.Errorf("runtime: write workspace checkpoint: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := os.Rename(temporary, path); err != nil {
		return "", fmt.Errorf("runtime: install workspace checkpoint: %w", err)
	}
	if err := syncDirectory(dir); err != nil {
		return "", fmt.Errorf("runtime: sync workspace checkpoint directory: %w", err)
	}
	return ref, nil
}

func checkpointFileMatches(path string, want [sha256.Size]byte) (bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return false, fmt.Errorf("runtime: open existing workspace checkpoint: %w", err)
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, file)
	closeErr := file.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return false, fmt.Errorf("runtime: verify existing workspace checkpoint: %w", err)
	}
	return bytes.Equal(hash.Sum(nil), want[:]), nil
}

func mkdirAllDurable(path string, perm os.FileMode) error {
	if err := os.MkdirAll(path, perm); err != nil {
		return err
	}
	// Existing directories may be left by an earlier failed installation or a
	// concurrent writer, so existence alone does not establish durability.
	for dir := filepath.Clean(path); ; dir = filepath.Dir(dir) {
		if err := syncDirectory(dir); err != nil {
			return err
		}
		if filepath.Dir(dir) == dir {
			return nil
		}
	}
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

func (s *Store) WorkspaceCheckpoint(
	ctx context.Context,
	workspaceID string,
	ref string,
	limit int64,
) ([]byte, error) {
	if workspaceID == "" || limit < 0 || limit == math.MaxInt64 {
		return nil, errors.New("runtime: invalid workspace checkpoint request")
	}
	path, decoded, err := s.workspaceCheckpointPath(workspaceID, ref)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, environment.ErrStateNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("runtime: open workspace checkpoint: %w", err)
	}
	defer file.Close()
	archive, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, fmt.Errorf("runtime: read workspace checkpoint: %w", err)
	}
	if int64(len(archive)) > limit {
		return nil, fmt.Errorf("runtime: workspace checkpoint exceeds %d bytes", limit)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	actual := sha256.Sum256(archive)
	if !bytes.Equal(actual[:], decoded) {
		return nil, errors.New("runtime: workspace checkpoint digest mismatch")
	}
	return archive, nil
}

func (s *Store) PruneWorkspaceCheckpoints(ctx context.Context, workspaceID string, keepRefs []string) error {
	if workspaceID == "" {
		return errors.New("runtime: workspace ID is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	keep := make(map[string]struct{}, len(keepRefs))
	for _, ref := range keepRefs {
		path, _, err := s.workspaceCheckpointPath(workspaceID, ref)
		if err != nil {
			return err
		}
		keep[filepath.Base(path)] = struct{}{}
	}
	dir := s.workspaceCheckpointDir(workspaceID)
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("runtime: list workspace checkpoints: %w", err)
	}
	removed := false
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, retained := keep[entry.Name()]; retained || !strings.HasSuffix(entry.Name(), ".tar") {
			continue
		}
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("runtime: prune workspace checkpoint: %w", err)
		}
		removed = true
	}
	if removed {
		if err := syncDirectory(dir); err != nil {
			return fmt.Errorf("runtime: sync workspace checkpoint pruning: %w", err)
		}
	}
	return nil
}

func (s *Store) workspaceCheckpointPath(workspaceID, ref string) (string, []byte, error) {
	if workspaceID == "" {
		return "", nil, errors.New("runtime: workspace ID is required")
	}
	algorithm, encoded, ok := strings.Cut(ref, ":")
	decoded, err := hex.DecodeString(encoded)
	if !ok || algorithm != "sha256" || err != nil || len(decoded) != sha256.Size {
		return "", nil, errors.New("runtime: invalid workspace checkpoint reference")
	}
	return filepath.Join(s.workspaceCheckpointDir(workspaceID), encoded+".tar"), decoded, nil
}

func (s *Store) workspaceCheckpointDir(workspaceID string) string {
	digest := sha256.Sum256([]byte(workspaceID))
	return filepath.Join(s.dir, hex.EncodeToString(digest[:]))
}
