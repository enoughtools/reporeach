package desktop

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/cloudflare/artifact-fs/internal/model"
)

const localHandoffJournalName = "local-handoff.json"

var errLocalHandoffRecoveryNeeded = errors.New("local checkout data was retained; restart RepoReach to recover the interrupted storage handoff")

// The catalogue is the transaction commit record. No private engine data is
// removed during Keep. A later Free may remove the retired copy only if this
// receipt proves both its original export and its unchanged contents.
type localCheckoutReceipt struct {
	Version         int                  `json:"version"`
	RepositoryID    string               `json:"repositoryID"`
	EngineVersion   string               `json:"engineVersion"`
	LocalPath       string               `json:"localPath"`
	Identity        localHandoffIdentity `json:"identity"`
	ExportDigest    string               `json:"exportDigest"`
	RetiredRoot     string               `json:"retiredRoot"`
	RetiredIdentity localHandoffIdentity `json:"retiredIdentity"`
	Retired         []localHandoffObject `json:"retired"`
}

type localHandoffObject struct {
	Original string               `json:"original"`
	Retired  string               `json:"retired"`
	Identity localHandoffIdentity `json:"identity"`
	Digest   string               `json:"digest"`
}

type localHandoffJournal struct {
	Version      int                   `json:"version"`
	Kind         string                `json:"kind"`
	RepositoryID string                `json:"repositoryID"`
	LocalPath    string                `json:"localPath"`
	Temporary    string                `json:"temporary"`
	Parent       localHandoffIdentity  `json:"parent"`
	Identity     localHandoffIdentity  `json:"identity"`
	Digest       string                `json:"digest"`
	Receipt      *localCheckoutReceipt `json:"receipt"`
}

// commitMaterializedCheckout runs under the service lifecycle lock, after a
// normal catalogue detach and StopCatalogRepositoryStorage. The caller checks
// the frozen source before detaching and its private Git copy again afterward.
// The destination's exact catalogue-owned link must have been released.
func (s *Service) commitMaterializedCheckout(ctx context.Context, id string, cfg model.RepoConfig, stage *StagedLocalCheckout) error {
	if stage == nil || stage.published || stage.parent == nil {
		return errors.New("the staged local checkout is unavailable")
	}
	s.mu.Lock()
	repo, exists := s.repositoryLocked(id)
	root := s.state.MountRoot
	s.mu.Unlock()
	if !exists || repo.LocalPath != "" || cfg.Name != engineName(id) || cfg.ID != model.RepoID(cfg.Name) || cfg.PreparedGitDir {
		return errors.New("the repository cannot be handed over to a local checkout")
	}
	parent, err := filepath.EvalSymlinks(filepath.Join(root, repo.Owner))
	if err != nil || stage.Destination != filepath.Join(parent, repo.Name) {
		return errors.New("the kept checkout must occupy its own ordinary catalogue folder")
	}
	if err := s.requireNoLocalHandoff(); err != nil {
		return err
	}
	if err := stage.VerifyPrivateGit(ctx); err != nil {
		return err
	}
	if err := verifyHandoffDirectory(filepath.Dir(stage.Destination), localHandoffIdentity{}, false); err != nil {
		return err
	}
	identity, err := handoffReadIdentity(stage.Path)
	if err != nil {
		return err
	}
	parentIdentity, err := handoffReadIdentity(filepath.Dir(stage.Destination))
	if err != nil {
		return err
	}
	retiredRoot, err := s.newRetiredEngineDirectory()
	if err != nil {
		return err
	}
	objects, err := s.captureHandoffStorage(ctx, id, cfg, retiredRoot)
	if err != nil {
		_ = os.Remove(retiredRoot) // Only an empty, unpublished directory.
		return err
	}
	digest := handoffManifestDigest(stage.finalManifest)
	retiredIdentity, err := handoffReadIdentity(retiredRoot)
	if err != nil {
		return err
	}
	receipt := &localCheckoutReceipt{Version: 1, RepositoryID: id, EngineVersion: cfg.ConfigVersion, LocalPath: stage.Destination, Identity: identity,
		ExportDigest: digest, RetiredRoot: retiredRoot, RetiredIdentity: retiredIdentity, Retired: objects}
	journal := localHandoffJournal{Version: 1, Kind: "keep", RepositoryID: id, LocalPath: stage.Destination,
		Temporary: stage.Path, Parent: parentIdentity, Identity: identity, Digest: digest, Receipt: receipt}
	if err := s.writeLocalHandoffJournal(journal); err != nil {
		_ = os.Remove(retiredRoot)
		return err
	}
	// Journal ownership transfers before publication. A publish or fsync failure
	// may already have moved the directory, so Close must never erase this stage.
	stage.published = true
	if err := handoffRenameOwnedDirectory(stage.Path, stage.Destination, parentIdentity, identity); err != nil {
		return s.markLocalHandoffRecovery(err)
	}
	if err := stage.VerifyPublished(ctx); err != nil {
		return s.markLocalHandoffRecovery(err)
	}
	if actual, err := handoffTreeDigest(ctx, stage.Destination); err != nil || actual != digest {
		return s.markLocalHandoffRecovery(errors.New("the published local checkout changed before its storage handoff"))
	}
	// Recheck private storage immediately before recording the exported checkout
	// as authoritative. External writes through .git bypass filesystem freezing.
	for _, object := range objects {
		if err := verifyHandoffObject(ctx, object.Original, object); err != nil {
			return s.markLocalHandoffRecovery(err)
		}
	}
	if err := stage.VerifyPrivateGit(ctx); err != nil {
		return s.markLocalHandoffRecovery(err)
	}
	s.mu.Lock()
	i := s.repositoryIndexLocked(id)
	if i < 0 || s.state.Repositories[i].LocalPath != "" {
		s.mu.Unlock()
		return s.markLocalHandoffRecovery(errors.New("the catalogue changed before local checkout publication"))
	}
	s.state.Repositories[i].LocalPath, s.state.Repositories[i].LocalKind = stage.Destination, "materialized"
	s.state.Repositories[i].State, s.state.Repositories[i].Pinned, s.state.Repositories[i].Error = "local", true, ""
	err = s.persistLocked()
	if err != nil {
		s.state.Repositories[i] = repo
	}
	s.mu.Unlock()
	if err != nil {
		return s.markLocalHandoffRecovery(err)
	}
	cleanupCtx, cleanupCancel := localHandoffCleanupContext(ctx)
	defer cleanupCancel()
	if err := s.finishCommittedKeep(cleanupCtx, journal); err != nil {
		return s.markLocalHandoffRecovery(err)
	}
	return s.removeLocalHandoffJournal()
}

