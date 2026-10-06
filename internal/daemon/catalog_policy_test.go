package daemon

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/cloudflare/artifact-fs/internal/gitstore"
	"github.com/cloudflare/artifact-fs/internal/meta"
	"github.com/cloudflare/artifact-fs/internal/model"
)

func catalogPolicyStage(t *testing.T, cfg model.RepoConfig, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "blob")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	oid := strings.TrimSpace(runCmdOutput(t, "git", "--git-dir", cfg.GitDir, "hash-object", "-w", path))
	runCmd(t, "git", "--git-dir", cfg.GitDir, "update-index", "--add", "--cacheinfo", "100644,"+oid+","+name)
	return oid
}

func TestPersistentCatalogPinsPreparedBaselineBeforeOpeningAndSurvivesGitGC(t *testing.T) {
	ctx := context.Background()
	_, sourceCfg, binary := newCatalogTestRepository(t)
	root := t.TempDir()
	svc, err := New(ctx, root, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	if err := svc.SetCatalogViewPolicy(CatalogViewPersistentWorkingTree); err != nil {
		t.Fatal(err)
	}
	cfg := model.RepoConfig{Name: "prepared", RemoteURL: sourceCfg.RemoteURL, Branch: sourceCfg.Branch, MountPath: filepath.Join(t.TempDir(), "prepared")}
	if err := svc.AddRepo(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	cfg, err = svc.registry.GetRepo(ctx, cfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	baseline := strings.TrimSpace(runCmdOutput(t, "git", "--git-dir", cfg.GitDir, "rev-parse", "HEAD"))
	if pin := strings.TrimSpace(runCmdOutput(t, "git", "--git-dir", cfg.GitDir, "rev-parse", gitstore.WorkingTreeBaselineRef)); pin != baseline {
		t.Fatalf("prepared but unopened baseline was not pinned: %s", pin)
	}
	// An unrelated HEAD and index no longer keep the original tree reachable.
	// The app-owned ref must protect visible bytes when native Git expires all
	// reflogs and performs immediate garbage collection before first activation.
	changed := append([]byte(nil), binary...)
	changed[0] ^= 0xff
	catalogPolicyStage(t, cfg, "binary.bin", changed)
	tree := strings.TrimSpace(runCmdOutput(t, "git", "--git-dir", cfg.GitDir, "write-tree"))
	unrelated := strings.TrimSpace(runCmdOutput(t, "git", "--git-dir", cfg.GitDir, "-c", "user.name=test", "-c", "user.email=test@example.invalid", "commit-tree", tree, "-m", "unrelated"))
	runCmd(t, "git", "--git-dir", cfg.GitDir, "reset", "--mixed", unrelated)
	for _, ref := range strings.Fields(runCmdOutput(t, "git", "--git-dir", cfg.GitDir, "for-each-ref", "--format=%(refname)", "refs/remotes")) {
		runCmd(t, "git", "--git-dir", cfg.GitDir, "update-ref", "--no-deref", "-d", ref)
	}
	indexBefore := readCatalogFile(t, filepath.Join(cfg.GitDir, "index"))
	configBefore := readCatalogFile(t, filepath.Join(cfg.GitDir, "config"))
	runCmd(t, "git", "--git-dir", cfg.GitDir, "reflog", "expire", "--expire=now", "--all")
	runCmd(t, "git", "--git-dir", cfg.GitDir, "gc", "--prune=now")
	if got := strings.TrimSpace(runCmdOutput(t, "git", "--git-dir", cfg.GitDir, "cat-file", "-t", baseline)); got != "commit" {
		t.Fatalf("Git GC removed workingtree baseline: %s", got)
	}
	view, err := svc.OpenCatalogRepository(ctx, cfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	if got := readCatalogFuseFile(t, view, "binary.bin", 0, len(binary)+32); !bytes.Equal(got, binary) {
		t.Fatalf("GC or first activation changed retained bytes: %x", got)
	}
	if got := readCatalogFile(t, filepath.Join(cfg.GitDir, "index")); !bytes.Equal(got, indexBefore) {
		t.Fatal("GC baseline recovery changed the native index")
	}
	if got := readCatalogFile(t, filepath.Join(cfg.GitDir, "config")); !bytes.Equal(got, configBefore) {
		t.Fatal("baseline pin changed native repository configuration")
	}
	if status, err := svc.Status(ctx, cfg.Name); err != nil || status.CurrentHEADOID != unrelated {
		t.Fatalf("baseline pin changed actual Git HEAD: %+v, %v", status, err)
	}
	source := strings.TrimPrefix(sourceCfg.RemoteURL, "file://")
	if got := strings.TrimSpace(runCmdOutput(t, "git", "--git-dir", source, "rev-parse", "HEAD")); got != baseline {
		t.Fatalf("private GC or baseline pin changed original source: %s", got)
	}
	if refs := runCmdOutput(t, "git", "--git-dir", source, "for-each-ref", "--format=%(refname)", gitstore.WorkingTreeBaselineRef); strings.TrimSpace(refs) != "" {
		t.Fatal("baseline pin was written to the original source")
	}
}

func catalogPolicyCommit(t *testing.T, cfg model.RepoConfig, parent string) string {
	t.Helper()
	tree := strings.TrimSpace(runCmdOutput(t, "git", "--git-dir", cfg.GitDir, "write-tree"))
	return strings.TrimSpace(runCmdOutput(t, "git", "--git-dir", cfg.GitDir, "-c", "user.name=test", "-c", "user.email=test@example.invalid", "commit-tree", tree, "-p", parent, "-m", "fixture"))
}

func TestPersistentCatalogWorkingtreeSurvivesCommitResetReopenAndRestart(t *testing.T) {
	for _, mode := range []string{"--soft", "--mixed"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			svc, cfg, binary := newCatalogTestRepository(t)
			if err := svc.SetCatalogViewPolicy(CatalogViewPersistentWorkingTree); err != nil {
				t.Fatal(err)
			}
			fs, err := svc.OpenCatalogRepository(ctx, cfg.Name)
			if err != nil {
				t.Fatal(err)
			}
			rt := svc.running[cfg.ID]
			baseline, baselineRef, generation, err := rt.snapshot.ReadState(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if rt.active {
				t.Fatal("persistent catalogue started watcher or refresh workers")
			}
			// A native commit writes Git metadata after its worktree writes.
			// These canonical Engine calls are the same in-band callbacks that
			// the mounted backend receives; binary.bin remains untouched.
			note := []byte{0, 255, 'N', 128, '\n'}
			if err := rt.engine.Create(ctx, "note.bin", 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := rt.engine.Write(ctx, "note.bin", 0, note); err != nil {
				t.Fatal(err)
			}
			catalogPolicyStage(t, cfg, "note.bin", note)
			committed := catalogPolicyCommit(t, cfg, baseline)
			runCmd(t, "git", "--git-dir", cfg.GitDir, "update-ref", "HEAD", committed)
			svc.onHEADChanged(ctx, rt) // even an accidental queued callback is inert.
			if rt.resolver.Generation() != generation {
				t.Fatal("HEAD commit published a new mounted generation")
			}
			status, err := svc.Status(ctx, cfg.Name)
			if err != nil || status.CurrentHEADOID != committed {
				t.Fatalf("status reported baseline as current Git HEAD: %+v, %v", status, err)
			}
			// Build another real commit whose untouched file differs. A soft
			// or mixed reset changes HEAD/index, without changing worktree bytes.
			changed := append([]byte(nil), binary...)
			changed[2] ^= 0xff
			catalogPolicyStage(t, cfg, "binary.bin", changed)
			target := catalogPolicyCommit(t, cfg, committed)
			runCmd(t, "git", "--git-dir", cfg.GitDir, "read-tree", committed)
			runCmd(t, "git", "--git-dir", cfg.GitDir, "reset", mode, target)
			index := readCatalogFile(t, filepath.Join(cfg.GitDir, "index"))
			overlays, err := rt.overlay.ListAll(ctx)
			if err != nil {
				t.Fatal(err)
			}
			assertView := func(service *Service, view *fusefs.ArtifactFuse) {
				t.Helper()
				if got := readCatalogFuseFile(t, view, "binary.bin", 0, len(binary)+32); !bytes.Equal(got, binary) {
					t.Fatalf("HEAD-only reset changed untouched worktree bytes: %x", got)
				}
				if got := readCatalogFuseFile(t, view, "note.bin", 0, len(note)+32); !bytes.Equal(got, note) {
					t.Fatalf("retained committed overlay changed: %x", got)
				}
				if got := readCatalogFile(t, filepath.Join(cfg.GitDir, "index")); !bytes.Equal(got, index) {
					t.Fatal("opening persistent view changed the native index")
				}
				current := service.running[cfg.ID]
				oid, ref, gen, err := current.snapshot.ReadState(ctx)
				if err != nil || oid != baseline || ref != baselineRef || gen != generation {
					t.Fatalf("persistent baseline changed: %s %s %d %v", oid, ref, gen, err)
				}
				entries, err := current.overlay.ListAll(ctx)
				if err != nil || !reflect.DeepEqual(entries, overlays) {
					t.Fatalf("reopen reconciled or changed retained overlay: %+v, %v", entries, err)
				}
				status, err := service.Status(ctx, cfg.Name)
				if err != nil || status.CurrentHEADOID != target || status.SnapshotGeneration != generation {
					t.Fatalf("Git HEAD and workingtree generation were conflated: %+v, %v", status, err)
				}
			}
			assertView(svc, fs)
			if err := svc.Unmount(ctx, cfg.Name); err != nil {
				t.Fatal(err)
			}
			fs, err = svc.OpenCatalogRepository(ctx, cfg.Name)
			if err != nil {
				t.Fatal(err)
			}
			assertView(svc, fs)
			if err := svc.Close(); err != nil {
				t.Fatal(err)
			}
			restarted, err := New(ctx, svc.root, svc.logger)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = restarted.Close() })
			if err := restarted.SetCatalogViewPolicy(CatalogViewPersistentWorkingTree); err != nil {
				t.Fatal(err)
			}
			fs, err = restarted.OpenCatalogRepository(ctx, cfg.Name)
			if err != nil {
				t.Fatal(err)
			}
			assertView(restarted, fs)
			remote := strings.TrimPrefix(cfg.RemoteURL, "file://")
			if sourceHead := strings.TrimSpace(runCmdOutput(t, "git", "--git-dir", remote, "rev-parse", "HEAD")); sourceHead != baseline {
				t.Fatal("private reset or policy reopening changed the source repository")
			}
		})
	}
}

func TestPersistentCatalogRejectsMissingBaselineAndAlternateMountPaths(t *testing.T) {
	ctx := context.Background()
	for _, corruption := range []string{"missing database", "missing generation", "missing nodes"} {
		t.Run(corruption, func(t *testing.T) {
			svc, cfg, _ := newCatalogTestRepository(t)
			if err := svc.SetCatalogViewPolicy(CatalogViewPersistentWorkingTree); err != nil {
				t.Fatal(err)
			}
			if corruption == "missing database" {
				if err := os.Remove(cfg.MetaDBPath); err != nil {
					t.Fatal(err)
				}
			} else {
				db, err := meta.OpenDB(cfg.MetaDBPath)
				if err != nil {
					t.Fatal(err)
				}
				query := "DELETE FROM repo_state WHERE key='current_generation'"
				if corruption == "missing nodes" {
					query = "DELETE FROM base_nodes"
				}
				if _, err := db.ExecContext(ctx, query); err != nil {
					t.Fatal(err)
				}
				_ = db.Close()
			}
			if _, err := svc.OpenCatalogRepository(ctx, cfg.Name); err == nil || !strings.Contains(err.Error(), "baseline") {
				t.Fatalf("corrupt baseline silently rebuilt from current HEAD: %v", err)
			}
		})
	}
	svc, cfg, _ := newCatalogTestRepository(t)
	if err := svc.SetCatalogViewPolicy(CatalogViewPersistentWorkingTree); err != nil {
		t.Fatal(err)
	}
	if err := svc.Start(ctx); err == nil {
		t.Fatal("persistent catalogue allowed ordinary daemon mounts")
	}
	if err := svc.Remount(ctx, cfg.Name); err == nil {
		t.Fatal("persistent catalogue allowed an alternate live mount")
	}
	if _, err := svc.OpenCatalogRepository(ctx, cfg.Name); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetCatalogViewPolicy(CatalogViewLive); err == nil {
		t.Fatal("active persistent catalogue changed its policy")
	}
	if err := svc.Prepare(ctx, cfg.Name); err == nil {
		t.Fatal("preparation replaced an active persistent baseline")
	}
	if err := svc.Unmount(ctx, cfg.Name); err != nil {
		t.Fatal(err)
	}
	before, err := svc.registry.GetRepo(ctx, cfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	index := readCatalogFile(t, filepath.Join(cfg.GitDir, "index"))
	for _, replace := range []func() error{func() error { return svc.Prepare(ctx, cfg.Name) }, func() error { return svc.AddRepo(ctx, cfg) }} {
		if err := replace(); err == nil || !strings.Contains(err.Error(), "persistent workingtree") {
			t.Fatalf("preparation replaced an unmounted persistent baseline: %v", err)
		}
		if after, err := svc.registry.GetRepo(ctx, cfg.Name); err != nil || !reflect.DeepEqual(after, before) {
			t.Fatalf("refused preparation changed registration: %+v, %v", after, err)
		}
		if got := readCatalogFile(t, filepath.Join(cfg.GitDir, "index")); !bytes.Equal(got, index) {
			t.Fatal("refused preparation changed staged state")
		}
	}
}

func TestPersistentCatalogRefusesAliasedPrivateStorageBeforePinning(t *testing.T) {
	ctx := context.Background()
	svc, cfg, _ := newCatalogTestRepository(t)
	if err := svc.SetCatalogViewPolicy(CatalogViewPersistentWorkingTree); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Dir(cfg.GitDir)
	external := filepath.Join(t.TempDir(), "external")
	if err := os.Rename(parent, external); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, parent); err != nil {
		t.Fatal(err)
	}
	config := readCatalogFile(t, filepath.Join(external, "git", "config"))
	if _, err := svc.OpenCatalogRepository(ctx, cfg.Name); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("activation accepted an ancestor alias outside owned state: %v", err)
	}
	if got := readCatalogFile(t, filepath.Join(external, "git", "config")); !bytes.Equal(got, config) {
		t.Fatal("refused activation modified aliased Git configuration")
	}
	if refs := runCmdOutput(t, "git", "--git-dir", filepath.Join(external, "git"), "for-each-ref", "--format=%(refname)", gitstore.WorkingTreeBaselineRef); strings.TrimSpace(refs) != "" {
		t.Fatal("refused activation wrote baseline pin through ancestor alias")
	}
	// Initial preparation must enforce the same ownership check before cloning
	// or creating any repository metadata beneath the aliased parent.
	root := t.TempDir()
	unprepared, err := New(ctx, root, svc.logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unprepared.Close() })
	if err := unprepared.SetCatalogViewPolicy(CatalogViewPersistentWorkingTree); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "repos"), 0o700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "repos", "new")); err != nil {
		t.Fatal(err)
	}
	if err := unprepared.AddRepo(ctx, model.RepoConfig{Name: "new", RemoteURL: cfg.RemoteURL, Branch: cfg.Branch}); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("initial preparation accepted private ancestor alias: %v", err)
	}
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Fatalf("refused preparation wrote outside private state: %v, %v", entries, err)
	}
	if repos, err := unprepared.ListRepos(ctx); err != nil || len(repos) != 0 {
		t.Fatalf("refused initial preparation registered an unsafe repo: %+v, %v", repos, err)
	}
}

