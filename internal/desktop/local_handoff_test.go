//go:build darwin || linux

package desktop

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/model"
)

func handoffFixture(t *testing.T) (*Service, Repository, model.RepoConfig, *StagedLocalCheckout) {
	t.Helper()
	s := newDesktopTestService(t, "exit 4")
	repo := Repository{ID: "octocat/local", Owner: "octocat", Name: "local", CloneURL: "https://github.com/octocat/local.git", HTMLURL: "https://github.com/octocat/local", State: "available"}
	seedDesktopCatalogue(t, s, repo)
	cfg := addMigrationRepository(t, s, repo, false)
	source, _ := adoptionSource(t)
	if err := os.MkdirAll(filepath.Dir(cfg.GitDir), 0700); err != nil {
		t.Fatal(err)
	}
	if err := copyCheckoutTree(context.Background(), filepath.Join(source, ".git"), cfg.GitDir, false); err != nil {
		t.Fatal(err)
	}
	// Include user metadata in every separately stored engine class so the test
	// proves retirement is not limited to the Git directory.
	for _, directory := range []string{cfg.OverlayDir, cfg.BlobCacheDir, filepath.Dir(cfg.MetaDBPath)} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	writeCheckoutFixture(t, filepath.Join(cfg.OverlayDir, "upper-binary"), []byte{0, 255, 128, 0}, 0600)
	writeCheckoutFixture(t, filepath.Join(cfg.BlobCacheDir, "cached"), []byte{254, 0, 255}, 0600)
	writeCheckoutFixture(t, cfg.MetaDBPath, []byte("closed snapshot"), 0600)
	writeCheckoutFixture(t, cfg.MetaDBPath+"-wal", []byte("closed wal"), 0600)
	writeCheckoutFixture(t, cfg.MetaDBPath+"-shm", []byte("closed shm"), 0600)
	parent := filepath.Join(s.state.MountRoot, repo.Owner)
	if err := os.MkdirAll(parent, 0755); err != nil {
		t.Fatal(err)
	}
	parent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		t.Fatal(err)
	}
	stage, err := StageLocalCheckout(context.Background(), cfg, source, filepath.Join(parent, repo.Name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stage.Close() })
	return s, repo, cfg, stage
}

func fixtureHandoffJournal(t *testing.T, s *Service, repo Repository, cfg model.RepoConfig, stage *StagedLocalCheckout) localHandoffJournal {
	t.Helper()
	retired, err := s.newRetiredEngineDirectory()
	if err != nil {
		t.Fatal(err)
	}
	objects, err := s.captureHandoffStorage(context.Background(), repo.ID, cfg, retired)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := handoffReadIdentity(stage.Path)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := handoffReadIdentity(filepath.Dir(stage.Destination))
	if err != nil {
		t.Fatal(err)
	}
	retiredIdentity, err := handoffReadIdentity(retired)
	if err != nil {
		t.Fatal(err)
	}
	digest := handoffManifestDigest(stage.finalManifest)
	receipt := &localCheckoutReceipt{Version: 1, RepositoryID: repo.ID, EngineVersion: cfg.ConfigVersion, LocalPath: stage.Destination,
		Identity: identity, ExportDigest: digest, RetiredRoot: retired, RetiredIdentity: retiredIdentity, Retired: objects}
	journal := localHandoffJournal{Version: 1, Kind: "keep", RepositoryID: repo.ID, LocalPath: stage.Destination,
		Temporary: stage.Path, Parent: parent, Identity: identity, Digest: digest, Receipt: receipt}
	if err := s.writeLocalHandoffJournal(journal); err != nil {
		t.Fatal(err)
	}
	return journal
}

