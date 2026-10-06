package desktop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/cloudflare/artifact-fs/internal/model"
)

const previewGitGenerationName = "preview-git-generation.json"

type previewGitGeneration struct {
	Version      int                       `json:"version"`
	RepositoryID string                    `json:"repositoryID"`
	Object       localHandoffCleanupObject `json:"object"`
}

type previewBlobGeneration struct {
	Version      int                      `json:"version"`
	RepositoryID string                   `json:"repositoryID"`
	OID          string                   `json:"oid"`
	RootIdentity localHandoffIdentity     `json:"rootIdentity"`
	Entry        localHandoffCleanupEntry `json:"entry"`
}

func previewGenerationDirectory(stateDir, id, kind string) (string, error) {
	root := filepath.Join(stateDir, "preview-content-generations")
	for _, directory := range []string{root, filepath.Join(root, engineName(id)), filepath.Join(root, engineName(id), kind)} {
		if err := handoffPrivateDirectory(directory, true); err != nil {
			return "", err
		}
	}
	return filepath.Join(root, engineName(id), kind), nil
}

func previewGitGenerationPath(stateDir, id, gitDir string, create bool) (string, error) {
	relative, err := filepath.Rel(canonicalPreviewGitRoot(stateDir, id), gitDir)
	parts := strings.Split(filepath.ToSlash(relative), "/")
	if err != nil || len(parts) != 3 || !validHandoffDigest(parts[0]) || !validPreviewOID(parts[1]) || parts[2] != "git" {
		return "", errors.New("preview source path is outside its canonical cache")
	}
	digest := sha256.Sum256([]byte(relative))
	directory := filepath.Join(stateDir, "preview-content-generations", engineName(id), "git", hex.EncodeToString(digest[:]))
	if create {
		if _, err := previewGenerationDirectory(stateDir, id, "git"); err != nil {
			return "", err
		}
		if err := handoffPrivateDirectory(directory, true); err != nil {
			return "", err
		}
	}
	return filepath.Join(directory, previewGitGenerationName), nil
}

// These receipts are made by the producer, before the cache can be offered for
// reclamation. Taking a new inventory at Free would silently adopt user files.
// Existing generations must match exactly before another owned Git operation.
func beginPreviewGitGeneration(ctx context.Context, stateDir, id, gitDir string) error {
	path, err := previewGitGenerationPath(stateDir, id, gitDir, true)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(gitDir); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if err := verifyHandoffDirectory(gitDir, localHandoffIdentity{}, true); err != nil {
		return err
	}
	entries, err := os.ReadDir(gitDir)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return nil
	}
	var saved previewGitGeneration
	if err := handoffReadJSON(path, &saved); err != nil {
		return errors.New("preview source has no complete producer receipt; its files were retained")
	}
	if saved.Version != 1 || saved.RepositoryID != id || saved.Object.Root != gitDir {
		return errors.New("preview source receipt does not match this acquisition")
	}
	return verifyPreviewGeneration(ctx, saved.Object)
}

func finishPreviewGitGeneration(ctx context.Context, stateDir, id, gitDir string) error {
	path, err := previewGitGenerationPath(stateDir, id, gitDir, true)
	if err != nil {
		return err
	}
	if err := validatePreviewGeneratedGit(ctx, gitDir); err != nil {
		return err
	}
	object, err := capturePreviewCleanupObject(ctx, gitDir)
	if err != nil {
		return err
	}
	if err := handoffSyncDirectory(gitDir); err != nil {
		return err
	}
	return handoffWriteJSON(path, previewGitGeneration{Version: 1, RepositoryID: id, Object: object}, false)
}

func verifyPreviewGeneration(ctx context.Context, object localHandoffCleanupObject) error {
	if err := verifyHandoffDirectory(object.Root, object.Identity, false); err != nil {
		return err
	}
	expected, err := handoffCleanupEntries(object)
	if err != nil {
		return err
	}
	current, err := checkoutTreeManifest(ctx, object.Root, false)
	if err != nil {
		return err
	}
	if len(current) != len(expected) {
		return errors.New("preview cache contains new or missing data; all of its files were retained")
	}
	for path, record := range current {
		entry, exists := expected[path]
		identity, err := handoffReadCleanupIdentity(filepath.Join(object.Root, path))
		if err != nil || !exists || identity != entry.Identity || handoffEntryProof(record) != entry.Proof {
			return errors.New("preview cache changed outside its producer; all of its files were retained")
		}
	}
	return nil
}

