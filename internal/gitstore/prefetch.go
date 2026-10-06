package gitstore

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/cloudflare/artifact-fs/internal/model"
)

// Bound each request and its batch-check metadata while transferring a normal
// repository's current tree in a single pack instead of one connection per blob.
const prefetchBlobBatchSize = 1024

// PrefetchBlobs acquires exactly the missing selected blobs in bulk. It leaves
// HEAD, the index, FETCH_HEAD, refs, native config, and unrelated history alone.
// The caller still streams each object into its verified binary-safe cache;
// successful network transfer alone is not a completed download.
func (s *Store) PrefetchBlobs(ctx context.Context, repo model.RepoConfig, objectOIDs []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	unique := make([]string, 0, len(objectOIDs))
	seen := make(map[string]bool, len(objectOIDs))
	for _, oid := range objectOIDs {
		if !verificationObjectOID(oid) {
			return errors.New("cannot download an invalid blob identifier")
		}
		oid = strings.ToLower(oid)
		if !seen[oid] {
			seen[oid] = true
			unique = append(unique, oid)
		}
	}
	if len(unique) == 0 {
		return nil
	}
	missingObjects := make([]string, 0, len(unique))
	for start := 0; start < len(unique); start += prefetchBlobBatchSize {
		batch := unique[start:min(start+prefetchBlobBatchSize, len(unique))]
		missing, err := missingBlobObjects(ctx, repo, batch)
		if err != nil {
			return err
		}
		missingObjects = append(missingObjects, missing...)
	}
	if len(missingObjects) == 0 {
		return ctx.Err()
	}
	_, credentials, err := transportCredentialEnv(repo)
	if err != nil {
		return err
	}
	// Git's own promisor fetch uses noop negotiation: each requested blob is an
	// immutable leaf, so negotiating the existing commit graph adds no value.
	env, err := gitCommandEnv(append(nonInteractiveGitEnv(), credentials...), []string{
		"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=fetch.negotiationAlgorithm", "GIT_CONFIG_VALUE_0=noop",
	})
	if err != nil {
		return err
	}
	remotesForLogging, cancelLookup := s.startRemotesForLogging(ctx, repo)
	defer cancelLookup()
	for start := 0; start < len(missingObjects); start += prefetchBlobBatchSize {
		missing := missingObjects[start:min(start+prefetchBlobBatchSize, len(missingObjects))]
		input := strings.Join(missing, "\n") + "\n"
		err := s.retryGitOperationForRemoteLookup(ctx, GitOperationFetch, repo.Name, remotesForLogging, func() error {
			// Only literal blob OIDs enter stdin. No destinations are specified,
			// and --refmap= ignores origin's configured tracking ref mappings.
			// A filter is unnecessary for leaf blobs; explicitly setting one
			// could convert a native repository into a promisor clone.
			_, err := runGitWithInputEnvCapture(ctx, repo.GitDir, env, strings.NewReader(input), false,
				"fetch", "--no-tags", "--no-write-fetch-head", "--recurse-submodules=no",
				"--refmap=", "--no-prune", "--no-prune-tags", "--no-auto-maintenance", "--no-write-commit-graph",
				"--stdin", "origin")
			return err
		})
		if err != nil {
			return fmt.Errorf("download current-tree blobs: %w", err)
		}
		remaining, err := missingBlobObjects(ctx, repo, missing)
		if err != nil {
			return err
		}
		if len(remaining) != 0 {
			return errors.New("source did not provide all requested current-tree blobs")
		}
	}
	return ctx.Err()
}

// Never let presence checks trigger hidden one-object network requests. Every
// returned record must match the corresponding requested object and be a blob.
func missingBlobObjects(ctx context.Context, repo model.RepoConfig, oids []string) ([]string, error) {
	input := strings.Join(oids, "\n") + "\n"
	output, err := runGitWithInputEnvCapture(ctx, repo.GitDir, []string{"GIT_NO_LAZY_FETCH=1", "GIT_NO_REPLACE_OBJECTS=1"},
		strings.NewReader(input), true, "cat-file", "--batch-check=%(objectname) %(objecttype)", "--buffer")
	if err != nil {
		return nil, err
	}
	lines := strings.Split(output, "\n")
	if len(lines) != len(oids) {
		return nil, errors.New("invalid bulk blob presence metadata")
	}
	missing := make([]string, 0, len(oids))
	for index, line := range lines {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] != oids[index] {
			return nil, errors.New("invalid bulk blob presence metadata")
		}
		switch fields[1] {
		case "blob":
		case "missing":
			missing = append(missing, oids[index])
		default:
			return nil, errors.New("current-tree object is not a blob")
		}
	}
	return missing, nil
}