func TestLocalHandoffKeepPublishesNativeCheckoutAndRetiresAllEngineStorage(t *testing.T) {
	s, repo, cfg, stage := handoffFixture(t)
	if err := s.commitMaterializedCheckout(context.Background(), repo.ID, cfg, stage); err != nil {
		t.Fatal(err)
	}
	status := s.Status().Repositories[0]
	if status.LocalPath != stage.Destination || status.LocalKind != "materialized" || status.State != "local" || !status.Pinned {
		t.Fatalf("wrong local state: %+v", status)
	}
	checkout, err := InspectLocalCheckout(context.Background(), stage.Destination)
	if err != nil || checkout.Path != stage.Destination {
		t.Fatalf("ordinary Git checkout: %+v %v", checkout, err)
	}
	configs, err := s.engine.ListRepos(context.Background())
	if err != nil || len(configs) != 0 {
		t.Fatalf("stale registration remained: %+v %v", configs, err)
	}
	receipt, err := s.readLocalCheckoutReceipt(repo.ID)
	if err != nil || len(receipt.Retired) != 6 {
		t.Fatalf("retirement receipt: %+v %v", receipt, err)
	}
	for _, object := range receipt.Retired {
		if _, err := os.Lstat(object.Original); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stale reusable storage at %s", object.Original)
		}
		if err := verifyHandoffObject(context.Background(), object.Retired, object); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Lstat(filepath.Join(s.opts.StateDir, localHandoffJournalName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("completed Keep retained active transaction")
	}
}

func TestLocalHandoffRejectsPrivateGitChangesAfterStage(t *testing.T) {
	s, repo, cfg, stage := handoffFixture(t)
	writeCheckoutFixture(t, filepath.Join(cfg.GitDir, "late-index-work"), []byte("preserve"), 0600)
	if err := s.commitMaterializedCheckout(context.Background(), repo.ID, cfg, stage); err == nil {
		t.Fatal("late private Git write was lost")
	}
	if s.Status().Repositories[0].LocalPath != "" {
		t.Fatal("invalid copy committed")
	}
	if _, err := os.Stat(stage.Destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stale checkout was published")
	}
}

func TestLocalHandoffRecoveryUsesDurableCatalogueCommit(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(map[bool]string{false: "before commit", true: "after commit"}[committed], func(t *testing.T) {
			s, repo, cfg, stage := handoffFixture(t)
			journal := fixtureHandoffJournal(t, s, repo, cfg, stage)
			stage.published = true
			if err := handoffRenameOwnedDirectory(stage.Path, stage.Destination, journal.Parent, journal.Identity); err != nil {
				t.Fatal(err)
			}
			if committed {
				s.mu.Lock()
				s.state.Repositories[0].LocalPath, s.state.Repositories[0].LocalKind = stage.Destination, "materialized"
				err := s.persistLocked()
				s.mu.Unlock()
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := s.recoverLocalHandoffs(context.Background()); err != nil {
				t.Fatal(err)
			}
			if committed {
				if _, err := InspectLocalCheckout(context.Background(), stage.Destination); err != nil {
					t.Fatal(err)
				}
				if _, err := s.readLocalCheckoutReceipt(repo.ID); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Lstat(cfg.GitDir); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("committed restart reused old private clone")
				}
			} else {
				if _, err := os.Stat(stage.Path); err != nil {
					t.Fatal("uncommitted copy was deleted instead of retained")
				}
				if _, err := os.Stat(cfg.GitDir); err != nil {
					t.Fatal("uncommitted restart lost its original clone")
				}
				if _, err := os.Lstat(stage.Destination); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("uncommitted copy still occupies catalogue leaf")
				}
				message, err := s.retainedLocalCheckoutMessage()
				if err != nil || message == "" {
					t.Fatalf("recovered large copy was left unaccounted: %q %v", message, err)
				}
				if _, err := os.Stat(journal.Receipt.RetiredRoot); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("empty uncommitted rollback container was not cleaned")
				}
				checkout, err := InspectLocalCheckout(context.Background(), stage.Path)
				if err != nil || checkout.Path != stage.Path {
					t.Fatalf("recovered checkout still points at virtual catalogue: %+v %v", checkout, err)
				}
			}
		})
	}
}

func TestLocalHandoffRecoveryNeverReplacesForeignCheckout(t *testing.T) {
	s, repo, cfg, stage := handoffFixture(t)
	journal := fixtureHandoffJournal(t, s, repo, cfg, stage)
	stage.published = true
	if err := handoffRenameOwnedDirectory(stage.Path, stage.Destination, journal.Parent, journal.Identity); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(stage.Path, 0700); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(stage.Path, "foreign-binary")
	contents := []byte{0, 255, 128, 0}
	writeCheckoutFixture(t, foreign, contents, 0600)
	if err := s.recoverLocalHandoffs(context.Background()); err == nil {
		t.Fatal("recovery replaced a foreign directory")
	}
	actual, err := os.ReadFile(foreign)
	if err != nil || !bytes.Equal(actual, contents) {
		t.Fatal("foreign files changed")
	}
	if _, err := os.Stat(stage.Destination); err != nil {
		t.Fatal("owned published data was lost")
	}
}