// freeMaterializedCheckout is deliberately unavailable for adopted checkouts.
// Caller normally detaches the private catalogue and stops every engine runtime
// before entry. A clean remote-backed native checkout is quarantined by an
// exclusive rename before it can be deleted. Open editor/Git handles, changed
// data, or replaced paths refuse cleanup and retain the journalled data.
func (s *Service) freeMaterializedCheckout(ctx context.Context, id string) error {
	s.mu.Lock()
	repo, exists := s.repositoryLocked(id)
	s.mu.Unlock()
	if !exists || repo.LocalKind != "materialized" || repo.LocalPath == "" {
		return errors.New("adopted checkouts belong to you and are never removed by RepoReach")
	}
	if err := s.requireNoLocalHandoff(); err != nil {
		return err
	}
	configs, err := s.engine.ListRepos(ctx)
	if err != nil {
		return err
	}
	for _, cfg := range configs {
		if cfg.Name == engineName(id) {
			return errors.New("private repository registration reappeared after export; recover it before freeing space")
		}
	}
	receipt, err := s.readLocalCheckoutReceipt(id)
	if err != nil {
		return fmt.Errorf("verify kept checkout ownership: %w", err)
	}
	if receipt.LocalPath != repo.LocalPath {
		return errors.New("the kept checkout location changed; its data was retained")
	}
	if err := verifyHandoffDirectory(repo.LocalPath, receipt.Identity, false); err != nil {
		return err
	}
	digest, err := handoffTreeDigest(ctx, repo.LocalPath)
	if err != nil {
		return err
	}
	if err := VerifyLocalCheckoutSafeToFree(ctx, repo.LocalPath, repo.CloneURL); err != nil {
		return err
	}
	// A mutated rollback copy might contain work absent from today's checkout.
	// Verify it before changing the ordinary checkout's location.
	if err := s.verifyRetiredReceipt(ctx, receipt); err != nil {
		return err
	}
	if current, err := handoffTreeDigest(ctx, repo.LocalPath); err != nil || current != digest {
		return errors.New("the kept checkout changed after remote verification; its files were retained")
	}
	parent := filepath.Dir(repo.LocalPath)
	parentIdentity, err := handoffReadIdentity(parent)
	if err != nil {
		return err
	}
	if err := verifyHandoffDirectory(parent, parentIdentity, false); err != nil {
		return err
	}
	temporary := filepath.Join(parent, ".reporeach-free-"+rand.Text())
	journal := localHandoffJournal{Version: 1, Kind: "free", RepositoryID: id, LocalPath: repo.LocalPath,
		Temporary: temporary, Parent: parentIdentity, Identity: receipt.Identity, Digest: digest, Receipt: &receipt}
	if err := s.writeLocalHandoffJournal(journal); err != nil {
		return err
	}
	if err := handoffRenameOwnedDirectory(repo.LocalPath, temporary, parentIdentity, receipt.Identity); err != nil {
		return s.rollbackLocalFree(ctx, journal, err)
	}
	if err := verifyHandoffQuarantine(ctx, journal); err != nil {
		return s.rollbackLocalFree(ctx, journal, err)
	}
	// A remote check in the quarantined path cannot use stale core.worktree.
	// The native source was checked before moving; the whole binary fingerprint
	// after moving proves that exact checked tree is the one being removed.
	s.mu.Lock()
	i := s.repositoryIndexLocked(id)
	if i < 0 || s.state.Repositories[i].LocalPath != repo.LocalPath || s.state.Repositories[i].LocalKind != "materialized" {
		s.mu.Unlock()
		return s.rollbackLocalFree(ctx, journal, errors.New("the kept checkout changed in the catalogue"))
	}
	s.state.Repositories[i].LocalPath, s.state.Repositories[i].LocalKind = "", ""
	s.state.Repositories[i].State, s.state.Repositories[i].Pinned = "virtual", false
	s.state.Repositories[i].DownloadedBytes, s.state.Repositories[i].Error = 0, ""
	delete(s.pins, id)
	err = s.persistLocked()
	if err != nil {
		s.state.Repositories[i] = repo
	}
	s.mu.Unlock()
	if err != nil {
		// writeState may have committed its rename before a directory fsync
		// failure. Recovery consults disk instead of guessing a rollback direction.
		return s.markLocalHandoffRecovery(err)
	}
	cleanupCtx, cleanupCancel := localHandoffCleanupContext(ctx)
	defer cleanupCancel()
	if err := s.finishCommittedFree(cleanupCtx, journal); err != nil {
		return s.markLocalHandoffRecovery(err)
	}
	return s.removeLocalHandoffJournal()
}

