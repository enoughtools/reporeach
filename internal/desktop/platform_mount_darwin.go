//go:build darwin

package desktop

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cloudflare/artifact-fs/internal/auth"
	"github.com/cloudflare/artifact-fs/internal/catalogfs"
	"github.com/cloudflare/artifact-fs/internal/fsbridge"
	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"golang.org/x/sys/unix"
)

const (
	fsKitMountTimeout     = 28 * time.Second
	fsKitStderrLimit      = 8 << 10
	fsKitCommandWaitDelay = 2 * time.Second
)

var errFSKitMountOwnership = errors.New("the repository mount could not be safely identified; keep RepoReach running and unmount that folder before retrying")

// The bridge's lifetime belongs to the kernel mount, rather than the context
// used to request or observe it. The narrow interface also permits lifecycle
// tests without invoking an operating-system mount.
type platformBridge interface {
	SourceDirectory() string
	Done() <-chan struct{}
	Err() error
	CloseDrain(context.Context) error
}

type fsKitMountIdentity struct {
	fsid                   [2]int32
	owner                  uint32
	typeName, root, source string
}

type fsKitMountOperations struct {
	ready       func() bool
	start       func(context.Context, string, string, *catalogfs.FileSystem) (platformBridge, error)
	command     func(context.Context, string, ...string) error
	rootFSID    func(string) ([2]int32, error)
	mounts      func() ([]fsKitMountIdentity, error)
	poll        time.Duration
	timeout     time.Duration
	verifyDelay time.Duration
}

func nativeFSKitOperations(logger *slog.Logger) fsKitMountOperations {
	return fsKitMountOperations{
		ready: platformDependencyReady,
		start: func(ctx context.Context, source, socketDir string, fs *catalogfs.FileSystem) (platformBridge, error) {
			return fsbridge.StartWithSocketDirectory(ctx, source, socketDir, fs)
		},
		command: func(ctx context.Context, program string, args ...string) error {
			return runFSKitMountCommandLogged(ctx, logger, program, args...)
		},
		rootFSID: func(root string) ([2]int32, error) {
			var stat unix.Statfs_t
			if err := unix.Statfs(root, &stat); err != nil {
				return [2]int32{}, err
			}
			return stat.Fsid.Val, nil
		},
		mounts: cachedDarwinMounts,
		poll:   500 * time.Millisecond, timeout: fsKitMountTimeout,
		verifyDelay: 2 * time.Second,
	}
}

func runFSKitMountCommand(ctx context.Context, program string, args ...string) error {
	return runFSKitMountCommandLogged(ctx, slog.Default(), program, args...)
}

// FSKit command stderr is text diagnostics, separate from bridge/blob data.
// A fixed capture limit prevents a helper's output from retaining unbounded data.
type fsKitCommandStderr struct {
	mu        sync.Mutex
	data      [fsKitStderrLimit]byte
	length    int
	truncated bool
}

func (b *fsKitCommandStderr) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := copy(b.data[b.length:], p)
	b.length += n
	b.truncated = b.truncated || n < len(p)
	return len(p), nil // Continue draining stderr after the capture limit.
}

func (b *fsKitCommandStderr) diagnostic() (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	data := b.data[:b.length]
	if b.truncated {
		// A cut URL could end before '@', hiding its credential structure from
		// redaction. Only retain complete whitespace-delimited diagnostic tokens.
		if end := bytes.LastIndexAny(data, " \t\r\n"); end >= 0 {
			data = data[:end]
		} else {
			data = nil
		}
	}
	return auth.RedactString(strings.TrimSpace(string(data))), b.truncated
}

func runFSKitMountCommandLogged(ctx context.Context, logger *slog.Logger, program string, args ...string) error {
	// Stdout stays /dev/null. WaitDelay bounds the stderr-copy goroutine if a
	// helper inherits its pipe. A process group also stops ordinary children on
	// request cancellation; the independently observed kernel mount owns its bridge.
	command := exec.CommandContext(ctx, program, args...)
	stderr := &fsKitCommandStderr{}
	command.Stderr = stderr
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return os.ErrProcessDone
		}
		err := unix.Kill(-command.Process.Pid, unix.SIGKILL)
		if errors.Is(err, unix.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	command.WaitDelay = fsKitCommandWaitDelay
	err := command.Run()
	if err != nil && logger != nil {
		diagnostic, truncated := stderr.diagnostic()
		exitCode := -1
		if command.ProcessState != nil {
			exitCode = command.ProcessState.ExitCode()
		}
		// Never log arguments, raw stderr or backend diagnostics in product errors.
		logger.Warn("native filesystem command failed", "command", filepath.Base(program), "exit_code", exitCode,
			"error", auth.RedactString(err.Error()), "stderr", diagnostic, "stderr_truncated", truncated)
	}
	return err
}

