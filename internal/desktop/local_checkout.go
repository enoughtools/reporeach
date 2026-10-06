package desktop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/cloudflare/artifact-fs/internal/gitstore"
	"github.com/cloudflare/artifact-fs/internal/model"
)

// LocalCheckout describes an existing ordinary working tree. Inspection never
// resets its index, changes its configuration, or contacts its remote.
type LocalCheckout struct {
	Path      string
	GitDir    string
	CommonDir string
	Branch    string
	HeadOID   string
	RemoteURL string
}

func InspectLocalCheckout(ctx context.Context, path string) (LocalCheckout, error) {
	var checkout LocalCheckout
	if !filepath.IsAbs(path) || hasAdoptionControl(path) {
		return checkout, errors.New("choose an absolute local checkout folder")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return checkout, fmt.Errorf("cannot inspect local checkout: %w", err)
	}
	metadata, err := localCheckoutGit(ctx, resolved, "rev-parse", "--path-format=absolute", "--show-toplevel", "--absolute-git-dir", "--git-common-dir")
	if err != nil {
		return checkout, errors.New("choose a Git working tree; bare repositories can be added by remote address")
	}
	parts := strings.Split(metadata, "\n")
	if len(parts) != 3 {
		return checkout, errors.New("Git returned invalid local checkout paths")
	}
	for _, part := range parts {
		if !filepath.IsAbs(part) || hasAdoptionControl(part) {
			return checkout, errors.New("Git returned invalid local checkout paths")
		}
	}
	root, err := filepath.EvalSymlinks(parts[0])
	if err != nil || root != resolved {
		return checkout, errors.New("choose the checkout's root folder rather than one of its subfolders")
	}
	gitDir, err := filepath.EvalSymlinks(parts[1])
	if err != nil {
		return checkout, errors.New("cannot resolve the checkout's Git metadata")
	}
	commonDir, err := filepath.EvalSymlinks(parts[2])
	if err != nil {
		return checkout, errors.New("cannot resolve the checkout's common Git metadata")
	}
	head, err := localCheckoutGit(ctx, resolved, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil || !validAdoptionOID(head) {
		return checkout, errors.New("the local checkout needs an initial commit before it can be adopted")
	}
	branch, err := localCheckoutGit(ctx, resolved, "symbolic-ref", "-q", "--short", "HEAD")
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			return checkout, errors.New("cannot inspect the checkout's branch")
		}
		branch = "" // Detached HEAD is valid and is preserved in place.
	}
	remote, err := localCheckoutGit(ctx, resolved, "config", "--local", "--get", "remote.origin.url")
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			return checkout, errors.New("cannot inspect the checkout's remote")
		}
		remote = ""
	}
	// Existing credentials stay in the user's configuration. They must not be
	// copied into the catalogue or exposed by its API.
	if remote != "" {
		if _, err := parseAdoptionRemote(remote); err != nil {
			remote = ""
		}
	}
	return LocalCheckout{Path: root, GitDir: gitDir, CommonDir: commonDir, Branch: branch, HeadOID: head, RemoteURL: remote}, nil
}

// StagedLocalCheckout owns an unpublished sibling directory. The lifecycle
// owner keeps the source mounted while staging, then detaches its virtual leaf
// and removes only its owned catalogue link before Publish. Close removes only
// an unpublished stage; it never deletes the source or published checkout.
type StagedLocalCheckout struct {
	Path                  string
	Destination           string
	parent                *os.File
	parentInfo            os.FileInfo
	stageInfo             os.FileInfo
	published             bool
	sourceGit             string
	sourceView            string
	gitManifest           map[string]checkoutFileRecord
	viewManifest          map[string]checkoutFileRecord
	finalManifest         map[string]checkoutFileRecord
	viewMetadataValidator func(string) error
}

