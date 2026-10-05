package overlay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/model"
)

func TestCreateFileOpenedWritesRestrictiveModes(t *testing.T) {
	for _, mode := range []uint32{0o400, 0} {
		t.Run(fmt.Sprintf("%03o", mode), func(t *testing.T) {
			s, _ := testStore(t)
			ctx := context.Background()
			created, file, err := s.CreateFileOpened(ctx, "restricted.bin", mode)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			data := []byte{0xff, 0, 0x80, 0x01}
			if n, err := s.WriteFileFrom(ctx, created.Path, 0, data, file); err != nil || n != len(data) {
				t.Fatalf("retained write = %d, %v", n, err)
			}
			if err := s.TruncateFrom(ctx, created.Path, int64(len(data)-1), file); err != nil {
				t.Fatalf("truncate retained descriptor: %v", err)
			}
			data = data[:len(data)-1]
			if err := s.SyncFileFrom(ctx, created.Path, file); err != nil {
				t.Fatalf("sync retained descriptor: %v", err)
			}
			assertOpenedFileContents(t, file, data)
			st, err := os.Stat(created.BackingPath)
			if err != nil {
				t.Fatal(err)
			}
			if st.Mode().Perm() != os.FileMode(mode) {
				t.Fatalf("backing mode = %03o, want %03o", st.Mode().Perm(), mode)
			}
			got, ok := s.Get(created.Path)
			if !ok || got.Mode != mode || got.SizeBytes != int64(len(data)) || got.Kind != model.OverlayKindCreate {
				t.Fatalf("written metadata = %+v, exists=%v", got, ok)
			}
			if got.MtimeUnixNs <= created.MtimeUnixNs || got.CtimeUnixNs <= created.CtimeUnixNs {
				t.Fatalf("write did not advance timestamps: before=%+v after=%+v", created, got)
			}
		})
	}
}