func platformDependencyReady() bool {
	release, err := unix.Sysctl("kern.osrelease")
	return err == nil && supportsNativeFSKit(release)
}

func supportsNativeFSKit(release string) bool {
	major, _, _ := strings.Cut(release, ".")
	version, err := strconv.Atoi(major)
	return err == nil && version >= 25 // Darwin 25 is macOS 26.
}

func platformDependencyMessage() string {
	return "Mounting repositories requires macOS 26 or later and the RepoReach File System Extension enabled in System Settings"
}

// Getfsstat with NOWAIT reads the kernel's retained mount information. Statfs
// against a dead FSKit volume can instead block on the extension we need to
// detach. Statfs is used only for the healthy, unmounted root's initial FSID.
func cachedDarwinMounts() ([]fsKitMountIdentity, error) {
	count, err := unix.Getfsstat(nil, unix.MNT_NOWAIT)
	if err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 3; attempt++ {
		buffer := make([]unix.Statfs_t, count+16)
		n, err := unix.Getfsstat(buffer, unix.MNT_NOWAIT)
		if err != nil {
			return nil, err
		}
		if n >= len(buffer) {
			count = n + 16
			continue
		}
		mounts := make([]fsKitMountIdentity, 0, n)
		for _, stat := range buffer[:n] {
			mounts = append(mounts, fsKitMountIdentity{
				fsid: stat.Fsid.Val, owner: stat.Owner,
				typeName: unix.ByteSliceToString(stat.Fstypename[:]),
				root:     unix.ByteSliceToString(stat.Mntonname[:]),
				source:   unix.ByteSliceToString(stat.Mntfromname[:]),
			})
		}
		return mounts, nil
	}
	return nil, errors.New("mount table changed while checking the repository folder")
}

func (s *Service) platformMountCatalogue(ctx context.Context, root string, fs *catalogfs.FileSystem) (fusefs.MountedFS, error) {
	return s.mountNativeFSKit(ctx, root, fs, nativeFSKitOperations(s.logger))
}