func TestPersistentCatalogRefusesAliasedStorageLeaves(t *testing.T) {
	for _, kind := range []string{"overlay database", "overlay upper", "blob cache", "snapshot WAL", "overlay SHM"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			svc, cfg, _ := newCatalogTestRepository(t)
			if err := svc.SetCatalogViewPolicy(CatalogViewPersistentWorkingTree); err != nil {
				t.Fatal(err)
			}
			path, directory := cfg.OverlayDBPath, false
			switch kind {
			case "overlay upper":
				path, directory = filepath.Join(cfg.OverlayDir, "upper"), true
			case "blob cache":
				path, directory = cfg.BlobCacheDir, true
			case "snapshot WAL":
				path = cfg.MetaDBPath + "-wal"
			case "overlay SHM":
				path = cfg.OverlayDBPath + "-shm"
			}
			if err := os.RemoveAll(path); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			outside := filepath.Join(t.TempDir(), "outside")
			if directory {
				if err := os.Mkdir(outside, 0o700); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(outside, []byte("external sentinel"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, path); err != nil {
				t.Fatal(err)
			}
			if _, err := svc.OpenCatalogRepository(ctx, cfg.Name); err == nil || !strings.Contains(err.Error(), "symbolic link") {
				t.Fatalf("activation followed %s alias: %v", kind, err)
			}
			if _, err := svc.Status(ctx, cfg.Name); err == nil || !strings.Contains(err.Error(), "symbolic link") {
				t.Fatalf("status opened stores through %s alias: %v", kind, err)
			}
			if directory {
				if files, err := os.ReadDir(outside); err != nil || len(files) != 0 {
					t.Fatalf("refused activation wrote outside storage: %v, %v", files, err)
				}
			} else if data := readCatalogFile(t, outside); !bytes.Equal(data, []byte("external sentinel")) {
				t.Fatal("refused activation changed external database sentinel")
			}
		})
	}
}