func recordPreviewBlobGeneration(ctx context.Context, stateDir, id, path string, node model.BaseNode) error {
	root := filepath.Join(stateDir, "engine", "cache", "blobs", engineName(id))
	if !validPreviewOID(node.ObjectOID) || path != filepath.Join(root, node.ObjectOID) {
		return errors.New("preview blob is outside its canonical cache")
	}
	file, err := openVerifiedPreviewBlob(ctx, path, node)
	if err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	rootIdentity, err := handoffReadIdentity(root)
	if err != nil {
		return err
	}
	manifest, err := checkoutTreeManifest(ctx, path, false)
	if err != nil {
		return err
	}
	identity, err := handoffReadCleanupIdentity(path)
	if err != nil {
		return err
	}
	directory, err := previewGenerationDirectory(stateDir, id, "blobs")
	if err != nil {
		return err
	}
	if err := handoffSyncDirectory(root); err != nil {
		return err
	}
	return handoffWriteJSON(filepath.Join(directory, node.ObjectOID+".json"), previewBlobGeneration{
		Version: 1, RepositoryID: id, OID: node.ObjectOID, RootIdentity: rootIdentity,
		Entry: localHandoffCleanupEntry{Identity: identity, Proof: handoffEntryProof(manifest["."])},
	}, false)
}

func verifyPreviewBlobGeneration(ctx context.Context, stateDir, id, root string) error {
	if _, err := os.Lstat(root); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err := validatePreviewBlobCache(ctx, root); err != nil {
		return err
	}
	rootIdentity, err := handoffReadIdentity(root)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		var saved previewBlobGeneration
		path := filepath.Join(root, entry.Name())
		receipt := filepath.Join(stateDir, "preview-content-generations", engineName(id), "blobs", entry.Name()+".json")
		if err := handoffReadJSON(receipt, &saved); err != nil {
			return errors.New("blob cache contains data without a producer receipt; all cached data was retained")
		}
		identity, err := handoffReadCleanupIdentity(path)
		manifest, manifestErr := checkoutTreeManifest(ctx, path, false)
		if err != nil || manifestErr != nil || saved.Version != 1 || saved.RepositoryID != id || saved.OID != entry.Name() || saved.RootIdentity != rootIdentity || saved.Entry.Identity != identity || saved.Entry.Proof != handoffEntryProof(manifest["."]) {
			return errors.New("cached blob was replaced or changed outside its producer; all cached data was retained")
		}
	}
	return nil
}

// A second check after the owned operation keeps accidental Git hooks, extra
// refs and unknown metadata out of a newly recorded producer generation.
func validatePreviewGeneratedGit(ctx context.Context, gitDir string) error {
	if err := validateStandaloneCheckoutGit(ctx, gitDir); err != nil {
		return err
	}
	allowed := map[string]bool{"HEAD": true, "config": true, "description": true, "index": true, "packed-refs": true, "FETCH_HEAD": true, "shallow": true, "objects": true, "refs": true, "logs": true, "hooks": true, "info": true, "branches": true}
	entries, err := os.ReadDir(gitDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !allowed[entry.Name()] {
			return errors.New("preview source contains unrecognized Git metadata; it was retained")
		}
	}
	refs, err := localCheckoutGit(ctx, gitDir, "for-each-ref", "--format=%(refname)")
	if err != nil || refs != "refs/remotes/artifact-fs/source" {
		return errors.New("preview source contains non-acquisition Git refs; it was retained")
	}
	return filepath.WalkDir(gitDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(gitDir, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if strings.HasPrefix(relative, "hooks/") && !strings.HasSuffix(relative, ".sample") || strings.HasPrefix(relative, "branches/") || strings.HasPrefix(relative, "info/") && relative != "info/exclude" {
			return errors.New("preview source contains local Git metadata; it was retained")
		}
		return nil
	})
}
