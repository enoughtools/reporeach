package desktop

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/cloudflare/artifact-fs/internal/auth"
)

// AdoptionRequest adds a Git source or adopts an existing ordinary checkout.
// Adoption preserves the existing checkout and all of its local Git state.
type AdoptionRequest struct {
	RemoteURL string `json:"remoteURL"`
	Owner     string `json:"owner,omitempty"`
	Name      string `json:"name,omitempty"`
	Branch    string `json:"branch,omitempty"`
}

type adoptionRemote struct {
	url, owner, name, htmlURL, localPath string
}

const adoptionProbeLimit = 1 << 20

var errAdoptionRemote = errors.New("use a standard Git remote or an absolute local Git folder; embedded credentials and remote helpers are not supported")

// Adopt discovers a remote's branch without downloading its tree or invoking
// GitHub. Preparation remains lazy, just as it does for discovered repositories.
func (s *Service) Adopt(ctx context.Context, request AdoptionRequest) (status Status, retErr error) {
	remote, err := parseAdoptionRemote(request.RemoteURL)
	if err != nil {
		return Status{}, err
	}
	owner, name := request.Owner, request.Name
	if owner == "" {
		owner = remote.owner
	}
	if name == "" {
		name = remote.name
	}
	if err := validateComponent(owner); err != nil {
		return Status{}, errors.New("choose an owner or group name using letters, numbers, periods, underscores, or dashes")
	}
	if err := validateComponent(name); err != nil {
		return Status{}, errors.New("choose a repository name using letters, numbers, periods, underscores, or dashes")
	}
	if request.Branch != "" && !validAdoptionBranch(request.Branch) {
		return Status{}, errors.New("choose a valid Git branch name")
	}
	s.mu.Lock()
	err = s.adoptionAllowedLocked(remote)
	if err == nil && remote.localPath != "" {
		var resolved string
		protectedRoot := s.state.MountRoot
		if s.hybridCatalogue {
			protectedRoot = s.opts.StateDir
		}
		resolved, err = validateAdoptionLocalSource(remote.localPath, protectedRoot, s.opts.StateDir)
		if err == nil {
			remote.localPath, remote.url = resolved, resolved
		} else {
			err = errors.New("could not validate the local Git source outside EnoughRepos's mount and private storage folders")
		}
	}
	s.mu.Unlock()
	if err != nil {
		return Status{}, err
	}
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	var branch, localPath string
	if remote.localPath != "" && s.hybridCatalogue {
		checkout, inspectErr := InspectLocalCheckout(probeCtx, remote.localPath)
		if inspectErr == nil {
			if request.Branch != "" && request.Branch != checkout.Branch {
				return Status{}, errors.New("adoption retains the checkout's current branch; remove the branch override")
			}
			branch, localPath = checkout.Branch, checkout.Path
		} else {
			bare, bareErr := localCheckoutGit(probeCtx, remote.localPath, "rev-parse", "--is-bare-repository")
			if bareErr != nil || bare != "true" {
				return Status{}, inspectErr
			}
		}
	}
	if localPath == "" {
		branch, err = probeAdoptionBranch(probeCtx, s.opts.StateDir, remote.url, request.Branch)
		if err != nil {
			return Status{}, err
		}
	}
	repo := Repository{ID: owner + "/" + name, Owner: owner, Name: name,
		CloneURL: remote.url, HTMLURL: remote.htmlURL, DefaultBranch: branch,
		Source: "manual", State: "virtual"}
	if localPath != "" {
		repo.LocalPath, repo.LocalKind, repo.State = localPath, "adopted", "local"
	}
	if err := validateRepository(repo); err != nil {
		return Status{}, err
	}
	// Do not hold lifecycle during network I/O. Recheck paths and state after
	// acquiring it, in case the user changed the mount folder during the probe.
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	s.mu.Lock()
	if err := s.adoptionAllowedLocked(remote); err != nil {
		s.mu.Unlock()
		return Status{}, err
	}
	if err := ctx.Err(); err != nil {
		s.mu.Unlock()
		return Status{}, err
	}
	repo.Owner = s.canonicalOwnerLocked(repo.Owner)
	repo.ID = repo.Owner + "/" + repo.Name
	if existing, found := s.repositoryLocked(repo.ID); found {
		s.mu.Unlock()
		if existing.Source == "manual" && existing.CloneURL == repo.CloneURL && existing.DefaultBranch == repo.DefaultBranch && existing.LocalPath == repo.LocalPath && existing.LocalKind == repo.LocalKind {
			return s.Status(), nil
		}
		if existing.Source == "manual" && existing.CloneURL == repo.CloneURL && existing.LocalPath == "" && repo.LocalPath != "" {
			return Status{}, errors.New("that name already represents a separate virtual checkout; choose another name to adopt the original folder without replacing its managed work")
		}
		return Status{}, errors.New("that owner and repository name are already in the catalogue; choose a different name or group")
	}
	s.mu.Unlock()
	remount, err := s.quiesceCatalogueChange(ctx)
	if err != nil {
		return Status{}, err
	}
	defer func() {
		retErr = errors.Join(retErr, s.restoreCatalogueAfterChange(ctx, remount))
		status = s.Status()
	}()
	s.mu.Lock()
	if s.closing || s.quitPrepared || s.recoveryRequired {
		s.mu.Unlock()
		return Status{}, errors.New("repository service is closing or needs recovery")
	}
	if err := ctx.Err(); err != nil {
		s.mu.Unlock()
		return Status{}, err
	}
	if err := s.adoptionSourceAllowedLocked(remote); err != nil {
		s.mu.Unlock()
		return Status{}, err
	}
	// Draining the old session may finish an unrelated operation. Capture the
	// rollback snapshot only now, so a failed save cannot undo its result.
	previous := s.state
	oldEntries := s.entriesLocked()
	s.state.Repositories = append(append([]Repository(nil), previous.Repositories...), repo)
	sort.Slice(s.state.Repositories, func(i, j int) bool {
		return strings.ToLower(s.state.Repositories[i].ID) < strings.ToLower(s.state.Repositories[j].ID)
	})
	catalog, entries := s.catalog, s.entriesLocked()
	err = ctx.Err()
	if err == nil {
		err = s.publishHybridCatalogueLocked()
	}
	if err == nil {
		err = s.persistLocked()
		if err == nil {
			err = ctx.Err()
		}
	}
	if err != nil {
		s.state = previous
		err = errors.Join(err, s.persistLocked())
	}
	if err != nil {
		s.mu.Unlock()
		return Status{}, err
	}
	if catalog != nil {
		if err := catalog.SetEntries(entries); err != nil {
			s.state = previous
			persistErr := s.persistLocked()
			catalogErr := catalog.SetEntries(oldEntries)
			s.mu.Unlock()
			return Status{}, errors.Join(err, persistErr, catalogErr)
		}
	}
	s.mu.Unlock()
	return s.Status(), nil
}