func (s *Service) markLocalHandoffRecovery(cause error) error {
	s.mu.Lock()
	s.maintenance, s.recoveryRequired = true, true
	s.message = "Local checkout data was retained. Restart RepoReach to recover the storage handoff."
	s.mu.Unlock()
	return errors.Join(cause, errLocalHandoffRecoveryNeeded)
}

// recoverLocalHandoffs must run before mount creation and state reconciliation.
// No ambiguous directory is deleted, even when the durable catalogue committed.
func (s *Service) recoverLocalHandoffs(ctx context.Context) error {
	journal, err := s.readLocalHandoffJournal()
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	s.mu.Lock()
	repo, exists := s.repositoryLocked(journal.RepositoryID)
	s.mu.Unlock()
	if !exists {
		return errors.New("the interrupted checkout is missing from the catalogue; its data was retained")
	}
	switch journal.Kind {
	case "keep":
		if repo.LocalKind == "materialized" && repo.LocalPath == journal.LocalPath {
			if err := verifyHandoffDirectory(journal.LocalPath, journal.Identity, false); err != nil {
				return err
			}
			if err := s.finishCommittedKeep(ctx, journal); err != nil {
				return err
			}
		} else if repo.LocalPath == "" && repo.LocalKind == "" {
			// Keep never retires engine data before catalogue commit. A published
			// but uncommitted checkout is retained under its original stage name.
			if err := handoffRestoreOwnedDirectory(journal.LocalPath, journal.Temporary, journal.Parent, journal.Identity); err != nil {
				return err
			}
			if err := s.retainUncommittedLocalCheckout(ctx, journal); err != nil {
				return err
			}
		} else {
			return errors.New("the interrupted checkout conflicts with the catalogue; its data was retained")
		}
	case "free":
		if repo.LocalPath == "" && repo.LocalKind == "" {
			if err := s.finishCommittedFree(ctx, journal); err != nil {
				return err
			}
		} else if repo.LocalKind == "materialized" && repo.LocalPath == journal.LocalPath {
			if err := handoffRestoreOwnedDirectory(journal.Temporary, journal.LocalPath, journal.Parent, journal.Identity); err != nil {
				return err
			}
		} else {
			return errors.New("the interrupted removal conflicts with the catalogue; its data was retained")
		}
	default:
		return errors.New("unknown local checkout transaction")
	}
	return s.removeLocalHandoffJournal()
}