func (s *Service) mountNativeFSKit(ctx context.Context, root string, fs *catalogfs.FileSystem, ops fsKitMountOperations) (fusefs.MountedFS, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.opts.FSKitSocketDir == "" {
		return nil, errors.New("the shared File System Extension connection folder is missing; start the service from the RepoReach app")
	}
	if !filepath.IsAbs(s.opts.FSKitSocketDir) || pathsLexicallyOverlap(root, s.opts.FSKitSocketDir) {
		return nil, errors.New("the File System Extension connection folder must be absolute and outside the repository mount folder")
	}
	if !ops.ready() {
		return nil, errors.New(platformDependencyMessage())
	}
	if err := privateDirectory(s.opts.FSKitSocketDir, false); err != nil {
		return nil, errors.New("the private shared File System Extension connection folder is unavailable")
	}
	socketDir, err := filepath.EvalSymlinks(s.opts.FSKitSocketDir)
	if err != nil || pathsOverlap(root, socketDir) {
		return nil, errors.New("mount folder and File System Extension connection folder must be separate")
	}
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, errors.New("the repository mount folder is unavailable")
	}
	before, err := ops.mounts()
	if err != nil {
		return nil, errors.New("the repository folder's mount state could not be checked")
	}
	if _, exists := mountAtRoot(before, canonicalRoot); exists {
		return nil, errors.New("the repository folder is already a mounted filesystem")
	}
	initialFSID, err := ops.rootFSID(canonicalRoot)
	if err != nil {
		return nil, errors.New("the repository mount folder is unavailable")
	}
	source := filepath.Join(s.opts.StateDir, "FSKit")
	if err := privateDirectory(source, true); err != nil {
		return nil, errors.New("the private File System Extension folder is unavailable")
	}
	source, err = filepath.EvalSymlinks(source)
	if err != nil {
		return nil, errors.New("the private File System Extension folder is unavailable")
	}
	for _, existing := range before {
		if mountSourceMatches(existing.source, source) {
			return nil, errors.New("the File System Extension resource is already mounted")
		}
	}
	// Starting a bridge does not grant the requesting context ownership of its
	// eventual kernel mount. Cancellation is handled explicitly below.
	bridge, err := ops.start(context.WithoutCancel(ctx), source, socketDir, fs)
	if err != nil {
		return nil, errors.New("the File System Extension connection could not be started")
	}
	mounted := &nativeFSKitMount{root: canonicalRoot, source: bridge.SourceDirectory(), initialFSID: initialFSID, bridge: bridge, ops: ops}
	if err := ctx.Err(); err != nil {
		if drainErr := mounted.drainWithTimeout(); drainErr != nil {
			return mounted, errors.Join(err, drainErr)
		}
		return nil, err
	}
	commandCtx, cancel := context.WithTimeout(ctx, ops.timeout)
	commandErr := ops.command(commandCtx, "/sbin/mount", "-F", "-t", "reporeach", mounted.source, canonicalRoot)
	mounted.pendingAttachment = commandCtx.Err() != nil || errors.Is(commandErr, context.Canceled) || errors.Is(commandErr, context.DeadlineExceeded)
	cancel()
	// An error or cancellation can arrive after the kernel has attached. Check
	// independently of the request context before releasing any bridge state.
	verifyCtx, verifyCancel := context.WithTimeout(context.Background(), ops.verifyDelay)
	defer verifyCancel()
	for {
		mounts, inspectErr := ops.mounts()
		if inspectErr != nil {
			mounted.pendingAttachment = true
			return mounted, errFSKitMountOwnership
		}
		if identity, exists := mountAtRoot(mounts, canonicalRoot); exists {
			mounted.identity = identity // Retain an unidentified candidate by FSID too.
			if identity.fsid != ([2]int32{}) && identity.fsid != initialFSID &&
				identity.typeName != "" && mountSourceMatches(identity.source, mounted.source) {
				mounted.verified, mounted.pendingAttachment = true, false
				if commandErr != nil {
					return mounted, errors.New("the repository folder attached, but mount setup did not finish; retry unmounting in RepoReach")
				}
				return mounted, nil
			}
			return mounted, errFSKitMountOwnership
		}
		if mounted.sessionPresent(mounts) {
			// The command's resource may have been moved before verification.
			// Absence at the requested root alone cannot authorize a drain.
			return mounted, errFSKitMountOwnership
		}
		if !waitMountPoll(verifyCtx, ops.poll) {
			if drainErr := mounted.drainWithTimeout(); drainErr != nil {
				return mounted, drainErr
			}
			return nil, errors.New("the repository folder could not be mounted; enable the RepoReach File System Extension in System Settings and try again")
		}
	}
}

func mountAtRoot(mounts []fsKitMountIdentity, root string) (fsKitMountIdentity, bool) {
	for _, mounted := range mounts {
		if mounted.root == root {
			return mounted, true
		}
	}
	return fsKitMountIdentity{}, false
}

func mountSourceMatches(source, expected string) bool {
	if source == expected {
		return true
	}
	resource, err := url.Parse(source)
	// Parse discards an empty fragment and records an empty query in ForceQuery.
	// Encoded '#' and '?' characters still belong to the resource's exact path.
	return err == nil && resource.Scheme == "file" && (resource.Host == "" || resource.Host == "localhost") &&
		resource.User == nil && !resource.ForceQuery && resource.RawQuery == "" && !strings.Contains(source, "#") &&
		resource.Opaque == "" && resource.Path == expected
}

type nativeFSKitMount struct {
	root, source       string
	initialFSID        [2]int32
	identity           fsKitMountIdentity
	verified           bool
	pendingAttachment  bool
	bridge             platformBridge
	ops                fsKitMountOperations
	mu                 sync.Mutex // Protects lifecycle state only; never held across callbacks.
	drained            bool
	explicitlyDetached bool
	drainGate          chan struct{}
	unmountGate        chan struct{}
}

// sessionPresent checks every mount-table entry: a moved volume must remain
// alive even when its original root no longer names it.
func (m *nativeFSKitMount) sessionPresent(mounts []fsKitMountIdentity) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.verified && m.pendingAttachment {
		for _, mounted := range mounts {
			if mounted.fsid != ([2]int32{}) && mounted.fsid != m.initialFSID && mounted.typeName != "" && mountSourceMatches(mounted.source, m.source) {
				m.identity, m.verified, m.pendingAttachment = mounted, true, false
				break
			}
		}
	}
	if !m.verified {
		if m.pendingAttachment {
			// A canceled mount command cannot cancel an already-dispatched FSKit
			// broker task. Hold the bridge until an attachment is observed, then
			// its captured identity is proven detached. A grace period alone is
			// insufficient evidence for closing this unresolved session.
			return true
		}
		for _, mounted := range mounts {
			if mounted.root == m.root || mountSourceMatches(mounted.source, m.source) ||
				(m.identity.fsid != ([2]int32{}) && mounted.fsid == m.identity.fsid) {
				return true
			}
		}
		return false
	}
	for _, mounted := range mounts {
		if mounted.fsid == m.identity.fsid {
			return true
		}
	}
	return false
}