// adoptionAllowedLocked rejects lexical overlap before resolving any source
// symlinks. Opening our own mounted tree can recursively activate repositories
// and, on Linux, deadlock Go's netpoll registration against its FUSE server.
func (s *Service) adoptionAllowedLocked(remote adoptionRemote) error {
	if s.closing || s.quitPrepared || s.maintenance || s.recoveryRequired {
		return errors.New("wait for the repository service to finish its current change")
	}
	return s.adoptionSourceAllowedLocked(remote)
}

func (s *Service) adoptionSourceAllowedLocked(remote adoptionRemote) error {
	if remote.localPath == "" {
		return nil
	}
	protectedRoot := s.state.MountRoot
	if s.hybridCatalogue {
		protectedRoot = s.opts.StateDir
	}
	if pathsLexicallyOverlap(remote.localPath, protectedRoot) || pathsLexicallyOverlap(remote.localPath, s.opts.StateDir) {
		return errors.New("choose a Git folder outside EnoughRepos's mount and private storage folders")
	}
	if _, err := validateAdoptionLocalSource(remote.localPath, protectedRoot, s.opts.StateDir); err != nil {
		return errors.New("choose a Git folder outside EnoughRepos's mount and private storage folders")
	}
	return nil
}

// validateManualSourceLocation is also called before deferred acquisition: a
// source's linked-worktree metadata can change after catalogue registration.
func validateManualSourceLocation(repo Repository, mountRoot, stateDir string) error {
	if repo.LocalPath != "" {
		if _, err := validateAdoptionLocalSource(repo.LocalPath, stateDir, stateDir); err != nil {
			return errors.New("the local checkout must stay outside EnoughRepos's private storage")
		}
		return nil
	}
	remote, err := parseAdoptionRemote(repo.CloneURL)
	if err != nil {
		return err
	}
	if remote.localPath == "" {
		return nil
	}
	if _, err := validateAdoptionLocalSource(remote.localPath, mountRoot, stateDir); err != nil {
		return errors.New("choose a Git source outside EnoughRepos's mount and private storage folders")
	}
	return nil
}

