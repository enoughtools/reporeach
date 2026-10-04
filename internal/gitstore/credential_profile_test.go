package gitstore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/model"
)

func profileHelper(t *testing.T, dir, name string) (string, string) {
	t.Helper()
	path := filepath.Join(dir, name+" helper")
	calls := filepath.Join(dir, name+" calls")
	script := "#!/bin/sh\nprintf '%s\\n' \"$1\" >> " + profileShellQuote(calls) + "\n" +
		"if [ \"$1\" = get ]; then printf '%s\\n' 'username=" + name + "' 'password=" + name + "-test-password'; fi\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return "!" + profileShellQuote(path), calls
}

func profileShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func profileBaseEnvironment(t *testing.T) ([]string, string, string, string) {
	t.Helper()
	dir := t.TempDir()
	native, nativeCalls := profileHelper(t, dir, "native")
	selected, selectedCalls := profileHelper(t, dir, "selected")
	env := append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+filepath.Join(dir, "empty-global"),
		"GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=credential.helper", "GIT_CONFIG_VALUE_0="+native,
		"GIT_CONFIG_KEY_1=user.name", "GIT_CONFIG_VALUE_1=Inherited identity",
		"SSH_AUTH_SOCK="+filepath.Join(dir, "native-agent"),
	)
	return env, selected, nativeCalls, selectedCalls
}

func profileCredentialCommand(env []string, verb, protocol, host string) (string, error) {
	cmd := exec.Command("git", "credential", verb)
	cmd.Env = env
	input := "protocol=" + protocol + "\nhost=" + host + "\n"
	if verb == "approve" {
		input += "username=selected\npassword=selected-test-password\n"
	}
	cmd.Stdin = strings.NewReader(input + "\n")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestCredentialProfilePreservesAmbientAuthenticationAndScopesGetAndStore(t *testing.T) {
	base, selected, nativeCalls, selectedCalls := profileBaseEnvironment(t)
	for _, test := range []struct {
		name     string
		cfg      model.RepoConfig
		protocol string
		host     string
		want     string
	}{
		{"selected origin", model.RepoConfig{RemoteURL: "https://example.com:8443/org/repo.git", CredentialHelper: selected}, "https", "EXAMPLE.COM:8443", "selected"},
		{"other origin", model.RepoConfig{RemoteURL: "https://example.com:8443/org/repo.git", CredentialHelper: selected}, "https", "other.example.com:8443", "native"},
		{"other port", model.RepoConfig{RemoteURL: "https://example.com:8443/org/repo.git", CredentialHelper: selected}, "https", "example.com:9443", "native"},
		{"HTTP downgrade", model.RepoConfig{RemoteURL: "https://example.com:8443/org/repo.git", CredentialHelper: selected}, "http", "example.com:8443", "native"},
		{"manual HTTPS", model.RepoConfig{RemoteURL: "https://example.com:8443/org/repo.git"}, "https", "example.com:8443", "native"},
		{"manual SSH", model.RepoConfig{RemoteURL: "git@example.com:org/repo.git"}, "https", "example.com:8443", "native"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, extra, err := transportCredentialEnv(test.cfg)
			if err != nil {
				t.Fatal(err)
			}
			env, err := gitCommandEnv(base, extra)
			if err != nil {
				t.Fatal(err)
			}
			if value, _ := environmentValue(env, "GIT_CONFIG_VALUE_1"); value != "Inherited identity" {
				t.Fatalf("inherited config changed: %q", value)
			}
			if got, _ := environmentValue(env, "SSH_AUTH_SOCK"); got != filepath.Join(filepath.Dir(nativeCalls), "native-agent") {
				t.Fatalf("SSH agent changed: %q", got)
			}
			out, err := profileCredentialCommand(env, "fill", test.protocol, test.host)
			if err != nil || !strings.Contains(out, "password="+test.want+"-test-password") {
				t.Fatalf("credential profile = %q, error = %v", out, err)
			}
		})
	}
	_ = os.Remove(nativeCalls)
	_ = os.Remove(selectedCalls)
	_, extra, err := transportCredentialEnv(model.RepoConfig{RemoteURL: "https://example.com:8443/repo", CredentialHelper: selected})
	if err != nil {
		t.Fatal(err)
	}
	env, err := gitCommandEnv(base, extra)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := profileCredentialCommand(env, "approve", "https", "example.com:8443"); err != nil {
		t.Fatalf("credential approve: %s: %v", out, err)
	}
	if calls, _ := os.ReadFile(selectedCalls); string(calls) != "store\n" {
		t.Fatalf("selected helper calls = %q", calls)
	}
	if calls, _ := os.ReadFile(nativeCalls); len(calls) != 0 {
		t.Fatalf("selected credentials reached native helper: %q", calls)
	}
}