func (s *Service) rollbackLocalFree(ctx context.Context, journal localHandoffJournal, cause error) error {
	if err := handoffRestoreOwnedDirectory(journal.Temporary, journal.LocalPath, journal.Parent, journal.Identity); err != nil {
		return s.markLocalHandoffRecovery(errors.Join(cause, err))
	}
	if err := s.removeLocalHandoffJournal(); err != nil {
		return s.markLocalHandoffRecovery(errors.Join(cause, err))
	}
	return cause
}

func (s *Service) finishCommittedKeep(ctx context.Context, journal localHandoffJournal) error {
	if err := verifyHandoffDirectory(journal.Receipt.RetiredRoot, journal.Receipt.RetiredIdentity, true); err != nil {
		return err
	}
	configs, err := s.engine.ListRepos(ctx)
	if err != nil {
		return err
	}
	for _, cfg := range configs {
		if cfg.Name == engineName(journal.RepositoryID) {
			if cfg.ConfigVersion != journal.Receipt.EngineVersion || cfg.ID != model.RepoID(cfg.Name) || cfg.PreparedGitDir || cfg.GitDir != filepath.Join(s.canonicalHandoffStorage(journal.RepositoryID)[0], "git") {
				return errors.New("private registration changed during local handoff; its data was retained")
			}
			if err := s.engine.RemoveRepo(ctx, cfg.Name); err != nil {
				return err
			}
		}
	}
	for _, object := range journal.Receipt.Retired {
		if err := s.retireHandoffObject(ctx, object); err != nil {
			return err
		}
	}
	if err := s.writeLocalCheckoutReceipt(*journal.Receipt); err != nil {
		return err
	}
	return nil
}

func (s *Service) finishCommittedFree(ctx context.Context, journal localHandoffJournal) error {
	// Publish first, so deleting recoverable data cannot leave an undiscoverable
	// repository. This publisher touches only its recorded owned symlinks.
	if err := s.publishHybridCatalogue(); err != nil {
		return err
	}
	plan, err := s.localHandoffCleanupPlan(ctx, journal)
	if err != nil {
		return err
	}
	// The flushed per-entry plan supports an interrupted deletion. Recovery
	// accepts missing owned entries while rejecting new or changed data.
	for _, object := range plan.Objects {
		if err := handoffRemovePlannedObject(ctx, object, nil); err != nil {
			return err
		}
	}
	if err := handoffRemoveOwnedEmptyDirectory(journal.Receipt.RetiredRoot, journal.Receipt.RetiredIdentity); err != nil {
		return err
	}
	if err := handoffSyncDirectory(filepath.Dir(journal.Receipt.RetiredRoot)); err != nil {
		return err
	}
	if err := s.removeLocalCheckoutReceipt(journal.RepositoryID); err != nil {
		return err
	}
	return s.removeLocalHandoffCleanupPlan(journal)
}

func verifyHandoffQuarantine(ctx context.Context, journal localHandoffJournal) error {
	if _, err := os.Lstat(journal.Temporary); errors.Is(err, os.ErrNotExist) {
		return nil // A previously committed cleanup already removed this copy.
	}
	if err := verifyHandoffDirectory(journal.Temporary, journal.Identity, false); err != nil {
		return err
	}
	if digest, err := handoffTreeDigest(ctx, journal.Temporary); err != nil || digest != journal.Digest {
		return errors.New("the retained local checkout changed; its files were preserved")
	}
	if err := handoffDirectoryNotInUse(ctx, journal.Temporary); err != nil {
		return err
	}
	return nil
}