func (m *nativeFSKitMount) verifiedIdentity() (fsKitMountIdentity, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.identity, m.verified
}

func (m *nativeFSKitMount) drain(ctx context.Context) error {
	m.mu.Lock()
	if m.drainGate == nil {
		m.drainGate = make(chan struct{}, 1)
	}
	gate := m.drainGate
	m.mu.Unlock()
	select {
	case gate <- struct{}{}:
		defer func() { <-gate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	m.mu.Lock()
	if m.drained {
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := m.bridge.CloseDrain(ctx); err != nil {
		return errors.New("the File System Extension connection is still draining; keep RepoReach running and retry")
	}
	m.mu.Lock()
	m.drained = true
	m.mu.Unlock()
	return nil
}

func (m *nativeFSKitMount) drainWithTimeout() error {
	ctx, cancel := context.WithTimeout(context.Background(), m.ops.timeout)
	defer cancel()
	return m.drain(ctx)
}

func (m *nativeFSKitMount) finishUnmount(ctx context.Context) error {
	if err := m.drain(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	m.explicitlyDetached = true
	m.mu.Unlock()
	return nil
}

func (m *nativeFSKitMount) Unmount() error {
	ctx, cancel := context.WithTimeout(context.Background(), m.ops.timeout)
	defer cancel()
	m.mu.Lock()
	if m.unmountGate == nil {
		m.unmountGate = make(chan struct{}, 1)
	}
	gate := m.unmountGate
	m.mu.Unlock()
	select {
	case gate <- struct{}{}:
		defer func() { <-gate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	mounts, err := m.ops.mounts()
	if err != nil {
		return errFSKitMountOwnership
	}
	if !m.sessionPresent(mounts) {
		return m.finishUnmount(ctx)
	}
	current, exists := mountAtRoot(mounts, m.root)
	identity, verified := m.verifiedIdentity()
	if !verified || !exists || current != identity {
		return errFSKitMountOwnership
	}
	// Normal unmount only. An identity mismatch is refused above; never force
	// detachment or close the bridge based on the command's exit status alone.
	// The system command is path-based, so an external replacement between this
	// identity check and the kernel unmount remains a platform limitation.
	commandErr := m.ops.command(ctx, "/sbin/umount", m.root)
	for {
		mounts, err = m.ops.mounts()
		if err != nil {
			return errFSKitMountOwnership
		}
		if !m.sessionPresent(mounts) {
			return m.finishUnmount(ctx)
		}
		current, exists = mountAtRoot(mounts, m.root)
		if !exists || current != identity {
			return errFSKitMountOwnership
		}
		if commandErr != nil || !waitMountPoll(ctx, m.ops.poll) {
			return errors.New("the repository folder is still mounted; close files using it and retry unmounting in RepoReach")
		}
	}
}

func (m *nativeFSKitMount) Join(ctx context.Context) error {
	var bridgeErr error
	serverDone := m.bridge.Done()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		mounts, err := m.ops.mounts()
		if err == nil && !m.sessionPresent(mounts) {
			if bridgeErr == nil {
				bridgeErr = m.bridge.Err()
			}
			drainCtx, cancel := context.WithTimeout(ctx, m.ops.timeout)
			drainErr := m.drain(drainCtx)
			cancel()
			if drainErr == nil {
				m.mu.Lock()
				explicitlyDetached := m.explicitlyDetached
				m.mu.Unlock()
				if explicitlyDetached {
					// A historical bridge failure is useful for unexpected-unmount
					// reporting, but must not make a completed normal cleanup fail
					// forever and retain already-drained repository stores.
					return nil
				}
				return bridgeErr
			}
			// A failed drain does not authorize closing repository stores. Retry
			// while this observation context lives; cancellation retains ownership.
		}
		timer := time.NewTimer(m.ops.poll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-serverDone:
			timer.Stop()
			bridgeErr = m.bridge.Err()
			serverDone = nil
		case <-timer.C:
		}
	}
}

func waitMountPoll(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