func TestCreateFileOpenedFailedPublicationCleansBacking(t *testing.T) {
	s, _ := testStore(t)
	before, err := os.ReadDir(s.upperDir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	entry, file, err := s.CreateFileOpened(ctx, "cancelled.bin", 0)
	if file != nil {
		defer file.Close()
	}
	if !errors.Is(err, context.Canceled) || file != nil || entry != (model.OverlayEntry{}) {
		t.Fatalf("create cancelled context = %+v, %v, %v", entry, file, err)
	}
	after, err := os.ReadDir(s.upperDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("failed publication left backing files: before=%v after=%v", before, after)
	}
	if entries, err := s.ListByPrefix(context.Background(), ""); err != nil || len(entries) != 0 {
		t.Fatalf("failed publication left metadata: entries=%+v err=%v", entries, err)
	}
}

func TestRetainedFileDescriptorWritesAndTruncatesAfterChmod(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	created, file, err := s.CreateFileOpened(ctx, "opened.bin", 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := s.WriteFileFrom(ctx, created.Path, 0, []byte{1, 2, 3, 4}, file); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMode(ctx, created.Path, 0o400); err != nil {
		t.Fatal(err)
	}
	before, _ := s.Get(created.Path)
	if err := s.TruncateFrom(ctx, created.Path, 2, file); err != nil {
		t.Fatalf("truncate retained descriptor after chmod: %v", err)
	}
	truncated, _ := s.Get(created.Path)
	if truncated.SizeBytes != 2 || truncated.MtimeUnixNs <= before.MtimeUnixNs || truncated.CtimeUnixNs <= before.CtimeUnixNs {
		t.Fatalf("truncate metadata = %+v, before=%+v", truncated, before)
	}
	if n, err := s.WriteFileFrom(ctx, created.Path, 4, []byte{0xff, 0}, file); err != nil || n != 2 {
		t.Fatalf("write retained descriptor after chmod = %d, %v", n, err)
	}
	if err := s.SyncFile(ctx, created.Path); err != nil {
		t.Fatalf("sync after retained mutation: %v", err)
	}
	assertOpenedFileContents(t, file, []byte{1, 2, 0, 0, 0xff, 0})
	st, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(created.Path)
	if got.SizeBytes != 6 || got.Mode != 0o400 || st.Mode().Perm() != 0o400 {
		t.Fatalf("post-chmod metadata = %+v, physical mode=%03o", got, st.Mode().Perm())
	}
}

func TestRetainedFileMutationRejectsWrongDescriptor(t *testing.T) {
	for _, differentStore := range []bool{false, true} {
		t.Run(fmt.Sprintf("different_store_%v", differentStore), func(t *testing.T) {
			s, _ := testStore(t)
			other := s
			if differentStore {
				other, _ = testStore(t)
			}
			ctx := context.Background()
			victim, victimFile, err := s.CreateFileOpened(ctx, "victim.bin", 0o644)
			if err != nil {
				t.Fatal(err)
			}
			defer victimFile.Close()
			sentinel, sentinelFile, err := other.CreateFileOpened(ctx, "sentinel.bin", 0o644)
			if err != nil {
				t.Fatal(err)
			}
			defer sentinelFile.Close()
			victimData := []byte{1, 2, 3, 4}
			sentinelData := []byte{5, 6, 7, 8}
			if _, err := s.WriteFileFrom(ctx, victim.Path, 0, victimData, victimFile); err != nil {
				t.Fatal(err)
			}
			if _, err := other.WriteFileFrom(ctx, sentinel.Path, 0, sentinelData, sentinelFile); err != nil {
				t.Fatal(err)
			}
			before, _ := s.Get(victim.Path)
			sentinelBefore, _ := other.Get(sentinel.Path)
			if n, err := s.WriteFileFrom(ctx, victim.Path, 0, []byte{9}, sentinelFile); n != 0 || !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("write wrong descriptor = %d, %v", n, err)
			}
			if err := s.TruncateFrom(ctx, victim.Path, 1, sentinelFile); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("truncate wrong descriptor = %v", err)
			}
			if err := s.SyncFileFrom(ctx, victim.Path, sentinelFile); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("sync wrong descriptor = %v", err)
			}
			assertOpenedFileContents(t, victimFile, victimData)
			assertOpenedFileContents(t, sentinelFile, sentinelData)
			if after, _ := s.Get(victim.Path); after != before {
				t.Fatalf("wrong descriptor changed victim metadata: before=%+v after=%+v", before, after)
			}
			if after, _ := other.Get(sentinel.Path); after != sentinelBefore {
				t.Fatalf("wrong descriptor changed sentinel metadata: before=%+v after=%+v", sentinelBefore, after)
			}
		})
	}
}

func TestRetainedFileMutationRejectsDetachedNamespace(t *testing.T) {
	for _, mutation := range []string{"remove", "forget", "replace_entry", "replace_backing"} {
		t.Run(mutation, func(t *testing.T) {
			s, _ := testStore(t)
			ctx := context.Background()
			created, file, err := s.CreateFileOpened(ctx, "file.bin", 0o644)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			oldData := []byte{1, 2, 3, 4}
			if _, err := s.WriteFileFrom(ctx, created.Path, 0, oldData, file); err != nil {
				t.Fatal(err)
			}
			var replacement *os.File
			switch mutation {
			case "remove":
				err = s.Remove(ctx, created.Path)
			case "forget":
				_, err = s.db.ExecContext(ctx, `DELETE FROM overlay_entries WHERE path=?`, created.Path)
			case "replace_entry":
				_, replacement, err = s.CreateFileOpened(ctx, created.Path, 0o644)
			case "replace_backing":
				replacement, err = os.CreateTemp(s.upperDir, ".replacement-*")
				if err == nil {
					err = os.Rename(replacement.Name(), created.BackingPath)
				}
			}
			if replacement != nil {
				defer replacement.Close()
			}
			if err != nil {
				t.Fatal(err)
			}
			before, existed := s.Get(created.Path)
			if n, err := s.WriteFileFrom(ctx, created.Path, 0, []byte{9}, file); n != 0 || !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("write detached descriptor = %d, %v", n, err)
			}
			if err := s.TruncateFrom(ctx, created.Path, 1, file); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("truncate detached descriptor = %v", err)
			}
			if err := s.SyncFileFrom(ctx, created.Path, file); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("sync detached descriptor = %v", err)
			}
			assertOpenedFileContents(t, file, oldData)
			if replacement != nil {
				assertOpenedFileContents(t, replacement, nil)
			}
			if after, exists := s.Get(created.Path); exists != existed || after != before {
				t.Fatalf("detached mutation changed namespace metadata: before=%+v exists=%v after=%+v exists=%v", before, existed, after, exists)
			}
		})
	}
}