// StageLocalCheckout exports a private ArtifactFS Git directory and its merged
// visible working tree without checkout/read-tree/reset. Consequently staged,
// unstaged, untracked, deleted, and binary working files survive the handoff.
// The caller must serialize repository lifecycle and drain filesystem writes;
// fingerprints additionally refuse a source that changes during the copy.
func StageLocalCheckout(ctx context.Context, cfg model.RepoConfig, visibleRoot, destination string) (_ *StagedLocalCheckout, retErr error) {
	return stageLocalCheckout(ctx, cfg, visibleRoot, destination, checkoutValidateNativeMetadata)
}

// StageLocalCheckoutForMountedView additionally receives the lifecycle owner's
// retained mount. Only the exact owned native FSKit session can attest that an
// unsupported ACL API represents the view's absence of ACL support. Host Git
// storage and the destination still require ordinary native metadata checks.
func StageLocalCheckoutForMountedView(ctx context.Context, cfg model.RepoConfig, visibleRoot, destination string, mounted fusefs.MountedFS) (*StagedLocalCheckout, error) {
	validator, err := checkoutMountedViewMetadataValidator(mounted, visibleRoot)
	if err != nil {
		return nil, err
	}
	return stageLocalCheckout(ctx, cfg, visibleRoot, destination, validator)
}

func stageLocalCheckout(ctx context.Context, cfg model.RepoConfig, visibleRoot, destination string, viewValidator func(string) error) (_ *StagedLocalCheckout, retErr error) {
	if cfg.PreparedGitDir || !filepath.IsAbs(cfg.GitDir) || !filepath.IsAbs(visibleRoot) || !filepath.IsAbs(destination) || hasAdoptionControl(destination) {
		return nil, errors.New("materialization requires private Git storage and absolute working folders")
	}
	if pathsLexicallyOverlap(cfg.GitDir, destination) || pathsLexicallyOverlap(visibleRoot, destination) {
		return nil, errors.New("the local checkout must be outside its virtual source and private Git storage")
	}
	if err := validateStandaloneCheckoutGit(ctx, cfg.GitDir); err != nil {
		return nil, err
	}
	parentPath := filepath.Dir(destination)
	parent, err := os.Open(parentPath)
	if err != nil {
		return nil, fmt.Errorf("cannot open local checkout parent: %w", err)
	}
	parentInfo, err := parent.Stat()
	if err != nil || !parentInfo.IsDir() {
		parent.Close()
		return nil, errors.New("local checkout parent must be a folder")
	}
	stagePath, err := os.MkdirTemp(parentPath, ".reporeach-checkout-")
	if err != nil {
		parent.Close()
		return nil, err
	}
	stage := &StagedLocalCheckout{Path: stagePath, Destination: destination, parent: parent, parentInfo: parentInfo, viewMetadataValidator: viewValidator}
	stage.stageInfo, err = os.Lstat(stagePath)
	if err != nil {
		stage.Close()
		return nil, err
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, stage.Close())
		}
	}()
	gitBefore, err := checkoutTreeManifest(ctx, cfg.GitDir, false)
	if err != nil {
		return nil, err
	}
	if err := copyCheckoutTree(ctx, cfg.GitDir, filepath.Join(stage.Path, ".git"), false); err != nil {
		return nil, err
	}
	viewBefore, err := checkoutTreeManifestWithValidator(ctx, visibleRoot, true, viewValidator)
	if err != nil {
		return nil, err
	}
	if err := copyCheckoutTree(ctx, visibleRoot, stage.Path, true); err != nil {
		return nil, err
	}
	viewAfter, err := checkoutTreeManifestWithValidator(ctx, visibleRoot, true, viewValidator)
	if err != nil || !reflect.DeepEqual(viewBefore, viewAfter) {
		return nil, errors.New("the working tree changed while it was being kept locally; try again after Git and editors finish")
	}
	gitAfter, err := checkoutTreeManifest(ctx, cfg.GitDir, false)
	if err != nil || !reflect.DeepEqual(gitBefore, gitAfter) {
		return nil, errors.New("Git metadata changed while the checkout was being kept locally; try again after Git finishes")
	}
	stagedView, err := checkoutTreeManifest(ctx, stage.Path, true)
	if err != nil {
		return nil, fmt.Errorf("cannot verify copied working file metadata: %w", err)
	}
	if !checkoutManifestContentEqual(viewBefore, stagedView) {
		return nil, fmt.Errorf("the local checkout copy did not preserve every working file and its metadata: %s", checkoutManifestDifference(viewBefore, stagedView))
	}
	stagedGit, err := checkoutTreeManifest(ctx, filepath.Join(stage.Path, ".git"), false)
	if err != nil || !checkoutManifestContentEqual(gitBefore, stagedGit) {
		return nil, errors.New("the local checkout copy did not preserve Git metadata")
	}
	if _, err := localCheckoutGit(ctx, stage.Path, "config", "--local", "core.worktree", destination); err != nil {
		return nil, errors.New("cannot make the kept checkout independent of RepoReach")
	}
	monitor, monitorErr := localCheckoutGit(ctx, stage.Path, "config", "--local", "--get", "core.fsmonitor")
	if monitorErr == nil && monitor == filepath.Join(cfg.GitDir, "hooks", "artifact-fs-fsmonitor") {
		if _, err := localCheckoutGit(ctx, stage.Path, "config", "--local", "core.fsmonitor", "false"); err != nil {
			return nil, errors.New("cannot disable the virtual checkout's filesystem monitor")
		}
	}
	if err := syncCheckoutTree(ctx, stage.Path); err != nil {
		return nil, err
	}
	stage.sourceGit, stage.sourceView = cfg.GitDir, visibleRoot
	stage.gitManifest, stage.viewManifest = gitAfter, viewAfter
	stage.finalManifest, err = checkoutTreeManifest(ctx, stage.Path, false)
	if err != nil {
		return nil, err
	}
	return stage, ctx.Err()
}

