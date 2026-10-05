package gitstore

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/cloudflare/artifact-fs/internal/model"
)

// WorkingTreeBaselineRef keeps a persistent catalogue's original workingtree
// reachable even when Git HEAD moves and ordinary reflogs are pruned.
const WorkingTreeBaselineRef = "refs/reporeach/worktree-baseline"

// PinWorkingTreeBaseline installs or repairs the application-owned baseline
// ref. The persisted snapshot is authoritative; no branch, index, worktree,
// repository configuration, or external source is changed.
// The lifecycle owner must establish that GitDir belongs to its state root and
// serialize repository replacement before calling this startup-only operation.
func (s *Store) PinWorkingTreeBaseline(ctx context.Context, repo model.RepoConfig, oid string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !verificationObjectOID(oid) {
		return errors.New("workingtree baseline must be a full Git object ID")
	}
	oid = strings.ToLower(oid)
	if repo.PreparedGitDir {
		return errors.New("workingtree baseline requires an application-owned Git directory")
	}
	if err := validateWorkingTreeBaselineStorage(ctx, repo.GitDir); err != nil {
		return err
	}
	env := []string{"GIT_NO_LAZY_FETCH=1", "GIT_NO_REPLACE_OBJECTS=1", "GIT_TERMINAL_PROMPT=0"}
	refStorage, err := runGitWithEnv(ctx, repo.GitDir, env, "config", "--local", "--default", "files", "--get", "extensions.refStorage")
	if err != nil {
		return fmt.Errorf("cannot inspect workingtree baseline ref storage: %w", err)
	}
	if refStorage != "files" {
		return errors.New("workingtree baseline requires the Git files ref backend")
	}
	objectType, err := runGitWithEnv(ctx, repo.GitDir, env, "cat-file", "-t", oid)
	if err != nil {
		return fmt.Errorf("workingtree baseline is unavailable: %w", err)
	}
	if objectType != "commit" {
		return errors.New("workingtree baseline must name a commit")
	}
	refs, err := runGitWithEnv(ctx, repo.GitDir, env, "for-each-ref", "--format=%(objectname) %(refname) %(symref)", WorkingTreeBaselineRef)
	if err != nil {
		return fmt.Errorf("cannot inspect workingtree baseline ref: %w", err)
	}
	oldOID := strings.Repeat("0", len(oid))
	if refs != "" {
		fields := strings.Fields(refs)
		if len(fields) != 2 || !verificationObjectOID(fields[0]) || fields[1] != WorkingTreeBaselineRef {
			return errors.New("workingtree baseline ref contains invalid or symbolic metadata")
		}
		oldOID = fields[0]
	}
	// Do not follow a replaced symbolic ref into the user's branches. Disabling
	// hooks prevents a global or repository reference-transaction hook from
	// introducing side effects into this private metadata update.
	_, err = runGitWithEnv(ctx, repo.GitDir, env,
		"-c", "core.hooksPath=/dev/null", "-c", "core.logAllRefUpdates=false",
		"update-ref", "--no-deref", WorkingTreeBaselineRef, strings.ToLower(oid), oldOID)
	return err
}

func validateWorkingTreeBaselineStorage(ctx context.Context, gitDir string) error {
	if gitDir == "" || !filepath.IsAbs(gitDir) {
		return errors.New("workingtree baseline requires an absolute private Git directory")
	}
	st, err := os.Lstat(gitDir)
	if err != nil {
		return fmt.Errorf("cannot inspect workingtree baseline storage: %w", err)
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return errors.New("workingtree baseline storage must be a private directory")
	}
	for _, relative := range []string{"commondir", "gitdir", "info/grafts", "objects/info/alternates", "objects/info/http-alternates", "reftable"} {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := os.Lstat(filepath.Join(gitDir, relative)); err == nil {
			return errors.New("workingtree baseline storage contains external or unsupported Git metadata")
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	for _, relative := range []string{"HEAD", "config", "packed-refs", "shallow"} {
		if err := ctx.Err(); err != nil {
			return err
		}
		st, err := os.Lstat(filepath.Join(gitDir, relative))
		if errors.Is(err, os.ErrNotExist) && relative != "HEAD" && relative != "config" {
			continue
		}
		if err != nil {
			return err
		}
		if !st.Mode().IsRegular() || !workingTreeBaselinePrivateFile(st) {
			return errors.New("workingtree baseline storage contains invalid Git metadata")
		}
	}
	for _, relative := range []string{"objects", "refs", "logs", "info"} {
		if err := ctx.Err(); err != nil {
			return err
		}
		path := filepath.Join(gitDir, relative)
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) && relative != "objects" && relative != "refs" {
			continue
		}
		st, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !st.IsDir() {
			return errors.New("workingtree baseline storage contains invalid Git metadata directories")
		}
		if err := filepath.WalkDir(path, func(_ string, entry fs.DirEntry, err error) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err != nil {
				return err
			}
			if !entry.IsDir() && !entry.Type().IsRegular() {
				return errors.New("workingtree baseline storage contains invalid or symbolic Git metadata")
			}
			if !entry.IsDir() && (relative == "refs" || relative == "logs") {
				st, err := entry.Info()
				if err != nil {
					return err
				}
				if !workingTreeBaselinePrivateFile(st) {
					return errors.New("workingtree baseline storage contains shared mutable Git metadata")
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	refPath := filepath.Join(gitDir, filepath.FromSlash(WorkingTreeBaselineRef))
	if st, err := os.Lstat(refPath); err == nil && st.Size() > 66 {
		return errors.New("workingtree baseline ref contains invalid metadata")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if data, err := os.ReadFile(refPath); err == nil {
		if !verificationObjectOID(strings.TrimSpace(string(data))) {
			return errors.New("workingtree baseline ref contains invalid or symbolic metadata")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
