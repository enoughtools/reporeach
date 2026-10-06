package desktop

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/gitstore"
)

func previewEvictionFixture(t *testing.T) (*Service, Repository, *RepositoryPreview, []byte) {
	t.Helper()
	s := newDesktopTestService(t, "exit 4")
	repo, _, binary := previewContentFixture(t)
	repo.State = "virtual"
	seedDesktopCatalogue(t, s, repo)
	if s.preview == nil {
		s.previewGit = gitstore.New(nil)
		s.preview = previewContentCache(t, s.opts.StateDir, s.previewGit)
	}
	preview, err := s.preview.Acquire(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	_ = readPreviewContent(t, preview, "binary.dat")
	s.notePreviewCachedBytes(repo.ID)
	return s, repo, preview, binary
}

func TestPreviewEvictionVirtualFreeOfflineWithoutWritableRegistration(t *testing.T) {
	s, repo, preview, binary := previewEvictionFixture(t)
	if got := s.Status().Repositories[0]; got.State != "virtual" || got.DownloadedBytes != int64(len(binary)) {
		t.Fatalf("preview accounting changed virtual state: %+v", got)
	}
	// This cannot be reached by Git. Free must use the already verified local
	// generation and keep the durable selected tree available offline.
	s.mu.Lock()
	s.state.Repositories[0].CloneURL = "https://example.invalid/offline/project.git"
	s.mu.Unlock()
	if err := s.freeRepository(context.Background(), repo.ID); err != nil {
		t.Fatal(err)
	}
	configs, err := s.engine.ListRepos(context.Background())
	if err != nil || len(configs) != 0 {
		t.Fatalf("cache-only Free activated writable storage: %+v %v", configs, err)
	}
	for _, path := range []string{preview.contentGitRoot(), preview.contentBlobDirectory(), filepath.Join(s.opts.StateDir, previewEvictionJournalName)} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("cache bytes or active receipt remain at %s: %v", path, err)
		}
	}
	if _, _, err := preview.Directory(context.Background(), "."); err != nil {
		t.Fatalf("Free discarded the offline metadata baseline: %v", err)
	}
	if got := s.Status().Repositories[0]; got.State != "virtual" || got.DownloadedBytes != 0 {
		t.Fatalf("cache-only Free left stale state: %+v", got)
	}
}

