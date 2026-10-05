package overlay

import (
	"context"
	"fmt"
	"math"
	"os"
	"time"

	"github.com/cloudflare/artifact-fs/internal/model"
)

// CreateFileOpened retains the writable descriptor opened before applying the
// requested mode. The caller owns it, including when the mode forbids reopening
// the backing file for writing.
func (s *Store) CreateFileOpened(ctx context.Context, path string, mode uint32) (model.OverlayEntry, *os.File, error) {
	return s.createFileOpened(ctx, path, mode, true)
}

func (s *Store) createFileOpened(ctx context.Context, path string, mode uint32, retain bool) (model.OverlayEntry, *os.File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.CreateTemp(s.upperDir, ".artifact-fs-entry-*")
	if err != nil {
		return model.OverlayEntry{}, nil, err
	}
	backing := f.Name()
	published := false
	defer func() {
		if !published {
			if f != nil {
				_ = f.Close()
			}
			_ = os.Remove(backing)
		}
	}()
	if err := f.Chmod(os.FileMode(mode)); err != nil {
		return model.OverlayEntry{}, nil, err
	}
	if !retain {
		if err := f.Close(); err != nil {
			return model.OverlayEntry{}, nil, err
		}
		f = nil
	}
	now := time.Now().UnixNano()
	e := model.OverlayEntry{RepoID: s.repo.ID, Path: model.CleanPath(path), Kind: model.OverlayKindCreate, BackingPath: backing, Mode: mode, MtimeUnixNs: now, CtimeUnixNs: now}
	if err := s.upsertEntry(ctx, e); err != nil {
		return model.OverlayEntry{}, nil, err
	}
	published = true
	return e, f, nil
}

// WriteFileFrom uses an already opened descriptor while the overlay namespace
// still points to that descriptor's file. The lock also keeps reconciliation
// and rename from changing the entry between checking it and publishing size.
func (s *Store) WriteFileFrom(ctx context.Context, path string, off int64, data []byte, file *os.File) (int, error) {
	if err := validateWriteOffset(off, len(data)); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, err := s.entryForOpenedFileLocked(ctx, path, file)
	if err != nil {
		return 0, err
	}
	return s.writeFileOpenedLocked(ctx, e, off, data, file)
}

// SyncFileFrom retains open-time access when the current mode prevents reopening
// the backing file. Syncing the upper directory makes its namespace durable too.
func (s *Store) SyncFileFrom(ctx context.Context, path string, file *os.File) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.entryForOpenedFileLocked(ctx, path, file); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	dir, err := os.Open(s.upperDir)
	if err != nil {
		return err
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return err
	}
	return dir.Close()
}

// TruncateFrom applies truncation through a retained descriptor without
// reopening a backing file whose permissions may have changed since open.
func (s *Store) TruncateFrom(ctx context.Context, path string, size int64, file *os.File) error {
	if size < 0 {
		return os.ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, err := s.entryForOpenedFileLocked(ctx, path, file)
	if err != nil {
		return err
	}
	if err := file.Truncate(size); err != nil {
		return err
	}
	return s.publishFileChangeLocked(ctx, e, size)
}

func validateWriteOffset(off int64, size int) error {
	if off < 0 || int64(size) > math.MaxInt64-off {
		return os.ErrInvalid
	}
	return nil
}

// entryForOpenedFileLocked never creates or republishes an absent entry. A
// descriptor from another path, a replacement, or an unlinked file must not be
// allowed to modify bytes while publishing metadata for the current entry.
func (s *Store) entryForOpenedFileLocked(ctx context.Context, path string, file *os.File) (model.OverlayEntry, error) {
	if file == nil {
		return model.OverlayEntry{}, os.ErrInvalid
	}
	e, ok, err := s.Lookup(ctx, path)
	if err != nil {
		return model.OverlayEntry{}, err
	}
	if !ok || e.IsDeleted() || e.NodeType() != "file" || e.BackingPath == "" {
		return model.OverlayEntry{}, os.ErrNotExist
	}
	opened, err := file.Stat()
	if err != nil {
		return model.OverlayEntry{}, err
	}
	current, err := os.Stat(e.BackingPath)
	if err != nil {
		return model.OverlayEntry{}, err
	}
	if !os.SameFile(opened, current) {
		return model.OverlayEntry{}, fmt.Errorf("overlay backing file changed at %s: %w", e.Path, os.ErrNotExist)
	}
	return e, nil
}

func (s *Store) writeFileOpenedLocked(ctx context.Context, e model.OverlayEntry, off int64, data []byte, file *os.File) (int, error) {
	n, err := file.WriteAt(data, off)
	if err != nil {
		return n, err
	}
	st, err := file.Stat()
	if err != nil {
		return n, err
	}
	return n, s.publishFileChangeLocked(ctx, e, st.Size())
}

func (s *Store) publishFileChangeLocked(ctx context.Context, e model.OverlayEntry, size int64) error {
	now := time.Now().UnixNano()
	e.SizeBytes = size
	e.MtimeUnixNs = now
	e.CtimeUnixNs = now
	if e.Kind != model.OverlayKindCreate {
		e.Kind = model.OverlayKindModify
	}
	return s.upsertEntry(ctx, e)
}