// VerifySource must run while filesystem writes remain frozen and before the
// virtual source is detached. A later write invalidates this proof; the caller
// must retain its private rollback data unless it owns that freeze.
func (s *StagedLocalCheckout) VerifySource(ctx context.Context) error {
	if s == nil || s.parent == nil || s.published {
		return errors.New("the local checkout stage is no longer available")
	}
	if err := s.VerifyPrivateGit(ctx); err != nil {
		return err
	}
	viewManifest, err := checkoutTreeManifestWithValidator(ctx, s.sourceView, true, s.viewMetadataValidator)
	if err != nil || !reflect.DeepEqual(s.viewManifest, viewManifest) {
		return errors.New("working files changed after the local copy was staged; they have been retained")
	}
	return nil
}

// VerifyPrivateGit remains usable after normal detachment because the managed
// Git directory is host storage. It detects native Git index/ref/config writes
// that bypassed the virtual filesystem's write freeze before ownership changes.
func (s *StagedLocalCheckout) VerifyPrivateGit(ctx context.Context) error {
	if s == nil || s.parent == nil {
		return errors.New("the local checkout stage is no longer available")
	}
	manifest, err := checkoutTreeManifest(ctx, s.sourceGit, false)
	if err != nil || !reflect.DeepEqual(s.gitManifest, manifest) {
		return errors.New("Git metadata changed after the local copy was staged; it has been retained")
	}
	return nil
}

// VerifyPublished checks every copied Git and working file and extended
// attribute before the caller records the ordinary checkout as authoritative.
func (s *StagedLocalCheckout) VerifyPublished(ctx context.Context) error {
	if s == nil || !s.published {
		return errors.New("the local checkout has not been published")
	}
	manifest, err := checkoutTreeManifest(ctx, s.Destination, false)
	if err != nil || !checkoutManifestContentEqual(s.finalManifest, manifest) {
		return errors.New("the published checkout changed or failed verification; retain its private rollback data")
	}
	return nil
}