// A linked checkout can put .git and commondir outside its working folder.
// Validate those metadata redirects before Git opens them, without changing
// the user's original repository or resolving targets in our mounted tree.
func validateAdoptionLocalSource(source, mountRoot, stateDir string) (string, error) {
	resolved, err := resolveAdoptionSource(source, mountRoot, stateDir)
	if err != nil {
		return "", err
	}
	gitDir := resolved // bare repositories store their metadata at the root.
	metadata, err := resolveAdoptionSource(filepath.Join(resolved, ".git"), mountRoot, stateDir)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(metadata)
	if err == nil && info.IsDir() {
		gitDir = metadata
	} else if err == nil && info.Mode().IsRegular() {
		data, err := readAdoptionMetadata(metadata)
		if err != nil {
			return "", err
		}
		target, found := strings.CutPrefix(data, "gitdir: ")
		if !found || target == "" {
			return "", errors.New("invalid Git directory pointer")
		}
		if !filepath.IsAbs(target) {
			target = resolved + string(os.PathSeparator) + target
		}
		gitDir, err = resolveAdoptionSource(target, mountRoot, stateDir)
		if err != nil {
			return "", err
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	} else if err == nil {
		return "", errors.New("Git metadata must be a folder or a regular gitfile")
	}
	commonPath, err := resolveAdoptionSource(filepath.Join(gitDir, "commondir"), mountRoot, stateDir)
	if err != nil {
		return "", err
	}
	if info, err := os.Lstat(commonPath); err == nil {
		if !info.Mode().IsRegular() {
			return "", errors.New("Git common directory pointer must be a regular file")
		}
		target, err := readAdoptionMetadata(commonPath)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(target) {
			target = gitDir + string(os.PathSeparator) + target
		}
		if _, err := resolveAdoptionSource(target, mountRoot, stateDir); err != nil {
			return "", err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return resolved, nil
}

func readAdoptionMetadata(path string) (string, error) {
	// All callers first resolve and check the metadata path. NOFOLLOW keeps a
	// concurrent final-component symlink replacement from redirecting this read.
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("Git metadata pointer must be a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, (16<<10)+1))
	if err != nil || len(data) > 16<<10 {
		return "", errors.New("Git metadata pointer is invalid or too large")
	}
	value := strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
	if value == "" || hasAdoptionControl(value) {
		return "", errors.New("Git metadata pointer is invalid")
	}
	return value, nil
}

// Resolve one symlink at a time, checking its destination before following it.
// EvalSymlinks alone would stat an alias's target in our mount before we could
// reject the physical overlap. This walk uses only metadata operations and
// never traverses a protected target, including through an intermediate alias.
func resolveAdoptionSource(source, mountRoot, stateDir string) (string, error) {
	physicalMount, err := resolveDirectoryPath(mountRoot)
	if err != nil {
		return "", err
	}
	physicalState, err := resolveDirectoryPath(stateDir)
	if err != nil {
		return "", err
	}
	protected := []string{mountRoot, stateDir, physicalMount, physicalState}
	var protectedInfos []os.FileInfo
	for _, root := range []string{physicalMount, physicalState} {
		if info, err := os.Lstat(root); err == nil {
			protectedInfos = append(protectedInfos, info)
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	for _, root := range protected {
		if pathsLexicallyOverlap(source, root) {
			return "", errors.New("source overlaps EnoughRepos storage")
		}
	}
	// Preserve redirect components until previous symlinks have been resolved.
	// Cleaning pivot/../repo first changes its meaning when pivot is a symlink.
	pending := strings.Split(strings.TrimLeft(source, string(os.PathSeparator)), string(os.PathSeparator))
	current := string(os.PathSeparator)
	links := 0
	for len(pending) > 0 {
		component := pending[0]
		pending = pending[1:]
		if component == "" || component == "." {
			continue
		}
		if component == ".." {
			current = filepath.Dir(current)
			continue
		}
		candidate := filepath.Join(current, component)
		for _, root := range protected {
			rel, err := filepath.Rel(root, candidate)
			if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
				return "", errors.New("source traverses EnoughRepos storage")
			}
		}
		info, err := os.Lstat(candidate)
		if errors.Is(err, os.ErrNotExist) {
			// A missing prefix cannot be traversed by Git. Keep the unresolved
			// suffix intact; the metadata probe will report the missing source.
			return candidate + string(os.PathSeparator) + strings.Join(pending, string(os.PathSeparator)), nil
		}
		if err != nil {
			return "", err
		}
		for _, protectedInfo := range protectedInfos {
			if os.SameFile(info, protectedInfo) {
				return "", errors.New("source traverses EnoughRepos storage")
			}
		}
		if info.Mode()&os.ModeSymlink == 0 {
			current = candidate
			continue
		}
		links++
		if links > 255 {
			return "", errors.New("too many source symlinks")
		}
		target, err := os.Readlink(candidate)
		if err != nil {
			return "", err
		}
		if filepath.IsAbs(target) {
			current = string(os.PathSeparator)
		}
		pending = append(strings.Split(strings.TrimLeft(target, string(os.PathSeparator)), string(os.PathSeparator)), pending...)
	}
	for _, root := range protected {
		if pathsLexicallyOverlap(current, root) {
			return "", errors.New("source overlaps EnoughRepos storage")
		}
	}
	return current, nil
}

func validateManualRepository(repo Repository) error {
	remote, err := parseAdoptionRemote(repo.CloneURL)
	if err != nil {
		return err
	}
	if remote.url != repo.CloneURL || repo.HTMLURL != remote.htmlURL {
		return errors.New("manual repository URLs must match their validated Git source")
	}
	if !(repo.LocalKind == "adopted" && repo.DefaultBranch == "") && !validAdoptionBranch(repo.DefaultBranch) {
		return errors.New("manual repository must have a valid Git branch")
	}
	return nil
}

func parseAdoptionRemote(raw string) (adoptionRemote, error) {
	var remote adoptionRemote
	if raw == "" || len(raw) > 8192 || strings.TrimSpace(raw) != raw || strings.HasPrefix(raw, "-") || hasAdoptionControl(raw) || strings.ContainsAny(raw, "?#") {
		return remote, errAdoptionRemote
	}
	if filepath.IsAbs(raw) {
		// Resolve dot components only after preceding symlinks: eagerly
		// cleaning an absolute source can silently select a different repo.
		remote.localPath = raw
		remote.url = remote.localPath
		remote.owner, remote.name = "local", strings.TrimSuffix(filepath.Base(remote.localPath), ".git")
		return remote, nil
	}
	if auth.HasInlineCredentials(raw) {
		return remote, errAdoptionRemote
	}
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil || u.Opaque != "" || u.Host == "" && u.Scheme != "file" || hasAdoptionControl(u.Path) || strings.Contains(u.Path, "\\") {
			return remote, errAdoptionRemote
		}
		switch u.Scheme {
		case "file":
			if u.User != nil || u.Host != "" && u.Host != "localhost" || !filepath.IsAbs(u.Path) {
				return remote, errAdoptionRemote
			}
			return parseAdoptionRemote(u.Path)
		case "http", "https", "git":
			if u.User != nil || !validAdoptionURLHost(u) {
				return remote, errAdoptionRemote
			}
		case "ssh":
			if !validAdoptionURLHost(u) || u.User != nil && (!validAdoptionUser(u.User.Username()) || auth.HasInlineCredentials(raw)) {
				return remote, errAdoptionRemote
			}
		default:
			return remote, errAdoptionRemote
		}
		if !validAdoptionRemotePath(u.Path) || strings.Contains(strings.ToLower(u.EscapedPath()), "%2f") || strings.Contains(strings.ToLower(u.EscapedPath()), "%5c") {
			return remote, errAdoptionRemote
		}
		remote.url = raw
		remote.owner, remote.name = adoptionIdentity(u.Hostname(), u.Path)
		if u.Scheme == "http" || u.Scheme == "https" {
			web := *u
			web.Path = strings.TrimSuffix(strings.TrimRight(u.Path, "/"), ".git")
			web.RawPath = ""
			remote.htmlURL = web.String()
		}
		return remote, nil
	}
	// Git's scp syntax has one colon after the optional SSH authority. A
	// bracketed IPv6 host may contain additional colons inside its brackets.
	separator := strings.IndexByte(raw, ':')
	if bracket := strings.IndexByte(raw, '['); bracket >= 0 {
		end := strings.IndexByte(raw, ']')
		if end < bracket || end+1 >= len(raw) || raw[end+1] != ':' {
			return remote, errAdoptionRemote
		}
		separator = end + 1
	}
	if separator <= 0 || separator == len(raw)-1 {
		return remote, errAdoptionRemote
	}
	authority, path := raw[:separator], raw[separator+1:]
	user, host := "", authority
	if before, after, found := strings.Cut(authority, "@"); found {
		user, host = before, after
		if user == "" {
			return remote, errAdoptionRemote
		}
	}
	if strings.ContainsAny(host, "[]") && !(strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]")) {
		return remote, errAdoptionRemote
	}
	hostName := strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if !validAdoptionHost(hostName) || user != "" && !validAdoptionUser(user) || strings.Contains(host, ":") && !strings.HasPrefix(host, "[") || strings.HasPrefix(path, ":") || !validAdoptionRemotePath(path) {
		return remote, errAdoptionRemote
	}
	if user != "" && auth.HasInlineCredentials("ssh://"+user+"@"+host+"/repository") {
		return remote, errAdoptionRemote
	}
	remote.url = raw
	remote.owner, remote.name = adoptionIdentity(hostName, path)
	return remote, nil
}

func hasAdoptionControl(value string) bool {
	return strings.IndexFunc(value, unicode.IsControl) >= 0
}

func validAdoptionHost(host string) bool {
	if net.ParseIP(host) != nil {
		return true
	}
	if host == "" || len(host) > 253 || strings.HasPrefix(host, "-") || strings.Contains(host, "..") {
		return false
	}
	for _, r := range host {
		if r != '.' && r != '_' && r != '-' && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

func validAdoptionPort(port string) bool {
	if port == "" {
		return true
	}
	n, err := strconv.Atoi(port)
	return err == nil && n > 0 && n <= 65535
}

func validAdoptionURLHost(u *url.URL) bool {
	host := u.Host
	if u.User != nil {
		host = strings.TrimPrefix(host, u.User.String()+"@")
	}
	return validAdoptionHost(u.Hostname()) && validAdoptionPort(u.Port()) && !strings.HasSuffix(host, ":") && (!strings.Contains(u.Hostname(), ":") || strings.HasPrefix(host, "["))
}

func validAdoptionUser(user string) bool {
	if user == "" || strings.HasPrefix(user, "-") || len(user) > 100 {
		return false
	}
	return validateComponent(user) == nil
}

func validAdoptionRemotePath(path string) bool {
	if path == "" || hasAdoptionControl(path) || strings.ContainsAny(path, "\\?#") {
		return false
	}
	for _, component := range strings.Split(strings.Trim(path, "/"), "/") {
		if component == "" || component == "." || component == ".." {
			return false
		}
	}
	return true
}

func adoptionIdentity(host, path string) (string, string) {
	components := strings.Split(strings.Trim(path, "/"), "/")
	name := strings.TrimSuffix(components[len(components)-1], ".git")
	owner := host
	if len(components) > 1 {
		owner = components[len(components)-2]
	}
	return owner, name
}

// A catalogue branch must be a literal heads ref, not a revision expression or
// the special check-ref-format --branch shorthand @{-1}.
func validAdoptionBranch(branch string) bool {
	if branch == "" || branch == "HEAD" || len(branch) > 1024 || strings.HasPrefix(branch, "-") || strings.Contains(branch, "..") || strings.Contains(branch, "@{") || strings.HasSuffix(branch, ".") || strings.ContainsAny(branch, " ~^:?*[\\") || hasAdoptionControl(branch) {
		return false
	}
	for _, part := range strings.Split(branch, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}

func nativeAdoptionEnvironment(environ []string) []string {
	env := make([]string, 0, len(environ)+1)
	for _, entry := range environ {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_PREFIX", "GIT_SHALLOW_FILE", "GIT_NAMESPACE", "GIT_REPLACE_REF_BASE", "GIT_GRAFT_FILE", "GIT_CONFIG", "GIT_TERMINAL_PROMPT", "GIT_CURL_VERBOSE":
			continue
		}
		if strings.HasPrefix(key, "GIT_TRACE") {
			continue
		}
		env = append(env, entry)
	}
	return append(env, "GIT_TERMINAL_PROMPT=0")
}

func probeAdoptionBranch(ctx context.Context, directory, remote, requestedBranch string) (string, error) {
	args := []string{"ls-remote", "--symref", "--upload-pack=git-upload-pack", "--", remote, "HEAD"}
	if requestedBranch != "" {
		args = append(args, "refs/heads/"+requestedBranch)
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = directory
	cmd.Env = nativeAdoptionEnvironment(os.Environ())
	// Kill SSH/upload-pack descendants too. A retained pipe in a grandchild
	// must not prevent the app from canceling an import promptly.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = 250 * time.Millisecond
	// nil stderr uses /dev/null directly, without an os/exec copier that can
	// keep Wait blocked when a subprocess inherits its stderr descriptor.
	cmd.Stderr = nil
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", errors.New("Git could not start the repository check")
	}
	if err := cmd.Start(); err != nil {
		_ = stdout.Close()
		return "", errors.New("Git is unavailable; install the command line tools and try again")
	}
	stop := context.AfterFunc(ctx, func() { _ = stdout.Close() })
	output, readErr := io.ReadAll(io.LimitReader(stdout, adoptionProbeLimit+1))
	if readErr != nil || len(output) > adoptionProbeLimit {
		_ = cmd.Cancel()
	}
	waitErr := cmd.Wait()
	stop()
	_ = stdout.Close()
	if ctx.Err() != nil {
		return "", fmt.Errorf("repository check canceled or timed out: %w", ctx.Err())
	}
	if len(output) > adoptionProbeLimit {
		return "", errors.New("the Git remote returned too much metadata")
	}
	if readErr != nil || waitErr != nil {
		return "", errors.New("could not reach the repository using your Git credentials; check its address and access, then try again")
	}
	return decodeAdoptionBranch(string(output), requestedBranch)
}

func decodeAdoptionBranch(output, requestedBranch string) (string, error) {
	branch := ""
	head, requested := false, false
	refs := map[string]string{}
	for _, line := range strings.Split(output, "\n") {
		if line == "" {
			continue
		}
		value, ref, found := strings.Cut(line, "\t")
		if !found {
			return "", errors.New("the Git remote returned invalid branch metadata")
		}
		if strings.HasPrefix(value, "ref: ") {
			if ref == "HEAD" {
				next := strings.TrimPrefix(value, "ref: refs/heads/")
				if next == value || !validAdoptionBranch(next) || branch != "" && branch != next {
					return "", errors.New("the Git remote returned an unsupported default branch")
				}
				branch = next
			}
			continue
		}
		if !validAdoptionOID(value) {
			return "", errors.New("the Git remote returned invalid branch metadata")
		}
		if previous, exists := refs[ref]; exists && previous != value {
			return "", errors.New("the Git remote returned conflicting branch metadata")
		}
		refs[ref] = value
		if ref == "HEAD" {
			head = true
		}
		if requestedBranch != "" && ref == "refs/heads/"+requestedBranch {
			requested = true
		}
	}
	if requestedBranch != "" {
		if !requested {
			return "", errors.New("that branch was not found in the Git remote")
		}
		return requestedBranch, nil
	}
	if !head {
		return "", errors.New("the Git remote has no committed default branch; choose an existing branch or create its first commit")
	}
	if branch == "" {
		return "", errors.New("the Git remote has a detached default revision; choose an existing branch")
	}
	return branch, nil
}

func validAdoptionOID(oid string) bool {
	if len(oid) != 40 && len(oid) != 64 {
		return false
	}
	for _, r := range oid {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return true
}
