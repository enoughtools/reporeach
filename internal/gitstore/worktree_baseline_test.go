package gitstore

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/model"
)

func baselineFixture(t *testing.T, format string) (model.RepoConfig, string, string, string, []byte) {
	t.Helper()
	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	run(t, "git", "init", "--object-format="+format, "--initial-branch=main", source)
	tracked := filepath.Join(source, "tracked.bin")
	if err := os.WriteFile(tracked, []byte("original\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", source, "add", "tracked.bin")
	run(t, "git", "-C", source, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "original")
	first := strings.TrimSpace(runOutput(t, "git", "-C", source, "rev-parse", "HEAD"))
	data := []byte{0, 0xff, '\n', 'b', 0x80, 0, 'e', '\r', 'n', 'd'}
	if err := os.WriteFile(tracked, data, 0o600); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", source, "add", "tracked.bin")
	run(t, "git", "-C", source, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "baseline")
	second := strings.TrimSpace(runOutput(t, "git", "-C", source, "rev-parse", "HEAD"))
	// --no-local gives the private clone independent ref/index/config metadata.
	// --no-checkout avoids any source worktree writes during acquisition.
	private := filepath.Join(dir, "private")
	run(t, "git", "clone", "--no-local", "--no-checkout", source, private)
	run(t, "git", "-C", private, "read-tree", "HEAD")
	cfg := model.RepoConfig{ID: "private", Name: "private", GitDir: filepath.Join(private, ".git"), MountPath: private, RemoteURL: source, Branch: "main"}
	return cfg, source, first, second, data
}

func TestPinWorkingTreeBaselinePreservesPrivateAndSourceState(t *testing.T) {
	cfg, source, first, second, _ := baselineFixture(t, "sha1")
	ctx := context.Background()
	global := filepath.Join(t.TempDir(), "global.gitconfig")
	if err := os.WriteFile(global, []byte("[user]\n\tname = Native User\n\temail = native@example.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	for _, worktree := range []string{source, cfg.MountPath} {
		if err := os.WriteFile(filepath.Join(worktree, "tracked.bin"), []byte("staged work\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		run(t, "git", "-C", worktree, "add", "tracked.bin")
		if err := os.WriteFile(filepath.Join(worktree, "tracked.bin"), []byte("unstaged work\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	paths := []string{global}
	for _, worktree := range []string{source, cfg.MountPath} {
		for _, relative := range []string{"HEAD", "index", "config", "refs/heads/main"} {
			paths = append(paths, filepath.Join(worktree, ".git", relative))
		}
		paths = append(paths, filepath.Join(worktree, "tracked.bin"))
	}
	before := make(map[string][]byte)
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		before[path] = data
	}
	// A service launched from Git must not inherit bindings to its source.
	sourceGitDir := filepath.Join(source, ".git")
	for key, value := range map[string]string{
		"GIT_DIR": sourceGitDir, "GIT_WORK_TREE": source, "GIT_COMMON_DIR": sourceGitDir,
		"GIT_INDEX_FILE": filepath.Join(sourceGitDir, "index"), "GIT_OBJECT_DIRECTORY": filepath.Join(sourceGitDir, "objects"),
		"GIT_ALTERNATE_OBJECT_DIRECTORIES": filepath.Join(sourceGitDir, "objects"), "GIT_NAMESPACE": "source",
		"GIT_REPLACE_REF_BASE": "refs/source/", "GIT_CONFIG": filepath.Join(sourceGitDir, "config"),
	} {
		t.Setenv(key, value)
	}
	store := New(nil)
	defer store.Close()
	for _, oid := range []string{second, second, first, strings.ToUpper(second)} {
		if err := store.PinWorkingTreeBaseline(ctx, cfg, oid); err != nil {
			t.Fatal(err)
		}
		if got, err := runGit(ctx, cfg.GitDir, "rev-parse", WorkingTreeBaselineRef); err != nil || !strings.EqualFold(got, oid) {
			t.Fatalf("baseline = %q, error = %v, want %q", got, err, oid)
		}
	}
	for path, wanted := range before {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, wanted) {
			t.Fatalf("baseline update changed preserved state at %s: %v", path, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(sourceGitDir, filepath.FromSlash(WorkingTreeBaselineRef))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("baseline ref reached source: %v", err)
	}
}

func TestPinWorkingTreeBaselineKeepsCommitTreeAndBinaryBlobThroughGC(t *testing.T) {
	for _, format := range []string{"sha1", "sha256"} {
		t.Run(format, func(t *testing.T) {
			cfg, _, first, baseline, data := baselineFixture(t, format)
			ctx := context.Background()
			store := New(nil)
			defer store.Close()
			if err := store.PinWorkingTreeBaseline(ctx, cfg, baseline); err != nil {
				t.Fatal(err)
			}
			// Packed refs are a normal result of gc and must remain repairable.
			run(t, "git", "-C", cfg.MountPath, "pack-refs", "--all", "--prune")
			if err := store.PinWorkingTreeBaseline(ctx, cfg, baseline); err != nil {
				t.Fatal(err)
			}
			tree := strings.TrimSpace(runOutput(t, "git", "-C", cfg.MountPath, "rev-parse", baseline+"^{tree}"))
			blob := strings.TrimSpace(runOutput(t, "git", "-C", cfg.MountPath, "rev-parse", baseline+":tracked.bin"))
			unreachable := strings.TrimSpace(runOutput(t, "git", "-C", cfg.MountPath, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit-tree", tree, "-m", "unreachable control"))
			run(t, "git", "-C", cfg.MountPath, "reset", "--hard", first)
			refs := strings.Fields(runOutput(t, "git", "-C", cfg.MountPath, "for-each-ref", "--format=%(refname)"))
			for _, ref := range refs {
				if ref != WorkingTreeBaselineRef && ref != "refs/heads/main" {
					run(t, "git", "-C", cfg.MountPath, "update-ref", "-d", ref)
				}
			}
			if err := os.Remove(filepath.Join(cfg.GitDir, "ORIG_HEAD")); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			run(t, "git", "-C", cfg.MountPath, "reflog", "expire", "--expire=now", "--expire-unreachable=now", "--all")
			run(t, "git", "-C", cfg.MountPath, "gc", "--prune=now")
			for _, object := range []string{baseline, tree, blob} {
				if _, err := runGit(ctx, cfg.GitDir, "cat-file", "-e", object); err != nil {
					t.Fatalf("gc lost baseline object %s: %v", object, err)
				}
			}
			if _, err := runGit(ctx, cfg.GitDir, "cat-file", "-e", unreachable); err == nil {
				t.Fatal("gc did not prune the unreferenced control commit")
			}
			got, err := store.ReadBlob(ctx, cfg, blob, 100)
			if err != nil || !bytes.Equal(got, data) {
				t.Fatalf("baseline binary blob was not preserved: %v", err)
			}
			if err := store.PinWorkingTreeBaseline(ctx, cfg, baseline); err != nil {
				t.Fatalf("baseline cannot reopen after gc: %v", err)
			}
		})
	}
}

func TestPinWorkingTreeBaselineRejectsUnsafeStorageBeforeGit(t *testing.T) {
	for _, name := range []string{"gitdir-symlink", "refs-symlink", "logs-symlink", "objects-symlink", "config-symlink", "packed-refs-symlink", "commondir", "alternates", "reftable", "symbolic-ref", "invalid-ref", "shared-log", "shared-ref", "prepared"} {
		t.Run(name, func(t *testing.T) {
			cfg, _, _, baseline, _ := baselineFixture(t, "sha1")
			outside := t.TempDir()
			refPath := filepath.Join(cfg.GitDir, filepath.FromSlash(WorkingTreeBaselineRef))
			switch name {
			case "gitdir-symlink":
				alias := filepath.Join(outside, "alias.git")
				if err := os.Symlink(cfg.GitDir, alias); err != nil {
					t.Fatal(err)
				}
				cfg.GitDir = alias
			case "refs-symlink", "logs-symlink", "objects-symlink", "config-symlink", "packed-refs-symlink":
				relative := strings.TrimSuffix(name, "-symlink")
				original := filepath.Join(cfg.GitDir, relative)
				target := filepath.Join(outside, relative)
				if err := os.Rename(original, target); errors.Is(err, os.ErrNotExist) {
					if err := os.WriteFile(target, []byte("# pack-refs with: peeled fully-peeled sorted\n"), 0o600); err != nil {
						t.Fatal(err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, original); err != nil {
					t.Fatal(err)
				}
			case "commondir", "alternates":
				relative := "commondir"
				if name == "alternates" {
					relative = "objects/info/alternates"
				}
				if err := os.WriteFile(filepath.Join(cfg.GitDir, relative), []byte(outside+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "reftable":
				if err := os.Mkdir(filepath.Join(cfg.GitDir, "reftable"), 0o700); err != nil {
					t.Fatal(err)
				}
			case "symbolic-ref", "invalid-ref", "shared-ref":
				if err := os.MkdirAll(filepath.Dir(refPath), 0o700); err != nil {
					t.Fatal(err)
				}
				contents := "ref: refs/heads/main\n"
				if name == "invalid-ref" {
					contents = "unrecognized metadata\n"
				}
				if name == "shared-ref" {
					contents = baseline + "\n"
				}
				if err := os.WriteFile(refPath, []byte(contents), 0o600); err != nil {
					t.Fatal(err)
				}
				if name == "shared-ref" {
					if err := os.Link(refPath, filepath.Join(outside, "source-ref")); err != nil {
						t.Fatal(err)
					}
				}
			case "shared-log":
				log := filepath.Join(cfg.GitDir, "logs", filepath.FromSlash(WorkingTreeBaselineRef))
				if err := os.MkdirAll(filepath.Dir(log), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(log, []byte("existing private reflog\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Link(log, filepath.Join(outside, "source-log")); err != nil {
					t.Fatal(err)
				}
			case "prepared":
				cfg.PreparedGitDir = true
			}
			marker := filepath.Join(outside, "git-was-run")
			bin := t.TempDir()
			if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\ntouch \"$BASELINE_TEST_MARKER\"\nexit 97\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("BASELINE_TEST_MARKER", marker)
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			if err := New(nil).PinWorkingTreeBaseline(context.Background(), cfg, baseline); err == nil {
				t.Fatal("unsafe Git metadata accepted")
			}
			if _, err := os.Lstat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("unsafe Git metadata reached a command")
			}
		})
	}
}

func TestPinWorkingTreeBaselineRejectsNonCommitAndInvalidOID(t *testing.T) {
	cfg, _, _, baseline, _ := baselineFixture(t, "sha1")
	ctx := context.Background()
	store := New(nil)
	defer store.Close()
	if err := store.PinWorkingTreeBaseline(ctx, cfg, baseline); err != nil {
		t.Fatal(err)
	}
	for _, object := range []string{baseline + "^{tree}", baseline + ":tracked.bin"} {
		oid := strings.TrimSpace(runOutput(t, "git", "-C", cfg.MountPath, "rev-parse", object))
		if err := store.PinWorkingTreeBaseline(ctx, cfg, oid); err == nil {
			t.Fatal("noncommit baseline accepted")
		}
	}
	for _, oid := range []string{"", "--help", "HEAD", baseline[:39], baseline + "\n", strings.Repeat("g", 40), strings.Repeat("0", 40), strings.Repeat("a", 64)} {
		if err := store.PinWorkingTreeBaseline(ctx, cfg, oid); err == nil {
			t.Fatalf("invalid/unavailable baseline accepted: %q", oid)
		}
	}
	if got, err := runGit(ctx, cfg.GitDir, "rev-parse", WorkingTreeBaselineRef); err != nil || got != baseline {
		t.Fatalf("ref changed after invalid request: %q, %v", got, err)
	}
	// A replacement mapping a blob to a commit must not bypass type checks.
	blob := strings.TrimSpace(runOutput(t, "git", "-C", cfg.MountPath, "rev-parse", baseline+":tracked.bin"))
	run(t, "git", "-C", cfg.MountPath, "replace", "--force", blob, baseline)
	if err := store.PinWorkingTreeBaseline(ctx, cfg, blob); err == nil {
		t.Fatal("replacement ref disguised a blob as a commit")
	}
}

func TestPinWorkingTreeBaselineSuppressesReferenceTransactionHooks(t *testing.T) {
	cfg, _, _, baseline, _ := baselineFixture(t, "sha1")
	dir := t.TempDir()
	marker := filepath.Join(dir, "hook-ran")
	hook := filepath.Join(dir, "reference-transaction")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntouch \"$BASELINE_TEST_HOOK_MARKER\"\nexit 99\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASELINE_TEST_HOOK_MARKER", marker)
	global := filepath.Join(t.TempDir(), "global.gitconfig")
	run(t, "git", "config", "--file", global, "core.hooksPath", dir)
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.hooksPath")
	t.Setenv("GIT_CONFIG_VALUE_0", dir)
	if err := New(nil).PinWorkingTreeBaseline(context.Background(), cfg, baseline); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("reference-transaction hook was invoked")
	}
}

func TestPinWorkingTreeBaselineCancellationAndRedaction(t *testing.T) {
	cfg, _, _, baseline, _ := baselineFixture(t, "sha1")
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	git := filepath.Join(bin, "git")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BASELINE_TEST_REAL_GIT", realGit)
	for _, mode := range []string{"redaction", "cancellation"} {
		t.Run(mode, func(t *testing.T) {
			script := "#!/bin/sh\necho 'https://user:baseline-secret@example.com/repo' >&2\nexit 97\n"
			if mode == "cancellation" {
				script = "#!/bin/sh\nexec sleep 20\n"
			}
			if err := os.WriteFile(git, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
			defer cancel()
			err := New(nil).PinWorkingTreeBaseline(ctx, cfg, baseline)
			if err == nil {
				t.Fatal("Git failure accepted")
			}
			if mode == "redaction" && strings.Contains(err.Error(), "baseline-secret") {
				t.Fatalf("credential leaked: %v", err)
			}
			if mode == "cancellation" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("deadline was lost: %v", err)
			}
		})
	}
}

func TestVerifySafeToDiscardRecognizesOnlyRecoverableWorkingTreeBaseline(t *testing.T) {
	for _, mode := range []string{"recoverable", "unpushed-baseline", "other-private-ref", "symbolic-baseline"} {
		t.Run(mode, func(t *testing.T) {
			cfg, _, _, baseline, _ := baselineFixture(t, "sha1")
			ctx := context.Background()
			store := New(nil)
			defer store.Close()
			if mode == "unpushed-baseline" {
				baseline = strings.TrimSpace(runOutput(t, "git", "-C", cfg.MountPath, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit-tree", "HEAD^{tree}", "-p", "HEAD", "-m", "private baseline"))
			}
			if err := store.PinWorkingTreeBaseline(ctx, cfg, baseline); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "other-private-ref":
				run(t, "git", "-C", cfg.MountPath, "update-ref", "refs/reporeach/keep-my-work", baseline)
			case "symbolic-baseline":
				run(t, "git", "-C", cfg.MountPath, "symbolic-ref", WorkingTreeBaselineRef, "refs/heads/main")
			}
			err := store.VerifySafeToDiscard(ctx, cfg)
			if mode == "recoverable" && err != nil {
				t.Fatalf("recoverable application-owned baseline prevented reclamation: %v", err)
			}
			if mode != "recoverable" && err == nil {
				t.Fatalf("unsafe %s baseline was accepted for reclamation", mode)
			}
			if mode == "unpushed-baseline" && !strings.Contains(err.Error(), "unpushed or recovered history") {
				t.Fatalf("baseline did not participate in object recoverability: %v", err)
			}
		})
	}
}