func TestRetainedFileMutationRejectsInvalidRanges(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	created, file, err := s.CreateFileOpened(ctx, "file.bin", 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	for _, offset := range []int64{-1, math.MaxInt64} {
		if n, err := s.WriteFileFrom(ctx, created.Path, offset, []byte{1}, file); n != 0 || !errors.Is(err, os.ErrInvalid) {
			t.Fatalf("write at %d = %d, %v", offset, n, err)
		}
	}
	if err := s.TruncateFrom(ctx, created.Path, -1, file); !errors.Is(err, os.ErrInvalid) {
		t.Fatalf("negative truncate = %v", err)
	}
	if n, err := s.WriteFileFrom(ctx, created.Path, 0, []byte{1}, nil); n != 0 || !errors.Is(err, os.ErrInvalid) {
		t.Fatalf("write nil descriptor = %d, %v", n, err)
	}
	if err := s.TruncateFrom(ctx, created.Path, 1, nil); !errors.Is(err, os.ErrInvalid) {
		t.Fatalf("truncate nil descriptor = %v", err)
	}
	if err := s.SyncFileFrom(ctx, created.Path, nil); !errors.Is(err, os.ErrInvalid) {
		t.Fatalf("sync nil descriptor = %v", err)
	}
	assertOpenedFileContents(t, file, nil)
	if after, _ := s.Get(created.Path); after != created {
		t.Fatalf("invalid range changed metadata: before=%+v after=%+v", created, after)
	}
}

func TestSyncFileFromAfterChmodZero(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	created, file, err := s.CreateFileOpened(ctx, "restricted.bin", 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := s.SetMode(ctx, created.Path, 0); err != nil {
		t.Fatal(err)
	}
	if n, err := s.WriteFileFrom(ctx, created.Path, 0, []byte{0xff, 0, 0x80}, file); n != 3 || err != nil {
		t.Fatalf("retained write after chmod zero = %d, %v", n, err)
	}
	if err := s.TruncateFrom(ctx, created.Path, 2, file); err != nil {
		t.Fatalf("retained truncate after chmod zero: %v", err)
	}
	before, _ := s.Get(created.Path)
	if err := s.SyncFileFrom(ctx, created.Path, file); err != nil {
		t.Fatalf("retained sync after chmod zero: %v", err)
	}
	assertOpenedFileContents(t, file, []byte{0xff, 0})
	st, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0 {
		t.Fatalf("retained sync changed backing mode: %03o", st.Mode().Perm())
	}
	if after, _ := s.Get(created.Path); after != before {
		t.Fatalf("sync changed metadata: before=%+v after=%+v", before, after)
	}
}

func assertOpenedFileContents(t *testing.T, file *os.File, want []byte) {
	t.Helper()
	st, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, st.Size())
	if len(data) != 0 {
		if _, err := file.ReadAt(data, 0); err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(data, want) {
		t.Fatalf("opened file bytes = %v, want %v", data, want)
	}
}
