package desktop

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/cloudflare/artifact-fs/internal/model"
)

type persistedState struct {
	SchemaVersion         int          `json:"schemaVersion"`
	MountRoot             string       `json:"mountRoot"`
	MountDesired          bool         `json:"mountDesired"`
	Account               *Account     `json:"account,omitempty"`
	Repositories          []Repository `json:"repositories"`
	DisabledOrganizations []string     `json:"disabledOrganizations,omitempty"`
}

func readState(path, mountRoot string) (persistedState, error) {
	state := persistedState{SchemaVersion: 2, MountRoot: mountRoot, Repositories: []Repository{}}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	defer f.Close()
	decoder := json.NewDecoder(io.LimitReader(f, 32<<20))
	decoder.DisallowUnknownFields()
	var saved persistedState
	if err := decoder.Decode(&saved); err != nil {
		return state, fmt.Errorf("read catalogue: %w", err)
	}
	if decoder.Decode(new(any)) != io.EOF {
		return state, errors.New("catalogue has trailing data")
	}
	if saved.SchemaVersion != 1 && saved.SchemaVersion != 2 {
		return state, fmt.Errorf("unsupported catalogue schema version %d", saved.SchemaVersion)
	}
	state = saved
	// Version 1 had neither manual sources nor visibility policies. Defaults
	// preserve its catalogue; the next atomic save records the upgraded schema.
	state.SchemaVersion = 2
	owners := make(map[string]bool, len(state.DisabledOrganizations))
	for _, owner := range state.DisabledOrganizations {
		if err := validateComponent(owner); err != nil {
			return state, fmt.Errorf("invalid disabled organization: %w", err)
		}
		key := strings.ToLower(owner)
		if owners[key] {
			return state, errors.New("catalogue contains duplicate disabled organizations")
		}
		owners[key] = true
	}
	if err := validateMountRoot(state.MountRoot); err != nil {
		return state, fmt.Errorf("invalid saved mount root: %w", err)
	}
	seen := map[string]bool{}
	ownerSpellings := map[string]string{}
	if state.Account != nil && (!validGitHubOwner(state.Account.Login) || githubAvatarURL(state.Account.AvatarURL) != state.Account.AvatarURL) {
		return state, errors.New("invalid saved GitHub account")
	}
	for i := range state.Repositories {
		repo := &state.Repositories[i]
		if err := validateRepository(*repo); err != nil {
			return state, fmt.Errorf("invalid catalogue entry: %w", err)
		}
		key := strings.ToLower(repo.ID)
		if seen[key] {
			return state, errors.New("catalogue contains duplicate repositories")
		}
		seen[key] = true
		ownerKey := strings.ToLower(repo.Owner)
		if previous, exists := ownerSpellings[ownerKey]; exists && previous != repo.Owner {
			return state, errors.New("catalogue contains conflicting owner spellings")
		}
		ownerSpellings[ownerKey] = repo.Owner
		// Interrupted work is never reported as a completed download.
		if repo.State == "preparing" {
			repo.State = "virtual"
		}
	}
	return state, nil
}

// writeState atomically replaces the catalogue on the same filesystem. fsync
// both the file and parent directory so a crash cannot truncate the last copy.
func writeState(path string, state persistedState) error {
	copyState := state
	copyState.Repositories = append([]Repository(nil), state.Repositories...)
	sort.Slice(copyState.Repositories, func(i, j int) bool {
		return strings.ToLower(copyState.Repositories[i].ID) < strings.ToLower(copyState.Repositories[j].ID)
	})
	f, err := os.CreateTemp(filepath.Dir(path), ".catalogue-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	defer f.Close()
	if err := f.Chmod(0o600); err != nil {
		return err
	}
	encoder := json.NewEncoder(f)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(copyState); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func validateRepository(repo Repository) error {
	if err := validateComponent(repo.Owner); err != nil {
		return fmt.Errorf("invalid owner: %w", err)
	}
	if err := validateComponent(repo.Name); err != nil {
		return fmt.Errorf("invalid repository name: %w", err)
	}
	if repo.ID != repo.Owner+"/"+repo.Name {
		return errors.New("repository ID must be owner/name")
	}
	if repo.Source == "manual" {
		return validateManualRepository(repo)
	}
	if repo.Source != "" && repo.Source != "github" {
		return errors.New("unsupported repository source")
	}
	if repo.CloneURL != "https://github.com/"+repo.ID+".git" {
		return errors.New("repository clone URL must match its GitHub identity")
	}
	if repo.HTMLURL != "https://github.com/"+repo.ID {
		return errors.New("repository web URL must match its GitHub identity")
	}
	return nil
}

func validateComponent(value string) error {
	if len(value) > 100 || value == "" || value == "." || value == ".." {
		return errors.New("empty or invalid path component")
	}
	if model.CleanPath(value) != value || strings.ContainsAny(value, "/\\") {
		return errors.New("path separators and relative path components are not allowed")
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_' && r != '.' {
			return errors.New("unsupported character in path component")
		}
	}
	return nil
}

func validateMountRoot(root string) error {
	if root == "" || !filepath.IsAbs(root) || model.CleanPath(root) == "." || strings.IndexByte(root, 0) >= 0 {
		return errors.New("choose an absolute folder path other than the filesystem root")
	}
	if home, err := os.UserHomeDir(); err == nil && model.CleanPath(root) == model.CleanPath(home) {
		return errors.New("choose a dedicated folder inside your home folder")
	}
	return nil
}