func (s *StagedLocalCheckout) Publish(ctx context.Context) error {
	if s == nil || s.parent == nil || s.published {
		return errors.New("the local checkout stage is no longer available")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	parentInfo, err := os.Stat(filepath.Dir(s.Destination))
	if err != nil || !os.SameFile(parentInfo, s.parentInfo) {
		return errors.New("the local checkout parent changed before publication")
	}
	stageInfo, err := os.Lstat(s.Path)
	if err != nil || !os.SameFile(stageInfo, s.stageInfo) || !stageInfo.IsDir() {
		return errors.New("the local checkout stage changed before publication")
	}
	if err := publishCheckoutDirectory(s.parent, filepath.Base(s.Path), filepath.Base(s.Destination)); err != nil {
		return fmt.Errorf("cannot publish local checkout without replacing existing files: %w", err)
	}
	// A sync error cannot undo a completed rename. Retain the published checkout
	// and report the durability failure so the lifecycle owner can recover it.
	s.published = true
	return s.parent.Sync()
}

// Published reports whether the atomic rename happened, including when the
// subsequent parent-directory sync failed. Recovery must preserve that checkout.
func (s *StagedLocalCheckout) Published() bool { return s != nil && s.published }

func (s *StagedLocalCheckout) Close() error {
	if s == nil || s.parent == nil {
		return nil
	}
	var err error
	if !s.published {
		info, statErr := os.Lstat(s.Path)
		if statErr == nil && os.SameFile(info, s.stageInfo) {
			err = os.RemoveAll(s.Path)
		} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			err = statErr
		} else if statErr == nil {
			err = errors.New("the unpublished checkout stage was replaced; retained for inspection")
		}
	}
	err = errors.Join(err, s.parent.Close())
	s.parent = nil
	return err
}

