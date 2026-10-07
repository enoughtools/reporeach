package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func fakeGitHub(t *testing.T, script string) *GitHub {
	t.Helper()
	// Do not make fixture behavior depend on developer credentials inherited by
	// the test process. Individual environment-credential tests set these after
	// creating their client.
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	path := filepath.Join(t.TempDir(), "gh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nset -eu\n"+script), 0o700); err != nil {
		t.Fatal(err)
	}
	client := NewGitHub(path)
	t.Cleanup(client.Close)
	return client
}

func TestGitHubDiscoverPaginated(t *testing.T) {
	trace := filepath.Join(t.TempDir(), "arguments")
	t.Setenv("FAKE_GH_TRACE", trace)
	t.Setenv("GH_DEBUG", "api")
	t.Setenv("GH_FORCE_TTY", "80")
	t.Setenv("GH_HOST", "unexpected.example")
	t.Setenv("GH_TELEMETRY", "true")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "user-git-config"))
	client := fakeGitHub(t, `
[ "${GH_PROMPT_DISABLED}" = "1" ]
[ "${GH_NO_UPDATE_NOTIFIER}" = "1" ]
[ "${GH_NO_EXTENSION_UPDATE_NOTIFIER}" = "1" ]
[ "${GIT_CONFIG_GLOBAL}" = "/dev/null" ]
[ "${GH_DEBUG:-}" = "" ]
[ "${GH_FORCE_TTY:-}" = "" ]
[ "${GH_HOST:-}" = "" ]
[ "${GH_TELEMETRY}" = "false" ]
printf '%s\n' "$@" >> "$FAKE_GH_TRACE"
case "$*" in
  'api --hostname github.com user')
    printf '%s\n' '{"login":"octocat","avatar_url":"https://avatars.githubusercontent.com/u/1?v=4"}'
    ;;
  'api --hostname github.com --paginate user/repos?per_page=100&visibility=all&affiliation=owner,collaborator,organization_member&sort=full_name&direction=asc')
    printf '%s\n' '[{"name":"b","owner":{"login":"z-org"},"description":null,"private":true,"default_branch":"main"}]'
    printf '%s\n' '[{"name":"Repo","owner":{"login":"octocat"},"description":"A repository","default_branch":"trunk","clone_url":"https://token:secret@evil.example/repo","html_url":"https://evil.example/repo"}]'
    printf '%s\n' '[{"name":"repo","owner":{"login":"OCTOCAT"},"description":"duplicate"}]'
    ;;
  *) exit 1 ;;
esac
`)
	repositories, account, err := client.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if account.Login != "octocat" || account.AvatarURL != "https://avatars.githubusercontent.com/u/1" {
		t.Fatalf("unexpected account: %+v", account)
	}
	want := []Repository{
		{ID: "octocat/Repo", Owner: "octocat", Name: "Repo", Description: "A repository", DefaultBranch: "trunk", HTMLURL: "https://github.com/octocat/Repo", CloneURL: "https://github.com/octocat/Repo.git", State: "virtual"},
		{ID: "z-org/b", Owner: "z-org", Name: "b", Private: true, DefaultBranch: "main", HTMLURL: "https://github.com/z-org/b", CloneURL: "https://github.com/z-org/b.git", State: "virtual"},
	}
	if !reflect.DeepEqual(repositories, want) {
		t.Fatalf("repositories = %+v, want %+v", repositories, want)
	}
	arguments, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(arguments), "secret") || strings.Contains(string(arguments), "token") || strings.Contains(string(arguments), "setup-git") {
		t.Fatal("credential or global-configuration command appeared in arguments")
	}
	if !strings.Contains(string(arguments), "--paginate") || !strings.Contains(string(arguments), "organization_member") {
		t.Fatal("discovery did not include pagination and organization access")
	}
	account.Login = "caller mutation"
	client.mu.Lock()
	if client.status.Account.Login != "octocat" {
		t.Error("discovery account leaked mutable internal state")
	}
	client.mu.Unlock()
}