func TestLocalHandoffNeverFreesAdoptedOriginal(t *testing.T) {
	s, repo, _, stage := handoffFixture(t)
	s.mu.Lock()
	s.state.Repositories[0].LocalKind, s.state.Repositories[0].LocalPath = "adopted", stage.sourceView
	s.mu.Unlock()
	before, err := handoffTreeDigest(context.Background(), stage.sourceView)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.freeMaterializedCheckout(context.Background(), repo.ID); err == nil {
		t.Fatal("adopted source was freed")
	}
	after, err := handoffTreeDigest(context.Background(), stage.sourceView)
	if err != nil || before != after {
		t.Fatal("adopted original changed")
	}
}

func prepareHandoffRemoteBackup(t *testing.T, s *Service, repo Repository, stage *StagedLocalCheckout) string {
	t.Helper()
	remote := filepath.Join(t.TempDir(), "backup.git")
	adoptionGit(t, stage.Destination, "clone", "--bare", stage.Destination, remote)
	adoptionGit(t, stage.Destination, "remote", "add", "origin", remote)
	adoptionGit(t, stage.Destination, "config", "--unset", "user.name")
	adoptionGit(t, stage.Destination, "config", "--unset", "user.email")
	if err := os.Remove(filepath.Join(stage.Destination, ".git", "COMMIT_EDITMSG")); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.state.Repositories[0].Source, s.state.Repositories[0].CloneURL, s.state.Repositories[0].HTMLURL, s.state.Repositories[0].DefaultBranch = "manual", remote, "", "trunk"
	err := s.persistLocked()
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	return remote
}

func TestLocalHandoffFreeReclaimsOnlyVerifiedCopiesAndPreservesRemote(t *testing.T) {
	if _, err := exec.LookPath("lsof"); err != nil {
		t.Skip("lsof unavailable; runtime conservatively refuses cleanup")
	}
	s, repo, cfg, stage := handoffFixture(t)
	if err := s.commitMaterializedCheckout(context.Background(), repo.ID, cfg, stage); err != nil {
		t.Fatal(err)
	}
	receipt, err := s.readLocalCheckoutReceipt(repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	remote := prepareHandoffRemoteBackup(t, s, repo, stage)
	remoteBefore, err := handoffTreeDigest(context.Background(), remote)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.freeMaterializedCheckout(context.Background(), repo.ID); err != nil {
		t.Fatal(err)
	}
	status := s.Status().Repositories[0]
	if status.LocalPath != "" || status.LocalKind != "" || status.Pinned || status.State != "virtual" {
		t.Fatalf("wrong freed state: %+v", status)
	}
	if _, err := os.Lstat(stage.Destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("full local checkout still occupies space")
	}
	if _, err := os.Lstat(receipt.RetiredRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("rollback copy still occupies space after successful Free")
	}
	if _, err := s.readLocalCheckoutReceipt(repo.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("completed Free retained ownership receipt")
	}
	remoteAfter, err := handoffTreeDigest(context.Background(), remote)
	if err != nil || remoteBefore != remoteAfter {
		t.Fatal("Free changed the remote backup")
	}
}

func TestLocalHandoffFreeRecoveryRestoresBeforeCommitAndCleansAfterCommit(t *testing.T) {
	if _, err := exec.LookPath("lsof"); err != nil {
		t.Skip("lsof unavailable; runtime conservatively refuses cleanup")
	}
	for _, committed := range []bool{false, true} {
		t.Run(map[bool]string{false: "before commit", true: "after commit"}[committed], func(t *testing.T) {
			s, repo, cfg, stage := handoffFixture(t)
			if err := s.commitMaterializedCheckout(context.Background(), repo.ID, cfg, stage); err != nil {
				t.Fatal(err)
			}
			remote := prepareHandoffRemoteBackup(t, s, repo, stage)
			if err := VerifyLocalCheckoutSafeToFree(context.Background(), stage.Destination, remote); err != nil {
				t.Fatal(err)
			}
			receipt, err := s.readLocalCheckoutReceipt(repo.ID)
			if err != nil {
				t.Fatal(err)
			}
			digest, err := handoffTreeDigest(context.Background(), stage.Destination)
			if err != nil {
				t.Fatal(err)
			}
			parent, err := handoffReadIdentity(filepath.Dir(stage.Destination))
			if err != nil {
				t.Fatal(err)
			}
			journal := localHandoffJournal{Version: 1, Kind: "free", RepositoryID: repo.ID,
				LocalPath: stage.Destination, Temporary: filepath.Join(filepath.Dir(stage.Destination), ".reporeach-free-recovery"),
				Parent: parent, Identity: receipt.Identity, Digest: digest, Receipt: &receipt}
			if err := s.writeLocalHandoffJournal(journal); err != nil {
				t.Fatal(err)
			}
			if err := handoffRenameOwnedDirectory(journal.LocalPath, journal.Temporary, journal.Parent, journal.Identity); err != nil {
				t.Fatal(err)
			}
			if committed {
				s.mu.Lock()
				s.state.Repositories[0].LocalPath, s.state.Repositories[0].LocalKind = "", ""
				s.state.Repositories[0].State = "virtual"
				err := s.persistLocked()
				s.mu.Unlock()
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := s.recoverLocalHandoffs(context.Background()); err != nil {
				t.Fatal(err)
			}
			if committed {
				if _, err := os.Lstat(journal.Temporary); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("committed recovery retained a full checkout")
				}
				if _, err := os.Lstat(receipt.RetiredRoot); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("committed recovery retained old engine data")
				}
			} else {
				if _, err := InspectLocalCheckout(context.Background(), stage.Destination); err != nil {
					t.Fatal("uncommitted Free failed to restore the native checkout")
				}
				if _, err := os.Stat(receipt.RetiredRoot); err != nil {
					t.Fatal("uncommitted Free deleted its rollback data")
				}
			}
		})
	}
}