func TestPersistentCatalogDownloadsBothHeadAndWorkingtreeBaseline(t *testing.T) {
	ctx := context.Background()
	svc, cfg, binary := newCatalogTestRepository(t)
	if err := svc.SetCatalogViewPolicy(CatalogViewPersistentWorkingTree); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.OpenCatalogRepository(ctx, cfg.Name); err != nil {
		t.Fatal(err)
	}
	baseline, _, _, err := svc.running[cfg.ID].snapshot.ReadState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	oldBlob := strings.TrimSpace(runCmdOutput(t, "git", "--git-dir", cfg.GitDir, "rev-parse", "HEAD:binary.bin"))
	changed := append([]byte(nil), binary...)
	changed[0] = 255
	newBlob := catalogPolicyStage(t, cfg, "binary.bin", changed)
	target := catalogPolicyCommit(t, cfg, baseline)
	runCmd(t, "git", "--git-dir", cfg.GitDir, "reset", "--mixed", target)
	result, err := svc.DownloadCurrentTree(ctx, cfg.Name, nil)
	if err != nil || !result.Complete || result.TotalBlobs != 2 || result.HeadOID != target {
		t.Fatalf("persistent view download = %+v, %v", result, err)
	}
	for oid, expected := range map[string][]byte{oldBlob: binary, newBlob: changed} {
		if data := readCatalogFile(t, filepath.Join(cfg.BlobCacheDir, oid)); !bytes.Equal(data, expected) {
			t.Fatalf("workingtree/current HEAD blob missing or changed: %s", oid)
		}
	}
	if err := svc.FreeRepositorySpace(ctx, cfg.Name); err == nil || !strings.Contains(err.Error(), "baseline differs") {
		t.Fatalf("cleanup dropped untouched workingtree differences after mixed reset: %v", err)
	}
	if _, err := os.Stat(cfg.GitDir); err != nil {
		t.Fatal("refused cleanup removed private checkout")
	}
}