func TestPreviewEvictionRefusesUnknownGitDataBeforeKeepAndFree(t *testing.T) {
	s, repo, preview, _ := previewEvictionFixture(t)
	release, err := s.pausePreviewContent(context.Background(), repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	note := filepath.Join(preview.contentConfig().GitDir, "personal-note.bin")
	writeCheckoutFixture(t, note, []byte{0, 255, 33}, 0600)
	if err := s.verifyPreviewContentOwnership(context.Background(), repo.ID, false); err == nil {
		t.Fatal("Keep would adopt a user file into its disposable storage proof")
	}
	if err := s.evictPreviewContent(context.Background(), repo.ID, true); err == nil {
		t.Fatal("Free accepted user-added Git metadata")
	}
	if data, err := os.ReadFile(note); err != nil || len(data) != 3 {
		t.Fatal("unowned Git metadata was not retained")
	}
}

func TestPreviewEvictionRefusesUnreceiptedValidOIDAndReplacedBlob(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(fmt.Sprint(replace), func(t *testing.T) {
			s, repo, preview, binary := previewEvictionFixture(t)
			release, err := s.pausePreviewContent(context.Background(), repo.ID)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			data := []byte{0, 12, 255}
			if replace {
				data = binary
			}
			hash := sha1.New()
			fmt.Fprintf(hash, "blob %d\x00", len(data))
			hash.Write(data)
			path := filepath.Join(preview.contentBlobDirectory(), hex.EncodeToString(hash.Sum(nil)))
			if replace {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			writeCheckoutFixture(t, path, data, 0600)
			if err := s.evictPreviewContent(context.Background(), repo.ID, true); err == nil {
				t.Fatal("unowned valid blob was silently adopted")
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatal("unowned valid blob was deleted")
			}
		})
	}
}

func TestPreviewEvictionHeldReaderRefusesThenSucceeds(t *testing.T) {
	s, repo, preview, _ := previewEvictionFixture(t)
	file, err := preview.OpenContent(context.Background(), "binary.dat")
	if err != nil {
		t.Fatal(err)
	}
	release, err := s.pausePreviewContent(context.Background(), repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := s.evictPreviewContent(context.Background(), repo.ID, true); err == nil {
		file.Close()
		t.Fatal("cache with an open reader was removed")
	}
	if _, err := io.ReadAll(file); err != nil {
		t.Fatal("refused eviction damaged its reader")
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.evictPreviewContent(context.Background(), repo.ID, true); err != nil {
		t.Fatal(err)
	}
}

func TestPreviewEvictionWritablePromotionSharesBlobCacheAndFreeOwnsBoth(t *testing.T) {
	s, repo, preview, _ := previewEvictionFixture(t)
	s.hybridCatalogue = true
	if _, err := s.ensureRepository(context.Background(), repo.ID); err != nil {
		t.Fatal(err)
	}
	configs, err := s.engine.ListRepos(context.Background())
	if err != nil || len(configs) != 1 || configs[0].BlobCacheDir != preview.contentBlobDirectory() {
		t.Fatalf("promotion did not retain its canonical cache: %+v %v", configs, err)
	}
	if _, err := os.Stat(configs[0].GitDir); err != nil {
		t.Fatalf("preview-git sibling prevented normal writable preparation: %v", err)
	}
	// A normal engine hydrator can add OIDs which are absent from the preview
	// producer's receipt. Registered Free delegates shared-cache ownership to
	// the daemon, after removing only the proved preview Git subtree.
	_, nodes, err := preview.Directory(context.Background(), ".")
	if err != nil || len(nodes) == 0 {
		t.Fatalf("missing promoted preview nodes: %v", err)
	}
	var oid string
	for _, node := range nodes {
		if node.Path == "link" {
			oid = node.ObjectOID
		}
	}
	if oid == "" {
		t.Fatal("fixture link blob is absent")
	}
	if _, err := s.previewGit.BlobToCache(context.Background(), configs[0], oid, filepath.Join(configs[0].BlobCacheDir, oid)); err != nil {
		t.Fatal(err)
	}
	if err := s.freeRepository(context.Background(), repo.ID); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{preview.contentGitRoot(), preview.contentBlobDirectory(), configs[0].GitDir} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("promoted Free retained physical content at %s: %v", path, err)
		}
	}
}

func TestPreviewEvictionUnsupportedFilterRetainsMetadataAndAccountsBytes(t *testing.T) {
	s := newDesktopTestService(t, "exit 4")
	repo, source, _ := previewContentFixture(t)
	adoptionGit(t, source, "config", "uploadpack.allowFilter", "false")
	repo.State = "virtual"
	seedDesktopCatalogue(t, s, repo)
	if s.preview == nil {
		s.previewGit = gitstore.New(nil)
		s.preview = previewContentCache(t, s.opts.StateDir, s.previewGit)
	}
	preview, err := s.preview.Acquire(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	_ = readPreviewContent(t, preview, "binary.dat")
	s.notePreviewCachedBytes(repo.ID)
	retained, err := retainedPreviewMetadataBlobBytes(context.Background(), s.opts.StateDir, repo)
	if err != nil || retained == 0 {
		t.Fatalf("unsupported-filter fixture acquired no metadata blobs: %d %v", retained, err)
	}
	if got := s.Status().Repositories[0].DownloadedBytes; got <= retained {
		t.Fatalf("preserved metadata bytes were absent from downloaded accounting: %d <= %d", got, retained)
	}
	if err := s.freeRepository(context.Background(), repo.ID); err == nil {
		t.Fatal("Free claimed success while preserved metadata held content blobs")
	}
	if _, err := os.Stat(preview.contentBlobDirectory()); err != nil {
		t.Fatal("failed unsupported-source Free removed cached content")
	}
}

func TestPreviewEvictionAccountingReusesMemoryOnCachedReads(t *testing.T) {
	s, repo, preview, binary := previewEvictionFixture(t)
	file, err := preview.OpenContent(context.Background(), "binary.dat")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	for range 3 {
		s.notePreviewCachedFile(repo.ID, file)
	}
	if got := s.Status().Repositories[0].DownloadedBytes; got != int64(len(binary)) {
		t.Fatalf("cached reads counted one OID repeatedly: %d", got)
	}
	// Status and the read-accounting callback keep this known value even if a
	// directory cannot be inventoried. They never rescan the canonical cache.
	if err := os.Chmod(preview.contentBlobDirectory(), 0000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(preview.contentBlobDirectory(), 0700)
	s.notePreviewCachedFile(repo.ID, file)
	if got := s.Status().Repositories[0].DownloadedBytes; got != int64(len(binary)) {
		t.Fatal("cached file accounting depended on a directory inventory")
	}
}

func fixturePreviewEvictionJournal(t *testing.T, s *Service, repo Repository, preview *RepositoryPreview) previewEvictionJournal {
	t.Helper()
	parent := filepath.Join(s.opts.StateDir, "preview-content-evictions")
	if err := handoffPrivateDirectory(parent, true); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(parent, "eviction-fixture")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	identity, err := handoffReadIdentity(root)
	if err != nil {
		t.Fatal(err)
	}
	journal := previewEvictionJournal{Version: 1, RepositoryID: repo.ID, Root: root, Identity: identity}
	for index, path := range []string{preview.contentGitRoot(), preview.contentBlobDirectory()} {
		object, err := capturePreviewCleanupObject(context.Background(), path)
		if err != nil {
			t.Fatal(err)
		}
		object.Root = filepath.Join(root, fmt.Sprint(index))
		journal.Objects = append(journal.Objects, previewEvictionObject{Original: path, Cleanup: object})
	}
	if err := handoffWriteJSON(filepath.Join(s.opts.StateDir, previewEvictionJournalName), journal, true); err != nil {
		t.Fatal(err)
	}
	return journal
}

func TestPreviewEvictionRecoversPartialGitAndBlobCleanup(t *testing.T) {
	for _, index := range []int{0, 1} {
		t.Run(fmt.Sprint(index), func(t *testing.T) {
			s, repo, preview, _ := previewEvictionFixture(t)
			release, err := s.pausePreviewContent(context.Background(), repo.ID)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			journal := fixturePreviewEvictionJournal(t, s, repo, preview)
			object := journal.Objects[index]
			if err := handoffRenameOwnedObject(object.Original, object.Cleanup.Root, object.Cleanup.Identity); err != nil {
				t.Fatal(err)
			}
			injected := errors.New("interrupted after one unlink")
			if err := handoffRemovePlannedObject(context.Background(), object.Cleanup, func() error { return injected }); !errors.Is(err, injected) {
				t.Fatalf("partial cleanup injection failed: %v", err)
			}
			if err := s.recoverPreviewContentEviction(context.Background()); err != nil {
				t.Fatal(err)
			}
			for _, object := range journal.Objects {
				if _, err := os.Stat(object.Original); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("recovery left original cache data")
				}
			}
			if _, err := os.Stat(journal.Root); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("recovery left quarantined cache data")
			}
		})
	}
}

func TestPreviewEvictionLargeJournalRoundTrip(t *testing.T) {
	s, repo, preview, _ := previewEvictionFixture(t)
	journal := fixturePreviewEvictionJournal(t, s, repo, preview)
	object := &journal.Objects[0].Cleanup
	for i := range 7000 {
		object.Entries = append(object.Entries, localHandoffCleanupEntry{
			PathRaw:  base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("generated-%06d", i))),
			Identity: object.Identity, Proof: hex.EncodeToString(make([]byte, 32)),
		})
	}
	path := filepath.Join(s.opts.StateDir, previewEvictionJournalName)
	if err := handoffWriteJSON(path, journal, false); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Size() <= 1<<20 {
		t.Fatal("fixture did not exceed the ordinary ownership-record limit")
	}
	var loaded previewEvictionJournal
	if err := handoffReadJSON(path, &loaded); err != nil {
		t.Fatal(err)
	}
	if len(loaded.Objects[0].Cleanup.Entries) != len(object.Entries) {
		t.Fatal("large cleanup plan was truncated")
	}
	if err := s.removePreviewEvictionJournal(); err != nil {
		t.Fatal(err)
	}
}