func (s *Service) newRetiredEngineDirectory() (string, error) {
	parent := filepath.Join(s.opts.StateDir, "retired-engine")
	if err := handoffPrivateDirectory(parent, true); err != nil {
		return "", err
	}
	path := filepath.Join(parent, "handoff-"+rand.Text())
	if err := os.Mkdir(path, 0o700); err != nil {
		return "", err
	}
	return path, errors.Join(handoffSyncDirectory(path), handoffSyncDirectory(parent))
}

func (s *Service) canonicalHandoffStorage(id string) []string {
	name, engine := engineName(id), filepath.Join(s.opts.StateDir, "engine")
	meta := filepath.Join(engine, "meta", name+".sqlite")
	return []string{filepath.Join(engine, "repos", name), filepath.Join(engine, "overlays", name),
		filepath.Join(engine, "cache", "blobs", name), meta, meta + "-wal", meta + "-shm"}
}

func (s *Service) captureHandoffStorage(ctx context.Context, id string, cfg model.RepoConfig, retired string) ([]localHandoffObject, error) {
	paths := s.canonicalHandoffStorage(id)
	if cfg.GitDir != filepath.Join(paths[0], "git") || cfg.OverlayDir != paths[1] || cfg.BlobCacheDir != paths[2] || cfg.MetaDBPath != paths[3] || cfg.OverlayDBPath != filepath.Join(paths[1], "meta.sqlite") {
		return nil, errors.New("repository storage is not owned by this catalogue")
	}
	var objects []localHandoffObject
	for i, path := range paths {
		identity, err := handoffReadIdentity(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		digest, err := handoffTreeDigest(ctx, path)
		if err != nil {
			return nil, err
		}
		objects = append(objects, localHandoffObject{Original: path, Retired: filepath.Join(retired, fmt.Sprint(i)), Identity: identity, Digest: digest})
	}
	if len(objects) == 0 || objects[0].Original != paths[0] {
		return nil, errors.New("the private Git source is missing; its handoff was refused")
	}
	return objects, nil
}

func (s *Service) retireHandoffObject(ctx context.Context, object localHandoffObject) error {
	if _, err := os.Lstat(object.Retired); err == nil {
		if err := verifyHandoffObject(ctx, object.Retired, object); err != nil {
			return err
		}
		if _, err := os.Lstat(object.Original); !errors.Is(err, os.ErrNotExist) {
			return errors.New("private repository storage reappeared during handoff; both copies were retained")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := verifyHandoffObject(ctx, object.Original, object); err != nil {
		return err
	}
	if err := handoffRenameOwnedObject(object.Original, object.Retired, object.Identity); err != nil {
		return err
	}
	return verifyHandoffObject(ctx, object.Retired, object)
}

func verifyHandoffObject(ctx context.Context, path string, object localHandoffObject) error {
	identity, err := handoffReadIdentity(path)
	if err != nil || identity != object.Identity {
		return errors.New("repository storage was replaced; its data was retained")
	}
	digest, err := handoffTreeDigest(ctx, path)
	if err != nil || digest != object.Digest {
		return errors.New("repository storage changed after export; its data was retained")
	}
	return nil
}

func (s *Service) verifyRetiredReceipt(ctx context.Context, receipt localCheckoutReceipt) error {
	if _, err := os.Lstat(receipt.RetiredRoot); errors.Is(err, os.ErrNotExist) {
		// An absent whole retired tree is an idempotent committed cleanup.
		return nil
	}
	if err := verifyHandoffDirectory(receipt.RetiredRoot, receipt.RetiredIdentity, true); err != nil {
		return err
	}
	known := make(map[string]bool, len(receipt.Retired))
	for _, object := range receipt.Retired {
		known[filepath.Base(object.Retired)] = true
	}
	entries, err := os.ReadDir(receipt.RetiredRoot)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !known[entry.Name()] {
			return errors.New("retired storage contains new data; all copies were retained")
		}
	}
	for _, object := range receipt.Retired {
		if _, err := os.Lstat(object.Retired); errors.Is(err, os.ErrNotExist) {
			continue // Committed cleanup may have already removed a previous object.
		}
		if err := verifyHandoffObject(ctx, object.Retired, object); err != nil {
			return err
		}
	}
	return nil
}

func handoffTreeDigest(ctx context.Context, root string) (string, error) {
	manifest, err := checkoutTreeManifest(ctx, root, false)
	if err != nil {
		return "", err
	}
	return handoffManifestDigest(manifest), nil
}

// Hash raw path/link bytes directly: Git filenames need not be valid UTF-8.
// JSON's Unicode replacement must not collapse distinct binary filenames.
func handoffManifestDigest(manifest map[string]checkoutFileRecord) string {
	h := sha256.New()
	keys := make([]string, 0, len(manifest))
	for key := range manifest {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		record := manifest[key]
		handoffHashBytes(h, []byte(key))
		_ = binary.Write(h, binary.BigEndian, uint64(record.Mode))
		_ = binary.Write(h, binary.BigEndian, record.Size)
		modified := record.Modified.UnixNano()
		if record.Mode.IsDir() || record.Mode&os.ModeSymlink != 0 {
			modified = 0
		}
		_ = binary.Write(h, binary.BigEndian, modified)
		handoffHashBytes(h, []byte(record.Digest))
		handoffHashBytes(h, []byte(record.Link))
		attrs := make([]string, 0, len(record.Xattrs))
		for attr := range record.Xattrs {
			attrs = append(attrs, attr)
		}
		sort.Strings(attrs)
		_ = binary.Write(h, binary.BigEndian, uint64(len(attrs)))
		for _, attr := range attrs {
			handoffHashBytes(h, []byte(attr))
			handoffHashBytes(h, record.Xattrs[attr])
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

func handoffHashBytes(h hash.Hash, value []byte) {
	_ = binary.Write(h, binary.BigEndian, uint64(len(value)))
	_, _ = h.Write(value)
}

func (s *Service) requireNoLocalHandoff() error {
	for _, name := range []string{localHandoffJournalName, localHandoffCleanupName} {
		_, err := os.Lstat(filepath.Join(s.opts.StateDir, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		return errLocalHandoffRecoveryNeeded
	}
	return nil
}

func (s *Service) writeLocalHandoffJournal(journal localHandoffJournal) error {
	if err := s.validateLocalHandoff(journal); err != nil {
		return err
	}
	return handoffWriteJSON(filepath.Join(s.opts.StateDir, localHandoffJournalName), journal, true)
}

func (s *Service) readLocalHandoffJournal() (localHandoffJournal, error) {
	var journal localHandoffJournal
	if err := handoffReadJSON(filepath.Join(s.opts.StateDir, localHandoffJournalName), &journal); err != nil {
		return journal, err
	}
	return journal, s.validateLocalHandoff(journal)
}

func (s *Service) validateLocalHandoff(journal localHandoffJournal) error {
	if journal.Version != 1 || (journal.Kind != "keep" && journal.Kind != "free") || journal.Receipt == nil || journal.RepositoryID != journal.Receipt.RepositoryID || journal.LocalPath != journal.Receipt.LocalPath || journal.Identity != journal.Receipt.Identity || !journal.Parent.valid() || !journal.Identity.valid() || !validHandoffDigest(journal.Digest) {
		return errors.New("invalid local checkout transaction; data was retained")
	}
	if err := s.validateLocalCheckoutReceipt(*journal.Receipt); err != nil {
		return err
	}
	prefix := ".reporeach-checkout-"
	if journal.Kind == "free" {
		prefix = ".reporeach-free-"
	}
	if filepath.Dir(journal.Temporary) != filepath.Dir(journal.LocalPath) || !strings.HasPrefix(filepath.Base(journal.Temporary), prefix) || validateComponent(filepath.Base(journal.Temporary)) != nil || !filepath.IsAbs(journal.Temporary) || hasAdoptionControl(journal.Temporary) {
		return errors.New("invalid retained checkout path; data was retained")
	}
	return nil
}

func (s *Service) validateLocalCheckoutReceipt(receipt localCheckoutReceipt) error {
	owner, name, ok := strings.Cut(receipt.RepositoryID, "/")
	if receipt.Version != 1 || receipt.EngineVersion == "" || !ok || validateComponent(owner) != nil || validateComponent(name) != nil || !receipt.Identity.valid() || !receipt.RetiredIdentity.valid() || !validHandoffDigest(receipt.ExportDigest) || !filepath.IsAbs(receipt.LocalPath) || hasAdoptionControl(receipt.LocalPath) || len(receipt.Retired) == 0 || len(receipt.Retired) > 6 {
		return errors.New("invalid kept checkout ownership receipt; data was retained")
	}
	retiredParent := filepath.Join(s.opts.StateDir, "retired-engine")
	if filepath.Dir(receipt.RetiredRoot) != retiredParent || !strings.HasPrefix(filepath.Base(receipt.RetiredRoot), "handoff-") || validateComponent(filepath.Base(receipt.RetiredRoot)) != nil {
		return errors.New("invalid retired private storage folder; data was retained")
	}
	canonical := s.canonicalHandoffStorage(receipt.RepositoryID)
	seen := make(map[string]bool)
	for _, object := range receipt.Retired {
		index := -1
		for i, path := range canonical {
			if path == object.Original {
				index = i
				break
			}
		}
		if index < 0 || seen[object.Original] || object.Retired != filepath.Join(receipt.RetiredRoot, fmt.Sprint(index)) || !object.Identity.valid() || !validHandoffDigest(object.Digest) {
			return errors.New("invalid retired private storage object; data was retained")
		}
		seen[object.Original] = true
	}
	if !seen[canonical[0]] {
		return errors.New("the kept checkout receipt does not prove its private Git export")
	}
	return nil
}

func validHandoffDigest(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && strings.ToLower(value) == value
}

func (s *Service) localCheckoutReceiptPath(id string) string {
	return filepath.Join(s.opts.StateDir, "local-checkouts", engineName(id)+".json")
}

func (s *Service) writeLocalCheckoutReceipt(receipt localCheckoutReceipt) error {
	if err := s.validateLocalCheckoutReceipt(receipt); err != nil {
		return err
	}
	path := s.localCheckoutReceiptPath(receipt.RepositoryID)
	if err := handoffPrivateDirectory(filepath.Dir(path), true); err != nil {
		return err
	}
	return handoffWriteJSON(path, receipt, false)
}

func (s *Service) readLocalCheckoutReceipt(id string) (localCheckoutReceipt, error) {
	var receipt localCheckoutReceipt
	if err := handoffReadJSON(s.localCheckoutReceiptPath(id), &receipt); err != nil {
		return receipt, err
	}
	if receipt.RepositoryID != id {
		return receipt, errors.New("the kept checkout receipt belongs to another repository")
	}
	return receipt, s.validateLocalCheckoutReceipt(receipt)
}

func (s *Service) removeLocalCheckoutReceipt(id string) error {
	path := s.localCheckoutReceiptPath(id)
	if _, err := s.readLocalCheckoutReceipt(id); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return handoffSyncDirectory(filepath.Dir(path))
}

func (s *Service) removeLocalHandoffJournal() error {
	path := filepath.Join(s.opts.StateDir, localHandoffJournalName)
	if _, err := s.readLocalHandoffJournal(); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return handoffSyncDirectory(filepath.Dir(path))
}

func handoffWriteJSON(path string, value any, exclusive bool) error {
	if err := handoffPrivateDirectory(filepath.Dir(path), false); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".local-handoff-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	if err := json.NewEncoder(file).Encode(value); err != nil {
		return err
	}
	maximum := int64(1 << 20)
	if filepath.Base(path) == localHandoffCleanupName {
		maximum = maxLocalHandoffCleanupBytes
	}
	if info, err := file.Stat(); err != nil || info.Size() > maximum {
		return errors.New("the ownership record exceeds the supported limit; all checkout data was retained")
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if exclusive {
		parent, err := handoffOpenDirectory(filepath.Dir(path))
		if err != nil {
			return err
		}
		defer parent.Close()
		// Exclusive rename publishes one link atomically, including after a kill.
		if err := publishCheckoutDirectory(parent, filepath.Base(file.Name()), filepath.Base(path)); err != nil {
			return err
		}
	} else {
		if _, err := os.Lstat(path); err == nil {
			if err := handoffReadJSON(path, new(json.RawMessage)); err != nil {
				return err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.Rename(file.Name(), path); err != nil {
			return err
		}
	}
	return handoffSyncDirectory(filepath.Dir(path))
}

func handoffReadJSON(path string, destination any) error {
	file, err := handoffOpenPrivateRegular(path)
	if err != nil {
		return err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, (1<<20)+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("local checkout ownership record has trailing data")
	}
	return nil
}

func handoffSyncDirectory(path string) error {
	file, err := handoffOpenDirectory(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}

// Cleanup uses its own short-lived context: cancellation may interrupt a
// preflight, but cannot abandon a rename whose durable catalogue committed.
func localHandoffCleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
}