func TestPersistentCatalogReclaimsPristineRemoteBackedBaseline(t *testing.T) {
	ctx := context.Background()
	svc, cfg, _ := storageFixture(t)
	if err := svc.SetCatalogViewPolicy(CatalogViewPersistentWorkingTree); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.OpenCatalogRepository(ctx, cfg.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.DownloadCurrentTree(ctx, cfg.Name, nil); err != nil {
		t.Fatal(err)
	}
	if err := svc.FreeRepositorySpace(ctx, cfg.Name); err != nil {
		t.Fatalf("app-owned baseline pin prevented safe cleanup: %v", err)
	}
	for _, path := range []string{cfg.GitDir, cfg.MetaDBPath, cfg.BlobCacheDir} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("pristine owned data was not reclaimed at %s: %v", path, err)
		}
	}
	if _, err := os.Stat(cfg.OverlayDBPath); err != nil {
		t.Fatalf("cleanup removed local metadata database: %v", err)
	}
	if err := verifyEmptyOverlayUpper(cfg.OverlayDir); err != nil {
		t.Fatal(err)
	}
	if sourceHead := strings.TrimSpace(runCmdOutput(t, "git", "--git-dir", strings.TrimPrefix(cfg.RemoteURL, "file://"), "rev-parse", "HEAD")); sourceHead == "" {
		t.Fatal("safe cleanup removed original source")
	}
}
