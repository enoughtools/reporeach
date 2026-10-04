package gitstore

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cloudflare/artifact-fs/internal/model"
)

// CloseRepository releases pooled cat-file processes before an owned Git
// directory is moved. The caller must first stop all readers for this repository.
func (s *Store) CloseRepository(gitDir string) { s.invalidateBatchPool(gitDir) }

// VerifySafeToDiscard fails closed unless the index is unchanged and every
// object reachable from local refs, HEAD, ORIG_HEAD, and reflogs is recoverable
// from a fresh remote fetch. The caller must quiesce filesystem access first.
// Verification refs are isolated from the user's branches and tags.
func (s *Store) VerifySafeToDiscard(ctx context.Context, repo model.RepoConfig) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if repo.PreparedGitDir || strings.TrimSpace(repo.RemoteURL) == "" {
		return errors.New("cannot free space for an external Git directory or an unknown remote")
	}
	if err := verifyDisposableGitFiles(repo.GitDir); err != nil {
		return err
	}
	if err := verifyDisposableGitConfig(ctx, repo); err != nil {
		return err
	}
	// GIT_WORK_TREE is deliberately excluded: the virtual checkout has already
	// been detached. diff-index --cached checks the index without a worktree.
	env := append(nonInteractiveGitEnv(), "GIT_NO_LAZY_FETCH=1", "GIT_OPTIONAL_LOCKS=0")
	changed, err := runGitWithEnv(ctx, repo.GitDir, env, "diff-index", "--cached", "--name-only", "-z", "HEAD", "--")
	if err != nil {
		return fmt.Errorf("cannot verify index: %w", err)
	}
	if changed != "" {
		return errors.New("repository has staged changes; commit and push them before freeing space")
	}
	refs, err := runGitWithEnv(ctx, repo.GitDir, env, "for-each-ref", "--format=%(refname)")
	if err != nil {
		return err
	}
	for _, ref := range strings.Fields(refs) {
		if !strings.HasPrefix(ref, "refs/heads/") && !strings.HasPrefix(ref, "refs/tags/") && !strings.HasPrefix(ref, "refs/remotes/") {
			return errors.New("repository contains a stash, notes, or other local-only refs")
		}
	}
	safeURL, credentials, err := credentialEnv(repo.RemoteURL)
	if err != nil {
		return err
	}
	namespace := "refs/artifact-fs-verify/" + rand.Text() + "/"
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		refs, err := runGitWithEnv(cleanupCtx, repo.GitDir, env, "for-each-ref", "--format=%(refname)", namespace)
		if err == nil {
			for _, ref := range strings.Fields(refs) {
				_, _ = runGitWithEnv(cleanupCtx, repo.GitDir, env, "update-ref", "-d", ref)
			}
		}
	}()
	fetchEnv := append(nonInteractiveGitEnv(), credentials...)
	if _, err := runGitWithEnv(ctx, repo.GitDir, fetchEnv, "fetch", "--no-tags", "--no-write-fetch-head", "--filter=blob:none", safeURL,
		"+refs/heads/*:"+namespace+"heads/*", "+refs/tags/*:"+namespace+"tags/*"); err != nil {
		return fmt.Errorf("cannot verify remote backup: %w", err)
	}
	tips, err := runGitWithEnv(ctx, repo.GitDir, env, "for-each-ref", "--format=%(objectname)", namespace)
	if err != nil {
		return err
	}
	remoteTips := strings.Fields(tips)
	if len(remoteTips) == 0 {
		return errors.New("remote has no branches or tags; cannot verify a recoverable copy")
	}
	localNamed, err := runGitWithEnv(ctx, repo.GitDir, env, "for-each-ref", "--format=%(refname)", "refs/heads/", "refs/tags/")
	if err != nil {
		return err
	}
	remoteNamed, err := runGitWithEnv(ctx, repo.GitDir, env, "for-each-ref", "--format=%(refname)", namespace)
	if err != nil {
		return err
	}
	remoteNames := map[string]bool{}
	for _, ref := range strings.Fields(remoteNamed) {
		remoteNames[strings.TrimPrefix(ref, namespace)] = true
	}
	for _, ref := range strings.Fields(localNamed) {
		if !remoteNames[strings.TrimPrefix(ref, "refs/")] {
			return errors.New("repository has a local branch or tag name absent from the remote; publish it before freeing space")
		}
	}
	args := []string{"rev-list", "--objects", "--all", "--reflog", "HEAD"}
	if _, err := os.Lstat(filepath.Join(repo.GitDir, "ORIG_HEAD")); err == nil {
		args = append(args, "ORIG_HEAD")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	args = append(args, "--not")
	args = append(args, remoteTips...)
	localOnly, err := runGitWithEnv(ctx, repo.GitDir, env, args...)
	if err != nil {
		return fmt.Errorf("cannot verify local history: %w", err)
	}
	if localOnly != "" {
		return errors.New("repository has unpushed or recovered history in refs or reflogs; push or archive it before freeing space")
	}
	// Refs and reflogs do not name every recoverable object: staging and then
	// resetting a file leaves a dangling blob, and commit-tree can create an
	// unattached commit. Refuse those objects too rather than silently pruning.
	unreachable, err := runGitWithEnv(ctx, repo.GitDir, env, "fsck", "--connectivity-only", "--unreachable", "--no-progress")
	if err != nil {
		return fmt.Errorf("cannot verify object storage: %w", err)
	}
	if unreachable != "" {
		return errors.New("repository has unreachable Git objects; preserve or explicitly prune them before freeing space")
	}
	return ctx.Err()
}