// VerifyLocalCheckoutSafeToFree is a conservative preflight, never a deletion.
// Run it only for application-materialized checkouts, never adopted originals.
// A temporary Git copy receives verification refs, leaving native source refs,
// reflogs, configuration and index unchanged. Caller must recheck identity and
// serialize user access immediately before an owned checkout is moved aside.
func VerifyLocalCheckoutSafeToFree(ctx context.Context, path, remote string) error {
	checkout, err := InspectLocalCheckout(ctx, path)
	if err != nil {
		return err
	}
	if checkout.GitDir != filepath.Join(checkout.Path, ".git") || checkout.CommonDir != checkout.GitDir {
		return errors.New("linked or external Git storage must remain in place")
	}
	parsed, err := parseAdoptionRemote(remote)
	if err != nil {
		return errors.New("a reachable remote backup is required before freeing a local checkout")
	}
	if parsed.localPath != "" {
		physicalRemote, err := filepath.EvalSymlinks(parsed.localPath)
		if err != nil || pathsLexicallyOverlap(physicalRemote, checkout.Path) {
			return errors.New("the backup remote must exist outside the checkout being freed")
		}
	}
	if err := validateStandaloneCheckoutGit(ctx, checkout.GitDir); err != nil {
		return err
	}
	status, err := localCheckoutGit(ctx, path, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignored=matching")
	if err != nil {
		return errors.New("cannot verify local working files")
	}
	if status != "" {
		return errors.New("the local checkout has staged, changed, untracked, or ignored files; preserve them before freeing space")
	}
	flags, err := localCheckoutGit(ctx, path, "ls-files", "-v", "-z")
	if err != nil {
		return errors.New("cannot inspect local Git index flags")
	}
	for _, record := range strings.Split(flags, "\x00") {
		if record == "" {
			continue
		}
		// Git status intentionally trusts assume-unchanged and skip-worktree
		// entries. Refuse those flags instead of clearing them or relying on a
		// status result that can conceal local bytes. Ordinary cached entries
		// use H; lowercase tags indicate assume-unchanged and S is skip-worktree.
		if len(record) < 3 || record[1] != ' ' || record[0] != 'H' {
			return errors.New("the local checkout has index flags that can hide working changes; preserve its files before freeing space")
		}
	}
	tracked, err := localCheckoutGit(ctx, path, "ls-files", "-z")
	if err != nil {
		return errors.New("cannot inspect tracked local working files")
	}
	trackedPaths := make(map[string]bool)
	for _, name := range strings.Split(tracked, "\x00") {
		if name == "" {
			continue
		}
		trackedPaths[model.CleanPath(name)] = true
		for dir := filepath.Dir(name); dir != "."; dir = filepath.Dir(dir) {
			trackedPaths[model.CleanPath(dir)] = true
		}
	}
	before, err := checkoutTreeManifest(ctx, path, false)
	if err != nil {
		return err
	}
	for relative, record := range before {
		for name := range record.Xattrs {
			// Provenance is assigned by macOS to a newly hydrated/copied file;
			// resource forks, Finder tags, and every application/user attribute
			// are local data that the remote Git tree cannot recover.
			if name != "com.apple.provenance" {
				return errors.New("the local checkout has extended file metadata; preserve it before freeing space")
			}
		}
		if relative == "." || relative == ".git" || strings.HasPrefix(relative, ".git"+string(os.PathSeparator)) {
			continue
		}
		if !trackedPaths[model.CleanPath(filepath.ToSlash(relative))] {
			return errors.New("the local checkout has untracked folders or metadata; preserve them before freeing space")
		}
	}
	temporary, err := os.MkdirTemp("", "reporeach-free-check-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temporary)
	gitDir := filepath.Join(temporary, ".git")
	if err := copyCheckoutTree(ctx, checkout.GitDir, gitDir, false); err != nil {
		return err
	}
	store := gitstore.New(nil)
	defer store.Close()
	if err := store.VerifySafeToDiscard(ctx, model.RepoConfig{GitDir: gitDir, MountPath: path, RemoteURL: remote}); err != nil {
		return err
	}
	after, err := checkoutTreeManifest(ctx, path, false)
	if err != nil || !reflect.DeepEqual(before, after) {
		return errors.New("the local checkout changed during remote verification; it has been retained")
	}
	return ctx.Err()
}

func validateStandaloneCheckoutGit(ctx context.Context, gitDir string) error {
	info, err := os.Lstat(gitDir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("materialization needs an application-owned Git metadata directory")
	}
	for _, relative := range []string{"commondir", "gitdir", "config.worktree", "objects/info/alternates", "objects/info/http-alternates", "info/grafts"} {
		if _, err := os.Lstat(filepath.Join(gitDir, relative)); !errors.Is(err, os.ErrNotExist) {
			return errors.New("the checkout has external or shared Git metadata; keep it in place")
		}
	}
	keys, err := localCheckoutGit(ctx, gitDir, "config", "--local", "--name-only", "--list")
	if err != nil {
		return errors.New("cannot inspect the checkout's local Git configuration")
	}
	for _, key := range strings.Split(keys, "\n") {
		key = strings.ToLower(key)
		if strings.HasPrefix(key, "include.") || strings.HasPrefix(key, "includeif.") || key == "extensions.worktreeconfig" {
			return errors.New("the checkout has external Git configuration; retain it in place")
		}
	}
	return filepath.WalkDir(gitDir, func(path string, entry fs.DirEntry, err error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() && !entry.Type().IsRegular() {
			return errors.New("Git metadata contains external links or special files")
		}
		if strings.HasSuffix(entry.Name(), ".lock") {
			return errors.New("Git is busy; finish its current operation before keeping or freeing the checkout")
		}
		return nil
	})
}

// Git output here contains only textual metadata/status, never blob contents.
// stderr is discarded because Git configuration may contain credential-bearing
// URLs. Repository and indexed-config environment overrides are removed;
// native SSH/credential helper and user-global configuration selectors remain
// available for a caller's explicit fetch. Index refreshes and filesystem-
// monitor hooks remain disabled for the local inspection commands.
func localCheckoutGit(ctx context.Context, directory string, args ...string) (string, error) {
	commandArgs := append([]string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-C", directory}, args...)
	cmd := exec.CommandContext(ctx, "git", commandArgs...)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(key, "GIT_") || key == "GIT_SSH" || key == "GIT_SSH_COMMAND" || key == "GIT_SSH_VARIANT" || key == "GIT_ASKPASS" || key == "GIT_CONFIG_GLOBAL" || key == "GIT_CONFIG_SYSTEM" || key == "GIT_CONFIG_NOSYSTEM" {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_OPTIONAL_LOCKS=0", "GIT_NO_LAZY_FETCH=1", "GIT_NO_REPLACE_OBJECTS=1", "GIT_TERMINAL_PROMPT=0")
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
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		stdout.Close()
		return "", err
	}
	stop := context.AfterFunc(ctx, func() { _ = stdout.Close() })
	output, readErr := io.ReadAll(io.LimitReader(stdout, (8<<20)+1))
	if readErr != nil || len(output) > 8<<20 {
		_ = cmd.Cancel()
	}
	waitErr := cmd.Wait()
	stop()
	stdout.Close()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if len(output) > 8<<20 {
		return "", errors.New("Git returned too much local checkout metadata")
	}
	return strings.TrimSuffix(string(output), "\n"), errors.Join(readErr, waitErr)
}

type checkoutFileRecord struct {
	Mode     fs.FileMode
	Size     int64
	Modified time.Time
	Digest   string
	Link     string
	Xattrs   map[string][]byte
}

func checkoutTreeManifest(ctx context.Context, root string, omitRootGit bool) (map[string]checkoutFileRecord, error) {
	return checkoutTreeManifestWithValidator(ctx, root, omitRootGit, checkoutValidateNativeMetadata)
}

func checkoutTreeManifestWithValidator(ctx context.Context, root string, omitRootGit bool, validateMetadata func(string) error) (map[string]checkoutFileRecord, error) {
	manifest := make(map[string]checkoutFileRecord)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if omitRootGit && relative == ".git" {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if err := validateMetadata(path); err != nil {
			return err
		}
		record := checkoutFileRecord{Mode: info.Mode(), Size: info.Size(), Modified: info.ModTime()}
		switch {
		case info.Mode().IsRegular():
			file, err := openCheckoutRegular(path)
			if err != nil {
				return err
			}
			hash := sha256.New()
			_, copyErr := io.Copy(hash, checkoutContextReader{ctx: ctx, reader: file})
			closeErr := file.Close()
			if err := errors.Join(copyErr, closeErr); err != nil {
				return err
			}
			record.Digest = hex.EncodeToString(hash.Sum(nil))
		case info.Mode()&os.ModeSymlink != 0:
			record.Link, err = os.Readlink(path)
			if err != nil {
				return err
			}
		case info.IsDir():
			record.Size = 0
		default:
			return errors.New("the checkout contains special files; preserve them before materializing or freeing it")
		}
		record.Xattrs, err = checkoutReadXattrs(path)
		if err != nil {
			return fmt.Errorf("cannot preserve checkout extended metadata: %w", err)
		}
		manifest[relative] = record
		return nil
	})
	return manifest, err
}

func checkoutManifestContentEqual(left, right map[string]checkoutFileRecord) bool {
	if len(left) != len(right) {
		return false
	}
	for path, value := range left {
		other, exists := right[path]
		if !exists {
			return false
		}
		// A directory's modified time changes as .git is inserted and files are
		// copied; symlink timestamps cannot be set portably without following it.
		if value.Mode.IsDir() || value.Mode&os.ModeSymlink != 0 {
			value.Modified, other.Modified = time.Time{}, time.Time{}
		}
		// macOS assigns provenance to ordinary newly created host files even
		// when the virtual source had no such attribute. This added OS
		// attestation does not replace source metadata. Existing provenance
		// and all user/application attributes must still match exactly.
		if _, sourceHasProvenance := value.Xattrs["com.apple.provenance"]; !sourceHasProvenance {
			if _, targetHasProvenance := other.Xattrs["com.apple.provenance"]; targetHasProvenance {
				attrs := make(map[string][]byte, len(other.Xattrs))
				for key, bytes := range other.Xattrs {
					if key != "com.apple.provenance" {
						attrs[key] = bytes
					}
				}
				if len(attrs) == 0 {
					attrs = nil
				}
				other.Xattrs = attrs
			}
		}
		if !reflect.DeepEqual(value, other) {
			return false
		}
	}
	return true
}

// Diagnostics report only metadata categories and numbers. Paths are hashed,
// and file content/xattr bytes, content digests and Git configuration are never
// included in an API error or log.
func checkoutManifestDifference(left, right map[string]checkoutFileRecord) string {
	if len(left) != len(right) {
		return fmt.Sprintf("entry count differs (source=%d copy=%d)", len(left), len(right))
	}
	paths := make([]string, 0, len(left))
	for path := range left {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		source := left[path]
		copied, exists := right[path]
		identity := sha256.Sum256([]byte(path))
		label := fmt.Sprintf("entry %x", identity[:6])
		if !exists {
			return label + " is missing"
		}
		if checkoutManifestContentEqual(map[string]checkoutFileRecord{path: source}, map[string]checkoutFileRecord{path: copied}) {
			continue
		}
		if source.Mode != copied.Mode {
			return fmt.Sprintf("%s mode differs (source=%#o copy=%#o)", label, uint32(source.Mode), uint32(copied.Mode))
		}
		if source.Size != copied.Size {
			return fmt.Sprintf("%s size differs (source=%d copy=%d)", label, source.Size, copied.Size)
		}
		if source.Digest != copied.Digest {
			return label + " file content differs"
		}
		if source.Link != copied.Link {
			return label + " symbolic link target differs"
		}
		if source.Mode.IsRegular() && source.Modified != copied.Modified {
			return fmt.Sprintf("%s modified time differs (source_ns=%d copy_ns=%d)", label, source.Modified.UnixNano(), copied.Modified.UnixNano())
		}
		if len(source.Xattrs) != len(copied.Xattrs) {
			return fmt.Sprintf("%s extended attribute count differs (source=%d copy=%d)", label, len(source.Xattrs), len(copied.Xattrs))
		}
		if (source.Xattrs == nil) != (copied.Xattrs == nil) {
			return label + " extended attribute empty representation differs"
		}
		return label + " extended attribute names or values differ"
	}
	return "copied metadata differs"
}

func copyCheckoutTree(ctx context.Context, source, destination string, omitRootGit bool) error {
	var directories []string
	err := filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if omitRootGit && relative == ".git" {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		switch {
		case info.IsDir():
			if err := os.Mkdir(target, 0700); err != nil && !(relative == "." && errors.Is(err, os.ErrExist)) {
				return err
			}
			directories = append(directories, relative)
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if err := os.Symlink(link, target); err != nil {
				return err
			}
			if err := checkoutCopySymlinkMode(target, info.Mode()); err != nil {
				return err
			}
		case info.Mode().IsRegular():
			cloned, err := checkoutCloneRegular(path, target)
			if err != nil {
				return err
			}
			if !cloned {
				input, err := openCheckoutRegular(path)
				if err != nil {
					return err
				}
				output, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
				if err != nil {
					input.Close()
					return err
				}
				_, copyErr := io.Copy(output, checkoutContextReader{ctx: ctx, reader: input})
				err = errors.Join(copyErr, input.Close(), output.Close())
				if err != nil {
					return err
				}
			}
			if err := os.Chmod(target, info.Mode()); err != nil {
				return err
			}
		default:
			return errors.New("the checkout contains special files that cannot be safely copied")
		}
		attrs, err := checkoutReadXattrs(path)
		if err != nil {
			return err
		}
		if err := checkoutWriteXattrs(target, attrs); err != nil {
			return err
		}
		// Writing a macOS resource fork changes the regular file's modified
		// time. Restore timestamps after every extended attribute is copied.
		if info.Mode().IsRegular() {
			return os.Chtimes(target, info.ModTime(), info.ModTime())
		}
		return nil
	})
	if err != nil {
		return err
	}
	for index := len(directories) - 1; index >= 0; index-- {
		relative := directories[index]
		info, err := os.Lstat(filepath.Join(source, relative))
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if err := os.Chmod(target, info.Mode()); err != nil {
			return err
		}
		if err := os.Chtimes(target, info.ModTime(), info.ModTime()); err != nil {
			return err
		}
	}
	return nil
}

func openCheckoutRegular(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		file.Close()
		return nil, errors.New("a checkout file changed while it was being read")
	}
	return file, nil
}

func syncCheckoutTree(ctx context.Context, root string) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		return errors.Join(file.Sync(), file.Close())
	})
}

type checkoutContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r checkoutContextReader) Read(destination []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(destination)
}
