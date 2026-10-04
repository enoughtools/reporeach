package desktop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	githubAccountLimit = 1 << 20
	githubRepoLimit    = 100 << 20
	githubMaxRepos     = 100000
	githubAuthLimit    = 128 << 10
	githubAuthLineSize = 16 << 10
)

var (
	errGitHubUnavailable = errors.New("GitHub CLI is unavailable. Install GitHub CLI or reinstall the app")
	errGitHubSignIn      = errors.New("Sign in to GitHub to discover your repositories")
	errGitHubEnvAuth     = errors.New("GitHub authentication is supplied by the environment. Clear GH_TOKEN or GITHUB_TOKEN before signing in")
	errGitHubResponse    = errors.New("GitHub returned an invalid repository response")
	errGitHubOutputLimit = errors.New("GitHub returned too much data; narrow your account access and try again")
	errGitHubClosed      = errors.New("GitHub connection is closed")
	// Enterprise Managed Users on github.com include an underscore followed by
	// the enterprise shortcode. These remain safe single path components.
	githubOwnerPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,38}$`)
	githubRepoPattern  = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)
	githubCodePattern  = regexp.MustCompile(`(?i)one-time code(?:\s*\(|\s*:)\s*([A-Z0-9]{4}-[A-Z0-9]{4})\b`)
)

// GitHub delegates credential storage and OAuth to the GitHub CLI. It never
// requests token output, and subprocess diagnostics are never returned over the
// desktop API. The CLI's authenticated API includes owner, collaborator, and
// organization-member repositories permitted by the current credential.
type GitHub struct {
	path   string
	life   context.Context
	cancel context.CancelFunc
	mu     sync.Mutex
	status AuthStatus
	run    *githubAuthRun
}

type githubAuthRun struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func NewGitHub(path string) *GitHub {
	if path == "" {
		path = "gh"
	}
	life, cancel := context.WithCancel(context.Background())
	return &GitHub{path: path, life: life, cancel: cancel}
}

// Close stops an in-flight authorization flow. Closing a management window
// does not call this; the local service owns the GitHub client's lifetime.
func (g *GitHub) Close() {
	g.cancel()
	g.mu.Lock()
	run := g.run
	g.mu.Unlock()
	if run != nil {
		run.cancel()
		<-run.done
	}
}

func (g *GitHub) Account(ctx context.Context) (*Account, error) {
	var response struct {
		Login     string `json:"login"`
		AvatarURL string `json:"avatar_url"`
	}
	err := g.api(ctx, time.Minute, githubAccountLimit, []string{"api", "--hostname", "github.com", "user"}, func(r io.Reader) error {
		decoder := json.NewDecoder(r)
		if err := decoder.Decode(&response); err != nil {
			return errGitHubResponse
		}
		var extra json.RawMessage
		if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
			return errGitHubResponse
		}
		if !validGitHubOwner(response.Login) {
			return errGitHubResponse
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &Account{Login: response.Login, AvatarURL: githubAvatarURL(response.AvatarURL)}, nil
}

func (g *GitHub) Discover(ctx context.Context) ([]Repository, *Account, error) {
	account, err := g.Account(ctx)
	if err != nil {
		return nil, nil, err
	}
	var repositories []Repository
	err = g.api(ctx, 5*time.Minute, githubRepoLimit, []string{
		"api", "--hostname", "github.com", "--paginate",
		"user/repos?per_page=100&visibility=all&affiliation=owner,collaborator,organization_member&sort=full_name&direction=asc",
	}, func(r io.Reader) error {
		var parseErr error
		repositories, parseErr = decodeGitHubRepositories(r)
		return parseErr
	})
	if err != nil {
		return nil, account, err
	}
	g.mu.Lock()
	if g.run == nil {
		g.status = cloneAuthStatus(AuthStatus{Authenticated: true, Account: account})
	}
	g.mu.Unlock()
	return repositories, account, nil
}

// AuthStatus refreshes the account when no authorization is running. During
// authorization it only reads local progress, avoiding repeated API requests.
func (g *GitHub) AuthStatus(ctx context.Context) AuthStatus {
	g.mu.Lock()
	if g.run != nil {
		status := cloneAuthStatus(g.status)
		g.mu.Unlock()
		return status
	}
	g.mu.Unlock()
	account, err := g.Account(ctx)
	g.mu.Lock()
	defer g.mu.Unlock()
	// StartAuth may have started while the account request was in flight.
	if g.run != nil {
		return cloneAuthStatus(g.status)
	}
	if err == nil {
		g.status = AuthStatus{Authenticated: true, Account: account}
	} else if g.status.Error == "" {
		g.status = AuthStatus{Error: err.Error()}
	} else {
		g.status.Authenticated = false
		g.status.Account = nil
	}
	return cloneAuthStatus(g.status)
}

// StartAuth initiates a device flow that survives the caller's HTTP request.
// gh receives a non-terminal stdin and disabled prompts, so it never offers to
// install a global Git credential helper. The app displays the code and opens
// the authorization URL returned by AuthStatus.
func (g *GitHub) StartAuth(ctx context.Context) (AuthStatus, error) {
	if err := ctx.Err(); err != nil {
		return AuthStatus{}, githubContextError(err)
	}
	if g.life.Err() != nil {
		return AuthStatus{}, errGitHubClosed
	}
	g.mu.Lock()
	if g.run != nil {
		status := cloneAuthStatus(g.status)
		g.mu.Unlock()
		return status, nil
	}
	g.mu.Unlock()
	// Reuse an existing CLI sign-in without starting an unnecessary OAuth flow.
	if account, err := g.Account(ctx); err == nil {
		g.mu.Lock()
		if g.run == nil {
			g.status = AuthStatus{Authenticated: true, Account: account}
		}
		status := cloneAuthStatus(g.status)
		g.mu.Unlock()
		return status, nil
	} else if errors.Is(err, errGitHubUnavailable) || ctx.Err() != nil || g.life.Err() != nil {
		return AuthStatus{}, err
	}
	// gh refuses web login while a token environment variable takes precedence
	// over stored credentials. A successful Account above can reuse that token;
	// a failed Account must not launch an OAuth flow that cannot finish.
	if githubHasEnvironmentToken(os.Environ()) {
		return AuthStatus{}, errGitHubEnvAuth
	}
	flowCtx, cancel := context.WithTimeout(g.life, 15*time.Minute)
	cmd := g.command(flowCtx, "auth", "login", "--hostname", "github.com", "--git-protocol", "https", "--web")
	cmd.Stdin = strings.NewReader("\n")
	run := &githubAuthRun{cancel: cancel, done: make(chan struct{})}
	output := &githubAuthOutput{g: g, run: run}
	cmd.Stdout, cmd.Stderr = output, output
	g.mu.Lock()
	if g.run != nil || g.life.Err() != nil {
		cancel()
		status := cloneAuthStatus(g.status)
		closed := g.life.Err() != nil
		g.mu.Unlock()
		if closed {
			return AuthStatus{}, errGitHubClosed
		}
		return status, nil
	}
	g.status = AuthStatus{Pending: true}
	g.run = run
	if err := cmd.Start(); err != nil {
		g.run = nil
		g.status = AuthStatus{Error: errGitHubUnavailable.Error()}
		cancel()
		close(run.done)
		g.mu.Unlock()
		return AuthStatus{}, errGitHubUnavailable
	}
	status := cloneAuthStatus(g.status)
	g.mu.Unlock()
	go g.finishAuth(flowCtx, cmd, run, output)
	return status, nil
}

func (g *GitHub) finishAuth(ctx context.Context, cmd *exec.Cmd, run *githubAuthRun, output *githubAuthOutput) {
	defer close(run.done)
	defer run.cancel()
	err := cmd.Wait()
	output.finish()
	status := AuthStatus{}
	if err != nil {
		if ctx.Err() != nil {
			status.Error = "GitHub sign-in was canceled or expired. Please try again"
		} else {
			status.Error = "GitHub sign-in failed. Please try again"
		}
	} else {
		account, accountErr := g.Account(ctx)
		if accountErr != nil {
			status.Error = accountErr.Error()
		} else {
			status = AuthStatus{Authenticated: true, Account: account}
		}
	}
	g.mu.Lock()
	if g.run == run {
		g.status = status
		g.run = nil
	}
	g.mu.Unlock()
}

func (g *GitHub) command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, g.path, args...)
	cmd.Env = githubEnvironment(os.Environ())
	// Avoid waiting indefinitely for output held by an orphaned subprocess.
	cmd.WaitDelay = time.Second
	return cmd
}

func githubEnvironment(environment []string) []string {
	blocked := map[string]bool{
		"GH_TELEMETRY": true,
		"GH_DEBUG":     true, "GH_FORCE_TTY": true, "GH_HOST": true,
		"GH_PAGER": true, "GH_PROMPT_DISABLED": true, "GH_NO_UPDATE_NOTIFIER": true,
		"GH_NO_EXTENSION_UPDATE_NOTIFIER": true, "GIT_CONFIG_GLOBAL": true, "NO_COLOR": true,
	}
	result := make([]string, 0, len(environment)+6)
	for _, entry := range environment {
		key, _, _ := strings.Cut(entry, "=")
		if !blocked[key] {
			result = append(result, entry)
		}
	}
	return append(result, "GH_TELEMETRY=false", "GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1", "GH_NO_EXTENSION_UPDATE_NOTIFIER=1", "GH_PAGER=cat", "GIT_CONFIG_GLOBAL="+os.DevNull, "NO_COLOR=1")
}

func githubHasEnvironmentToken(environment []string) bool {
	for _, entry := range environment {
		for _, prefix := range []string{"GH_TOKEN=", "GITHUB_TOKEN="} {
			if strings.HasPrefix(entry, prefix) && len(entry) > len(prefix) {
				return true
			}
		}
	}
	return false
}

func (g *GitHub) api(ctx context.Context, timeout time.Duration, limit int64, args []string, decode func(io.Reader) error) error {
	if g.life.Err() != nil {
		return errGitHubClosed
	}
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	stop := context.AfterFunc(g.life, cancel)
	defer func() { stop(); cancel() }()
	cmd := g.command(requestCtx, args...)
	cmd.Stderr = io.Discard
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return errGitHubUnavailable
	}
	if err := cmd.Start(); err != nil {
		return errGitHubUnavailable
	}
	// An orphaned child can retain gh's stdout after the parent is killed.
	// Closing our read end on cancellation wakes the decoder before Wait;
	// WaitDelay alone cannot help while a StdoutPipe read is still blocked.
	stopOutput := context.AfterFunc(requestCtx, func() { _ = stdout.Close() })
	defer stopOutput()
	reader := &io.LimitedReader{R: stdout, N: limit + 1}
	decodeErr := decode(reader)
	// Drain bounded remaining output before waiting. Killing on a decoder EOF
	// can race gh's authentication exit code and hide an actionable sign-in
	// error. A limit violation is the only parser error that cancels early.
	if !errors.Is(decodeErr, errGitHubOutputLimit) {
		_, _ = io.Copy(io.Discard, reader)
	}
	if reader.N <= 0 {
		decodeErr = errGitHubOutputLimit
	}
	if errors.Is(decodeErr, errGitHubOutputLimit) {
		cancel()
	}
	waitErr := cmd.Wait()
	if ctx.Err() != nil {
		return githubContextError(ctx.Err())
	}
	if g.life.Err() != nil {
		return errGitHubClosed
	}
	if requestCtx.Err() != nil && !errors.Is(decodeErr, errGitHubOutputLimit) {
		return githubContextError(requestCtx.Err())
	}
	// Failed commands often emit no JSON. Prefer the actionable sign-in error
	// over a decoder EOF, and never include raw command output in the error.
	if waitErr != nil && errors.Is(decodeErr, errGitHubResponse) {
		if exit, ok := errors.AsType[*exec.ExitError](waitErr); ok && exit.ExitCode() == 4 {
			return errGitHubSignIn
		}
	}
	if decodeErr != nil {
		return decodeErr
	}
	if requestCtx.Err() != nil {
		return githubContextError(requestCtx.Err())
	}
	if waitErr != nil {
		if exit, ok := errors.AsType[*exec.ExitError](waitErr); ok && exit.ExitCode() == 4 {
			return errGitHubSignIn
		}
		return errors.New("GitHub request failed. Check your connection and account access, then try again")
	}
	return nil
}

func githubContextError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return errors.New("GitHub request timed out. Please try again")
	}
	return errors.New("GitHub request was canceled")
}

func decodeGitHubRepositories(r io.Reader) ([]Repository, error) {
	decoder := json.NewDecoder(r)
	repositories := make([]Repository, 0)
	seen := make(map[string]bool)
	count := 0
	pages := 0
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil || token != json.Delim('[') {
			return nil, errGitHubResponse
		}
		pages++
		for decoder.More() {
			count++
			if count > githubMaxRepos {
				return nil, errGitHubOutputLimit
			}
			var response struct {
				Name          string `json:"name"`
				Description   string `json:"description"`
				DefaultBranch string `json:"default_branch"`
				Private       bool   `json:"private"`
				Owner         struct {
					Login string `json:"login"`
				} `json:"owner"`
			}
			if err := decoder.Decode(&response); err != nil || !validGitHubOwner(response.Owner.Login) || !validGitHubRepositoryName(response.Name) {
				return nil, errGitHubResponse
			}
			id := response.Owner.Login + "/" + response.Name
			key := strings.ToLower(id)
			if seen[key] {
				continue
			}
			seen[key] = true
			htmlURL := "https://github.com/" + id
			repositories = append(repositories, Repository{
				ID: id, Owner: response.Owner.Login, Name: response.Name,
				Description: response.Description, DefaultBranch: response.DefaultBranch,
				Private: response.Private, HTMLURL: htmlURL, CloneURL: htmlURL + ".git", State: "virtual",
			})
		}
		if token, err := decoder.Token(); err != nil || token != json.Delim(']') {
			return nil, errGitHubResponse
		}
	}
	if pages == 0 {
		return nil, errGitHubResponse
	}
	sort.Slice(repositories, func(i, j int) bool { return strings.ToLower(repositories[i].ID) < strings.ToLower(repositories[j].ID) })
	return repositories, nil
}

func validGitHubOwner(owner string) bool {
	return githubOwnerPattern.MatchString(owner) && !strings.HasSuffix(owner, "-")
}

func validGitHubRepositoryName(name string) bool {
	return githubRepoPattern.MatchString(name) && name != "." && name != ".."
}

func githubAvatarURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || u.Fragment != "" {
		return ""
	}
	if u.Host != "avatars.githubusercontent.com" && u.Host != "github.com" {
		return ""
	}
	// Avatar cache parameters are optional; omit arbitrary query data rather
	// than relaying credential-like values supplied by an unexpected response.
	u.RawQuery, u.ForceQuery = "", false
	return u.String()
}

func cloneAuthStatus(status AuthStatus) AuthStatus {
	if status.Account != nil {
		account := *status.Account
		status.Account = &account
	}
	return status
}

// githubAuthOutput is a bounded streaming filter. Only a recognized one-time
// device code and the fixed GitHub verification URL ever leave this filter.
// Output is deliberately not saved for diagnostics because gh may print
// credentials when debug settings or future command behavior changes.
type githubAuthOutput struct {
	mu    sync.Mutex
	g     *GitHub
	run   *githubAuthRun
	line  []byte
	total int
}

func (w *githubAuthOutput) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.total += len(p)
	if w.total > githubAuthLimit {
		w.run.cancel()
		return 0, errGitHubOutputLimit
	}
	for _, b := range p {
		if b == '\n' || b == '\r' {
			w.parseLine(w.line)
			w.line = w.line[:0]
		} else if len(w.line) < githubAuthLineSize {
			w.line = append(w.line, b)
		}
	}
	return len(p), nil
}

func (w *githubAuthOutput) finish() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.parseLine(w.line)
	w.line = nil
}

func (w *githubAuthOutput) parseLine(line []byte) {
	code := githubCodePattern.FindSubmatch(line)
	hasURL := bytes.Contains(line, []byte("https://github.com/login/device"))
	if len(code) == 0 && !hasURL {
		return
	}
	w.g.mu.Lock()
	defer w.g.mu.Unlock()
	if w.g.run != w.run {
		return
	}
	if len(code) == 2 {
		w.g.status.DeviceCode = strings.ToUpper(string(code[1]))
	}
	if hasURL {
		w.g.status.AuthorizationURL = "https://github.com/login/device"
	}
}
