package desktop

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func catalogueRepository(owner, name string) Repository {
	id := owner + "/" + name
	return Repository{
		ID: id, Owner: owner, Name: name, DefaultBranch: "main",
		CloneURL: "https://github.com/" + id + ".git",
		HTMLURL:  "https://github.com/" + id, State: "virtual",
	}
}

func TestStateMissingStartsWithEmptyCatalogue(t *testing.T) {
	root := filepath.Join(t.TempDir(), "repositories")
	state, err := readState(filepath.Join(t.TempDir(), "missing.json"), root)
	if err != nil {
		t.Fatal(err)
	}
	if state.SchemaVersion != 2 || state.MountRoot != root || state.Repositories == nil || len(state.Repositories) != 0 || state.MountDesired {
		t.Fatalf("unexpected initial state: %+v", state)
	}
}

func TestStateRoundTripPersistsPinAndPrivatePermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "catalogue.json")
	pinned := catalogueRepository("z-org", "pinned")
	pinned.Pinned, pinned.State, pinned.DownloadedBytes = true, "pinned", 8192
	preparing := catalogueRepository("octocat", "preparing")
	preparing.State = "preparing"
	state := persistedState{
		SchemaVersion: 1, MountRoot: filepath.Join(dir, "mount"), MountDesired: true,
		Account: &Account{Login: "octocat"}, Repositories: []Repository{pinned, preparing},
	}
	before := append([]Repository(nil), state.Repositories...)
	if err := os.WriteFile(path, []byte("previous catalogue"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeState(path, state); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(state.Repositories, before) {
		t.Fatal("persisting state changed the caller's repository ordering")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("catalogue permissions = %o, want 600", info.Mode().Perm())
	}
	loaded, err := readState(path, filepath.Join(dir, "ignored-default"))
	if err != nil {
		t.Fatal(err)
	}
	if loaded.MountRoot != state.MountRoot || !loaded.MountDesired || loaded.Account.Login != "octocat" {
		t.Fatalf("settings were not retained: %+v", loaded)
	}
	if len(loaded.Repositories) != 2 || loaded.Repositories[0].ID != preparing.ID || loaded.Repositories[0].State != "virtual" {
		t.Fatalf("interrupted preparation was not reset: %+v", loaded.Repositories)
	}
	if loaded.Repositories[1] != pinned {
		t.Fatalf("durable pin changed: %+v", loaded.Repositories[1])
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".catalogue-") {
			t.Fatal("temporary catalogue file was left behind")
		}
	}
}

func TestStateRejectsMalformedAndUnsafeCatalogues(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mount")
	valid := persistedState{SchemaVersion: 1, MountRoot: root, Repositories: []Repository{catalogueRepository("octocat", "repo")}}
	encode := func(state persistedState) string {
		data, err := json.Marshal(state)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	tests := map[string]string{
		"empty": "", "incomplete": "{", "null": "null", "missing schema": `{}`, "missing root": `{"schemaVersion":1}`, "array": "[]", "unknown field": `{"secret":"unexpected"}`,
		"trailing value": encode(valid) + `{}`, "trailing junk": encode(valid) + ` trailing`,
	}
	mutations := map[string]func(*persistedState){
		"schema":               func(s *persistedState) { s.SchemaVersion = 3 },
		"relative root":        func(s *persistedState) { s.MountRoot = "relative" },
		"filesystem root":      func(s *persistedState) { s.MountRoot = "/" },
		"owner traversal":      func(s *persistedState) { s.Repositories[0].Owner = "../outside" },
		"repository traversal": func(s *persistedState) { s.Repositories[0].Name = ".." },
		"ID mismatch":          func(s *persistedState) { s.Repositories[0].ID = "other/repo" },
		"remote credentials": func(s *persistedState) {
			s.Repositories[0].CloneURL = "https://user:secret@github.com/octocat/repo.git"
		},
		"remote other host": func(s *persistedState) { s.Repositories[0].CloneURL = "https://example.com/octocat/repo.git" },
		"remote file":       func(s *persistedState) { s.Repositories[0].CloneURL = "file:///tmp/repository" },
		"remote option":     func(s *persistedState) { s.Repositories[0].CloneURL = "--upload-pack=unexpected" },
		"duplicate identity": func(s *persistedState) {
			s.Repositories = append(s.Repositories, catalogueRepository("OCTOCAT", "REPO"))
		},
		"owner spelling collision": func(s *persistedState) {
			s.Repositories = append(s.Repositories, catalogueRepository("OCTOCAT", "different"))
		},
	}
	for name, mutate := range mutations {
		state := valid
		state.Repositories = append([]Repository(nil), valid.Repositories...)
		mutate(&state)
		tests[name] = encode(state)
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "catalogue.json")
			if err := os.WriteFile(path, []byte(input), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := readState(path, root); err == nil {
				t.Fatal("unsafe or malformed saved catalogue was accepted")
			}
		})
	}
}

func TestValidateMountRootRequiresDedicatedAbsoluteFolder(t *testing.T) {
	invalid := []string{"", "repositories", "/", "/tmp/bad\x00path"}
	if home, err := os.UserHomeDir(); err == nil {
		invalid = append(invalid, home)
	}
	for _, path := range invalid {
		if err := validateMountRoot(path); err == nil {
			t.Fatalf("invalid mount folder %q was accepted", path)
		}
	}
	if err := validateMountRoot(filepath.Join(t.TempDir(), "chosen folder")); err != nil {
		t.Fatalf("dedicated folder with spaces was rejected: %v", err)
	}
}

func TestWriteStateFailureDoesNotLeaveTemporaryFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "catalogue.json")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	state := persistedState{SchemaVersion: 1, MountRoot: filepath.Join(dir, "mount")}
	if err := writeState(path, state); err == nil {
		t.Fatal("replacing a directory with catalogue JSON succeeded")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "catalogue.json" || !entries[0].IsDir() {
		t.Fatalf("failed persistence altered unrelated directory contents: %+v", entries)
	}
}