func TestPreviewEvictionRecoveryRetainsNewQuarantineData(t *testing.T) {
	s, repo, preview, _ := previewEvictionFixture(t)
	release, err := s.pausePreviewContent(context.Background(), repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	journal := fixturePreviewEvictionJournal(t, s, repo, preview)
	object := journal.Objects[0]
	if err := handoffRenameOwnedObject(object.Original, object.Cleanup.Root, object.Cleanup.Identity); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(object.Cleanup.Root, "new-user-data")
	writeCheckoutFixture(t, foreign, []byte{0, 255, 42}, 0600)
	if err := s.recoverPreviewContentEviction(context.Background()); err == nil {
		t.Fatal("recovery adopted a new quarantine file")
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatal("recovery deleted data added after the cleanup commit")
	}
	if _, err := os.Stat(filepath.Join(s.opts.StateDir, previewEvictionJournalName)); err != nil {
		t.Fatal("refused cleanup lost its durable receipt")
	}
}

func TestPreviewEvictionMaterializedFreeResetsSameOIDAccounting(t *testing.T) {
	s, repo, cfg, stage := handoffFixture(t)
	if err := s.commitMaterializedCheckout(context.Background(), repo.ID, cfg, stage); err != nil {
		t.Fatal(err)
	}
	prepareHandoffRemoteBackup(t, s, repo, stage)
	data := []byte{0, 255, 1}
	hash := sha1.New()
	fmt.Fprintf(hash, "blob %d\x00", len(data))
	hash.Write(data)
	oid := hex.EncodeToString(hash.Sum(nil))
	s.previewBlobSizes = map[string]map[string]int64{repo.ID: {oid: int64(len(data))}}
	s.previewMetaCounts = map[string]previewMetadataAccounting{repo.ID: {Bytes: 13}}
	if err := s.freeMaterializedCheckout(context.Background(), repo.ID); err != nil {
		t.Fatal(err)
	}
	if _, exists := s.previewBlobSizes[repo.ID]; exists {
		t.Fatal("materialized Free retained the old cache accounting generation")
	}
	root := filepath.Join(s.opts.StateDir, "engine", "cache", "blobs", engineName(repo.ID))
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, oid)
	writeCheckoutFixture(t, path, data, 0600)
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	s.notePreviewCachedFile(repo.ID, file)
	if got := s.Status().Repositories[0].DownloadedBytes; got != int64(len(data)) {
		t.Fatalf("same OID was not counted in its new cache generation: %d", got)
	}
}

func TestPreviewEvictionCachedReadWithoutSourceLeavesReclaimableEmptyRoot(t *testing.T) {
	s, repo, preview, _ := previewEvictionFixture(t)
	release, err := s.pausePreviewContent(context.Background(), repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.evictPreviewContent(context.Background(), repo.ID, false); err != nil {
		t.Fatal(err)
	}
	release()
	_ = readPreviewContent(t, preview, "binary.dat")
	entries, err := os.ReadDir(preview.contentGitRoot())
	if err != nil || len(entries) != 0 {
		t.Fatalf("cached read created unreceipted source parents: %v %v", entries, err)
	}
	release, err = s.pausePreviewContent(context.Background(), repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := s.evictPreviewContent(context.Background(), repo.ID, true); err != nil {
		t.Fatalf("cached-only generation could not be reclaimed: %v", err)
	}
}