func TestDecodeGitHubRepositoriesRejectsUnsafeAndMalformedData(t *testing.T) {
	tests := []string{
		"", `null`, `{}`, `[] trailing`, `[[{}]]`,
		`[{"name":"..","owner":{"login":"octocat"}}]`,
		`[{"name":"../escape","owner":{"login":"octocat"}}]`,
		`[{"name":"a/b","owner":{"login":"octocat"}}]`,
		`[{"name":"repo","owner":{"login":"../escape"}}]`,
		`[{"name":"repo","owner":{"login":"-flag"}}]`,
		`[{"name":"repo","owner":null}]`,
		`[{"name":"repo","owner":{"login":"octocat"}}`,
		`[{"name":"repo","owner":{"login":"octocat"}},`,
	}
	for _, input := range tests {
		t.Run(input, func(t *testing.T) {
			_, err := decodeGitHubRepositories(strings.NewReader(input))
			if !errors.Is(err, errGitHubResponse) {
				t.Fatalf("error = %v, want invalid response", err)
			}
		})
	}
	for _, input := range []string{`[]`, `[] []`, `[{"name":".github","owner":{"login":"octocat"}}]`} {
		if _, err := decodeGitHubRepositories(strings.NewReader(input)); err != nil {
			t.Fatalf("valid data failed: %v", err)
		}
	}
}

func TestDecodeGitHubRepositoryCountBound(t *testing.T) {
	entry := `{"name":"repo","owner":{"login":"octocat"}},`
	input := "[" + strings.Repeat(entry, githubMaxRepos) + strings.TrimSuffix(entry, ",") + "]"
	_, err := decodeGitHubRepositories(strings.NewReader(input))
	if !errors.Is(err, errGitHubOutputLimit) {
		t.Fatalf("error = %v, want repository count limit", err)
	}
}

func TestGitHubAccountRejectsUnsafeData(t *testing.T) {
	for _, response := range []string{`{"login":"../escape"}`, `{"login":"octocat"}{"extra":true}`, `{"login":"octocat","avatar_url":123}`, `null`} {
		t.Run(response, func(t *testing.T) {
			t.Setenv("FAKE_GH_RESPONSE", response)
			client := fakeGitHub(t, `printf '%s' "$FAKE_GH_RESPONSE"`)
			if _, err := client.Account(context.Background()); !errors.Is(err, errGitHubResponse) {
				t.Fatalf("error = %v, want invalid response", err)
			}
		})
	}
}

func TestGitHubEnterpriseManagedUser(t *testing.T) {
	client := fakeGitHub(t, `
case "$*" in
  'api --hostname github.com user')
    printf '%s' '{"login":"mona-cat_octo"}'
    ;;
  *)
    printf '%s' '[{"name":"repo","owner":{"login":"mona-cat_octo"}}]'
    ;;
esac
`)
	repositories, account, err := client.Discover(context.Background())
	if err != nil || account == nil || account.Login != "mona-cat_octo" || len(repositories) != 1 || repositories[0].ID != "mona-cat_octo/repo" {
		t.Fatalf("managed user discovery = %+v, %+v, %v", repositories, account, err)
	}
}

func TestGitHubAvatarURLSanitization(t *testing.T) {
	for _, input := range []string{"http://avatars.githubusercontent.com/u/1", "https://token:secret@avatars.githubusercontent.com/u/1", "https://evil.example/u/1", "https://avatars.githubusercontent.com:443/u/1", "https://avatars.githubusercontent.com/u/1#secret"} {
		if got := githubAvatarURL(input); got != "" {
			t.Fatalf("unsafe avatar URL was accepted")
		}
	}
	if got := githubAvatarURL("https://avatars.githubusercontent.com/u/1?access_token=secret"); got != "https://avatars.githubusercontent.com/u/1" {
		t.Fatal("avatar URL query data was returned")
	}
}

func TestGitHubErrorsNeverExposeOutput(t *testing.T) {
	const secret = "ghp_do_not_expose_this_credential"
	client := fakeGitHub(t, `
printf '%s\n' 'Authorization: token ghp_do_not_expose_this_credential' >&2
printf '%s\n' 'https://user:ghp_do_not_expose_this_credential@github.com/'
exit 1
`)
	_, err := client.Account(context.Background())
	if err == nil || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "Authorization") {
		t.Fatal("raw command diagnostics were exposed")
	}
	status := client.AuthStatus(context.Background())
	encoded, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), secret) {
		t.Fatal("credential appeared in API JSON")
	}
}