func TestLocalHandoffBusyFreeRestoresOrdinaryCheckout(t *testing.T) {
	s, repo, cfg, stage := handoffFixture(t)
	if err := s.commitMaterializedCheckout(context.Background(), repo.ID, cfg, stage); err != nil {
		t.Fatal(err)
	}
	prepareHandoffRemoteBackup(t, s, repo, stage)
	open, err := os.Open(filepath.Join(stage.Destination, "tracked.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer open.Close()
	if err := s.freeMaterializedCheckout(context.Background(), repo.ID); err == nil {
		t.Fatal("checkout with an open editor handle was freed")
	}
	if _, err := InspectLocalCheckout(context.Background(), stage.Destination); err != nil {
		t.Fatal("busy checkout was not restored")
	}
	status := s.Status().Repositories[0]
	if status.LocalPath != stage.Destination || status.LocalKind != "materialized" {
		t.Fatal("busy Free committed virtual state")
	}
	if _, err := os.Stat(filepath.Join(s.opts.StateDir, localHandoffJournalName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("successful rollback left a stale journal")
	}
}

func TestLocalHandoffCleanupRefusesChangedOrForeignRetiredStorage(t *testing.T) {
	s, repo, cfg, stage := handoffFixture(t)
	if err := s.commitMaterializedCheckout(context.Background(), repo.ID, cfg, stage); err != nil {
		t.Fatal(err)
	}
	receipt, err := s.readLocalCheckoutReceipt(repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	object := receipt.Retired[0]
	writeCheckoutFixture(t, filepath.Join(object.Retired, "late-work"), []byte("must survive"), 0600)
	if err := s.verifyRetiredReceipt(context.Background(), receipt); err == nil {
		t.Fatal("mutated rollback data was considered deletable")
	}
	if err := handoffRemoveOwnedTree(context.Background(), object.Retired, object.Identity, object.Digest); err == nil {
		t.Fatal("mutated rollback tree was deleted")
	}
	if _, err := os.Stat(filepath.Join(object.Retired, "late-work")); err != nil {
		t.Fatal("late work was lost")
	}
}

func committedFreeFixtureJournal(t *testing.T, s *Service, repo Repository, stage *StagedLocalCheckout) localHandoffJournal {
	t.Helper()
	remote := prepareHandoffRemoteBackup(t, s, repo, stage)
	if err := VerifyLocalCheckoutSafeToFree(context.Background(), stage.Destination, remote); err != nil {
		t.Fatal(err)
	}
	receipt, err := s.readLocalCheckoutReceipt(repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := handoffTreeDigest(context.Background(), stage.Destination)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := handoffReadIdentity(filepath.Dir(stage.Destination))
	if err != nil {
		t.Fatal(err)
	}
	journal := localHandoffJournal{Version: 1, Kind: "free", RepositoryID: repo.ID,
		LocalPath: stage.Destination, Temporary: filepath.Join(filepath.Dir(stage.Destination), ".reporeach-free-interrupted"),
		Parent: parent, Identity: receipt.Identity, Digest: digest, Receipt: &receipt}
	if err := s.writeLocalHandoffJournal(journal); err != nil {
		t.Fatal(err)
	}
	if err := handoffRenameOwnedDirectory(journal.LocalPath, journal.Temporary, journal.Parent, journal.Identity); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.state.Repositories[0].LocalPath, s.state.Repositories[0].LocalKind = "", ""
	s.state.Repositories[0].State, s.state.Repositories[0].Pinned = "virtual", false
	err = s.persistLocked()
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	return journal
}

func TestLocalHandoffCommittedFreeResumesPartialCheckoutAndRetiredDeletion(t *testing.T) {
	if _, err := exec.LookPath("lsof"); err != nil {
		t.Skip("lsof unavailable")
	}
	for _, index := range []int{0, 1} {
		t.Run(map[int]string{0: "partial ordinary checkout", 1: "partial retired Git"}[index], func(t *testing.T) {
			s, repo, cfg, stage := handoffFixture(t)
			if err := s.commitMaterializedCheckout(context.Background(), repo.ID, cfg, stage); err != nil {
				t.Fatal(err)
			}
			journal := committedFreeFixtureJournal(t, s, repo, stage)
			plan, err := s.localHandoffCleanupPlan(context.Background(), journal)
			if err != nil {
				t.Fatal(err)
			}
			injected := errors.New("injected deletion failure after one child")
			removed := 0
			err = handoffRemovePlannedObject(context.Background(), plan.Objects[index], func() error { removed++; return injected })
			if !errors.Is(err, injected) || removed != 1 {
				t.Fatalf("failure injection did not delete exactly one child: %d %v", removed, err)
			}
			if err := verifyHandoffCleanupSubset(context.Background(), plan.Objects[index]); err != nil {
				t.Fatalf("valid partial cleanup was rejected: %v", err)
			}
			if err := s.recoverLocalHandoffs(context.Background()); err != nil {
				t.Fatalf("committed partial cleanup failed recovery: %v", err)
			}
			if _, err := os.Stat(journal.Temporary); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("recovery left full or partial local storage")
			}
			if _, err := os.Stat(journal.Receipt.RetiredRoot); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("recovery left retired storage")
			}
			if _, err := os.Stat(filepath.Join(s.opts.StateDir, localHandoffCleanupName)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("recovery left stale deletion plan")
			}
		})
	}
}

func TestLocalHandoffPartialCleanupPreservesNewData(t *testing.T) {
	if _, err := exec.LookPath("lsof"); err != nil {
		t.Skip("lsof unavailable")
	}
	s, repo, cfg, stage := handoffFixture(t)
	if err := s.commitMaterializedCheckout(context.Background(), repo.ID, cfg, stage); err != nil {
		t.Fatal(err)
	}
	journal := committedFreeFixtureJournal(t, s, repo, stage)
	plan, err := s.localHandoffCleanupPlan(context.Background(), journal)
	if err != nil {
		t.Fatal(err)
	}
	_ = handoffRemovePlannedObject(context.Background(), plan.Objects[0], func() error { return errors.New("stop after one unlink") })
	newPath := filepath.Join(journal.Temporary, "new-work")
	newBytes := []byte{0, 255, 128, 0}
	writeCheckoutFixture(t, newPath, newBytes, 0600)
	if err := s.recoverLocalHandoffs(context.Background()); err == nil {
		t.Fatal("recovery deleted newly added files")
	}
	contents, err := os.ReadFile(newPath)
	if err != nil || !bytes.Equal(contents, newBytes) {
		t.Fatal("new local work was lost during recovery")
	}
	if _, err := os.Stat(filepath.Join(s.opts.StateDir, localHandoffJournalName)); err != nil {
		t.Fatal("recovery forgot retained data")
	}
}

func TestLocalHandoffBinaryFilenameProofIsDistinctAndCleanupDoesNotFollowLinks(t *testing.T) {
	record := checkoutFileRecord{Mode: 0600, Size: 1, Digest: "same"}
	left := handoffManifestDigest(map[string]checkoutFileRecord{"name\xff": record})
	right := handoffManifestDigest(map[string]checkoutFileRecord{"name\xfe": record})
	if left == right {
		t.Fatal("distinct binary filenames collapsed into one export proof")
	}
	root, outside := t.TempDir(), t.TempDir()
	writeCheckoutFixture(t, filepath.Join(outside, "preserve"), []byte{0, 255}, 0600)
	if err := os.Symlink(outside, filepath.Join(root, "external")); err != nil {
		t.Fatal(err)
	}
	file, err := handoffOpenDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := handoffRemoveChildren(file); err != nil {
		t.Fatal(err)
	}
	if actual, err := os.ReadFile(filepath.Join(outside, "preserve")); err != nil || !bytes.Equal(actual, []byte{0, 255}) {
		t.Fatal("cleanup followed an external symlink")
	}
}