func verifyDisposableGitConfig(ctx context.Context, repo model.RepoConfig) error {
	keys, err := runGitWithEnv(ctx, repo.GitDir, nonInteractiveGitEnv(), "config", "--local", "--name-only", "--list")
	if err != nil {
		return err
	}
	allowed := map[string]bool{
		"core.repositoryformatversion": true, "core.filemode": true, "core.bare": true,
		"core.logallrefupdates": true, "core.ignorecase": true, "core.precomposeunicode": true,
		"core.symlinks": true, "core.worktree": true, "core.fsmonitor": true,
		"core.untrackedcache": true, "index.version": true, "fsmonitor.allowremote": true,
		"remote.origin.url": true, "remote.origin.fetch": true, "remote.origin.promisor": true, "remote.origin.tagopt": true,
		"remote.origin.partialclonefilter": true, "extensions.partialclone": true, "extensions.objectformat": true,
	}
	for _, key := range strings.Split(keys, "\n") {
		key = strings.ToLower(strings.TrimSpace(key))
		if key == "" {
			continue
		}
		branchSetting := strings.HasPrefix(key, "branch.") && (strings.HasSuffix(key, ".remote") || strings.HasSuffix(key, ".merge"))
		githubHelper := key == "credential.https://github.com.helper"
		if !allowed[key] && !branchSetting && !githubHelper {
			return errors.New("repository has custom local Git configuration; preserve it before freeing space")
		}
		if key != "core.worktree" && key != "core.fsmonitor" && !githubHelper {
			continue
		}
		value, err := runGitWithEnv(ctx, repo.GitDir, nonInteractiveGitEnv(), "config", "--local", "--get-all", key)
		if err != nil {
			return err
		}
		switch key {
		case "core.worktree":
			if value != repo.MountPath {
				return errors.New("repository has a custom Git worktree")
			}
		case "core.fsmonitor":
			if value != filepath.Join(repo.GitDir, "hooks", "artifact-fs-fsmonitor") {
				return errors.New("repository has a custom Git filesystem monitor")
			}
		default:
			// RepoReach installs this helper using the bundled gh binary. The
			// helper owns no secret; gh stores credentials outside this Git dir.
			path, ok := strings.CutPrefix(value, "!GH_TELEMETRY=false '")
			if ok {
				path, ok = strings.CutSuffix(path, "' auth git-credential")
			}
			if !ok || !filepath.IsAbs(path) || filepath.Base(path) != "gh" || strings.ContainsAny(path, "'\n\r") {
				return errors.New("repository has a custom credential helper")
			}
		}
	}
	return nil
}

func verifyDisposableGitFiles(gitDir string) error {
	allowed := map[string]bool{
		"HEAD": true, "config": true, "description": true, "index": true,
		"packed-refs": true, "FETCH_HEAD": true, "ORIG_HEAD": true,
		"shallow": true, "objects": true, "refs": true, "logs": true,
		"hooks": true, "info": true, "branches": true,
	}
	entries, err := os.ReadDir(gitDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !allowed[entry.Name()] {
			return errors.New("repository has an in-progress Git operation or unrecognized local metadata")
		}
	}
	return filepath.WalkDir(gitDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("repository storage contains a symbolic link; refusing to discard it")
		}
		if strings.HasSuffix(entry.Name(), ".lock") {
			return errors.New("repository has a Git lock; wait for the operation to finish")
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(gitDir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, "hooks/") && !strings.HasSuffix(rel, ".sample") && rel != "hooks/artifact-fs-fsmonitor" {
			return errors.New("repository has custom Git hooks; preserve them before freeing space")
		}
		if strings.HasPrefix(rel, "branches/") || rel == "objects/info/alternates" || rel == "info/grafts" {
			return errors.New("repository has local metadata that cannot be recovered from the remote")
		}
		if strings.HasPrefix(rel, "info/") && rel != "info/exclude" && rel != "info/attributes" {
			return errors.New("repository has unrecognized local Git metadata")
		}
		if strings.HasPrefix(rel, "objects/") && !disposableObjectPath(strings.TrimPrefix(rel, "objects/")) {
			return errors.New("repository object storage contains unrecognized local files")
		}
		if strings.HasPrefix(rel, "logs/") && rel != "logs/HEAD" && !strings.HasPrefix(rel, "logs/refs/") {
			return errors.New("repository logs contain unrecognized local files")
		}
		if rel == "info/exclude" || rel == "info/attributes" {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, line := range strings.Split(string(data), "\n") {
				line = strings.TrimSpace(line)
				if line != "" && !strings.HasPrefix(line, "#") {
					return errors.New("repository has local ignore or attribute rules; preserve them before freeing space")
				}
			}
		}
		return nil
	})
}

func disposableObjectPath(path string) bool {
	parts := strings.Split(path, "/")
	if len(parts) != 2 {
		return strings.HasPrefix(path, "info/commit-graphs/") &&
			(strings.HasSuffix(path, ".graph") || strings.HasSuffix(path, "/commit-graph-chain"))
	}
	if len(parts[0]) == 2 && verificationObjectOID(parts[0]+parts[1]) {
		return true
	}
	if parts[0] == "info" {
		return parts[1] == "packs" || parts[1] == "commit-graph"
	}
	if parts[0] != "pack" {
		return false
	}
	if parts[1] == "multi-pack-index" {
		return true
	}
	name, ok := strings.CutPrefix(parts[1], "pack-")
	if !ok {
		return false
	}
	oid, extension, ok := strings.Cut(name, ".")
	if !ok || !verificationObjectOID(oid) {
		return false
	}
	switch extension {
	case "pack", "idx", "rev", "promisor", "bitmap":
		return true
	}
	return false
}

func verificationObjectOID(oid string) bool {
	if len(oid) != 40 && len(oid) != 64 {
		return false
	}
	for _, c := range oid {
		if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			return false
		}
	}
	return true
}