func TestGitHubMissingCLI(t *testing.T) {
	client := NewGitHub(filepath.Join(t.TempDir(), "missing"))
	defer client.Close()
	if _, err := client.Account(context.Background()); !errors.Is(err, errGitHubUnavailable) {
		t.Fatalf("error = %v, want unavailable CLI", err)
	}
	if _, err := client.StartAuth(context.Background()); !errors.Is(err, errGitHubUnavailable) {
		t.Fatalf("error = %v, want unavailable CLI", err)
	}
}

func TestGitHubAuthRequiredExitCode(t *testing.T) {
	client := fakeGitHub(t, `exit 4`)
	if _, err := client.Account(context.Background()); !errors.Is(err, errGitHubSignIn) {
		t.Fatalf("error = %v, want sign-in required", err)
	}
}

func TestGitHubOutputByteLimit(t *testing.T) {
	client := fakeGitHub(t, `printf '%01024d' 0`)
	err := client.api(context.Background(), time.Minute, 128, []string{"api"}, func(r io.Reader) error {
		_, err := io.Copy(io.Discard, r)
		return err
	})
	if !errors.Is(err, errGitHubOutputLimit) {
		t.Fatalf("error = %v, want byte limit", err)
	}
}

func TestGitHubCanceledRequest(t *testing.T) {
	client := fakeGitHub(t, `exec sleep 10`)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := client.Account(ctx)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("error = %v, want timeout", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("canceled request kept its subprocess running")
	}
}

func TestGitHubCanceledReadWithInheritedStdout(t *testing.T) {
	// Killing gh's parent leaves stdout and stderr open in its children. Both
	// decoding and subprocess Wait must finish promptly on cancellation; an
	// os/exec stderr-copy pipe can otherwise add the complete WaitDelay.
	client := fakeGitHub(t, `sleep 3 >&2 2>/dev/null &
sleep 3 &
wait`)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := client.Account(ctx)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("error = %v, want timeout", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("inherited stdout prevented request cancellation")
	}
}

func TestGitHubStartAuthRejectsFailedEnvironmentCredential(t *testing.T) {
	for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		t.Run(key, func(t *testing.T) {
			client := fakeGitHub(t, `
if [ "$1" = auth ]; then exit 99; fi
exit 4
`)
			t.Setenv(key, "credential-never-output")
			if _, err := client.StartAuth(context.Background()); !errors.Is(err, errGitHubEnvAuth) {
				t.Fatalf("error = %v, want environment credential guidance", err)
			}
		})
	}
}

func TestGitHubStartAuthReusesEnvironmentCredential(t *testing.T) {
	client := fakeGitHub(t, `
if [ "$1" = auth ]; then exit 99; fi
printf '%s' '{"login":"octocat"}'
`)
	t.Setenv("GH_TOKEN", "credential-never-output")
	status, err := client.StartAuth(context.Background())
	if err != nil || !status.Authenticated || status.Account.Login != "octocat" {
		t.Fatalf("status = %+v, error = %v", status, err)
	}
}

func TestGitHubStartAuthReusesExistingAccount(t *testing.T) {
	client := fakeGitHub(t, `
if [ "$*" != 'api --hostname github.com user' ]; then exit 1; fi
printf '%s' '{"login":"octocat"}'
`)
	status, err := client.StartAuth(context.Background())
	if err != nil || !status.Authenticated || status.Pending || status.Account.Login != "octocat" {
		t.Fatalf("status = %+v, error = %v", status, err)
	}
}

