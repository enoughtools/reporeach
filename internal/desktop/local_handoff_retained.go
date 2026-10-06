package desktop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type retainedLocalCheckout struct {
	Version      int                  `json:"version"`
	RepositoryID string               `json:"repositoryID"`
	Path         string               `json:"path"`
	Identity     localHandoffIdentity `json:"identity"`
	Digest       string               `json:"digest"`
}

// A published but uncommitted copy can contain later native edits. Recovery
// retains it and records its location durably rather than losing ownership
// accounting when the active transaction is resolved.
func (s *Service) retainUncommittedLocalCheckout(ctx context.Context, journal localHandoffJournal) error {
	if err := verifyHandoffDirectory(journal.Temporary, journal.Identity, false); err != nil {
		return err
	}
	// The published export originally pointed core.worktree at the catalogue
	// leaf. This recovered ordinary copy must point at its retained location.
	if _, err := localCheckoutGit(ctx, journal.Temporary, "config", "--local", "core.worktree", journal.Temporary); err != nil {
		return errors.New("could not make the retained checkout independent; its data and recovery journal were preserved")
	}
	if err := syncCheckoutTree(ctx, journal.Temporary); err != nil {
		return err
	}
	digest, err := handoffTreeDigest(ctx, journal.Temporary)
	if err != nil {
		return err
	}
	record := retainedLocalCheckout{Version: 1, RepositoryID: journal.RepositoryID, Path: journal.Temporary, Identity: journal.Identity, Digest: digest}
	parent := filepath.Join(s.opts.StateDir, "retained-local-checkouts")
	if err := handoffPrivateDirectory(parent, true); err != nil {
		return err
	}
	hash := sha256.Sum256([]byte(journal.Temporary))
	path := filepath.Join(parent, hex.EncodeToString(hash[:])+".json")
	var saved retainedLocalCheckout
	if err := handoffReadJSON(path, &saved); err == nil {
		if saved.Version != 1 || saved.Path != record.Path || saved.Identity != record.Identity || saved.RepositoryID != record.RepositoryID {
			return errors.New("retained checkout ownership record changed; both copies were preserved")
		}
	} else if errors.Is(err, os.ErrNotExist) {
		if err := handoffWriteJSON(path, record, true); err != nil {
			return err
		}
	} else {
		return err
	}
	// No engine data moves before Keep commits. The original rollback container
	// is therefore still empty and can be removed by its exact owned identity.
	return handoffRemoveOwnedEmptyDirectory(journal.Receipt.RetiredRoot, journal.Receipt.RetiredIdentity)
}

// Load once at startup and retain this message separately from transient mount
// status. This inspects private ownership records, not Git or checkout bytes.
func (s *Service) retainedLocalCheckoutMessage() (string, error) {
	root := filepath.Join(s.opts.StateDir, "retained-local-checkouts")
	if _, err := os.Lstat(root); errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err := handoffPrivateDirectory(root, false); err != nil {
		return "", err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", err
	}
	var paths []string
	for _, entry := range entries {
		if !entry.Type().IsRegular() || len(entry.Name()) != 69 || !strings.HasSuffix(entry.Name(), ".json") {
			return "", errors.New("retained checkout ownership folder contains unknown records")
		}
		var record retainedLocalCheckout
		if err := handoffReadJSON(filepath.Join(root, entry.Name()), &record); err != nil {
			return "", err
		}
		owner, name, ok := strings.Cut(record.RepositoryID, "/")
		hash := sha256.Sum256([]byte(record.Path))
		if record.Version != 1 || !ok || validateComponent(owner) != nil || validateComponent(name) != nil || !record.Identity.valid() || !validHandoffDigest(record.Digest) || !filepath.IsAbs(record.Path) || hasAdoptionControl(record.Path) || !strings.HasPrefix(filepath.Base(record.Path), ".reporeach-checkout-") || entry.Name() != hex.EncodeToString(hash[:])+".json" {
			return "", errors.New("invalid retained local checkout ownership record")
		}
		if _, err := os.Lstat(record.Path); errors.Is(err, os.ErrNotExist) {
			continue
		}
		paths = append(paths, record.Path)
	}
	if len(paths) == 0 {
		return "", nil
	}
	if len(paths) == 1 {
		return "An interrupted Keep left a recovered checkout copy at " + paths[0] + ". Its files were retained.", nil
	}
	return fmt.Sprintf("%d recovered checkout copies were retained after interrupted Keep operations. Their locations are recorded in %s.", len(paths), root), nil
}
