package desktop

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/catalogfs"
	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/cloudflare/artifact-fs/internal/overlay"
	"github.com/jacobsa/fuse/fuseops"
)

func catalogueMetadataLookup(t *testing.T, fs *catalogfs.FileSystem, parent fuseops.InodeID, name string) fuseops.InodeID {
	t.Helper()
	op := &fuseops.LookUpInodeOp{Parent: parent, Name: name}
	if err := fs.LookUpInode(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	return op.Entry.Child
}

func readDesktopCatalogueXattr(t *testing.T, fs *catalogfs.FileSystem, inode fuseops.InodeID) []byte {
	t.Helper()
	op := &fuseops.GetXattrOp{Inode: inode, Name: "user.catalogue", Dst: make([]byte, 128)}
	if err := fs.GetXattr(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	return op.Dst[:op.BytesRead]
}

func TestCatalogueMetadataPersistsAcrossMountAndServiceLifetimesWithoutActivation(t *testing.T) {
	s := newDesktopTestService(t, "exit 4")
	seedDesktopCatalogue(t, s, catalogueRepository("Team", "repo"))
	mount := func(s *Service) *catalogfs.FileSystem {
		t.Helper()
		s.dependencyReady = func() bool { return true }
		s.mountCatalogue = func(context.Context, string, *catalogfs.FileSystem) (fusefs.MountedFS, error) {
			return newDesktopFakeMount(), nil
		}
		if err := s.Mount(context.Background()); err != nil {
			t.Fatal(err)
		}
		return s.catalog
	}
	inodes := func(fs *catalogfs.FileSystem) []fuseops.InodeID {
		owner := catalogueMetadataLookup(t, fs, fuseops.RootInodeID, "Team")
		return []fuseops.InodeID{fuseops.RootInodeID, owner, catalogueMetadataLookup(t, fs, owner, "repo")}
	}
	fs := mount(s)
	values := [][]byte{{0, 0xff, 1}, {0x80, 0}, {2, 0, 0xfe}}
	for i, inode := range inodes(fs) {
		if err := fs.SetXattr(context.Background(), &fuseops.SetXattrOp{Inode: inode, Name: "user.catalogue", Value: values[i]}); err != nil {
			t.Fatal(err)
		}
	}
	assertValues := func(s *Service, fs *catalogfs.FileSystem) {
		t.Helper()
		for i, inode := range inodes(fs) {
			if got := readDesktopCatalogueXattr(t, fs, inode); !bytes.Equal(got, values[i]) {
				t.Fatalf("catalogue object %d lost binary metadata: %x", i, got)
			}
		}
		if repos, err := s.engine.ListRepos(context.Background()); err != nil || len(repos) != 0 {
			t.Fatalf("synthetic attributes activated repository storage: %+v, %v", repos, err)
		}
	}
	assertValues(s, fs)
	oldStore := s.catalogMetadata
	if err := s.Unmount(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := oldStore.HasMetadataXattrs(context.Background()); err == nil {
		t.Fatal("normal unmount left the catalogue database open")
	}
	assertValues(s, mount(s))
	for _, path := range []string{"catalog-metadata", "catalog-metadata/overlay", "catalog-metadata/overlay/upper"} {
		info, err := os.Lstat(filepath.Join(s.opts.StateDir, path))
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 || !ownedByCurrentUser(info) {
			t.Fatalf("metadata directory is not private and owned: %s, %v", path, err)
		}
	}
	database, err := os.Lstat(filepath.Join(s.opts.StateDir, "catalog-metadata", "catalog-overlay.db"))
	if err != nil || !database.Mode().IsRegular() || database.Mode().Perm() != 0o600 {
		t.Fatalf("metadata database permissions: %v", err)
	}
	opts := s.opts
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
	assertValues(restarted, mount(restarted))
}

func TestCatalogueMetadataMountFailureDestroysBeforeClose(t *testing.T) {
	s := newDesktopTestService(t, "exit 4")
	s.dependencyReady = func() bool { return true }
	mountErr := errors.New("mount did not start")
	var store *overlay.Store
	var fs *catalogfs.FileSystem
	var handle fuseops.HandleID
	s.mountCatalogue = func(_ context.Context, _ string, catalogue *catalogfs.FileSystem) (fusefs.MountedFS, error) {
		fs, store = catalogue, s.catalogMetadata
		op := &fuseops.OpenDirOp{Inode: fuseops.RootInodeID}
		if err := fs.OpenDir(context.Background(), op); err != nil {
			t.Fatal(err)
		}
		handle = op.Handle
		return nil, mountErr
	}
	closed := 0
	s.closeCatalogueMetadata = func(metadata *overlay.Store) error {
		closed++
		if err := fs.ReadDir(context.Background(), &fuseops.ReadDirOp{Inode: fuseops.RootInodeID, Handle: handle, Dst: make([]byte, 256)}); err != syscall.EBADF {
			t.Fatalf("database closed before catalogue handles were destroyed: %v", err)
		}
		return metadata.Close()
	}
	if err := s.Mount(context.Background()); !errors.Is(err, mountErr) {
		t.Fatalf("mount result = %v", err)
	}
	if closed != 1 || s.catalog != nil || s.catalogMetadata != nil || s.Status().Mounted {
		t.Fatal("failed mount retained or discarded incorrect lifecycle ownership")
	}
	if _, err := store.HasMetadataXattrs(context.Background()); err == nil {
		t.Fatal("failed mount leaked its catalogue database")
	}
}

type metadataDrainMount struct {
	*desktopFakeMount
	mu       sync.Mutex
	drainErr error
}

func (m *metadataDrainMount) Join(ctx context.Context) error {
	if _, bounded := ctx.Deadline(); bounded {
		m.mu.Lock()
		err := m.drainErr
		m.mu.Unlock()
		if err != nil {
			return err
		}
	}
	if err := m.desktopFakeMount.Join(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.drainErr
}

type metadataObservationFailureMount struct {
	*desktopFakeMount
	err error
}

func (m *metadataObservationFailureMount) Join(ctx context.Context) error {
	if _, bounded := ctx.Deadline(); !bounded {
		return m.err
	}
	return m.desktopFakeMount.Join(ctx)
}

func TestCatalogueMetadataObservationFailureDoesNotProveDetachment(t *testing.T) {
	s := newDesktopTestService(t, "exit 4")
	s.dependencyReady = func() bool { return true }
	observationErr := errors.New("mount ownership inspection failed")
	mt := &metadataObservationFailureMount{desktopFakeMount: newDesktopFakeMount(), err: observationErr}
	s.mountCatalogue = func(context.Context, string, *catalogfs.FileSystem) (fusefs.MountedFS, error) { return mt, nil }
	if err := s.Mount(context.Background()); err != nil {
		t.Fatal(err)
	}
	store := s.catalogMetadata
	awaitDesktop(t, func() bool { return s.Status().Message != "" })
	if !s.Status().Mounted || s.catalogMetadata != store {
		t.Fatal("observation error discarded a possibly live session")
	}
	if _, err := store.HasMetadataXattrs(context.Background()); err != nil {
		t.Fatalf("observation error closed a possibly live database: %v", err)
	}
	if err := s.Unmount(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.HasMetadataXattrs(context.Background()); err == nil {
		t.Fatal("normal detach did not finish database cleanup")
	}
}

func TestCatalogueMetadataRetainedAcrossBusyUnmountAndFailedDrain(t *testing.T) {
	s := newDesktopTestService(t, "exit 4")
	s.dependencyReady = func() bool { return true }
	mt := &metadataDrainMount{desktopFakeMount: newDesktopFakeMount()}
	s.mountCatalogue = func(context.Context, string, *catalogfs.FileSystem) (fusefs.MountedFS, error) { return mt, nil }
	if err := s.Mount(context.Background()); err != nil {
		t.Fatal(err)
	}
	store := s.catalogMetadata
	fs := s.catalog
	if err := fs.SetXattr(context.Background(), &fuseops.SetXattrOp{Inode: fuseops.RootInodeID, Name: "user.catalogue", Value: []byte("retained")}); err != nil {
		t.Fatal(err)
	}
	mt.desktopFakeMount.mu.Lock()
	mt.unmountErr = syscall.EBUSY
	mt.desktopFakeMount.mu.Unlock()
	if err := s.Unmount(context.Background()); !errors.Is(err, syscall.EBUSY) {
		t.Fatalf("busy unmount = %v", err)
	}
	if s.catalogMetadata != store || s.catalog != fs || !s.Status().Mounted {
		t.Fatal("busy unmount discarded catalogue ownership")
	}
	if got := readDesktopCatalogueXattr(t, fs, fuseops.RootInodeID); !bytes.Equal(got, []byte("retained")) {
		t.Fatal("busy unmount made live metadata unavailable")
	}
	mt.desktopFakeMount.mu.Lock()
	mt.unmountErr = nil
	mt.desktopFakeMount.mu.Unlock()
	drainErr := errors.New("catalogue requests still draining")
	mt.mu.Lock()
	mt.drainErr = drainErr
	mt.mu.Unlock()
	if err := s.Unmount(context.Background()); !errors.Is(err, drainErr) {
		t.Fatalf("failed drain = %v", err)
	}
	if s.catalogMetadata != store || s.catalog != fs || !s.Status().Mounted {
		t.Fatal("failed drain discarded catalogue ownership")
	}
	if _, err := store.HasMetadataXattrs(context.Background()); err != nil {
		t.Fatalf("failed drain closed live database: %v", err)
	}
	mt.mu.Lock()
	mt.drainErr = nil
	mt.mu.Unlock()
	if err := s.Unmount(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.HasMetadataXattrs(context.Background()); err == nil {
		t.Fatal("successful drain retained the database")
	}
}

func TestCatalogueMetadataCloseFailureRetainsRetryableOwner(t *testing.T) {
	s := newDesktopTestService(t, "exit 4")
	s.dependencyReady = func() bool { return true }
	s.mountCatalogue = func(context.Context, string, *catalogfs.FileSystem) (fusefs.MountedFS, error) {
		return newDesktopFakeMount(), nil
	}
	if err := s.Mount(context.Background()); err != nil {
		t.Fatal(err)
	}
	store := s.catalogMetadata
	closeErr := errors.New("metadata close failed")
	calls := 0
	s.closeCatalogueMetadata = func(metadata *overlay.Store) error {
		calls++
		if calls == 1 {
			return closeErr
		}
		return metadata.Close()
	}
	if err := s.Close(); !errors.Is(err, closeErr) {
		t.Fatalf("close result = %v", err)
	}
	if s.closed || s.catalogMetadata != store || s.Status().Mounted {
		t.Fatal("failed close lost retryable detached database ownership")
	}
	if err := s.Close(); err != nil || !s.closed || calls != 2 || s.catalogMetadata != nil {
		t.Fatalf("close retry = %v, calls = %d", err, calls)
	}
}

func TestCatalogueMetadataRefusesSymlinkStorageBeforeMount(t *testing.T) {
	for _, component := range []string{"catalog-metadata", "catalog-metadata/catalog-overlay.db", "catalog-metadata/catalog-overlay.db-wal", "catalog-metadata/catalog-overlay.db-journal"} {
		t.Run(component, func(t *testing.T) {
			s := newDesktopTestService(t, "exit 4")
			s.dependencyReady = func() bool { return true }
			s.mountCatalogue = func(context.Context, string, *catalogfs.FileSystem) (fusefs.MountedFS, error) {
				t.Fatal("mount started with unsafe catalogue metadata")
				return nil, nil
			}
			target := filepath.Join(t.TempDir(), "retained")
			if err := os.WriteFile(target, []byte("unchanged"), 0o600); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(s.opts.StateDir, component)
			if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			if err := s.Mount(context.Background()); err == nil {
				t.Fatal("unsafe metadata storage accepted")
			}
			if got, err := os.ReadFile(target); err != nil || !bytes.Equal(got, []byte("unchanged")) {
				t.Fatal("unsafe storage altered the symlink target")
			}
		})
	}
}

func TestCatalogueMetadataRefusesRollbackJournalFIFOWithoutStartingSQLite(t *testing.T) {
	s := newDesktopTestService(t, "exit 4")
	// Populate a real database before replacing its absent rollback journal.
	store, err := openCatalogueMetadata(context.Background(), s.opts.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(s.opts.StateDir, "catalog-metadata", "catalog-overlay.db-journal")
	if err := syscall.Mkfifo(journal, 0o600); err != nil {
		t.Fatal(err)
	}
	s.dependencyReady = func() bool { return true }
	s.mountCatalogue = func(context.Context, string, *catalogfs.FileSystem) (fusefs.MountedFS, error) {
		t.Fatal("mount started with a rollback journal FIFO")
		return nil, nil
	}
	if err := s.Mount(context.Background()); err == nil {
		t.Fatal("rollback journal FIFO was accepted")
	}
	if s.catalogMetadata != nil || s.Status().Mounted {
		t.Fatal("rejected storage acquired a catalogue lifecycle owner")
	}
	info, err := os.Lstat(journal)
	if err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("rejection changed the unsafe journal: %v", err)
	}
}