func TestGitHubDeviceAuthSurvivesRequestAndSanitizesOutput(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("FAKE_GH_READY", filepath.Join(directory, "ready"))
	t.Setenv("FAKE_GH_RELEASE", filepath.Join(directory, "release"))
	t.Setenv("FAKE_GH_SIGNED", filepath.Join(directory, "signed"))
	t.Setenv("FAKE_GH_LOGIN_TRACE", filepath.Join(directory, "login-trace"))
	client := fakeGitHub(t, `
case "$*" in
  'api --hostname github.com user')
    if [ ! -f "$FAKE_GH_SIGNED" ]; then exit 4; fi
    printf '%s\n' '{"login":"octocat"}'
    ;;
  'auth login --hostname github.com --git-protocol https --web')
    [ "${GH_PROMPT_DISABLED}" = "1" ]
    [ "${GIT_CONFIG_GLOBAL}" = "/dev/null" ]
    printf '%s\n' "$@" >> "$FAKE_GH_LOGIN_TRACE"
    printf '%s' '! First copy your one-time co' >&2
    printf '%s\n' 'de: AB12-CD34' >&2
    printf '%s\n' 'Open this URL to continue in your web browser: https://github.com/login/device?access_token=do-not-expose' >&2
    printf '%s\n' 'token=ghp_do_not_expose_this_credential' >&2
    touch "$FAKE_GH_READY"
    while [ ! -f "$FAKE_GH_RELEASE" ]; do sleep 0.02; done
    touch "$FAKE_GH_SIGNED"
    ;;
  *) exit 1 ;;
esac
`)
	ctx, cancel := context.WithCancel(context.Background())
	status, err := client.StartAuth(ctx)
	if err != nil || !status.Pending {
		t.Fatalf("initial status = %+v, error = %v", status, err)
	}
	cancel()
	awaitGitHub(t, func() bool {
		status = client.AuthStatus(context.Background())
		return status.DeviceCode != "" && status.AuthorizationURL != ""
	})
	if status.DeviceCode != "AB12-CD34" || status.AuthorizationURL != "https://github.com/login/device" || !status.Pending {
		t.Fatalf("unexpected authorization status: %+v", status)
	}
	encoded, _ := json.Marshal(status)
	if strings.Contains(string(encoded), "do-not-expose") || strings.Contains(string(encoded), "ghp_") {
		t.Fatal("authorization status exposed extra output")
	}
	if _, err := client.StartAuth(context.Background()); err != nil {
		t.Fatal(err)
	}
	trace, err := os.ReadFile(os.Getenv("FAKE_GH_LOGIN_TRACE"))
	if err != nil || strings.Count(string(trace), "login") != 1 {
		t.Fatal("duplicate authorization flow was started")
	}
	if err := os.WriteFile(os.Getenv("FAKE_GH_RELEASE"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	awaitGitHub(t, func() bool {
		status = client.AuthStatus(context.Background())
		return !status.Pending
	})
	if !status.Authenticated || status.Account == nil || status.Account.Login != "octocat" || status.DeviceCode != "" || status.AuthorizationURL != "" {
		t.Fatalf("completed status = %+v", status)
	}
	// Callers cannot modify the client's retained Account through a result.
	status.Account.Login = "changed"
	client.mu.Lock()
	if client.status.Account.Login != "octocat" {
		t.Error("authorization status leaked mutable internal state")
	}
	client.mu.Unlock()
}

func TestGitHubCloseCancelsAuthorization(t *testing.T) {
	client := fakeGitHub(t, `
case "$1" in
  api) exit 4 ;;
  auth) exec sleep 10 ;;
esac
`)
	if _, err := client.StartAuth(context.Background()); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	client.Close()
	if time.Since(start) > 2*time.Second {
		t.Fatal("Close did not stop authorization promptly")
	}
	if _, err := client.Account(context.Background()); !errors.Is(err, errGitHubClosed) {
		t.Fatalf("error = %v, want closed", err)
	}
	if _, err := client.StartAuth(context.Background()); !errors.Is(err, errGitHubClosed) {
		t.Fatalf("error = %v, want closed", err)
	}
}

func TestGitHubAuthOutputBounded(t *testing.T) {
	client := NewGitHub("unused")
	defer client.Close()
	canceled := false
	run := &githubAuthRun{cancel: func() { canceled = true }}
	output := &githubAuthOutput{g: client, run: run}
	if _, err := output.Write(make([]byte, githubAuthLimit+1)); !errors.Is(err, errGitHubOutputLimit) || !canceled {
		t.Fatal("unbounded authorization output was accepted")
	}
	if len(output.line) > githubAuthLineSize {
		t.Fatal("authorization line exceeded its memory bound")
	}
}

func awaitGitHub(t *testing.T, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for fake GitHub subprocess")
}