func TestCredentialProfileInlineCredentialsRemainAuthoritative(t *testing.T) {
	base, selected, nativeCalls, selectedCalls := profileBaseEnvironment(t)
	cfg := model.RepoConfig{RemoteURL: "https://inline-user:inline-password@example.com:8443/repo", CredentialHelper: selected}
	safeURL, extra, err := transportCredentialEnv(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if safeURL != "https://example.com:8443/repo" {
		t.Fatalf("sanitized URL = %q", safeURL)
	}
	env, err := gitCommandEnv(base, extra)
	if err != nil {
		t.Fatal(err)
	}
	out, err := profileCredentialCommand(env, "fill", "https", "example.com:8443")
	if err != nil || !strings.Contains(out, "username=inline-user") || !strings.Contains(out, "password=inline-password") {
		t.Fatalf("inline profile = %q, error = %v", out, err)
	}
	for _, path := range []string{nativeCalls, selectedCalls} {
		if calls, _ := os.ReadFile(path); len(calls) != 0 {
			t.Fatalf("inline credentials invoked another helper: %q", calls)
		}
	}
	// A selected profile must not be installed durably when inline credentials
	// are the authority, even if no private clone exists yet.
	if err := ConfigureCredentialHelper(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
}

func TestCredentialProfileRejectsUnsupportedRemotesWithoutExposingCredentials(t *testing.T) {
	for _, remote := range []string{"ssh://git@example.com/repo", "git@example.com:org/repo", "http://example.com/repo", "file:///repo", "/local/repo", "https://inline-user:inline-password@/repo"} {
		_, _, err := transportCredentialEnv(model.RepoConfig{RemoteURL: remote, CredentialHelper: "!/trusted/helper"})
		if err == nil {
			t.Fatalf("accepted helper for unsupported remote %q", remote)
		}
		if strings.Contains(err.Error(), "inline-user") || strings.Contains(err.Error(), "inline-password") {
			t.Fatalf("credentials leaked in validation: %v", err)
		}
	}
	for _, helper := range []string{" ", "!/helper\nunsafe", "!/helper\x00unsafe"} {
		if _, _, err := transportCredentialEnv(model.RepoConfig{RemoteURL: "https://example.com/repo", CredentialHelper: helper}); err == nil {
			t.Fatal("accepted malformed helper")
		}
	}
	data, err := json.Marshal(model.RepoConfig{CredentialHelper: "!/trusted/helper"})
	if err != nil || bytes.Contains(data, []byte("helper")) {
		t.Fatalf("transient helper serialized: %s, error = %v", data, err)
	}
}

func TestGitCommandEnvRejectsIncompleteConfigWithoutMutatingInputs(t *testing.T) {
	_, extra, err := transportCredentialEnv(model.RepoConfig{RemoteURL: "https://example.com/repo", CredentialHelper: "!/trusted/helper"})
	if err != nil {
		t.Fatal(err)
	}
	for _, base := range [][]string{
		{"GIT_CONFIG_COUNT=-1"},
		{"GIT_CONFIG_COUNT=secret-invalid-count"},
		{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=user.name"},
		{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_VALUE_0=secret-identity"},
	} {
		before := strings.Join(base, "\n")
		_, err := gitCommandEnv(base, extra)
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("invalid inherited config error = %v", err)
		}
		if strings.Join(base, "\n") != before {
			t.Fatal("inherited environment mutated")
		}
	}
	// Git defines an empty count as zero, including in inherited environments.
	merged, err := gitCommandEnv([]string{"GIT_CONFIG_COUNT=", "SSH_AUTH_SOCK=native-agent"}, extra)
	if err != nil {
		t.Fatal(err)
	}
	if count, _ := environmentValue(merged, "GIT_CONFIG_COUNT"); count != "2" {
		t.Fatalf("empty inherited config count = %q", count)
	}
	merged, err = gitCommandEnv([]string{"GIT_CONFIG_COUNT="}, []string{"GIT_CONFIG_COUNT="})
	if err != nil {
		t.Fatal(err)
	}
	if count, _ := environmentValue(merged, "GIT_CONFIG_COUNT"); count != "0" {
		t.Fatalf("empty command config count = %q", count)
	}
}

func TestCredentialProfilesRemainIsolatedAcrossConcurrentGitCommands(t *testing.T) {
	base, selected, _, _ := profileBaseEnvironment(t)
	var workers sync.WaitGroup
	errs := make(chan error, 24)
	for i := 0; i < cap(errs); i++ {
		workers.Add(1)
		go func(i int) {
			defer workers.Done()
			cfg := model.RepoConfig{RemoteURL: "https://example.com:8443/repo"}
			want := "native"
			if i%2 == 0 {
				cfg.CredentialHelper = selected
				want = "selected"
			}
			_, extra, err := transportCredentialEnv(cfg)
			if err != nil {
				errs <- err
				return
			}
			env, err := gitCommandEnv(base, extra)
			if err != nil {
				errs <- err
				return
			}
			out, err := profileCredentialCommand(env, "fill", "https", "example.com:8443")
			if err != nil || !strings.Contains(out, "password="+want+"-test-password") {
				errs <- fmt.Errorf("command %d selected another profile: %q, %v", i, out, err)
			}
		}(i)
	}
	workers.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestConfigureCredentialHelperDurableScopeAndRotation(t *testing.T) {
	base, selected, _, _ := profileBaseEnvironment(t)
	gitDir := filepath.Join(t.TempDir(), "private.git")
	if _, err := runGit(context.Background(), "", "init", "--bare", gitDir); err != nil {
		t.Fatal(err)
	}
	cfg := model.RepoConfig{GitDir: gitDir, RemoteURL: "https://example.com:8443/repo", CredentialHelper: selected}
	if err := ConfigureCredentialHelper(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	values, err := runGit(context.Background(), gitDir, "config", "--local", "--get-all", "credential.https://example.com:8443.helper")
	// runGit trims the empty reset at the start; inspect the file as well.
	if err != nil || values != selected {
		t.Fatalf("durable helpers = %q, error = %v", values, err)
	}
	config, err := os.ReadFile(filepath.Join(gitDir, "config"))
	if err != nil || !strings.Contains(string(config), "helper = \n") {
		t.Fatalf("durable helper reset missing: %s, %v", config, err)
	}
	// Remove command-level helper entries to model an editor inheriting normal
	// global helpers. Local reset must replace the global native helper.
	native := "!f() { printf '%s\\n' username=native password=native-test-password; }; f"
	global := filepath.Join(t.TempDir(), "global-config")
	if _, err := runGit(context.Background(), "", "config", "--file", global, "credential.helper", native); err != nil {
		t.Fatal(err)
	}
	base = append(base, "GIT_DIR="+gitDir, "GIT_CONFIG_COUNT=0", "GIT_CONFIG_GLOBAL="+global)
	out, err := profileCredentialCommand(base, "fill", "https", "example.com:8443")
	if err != nil || !strings.Contains(out, "password=selected-test-password") {
		t.Fatalf("editor credential profile = %q, error = %v", out, err)
	}
	other, err := profileCredentialCommand(base, "fill", "https", "other.example.com:8443")
	if err != nil || !strings.Contains(other, "password=native-test-password") {
		t.Fatalf("unrelated editor profile = %q, error = %v", other, err)
	}
	rotated, _ := profileHelper(t, t.TempDir(), "rotated")
	cfg.CredentialHelper = rotated
	if err := ConfigureCredentialHelper(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	out, err = profileCredentialCommand(base, "fill", "https", "example.com:8443")
	if err != nil || !strings.Contains(out, "password=rotated-test-password") {
		t.Fatalf("rotated profile = %q, error = %v", out, err)
	}
	before, _ := os.ReadFile(filepath.Join(gitDir, "config"))
	cfg.CredentialHelper = ""
	if err := ConfigureCredentialHelper(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(gitDir, "config"))
	if !bytes.Equal(before, after) {
		t.Fatal("ambient profile modified existing clone config")
	}
}

func TestCloneCredentialProfileAppliesBeforeAcquisitionAndPersists(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	local := filepath.Join(dir, "source")
	run(t, "git", "init", "--initial-branch=main", local)
	if err := os.WriteFile(filepath.Join(local, "file"), []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", local, "add", "file")
	run(t, "git", "-C", local, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "source")
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	selected, _ := profileHelper(t, dir, "selected")
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	commands := filepath.Join(dir, "commands")
	credentials := filepath.Join(dir, "credentials")
	inherited := filepath.Join(dir, "inherited")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$PROFILE_COMMANDS\"\n" +
		"if [ \"$1\" = clone ]; then\n" +
		"  \"$PROFILE_REAL_GIT\" config --get user.name > \"$PROFILE_INHERITED\" || exit $?\n" +
		"  \"$PROFILE_REAL_GIT\" credential fill > \"$PROFILE_CREDENTIALS\" <<EOF\nprotocol=https\nhost=example.com:8443\n\nEOF\n" +
		"  for target do :; done\n" +
		"  \"$PROFILE_REAL_GIT\" clone --filter=blob:none --no-checkout --single-branch --no-tags --branch main \"$PROFILE_LOCAL\" \"$target\" || exit $?\n" +
		"  exec \"$PROFILE_REAL_GIT\" --git-dir \"$target/.git\" remote set-url origin https://example.com:8443/repo\n" +
		"fi\nexec \"$PROFILE_REAL_GIT\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("PROFILE_REAL_GIT", realGit)
	t.Setenv("PROFILE_LOCAL", local)
	t.Setenv("PROFILE_COMMANDS", commands)
	t.Setenv("PROFILE_CREDENTIALS", credentials)
	t.Setenv("PROFILE_INHERITED", inherited)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "user.name")
	t.Setenv("GIT_CONFIG_VALUE_0", "Inherited identity")
	var logs bytes.Buffer
	store := New(slog.New(slog.NewTextHandler(&logs, nil)))
	defer store.Close()
	cfg := model.RepoConfig{Name: "profile", RemoteURL: "https://example.com:8443/repo", CredentialHelper: selected, GitDir: filepath.Join(dir, "private.git"), Branch: "main"}
	if err := store.CloneBloblessNonInteractive(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(credentials)
	if err != nil || !bytes.Contains(actual, []byte("password=selected-test-password")) {
		t.Fatalf("initial acquisition profile = %q, error = %v", actual, err)
	}
	identity, err := os.ReadFile(inherited)
	if err != nil || string(identity) != "Inherited identity\n" {
		t.Fatalf("initial acquisition inherited config = %q, error = %v", identity, err)
	}
	installed, err := runGit(ctx, cfg.GitDir, "config", "--local", "--get-all", "credential.https://example.com:8443.helper")
	if err != nil || installed != selected {
		t.Fatalf("acquired helper = %q, error = %v", installed, err)
	}
	commandData, _ := os.ReadFile(commands)
	if bytes.Contains(commandData, []byte("selected-test-password")) || strings.Contains(logs.String(), "selected-test-password") {
		t.Fatal("credentials reached Git arguments or logs")
	}
	// The fast path updates helper selection without cloning again.
	rotated, _ := profileHelper(t, dir, "rotated")
	cfg.CredentialHelper = rotated
	if err := store.CloneBloblessNonInteractive(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	installed, err = runGit(ctx, cfg.GitDir, "config", "--local", "--get-all", "credential.https://example.com:8443.helper")
	if err != nil || installed != rotated {
		t.Fatalf("existing clone helper = %q, error = %v", installed, err)
	}
}

func TestPrepareSourceCredentialProfileAppliesBeforeFetchAndReceiptReuse(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	local := filepath.Join(dir, "source")
	run(t, "git", "init", "--initial-branch=main", local)
	if err := os.WriteFile(filepath.Join(local, "file"), []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", local, "add", "file")
	run(t, "git", "-C", local, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "source")
	commit, err := runGit(ctx, filepath.Join(local, ".git"), "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	selected, _ := profileHelper(t, dir, "selected")
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	credentials := filepath.Join(dir, "credentials")
	commands := filepath.Join(dir, "commands")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$PROFILE_COMMANDS\"\n" +
		"if [ \"$1\" = fetch ]; then\n" +
		"  \"$PROFILE_REAL_GIT\" credential fill > \"$PROFILE_CREDENTIALS\" <<EOF\nprotocol=https\nhost=example.com:8443\n\nEOF\n" +
		"  \"$PROFILE_REAL_GIT\" remote set-url origin \"$PROFILE_LOCAL\" || exit $?\n" +
		"  \"$PROFILE_REAL_GIT\" \"$@\"; profile_fetch_status=$?\n" +
		"  \"$PROFILE_REAL_GIT\" remote set-url origin https://example.com:8443/repo\n" +
		"  exit \"$profile_fetch_status\"\n" +
		"fi\nexec \"$PROFILE_REAL_GIT\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("PROFILE_REAL_GIT", realGit)
	t.Setenv("PROFILE_LOCAL", local)
	t.Setenv("PROFILE_COMMANDS", commands)
	t.Setenv("PROFILE_CREDENTIALS", credentials)
	store := New(nil)
	defer store.Close()
	cfg := model.RepoConfig{Name: "profile", RemoteURL: "https://example.com:8443/repo", CredentialHelper: selected, GitDir: filepath.Join(dir, "private.git")}
	requirement := model.SourceRequirement{Ref: "refs/heads/main", RequiredCommit: commit}
	prepared, err := store.PrepareSource(ctx, cfg, requirement)
	if err != nil || !prepared.Acquired || !prepared.Verified {
		t.Fatalf("verified acquisition = %#v, error = %v", prepared, err)
	}
	actual, err := os.ReadFile(credentials)
	if err != nil || !bytes.Contains(actual, []byte("password=selected-test-password")) {
		t.Fatalf("verified fetch profile = %q, error = %v", actual, err)
	}
	rotated, _ := profileHelper(t, dir, "rotated")
	cfg.CredentialHelper = rotated
	cfg.AcquiredRef, cfg.AcquiredCommit = prepared.Ref, prepared.Commit
	prepared, err = store.PrepareSource(ctx, cfg, requirement)
	if err != nil || prepared.Acquired {
		t.Fatalf("receipt reuse = %#v, error = %v", prepared, err)
	}
	installed, err := runGit(ctx, cfg.GitDir, "config", "--local", "--get-all", "credential.https://example.com:8443.helper")
	if err != nil || installed != rotated {
		t.Fatalf("receipt helper = %q, error = %v", installed, err)
	}
	commandData, _ := os.ReadFile(commands)
	if strings.Count(string(commandData), "fetch --filter=blob:none") != 1 || bytes.Contains(commandData, []byte("selected-test-password")) {
		t.Fatalf("unexpected acquisition commands: %s", commandData)
	}
}

func TestCloneCredentialProfileInlineFailureRedactsCredentials(t *testing.T) {
	dir := t.TempDir()
	commands := filepath.Join(dir, "commands")
	fakeGit := filepath.Join(dir, "git")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$PROFILE_COMMANDS\"\nprintf '%s\\n' \"Authentication failed: $ARTIFACT_FS_GIT_USERNAME $ARTIFACT_FS_GIT_PASSWORD\" >&2\nexit 1\n"
	if err := os.WriteFile(fakeGit, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("PROFILE_COMMANDS", commands)
	var logs bytes.Buffer
	store := New(slog.New(slog.NewTextHandler(&logs, nil)))
	defer store.Close()
	cfg := model.RepoConfig{Name: "profile", RemoteURL: "https://inline-user:inline-password@example.com/repo", CredentialHelper: "!/trusted/helper", GitDir: filepath.Join(dir, "private.git"), Branch: "main"}
	err := store.CloneBloblessNonInteractive(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected transport failure")
	}
	commandData, _ := os.ReadFile(commands)
	for _, data := range []string{string(commandData), logs.String(), err.Error()} {
		if strings.Contains(data, "inline-user") || strings.Contains(data, "inline-password") {
			t.Fatalf("inline credential leaked: %s", data)
		}
	}
	if !strings.Contains(string(commandData), "https://example.com/repo") {
		t.Fatalf("sanitized acquisition URL absent: %s", commandData)
	}
}

func TestConcurrentCloneAcquisitionsPreserveManualAndDiscoveryProfiles(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	local := filepath.Join(dir, "source")
	run(t, "git", "init", "--initial-branch=main", local)
	if err := os.WriteFile(filepath.Join(local, "file"), []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", local, "add", "file")
	run(t, "git", "-C", local, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "source")
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	native, nativeCalls := profileHelper(t, dir, "native")
	selected, selectedCalls := profileHelper(t, dir, "selected")
	global := filepath.Join(dir, "global-config")
	if _, err := runGit(ctx, "", "config", "--file", global, "credential.helper", native); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	// Keep all actual Git operations local, while exercising exactly the
	// environment selected by each initial clone before its transport runs.
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = clone ]; then\n" +
		"  profile_previous_arg=\n" +
		"  for profile_arg do profile_source=$profile_previous_arg; profile_previous_arg=$profile_arg; done\n" +
		"  profile_target=$profile_previous_arg\n" +
		"  profile_parent=$(dirname \"$profile_target\")\n" +
		"  printf '%s\\n' \"$*\" > \"$profile_parent/arguments\"\n" +
		"  case \"$profile_source\" in\n" +
		"    git@*) printf '%s\\n' \"$SSH_AUTH_SOCK\" > \"$profile_parent/credentials\";;\n" +
		"    *) \"$PROFILE_REAL_GIT\" credential fill > \"$profile_parent/credentials\" <<EOF\nprotocol=https\nhost=example.com:8443\n\nEOF\n" +
		"       ;;\n" +
		"  esac\n" +
		"  \"$PROFILE_REAL_GIT\" clone --filter=blob:none --no-checkout --single-branch --no-tags --branch main \"$PROFILE_LOCAL\" \"$profile_target\" || exit $?\n" +
		"  exec \"$PROFILE_REAL_GIT\" --git-dir \"$profile_target/.git\" remote set-url origin \"$profile_source\"\n" +
		"fi\nexec \"$PROFILE_REAL_GIT\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("PROFILE_REAL_GIT", realGit)
	t.Setenv("PROFILE_LOCAL", local)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_CONFIG_COUNT", "")
	t.Setenv("GIT_TERMINAL_PROMPT", "0")
	agent := filepath.Join(dir, "native-agent")
	t.Setenv("SSH_AUTH_SOCK", agent)
	profiles := []struct {
		name   string
		remote string
		helper string
		want   string
	}{
		{"discovered", "https://example.com:8443/repo", selected, "password=selected-test-password"},
		{"manual-https", "https://example.com:8443/repo", "", "password=native-test-password"},
		{"manual-ssh", "git@example.com:org/repo.git", "", agent},
		{"inline", "https://inline-user:inline-password@example.com:8443/repo", selected, "password=inline-password"},
	}
	store := New(nil)
	defer store.Close()
	var workers sync.WaitGroup
	errs := make(chan error, len(profiles))
	for _, profile := range profiles {
		workers.Add(1)
		go func() {
			defer workers.Done()
			cfg := model.RepoConfig{Name: profile.name, RemoteURL: profile.remote, CredentialHelper: profile.helper, GitDir: filepath.Join(dir, profile.name, "private.git"), Branch: "main"}
			if err := store.CloneBloblessNonInteractive(ctx, cfg); err != nil {
				errs <- fmt.Errorf("%s acquisition: %w", profile.name, err)
				return
			}
			data, err := os.ReadFile(filepath.Join(dir, profile.name, "credentials"))
			if err != nil || !bytes.Contains(data, []byte(profile.want)) {
				errs <- fmt.Errorf("%s acquisition used another profile: %q, %v", profile.name, data, err)
			}
			arguments, err := os.ReadFile(filepath.Join(dir, profile.name, "arguments"))
			if err != nil || bytes.Contains(arguments, []byte("inline-user")) || bytes.Contains(arguments, []byte("inline-password")) || bytes.Contains(arguments, []byte("test-password")) {
				errs <- fmt.Errorf("%s acquisition arguments were not sanitized: %q, %v", profile.name, arguments, err)
			}
			config, err := os.ReadFile(filepath.Join(cfg.GitDir, "config"))
			if err != nil {
				errs <- err
				return
			}
			selectedPersisted := bytes.Contains(config, []byte("selected helper"))
			if selectedPersisted != (profile.name == "discovered") {
				errs <- fmt.Errorf("%s acquisition persisted the wrong helper", profile.name)
			}
		}()
	}
	workers.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	for _, calls := range []string{nativeCalls, selectedCalls} {
		data, err := os.ReadFile(calls)
		if err != nil || string(data) != "get\n" {
			t.Fatalf("each native/selected helper should serve one acquisition: %q, %v", data, err)
		}
	}
}
