//go:build darwin

package main

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/cloudflare/artifact-fs/internal/auth"
	"github.com/cloudflare/artifact-fs/internal/desktop"
	"golang.org/x/sys/unix"
)

// This is an acceptance test for an already installed, enabled native module.
// It intentionally does not install an app, change extension settings, or use
// macFUSE. See docs/reporeach/fskit-acceptance.md before opting in.
func TestFSKitMountedAcceptance(t *testing.T) {
	if os.Getenv("AFS_RUN_FSKIT_E2E_TESTS") != "1" {
		t.Skip("set AFS_RUN_FSKIT_E2E_TESTS=1 for real macOS 26 FSKit acceptance")
	}
	release, err := unix.Sysctl("kern.osrelease")
	if err != nil {
		t.Fatal(err)
	}
	major, err := strconv.Atoi(strings.Split(release, ".")[0])
	if err != nil || major < 25 {
		t.Skipf("real FSKit acceptance requires macOS 26+ (Darwin 25+); host Darwin %s provides no mounted proof", release)
	}
	prerequisites := fsKitAcceptanceModule(t)

	root, err := os.MkdirTemp("/tmp", "rr-fskit-")
	if err != nil {
		t.Fatal(err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	h := &fsKitAcceptanceHarness{root: root, state: filepath.Join(root, "state"), mount: filepath.Join(root, "mount"), prerequisites: prerequisites}
	t.Cleanup(func() { h.cleanup(t) })
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(root, "global-config"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GH_CONFIG_DIR", filepath.Join(root, "gh-config"))
	// Fail on any GitHub call, including accidental background authentication.
	ghSentinel := filepath.Join(root, "gh-called")
	t.Setenv("AFS_E2E_DESKTOP_GH_SENTINEL", ghSentinel)
	h.gh = filepath.Join(root, "gh")
	fsKitWrite(t, h.gh, []byte("#!/bin/sh\nprintf '%s\\n' called >> \"$AFS_E2E_DESKTOP_GH_SENTINEL\"\nexit 99\n"), 0o700)

	source, bare := filepath.Join(root, "original"), filepath.Join(root, "remote.git")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	fsKitGit(t, source, "init", "--initial-branch=trunk")
	fsKitGit(t, source, "config", "user.name", "FSKit fixture")
	fsKitGit(t, source, "config", "user.email", "fixture@example.invalid")
	text := []byte("initial committed text\n")
	binary := []byte{0, 255, 128, '\n', 0, 254, 1, 2}
	fsKitWrite(t, filepath.Join(source, "tracked.txt"), text, 0o644)
	fsKitWrite(t, filepath.Join(source, "binary.dat"), binary, 0o644)
	fsKitWrite(t, filepath.Join(source, "executable"), []byte("#!/bin/sh\nprintf '%s\\n' fixture-executable\n"), 0o755)
	if err := os.Symlink("tracked.txt", filepath.Join(source, "link")); err != nil {
		t.Fatal(err)
	}
	fsKitGit(t, source, "add", ".")
	fsKitGit(t, source, "commit", "-m", "initial fixture")
	initialHead := strings.TrimSpace(fsKitGit(t, source, "rev-parse", "HEAD"))
	fsKitGit(t, root, "clone", "--bare", source, bare)
	fsKitGit(t, source, "remote", "add", "origin", bare)
	fsKitWrite(t, filepath.Join(source, "tracked.txt"), []byte("staged original\n"), 0o644)
	fsKitGit(t, source, "add", "tracked.txt")
	fsKitWrite(t, filepath.Join(source, "tracked.txt"), []byte("dirty original\n"), 0o644)
	fsKitWrite(t, filepath.Join(source, "untracked.txt"), []byte("untracked original\n"), 0o644)
	beforeSource := snapshotDesktopAdoptionSource(t, source)

	h.copyServerImage(t)
	h.start(t)
	const owner, repoID = "Acceptance", "Acceptance/project"
	var status desktop.Status
	h.request(t, http.MethodPost, "/v1/repositories/adopt", desktop.AdoptionRequest{RemoteURL: source, Owner: owner, Name: "project"}, &status)
	if status.Account != nil || desktopAdoptedRepository(t, status, repoID).State != "virtual" {
		t.Fatalf("manual adoption was not unauthenticated and lazy: %+v", status)
	}
	h.request(t, http.MethodPost, "/v1/mount", nil, &status)
	if !status.Mounted {
		t.Fatalf("native catalogue did not mount: %+v", status)
	}
	h.identity(t)
	waitDesktopCatalogueNames(t, h.mount, []string{owner})
	waitDesktopCatalogueNames(t, filepath.Join(h.mount, owner), []string{"project"})
	h.request(t, http.MethodGet, "/v1/status", nil, &status)
	if desktopAdoptedRepository(t, status, repoID).State != "virtual" {
		t.Fatal("listing catalogue placeholders acquired the Git checkout")
	}
	repo := filepath.Join(h.mount, owner, "project")
	fsKitReadEqual(t, filepath.Join(repo, "tracked.txt"), text)
	fsKitReadEqual(t, filepath.Join(repo, "binary.dat"), binary)
	fsKitReadEqual(t, filepath.Join(repo, "link"), text)
	if target, err := os.Readlink(filepath.Join(repo, "link")); err != nil || target != "tracked.txt" {
		t.Fatalf("native symlink target=%q error=%v", target, err)
	}
	if info, err := os.Stat(filepath.Join(repo, "executable")); err != nil || info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("native executable mode: %v, %v", info, err)
	}
	if output := run(t, repo, filepath.Join(repo, "executable")); output != "fixture-executable\n" {
		t.Fatalf("native execution output %q", output)
	}
	waitDesktopCataloguePathMissing(t, filepath.Join(repo, "untracked.txt"))
	if head := strings.TrimSpace(fsKitGit(t, repo, "rev-parse", "HEAD")); head != initialHead {
		t.Fatalf("adoption HEAD=%s want %s", head, initialHead)
	}
	fsKitCleanGit(t, repo)

	// Native Git writes must remain coherent after warm reads and branch checkout.
	committed := []byte("committed through native mount\n")
	fsKitWrite(t, filepath.Join(repo, "tracked.txt"), committed, 0o644)
	fsKitGit(t, repo, "add", "tracked.txt")
	if staged := fsKitGit(t, repo, "status", "--porcelain=v1"); staged != "M  tracked.txt\n" {
		t.Fatalf("native staged state %q", staged)
	}
	fsKitGit(t, repo, "-c", "user.name=FSKit fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "mounted commit")
	fsKitGit(t, repo, "checkout", "-b", "acceptance")
	branchText := []byte("alternate branch bytes\n")
	fsKitWrite(t, filepath.Join(repo, "tracked.txt"), branchText, 0o644)
	fsKitWrite(t, filepath.Join(repo, "branch-only.txt"), []byte("branch-only\n"), 0o644)
	fsKitGit(t, repo, "add", "tracked.txt", "branch-only.txt")
	fsKitGit(t, repo, "-c", "user.name=FSKit fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "alternate tree")
	fsKitReadEqual(t, filepath.Join(repo, "tracked.txt"), branchText)
	fsKitGit(t, repo, "checkout", "trunk")
	fsKitReadEqual(t, filepath.Join(repo, "tracked.txt"), committed)
	waitDesktopCataloguePathMissing(t, filepath.Join(repo, "branch-only.txt"))
	fsKitGit(t, repo, "checkout", "acceptance")
	fsKitReadEqual(t, filepath.Join(repo, "tracked.txt"), branchText)
	fsKitReadEqual(t, filepath.Join(repo, "branch-only.txt"), []byte("branch-only\n"))
	fsKitCleanGit(t, repo)
	fsKitRetainedFile(t, repo)

	// A cwd and open file are retained during a normal detach attempt. No saved
	// visibility, pin, operation or source state may change when it is refused.
	h.server.action(t, repoID, "keep")
	h.request(t, http.MethodGet, "/v1/status", nil, &status)
	beforeStatus := status
	beforeState, err := os.ReadFile(filepath.Join(h.state, "catalogue.json"))
	if err != nil {
		t.Fatal(err)
	}
	beforeIdentity := h.identity(t)
	beforeSession := h.session(t)
	held, err := os.Open(filepath.Join(repo, "tracked.txt"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Close() })
	sleeper := exec.Command("/bin/sleep", "600")
	sleeper.Dir = repo
	if err := sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	sleeperDone := make(chan error, 1)
	go func() { sleeperDone <- sleeper.Wait() }()
	sleeperStopped := false
	stopSleeper := func() {
		if sleeperStopped {
			return
		}
		sleeperStopped = true
		_ = sleeper.Process.Signal(syscall.SIGTERM)
		select {
		case <-sleeperDone:
		case <-time.After(5 * time.Second):
			t.Errorf("fixture cwd process PID %d did not stop; preserving native mount", sleeper.Process.Pid)
		}
	}
	t.Cleanup(stopSleeper)
	data, code := h.rawRequest(t, http.MethodPost, "/v1/repositories/visibility", map[string]any{"id": repoID, "enabled": false})
	if code != http.StatusBadRequest {
		t.Fatalf("busy normal detach did not refuse visibility change: %d %s", code, data)
	}
	h.request(t, http.MethodGet, "/v1/status", nil, &status)
	afterState, err := os.ReadFile(filepath.Join(h.state, "catalogue.json"))
	if err != nil || !bytes.Equal(beforeState, afterState) || !reflect.DeepEqual(beforeStatus.Repositories, status.Repositories) || !reflect.DeepEqual(beforeStatus.Organizations, status.Organizations) || !reflect.DeepEqual(beforeStatus.Operations, status.Operations) {
		t.Fatalf("busy refusal changed saved catalogue or repository operations: %v", err)
	}
	if !status.Mounted || h.identity(t) != beforeIdentity || h.session(t) != beforeSession {
		t.Fatal("busy refusal replaced or detached the active native session")
	}
	buffer := make([]byte, len(branchText))
	if n, err := held.ReadAt(buffer, 0); n != len(buffer) || err != nil || !bytes.Equal(buffer, branchText) {
		t.Fatalf("busy refusal invalidated retained file: n=%d err=%v bytes=%v", n, err, buffer)
	}
	stopSleeper()
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}

	// Every membership change must create a fresh connection for the same path
	// resource. Native FSKit 26 cannot publish this via cache invalidation.
	reconnect := func(path string, body any) {
		t.Helper()
		previous := h.session(t)
		h.request(t, http.MethodPost, path, body, nil)
		h.identity(t)
		if h.session(t) == previous {
			t.Fatal("catalogue change reused its old FSKit bridge session")
		}
	}
	reconnect("/v1/repositories/visibility", map[string]any{"id": repoID, "enabled": false})
	waitDesktopCataloguePathMissing(t, repo)
	reconnect("/v1/repositories/visibility", map[string]any{"id": repoID, "enabled": true})
	waitDesktopCataloguePathPresent(t, repo)
	fsKitReadEqual(t, filepath.Join(repo, "tracked.txt"), branchText)
	reconnect("/v1/organizations/settings", map[string]any{"owner": owner, "enabled": false})
	waitDesktopCataloguePathMissing(t, filepath.Join(h.mount, owner))
	reconnect("/v1/repositories/visibility", map[string]any{"id": repoID, "enabled": false})
	reconnect("/v1/organizations/settings", map[string]any{"owner": owner, "enabled": true})
	waitDesktopCataloguePathMissing(t, repo)
	reconnect("/v1/repositories/visibility", map[string]any{"id": repoID, "enabled": true})
	fsKitReadEqual(t, filepath.Join(repo, "tracked.txt"), branchText)

	previous := h.session(t)
	h.request(t, http.MethodPost, "/v1/unmount", nil, &status)
	if status.Mounted || h.resourceAttached(t) {
		t.Fatal("normal unmount did not detach the private resource")
	}
	h.request(t, http.MethodPost, "/v1/mount", nil, &status)
	h.identity(t)
	if h.session(t) == previous {
		t.Fatal("explicit remount reused the old bridge capability")
	}
	previous = h.session(t)
	if !h.stop(t) {
		t.Fatal("normal prepare-quit could not safely stop the private daemon")
	}
	h.start(t)
	h.request(t, http.MethodGet, "/v1/status", nil, &status)
	if !status.Mounted || !desktopAdoptedRepository(t, status, repoID).Pinned {
		t.Fatalf("restart lost desired mount or download intent: %+v", status)
	}
	h.identity(t)
	if h.session(t) == previous {
		t.Fatal("service restart reused the old bridge capability")
	}
	fsKitReadEqual(t, filepath.Join(repo, "tracked.txt"), branchText)
	fsKitReadEqual(t, filepath.Join(repo, "binary.dat"), binary)
	fsKitReadEqual(t, filepath.Join(repo, "branch-only.txt"), []byte("branch-only\n"))
	if branch := strings.TrimSpace(fsKitGit(t, repo, "branch", "--show-current")); branch != "acceptance" {
		t.Fatalf("restart branch=%q", branch)
	}
	fsKitCleanGit(t, repo)
	if after := snapshotDesktopAdoptionSource(t, source); !reflect.DeepEqual(beforeSource, after) {
		t.Fatal("mounted operations changed the original checkout bytes, modes, index, refs or configuration")
	}
	if _, err := os.Stat(ghSentinel); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("manual native workflow invoked GitHub CLI: %v", err)
	}
}

func selectedFSKitModuleIdentifier(value string) (string, error) {
	const production = "com.enoughtools.reporeach.fskit"
	const validation = "com.enoughtools.reporeach.validation.fskit"
	switch value {
	case "", production:
		return production, nil
	case validation:
		return validation, nil
	default:
		return "", fmt.Errorf("prerequisite: AFS_FSKIT_MODULE_ID must be %s or the isolated validation identity %s; got %q", production, validation, value)
	}
}

func TestSelectedFSKitModuleIdentifier(t *testing.T) {
	const production = "com.enoughtools.reporeach.fskit"
	const validation = "com.enoughtools.reporeach.validation.fskit"
	for _, test := range []struct {
		name, value, want string
	}{
		{name: "default", want: production},
		{name: "explicit production", value: production, want: production},
		{name: "isolated validation", value: validation, want: validation},
		{name: "other module", value: "com.example.fskit"},
		{name: "containing app", value: "com.enoughtools.reporeach"},
		{name: "validation app", value: "com.enoughtools.reporeach.validation"},
		{name: "leading whitespace", value: " " + validation},
		{name: "trailing whitespace", value: production + "\n"},
		{name: "case change", value: "COM.enoughtools.reporeach.fskit"},
		{name: "embedded NUL", value: validation + "\x00"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := selectedFSKitModuleIdentifier(test.value)
			if test.want == "" {
				if err == nil || got != "" || !strings.Contains(err.Error(), "AFS_FSKIT_MODULE_ID") {
					t.Fatalf("unexpected module accepted: value=%q got=%q error=%v", test.value, got, err)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("selected module=%q error=%v want %q", got, err, test.want)
			}
		})
	}
}

type fsKitAcceptancePrerequisites struct {
	identity, team, group, socketDir string
}

func fsKitAcceptanceModule(t *testing.T) fsKitAcceptancePrerequisites {
	t.Helper()
	moduleID, err := selectedFSKitModuleIdentifier(os.Getenv("AFS_FSKIT_MODULE_ID"))
	if err != nil {
		t.Fatal(err)
	}
	app := os.Getenv("AFS_FSKIT_APP")
	if os.Getenv("AFS_FSKIT_MODULE_CONFIRMED") != "1" || !filepath.IsAbs(app) {
		t.Fatal("prerequisite: AFS_FSKIT_APP must name the installed matching RepoReach.app and AFS_FSKIT_MODULE_CONFIRMED=1 must confirm its matching native module is already enabled; see docs/reporeach/fskit-acceptance.md")
	}
	app, err = filepath.EvalSymlinks(app)
	if err != nil {
		t.Fatalf("canonical installed native app path: %v", err)
	}
	module := filepath.Join(app, "Contents", "Extensions", "RepoReachFSKit.appex")
	plist := filepath.Join(module, "Contents", "Info.plist")
	read := func(key string) string {
		return fsKitReadPlistValue(t, plist, key)
	}
	for key, wanted := range map[string]string{
		"CFBundleIdentifier": moduleID,
		"EXAppExtensionAttributes:EXExtensionPointIdentifier": "com.apple.fskit.fsmodule",
		"EXAppExtensionAttributes:FSShortName":                "reporeach",
		"EXAppExtensionAttributes:FSSupportsPathURLs":         "true",
	} {
		if got := read(key); got != wanted {
			t.Fatalf("installed native module %s=%q want %q", key, got, wanted)
		}
	}
	executable := read("CFBundleExecutable")
	if executable == "" || filepath.Base(executable) != executable {
		t.Fatal("installed native module has invalid executable name")
	}
	if info, err := os.Stat(filepath.Join(module, "Contents", "MacOS", executable)); err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("installed native module executable: %v, %v", info, err)
	}
	fsKitInspectInstalledModule(t, moduleID, module)
	prerequisites := fsKitAcceptanceAppGroup(t, app, module, moduleID)
	t.Logf("Darwin native acceptance uses already installed module %s with identifier %s version %s (%s); matching source and enablement explicitly confirmed by operator", module, moduleID, read("CFBundleShortVersionString"), read("CFBundleVersion"))
	return prerequisites
}

func fsKitReadPlistValue(t *testing.T, plist, key string) string {
	t.Helper()
	output := fsKitAcceptanceCommand(t, nil, "/usr/libexec/PlistBuddy", "-c", "Print :"+key, plist)
	return strings.TrimSpace(string(output))
}

func validateFSKitSigningIdentity(identity string) error {
	if len(identity) != 40 || strings.IndexFunc(identity, func(r rune) bool {
		return !(r >= '0' && r <= '9' || r >= 'A' && r <= 'F')
	}) >= 0 {
		return errors.New("prerequisite: AFS_FSKIT_SIGNING_IDENTITY must provide the full uppercase SHA-1 fingerprint of the module's existing local signing identity")
	}
	return nil
}

func fsKitCanonicalAppGroup(team string) (string, error) {
	if len(team) != 10 || strings.IndexFunc(team, func(r rune) bool {
		return !(r >= '0' && r <= '9' || r >= 'A' && r <= 'Z')
	}) >= 0 {
		return "", errors.New("signed component has no canonical ten-character developer team")
	}
	return team + ".rr", nil
}

func validateFSKitGroupClaims(claims map[string]any, group string, onlyGroup bool) error {
	groups, ok := claims["com.apple.security.application-groups"].([]any)
	if !ok || len(groups) != 1 || groups[0] != group {
		return errors.New("signed component must claim exactly the shared TeamID.rr app group")
	}
	for _, key := range []string{"get-task-allow", "com.apple.security.get-task-allow"} {
		if _, present := claims[key]; present {
			return errors.New("debugger access is forbidden in native acceptance components")
		}
	}
	if onlyGroup && len(claims) != 1 {
		return errors.New("native acceptance helper must have only its shared app-group entitlement")
	}
	return nil
}

func validateFSKitModuleClaims(claims map[string]any, team, moduleID string) error {
	// Inspect signed claims only; provisioning CMS authorization is a separate
	// prerequisite. An older App ID prefix may differ from the signing team.
	if claims["com.apple.developer.fskit.fsmodule"] != true || claims["com.apple.security.app-sandbox"] != true || claims["com.apple.developer.team-identifier"] != team {
		return errors.New("native module must claim its FSKit capability, sandbox and exact signing team")
	}
	identifierCount := 0
	for _, key := range []string{"com.apple.application-identifier", "application-identifier"} {
		if value, present := claims[key]; present {
			identifier, ok := value.(string)
			prefix, suffixOK := strings.CutSuffix(identifier, "."+moduleID)
			if _, err := fsKitCanonicalAppGroup(prefix); !ok || !suffixOK || err != nil {
				return errors.New("native module must have one signed explicit application identifier for its exact bundle")
			}
			identifierCount++
		}
	}
	if identifierCount != 1 || len(claims) != 5 {
		return errors.New("native module must have only its group, FSKit capability, sandbox and signed identifier/team claims")
	}
	return nil
}

type fsKitSignedComponent struct {
	identity, team string
	claims         map[string]any
}

func fsKitInspectSignedComponent(t *testing.T, path string) fsKitSignedComponent {
	t.Helper()
	fsKitAcceptanceCommand(t, nil, "/usr/bin/codesign", "--verify", "--strict", path)
	// Display details go to stderr; keep them separate from typed entitlement XML.
	_, details, err := fsKitBoundedCommand(context.Background(), nil, "/usr/bin/codesign", "-d", "--verbose=4", path)
	if err != nil {
		t.Fatalf("inspect native component signature: %v\n%s", err, auth.RedactString(string(details)))
	}
	var team string
	authority, hardened := false, false
	for _, line := range strings.Split(string(details), "\n") {
		if value, ok := strings.CutPrefix(line, "TeamIdentifier="); ok {
			if team != "" {
				t.Fatal("native component signature repeats its developer team")
			}
			team = value
		}
		authority = authority || strings.HasPrefix(line, "Authority=Apple Development:") || strings.HasPrefix(line, "Authority=Developer ID Application:")
		hardened = hardened || strings.HasPrefix(line, "CodeDirectory ") && strings.Contains(line, "(runtime)")
	}
	if _, err := fsKitCanonicalAppGroup(team); err != nil || !authority || !hardened {
		t.Fatal("native component requires Apple Development or Developer ID Application signing, its developer team, and hardened runtime")
	}
	private := t.TempDir()
	prefix := filepath.Join(private, "certificate")
	fsKitAcceptanceCommand(t, nil, "/usr/bin/codesign", "-d", "--extract-certificates="+prefix, path)
	file, err := os.Open(prefix + "0")
	if err != nil {
		t.Fatalf("read public native signing certificate: %v", err)
	}
	encoded, readErr := io.ReadAll(io.LimitReader(file, 64*1024+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || len(encoded) > 64*1024 {
		t.Fatal("public native signing certificate is unreadable or exceeds its bound")
	}
	certificate, err := x509.ParseCertificate(encoded)
	now := time.Now()
	if err != nil || len(certificate.Subject.OrganizationalUnit) != 1 || certificate.Subject.OrganizationalUnit[0] != team || now.Before(certificate.NotBefore) || !now.Before(certificate.NotAfter) {
		t.Fatal("native signing certificate does not establish the current developer team and validity")
	}
	xml := fsKitAcceptanceCommand(t, nil, "/usr/bin/codesign", "-d", "--entitlements", "-", "--xml", path)
	encodedClaims := fsKitAcceptanceCommand(t, xml, "/usr/bin/plutil", "-convert", "json", "-o", "-", "-")
	var claims map[string]any
	if err := json.Unmarshal(encodedClaims, &claims); err != nil || claims == nil {
		t.Fatal("native signature entitlements are not a typed dictionary")
	}
	return fsKitSignedComponent{identity: fmt.Sprintf("%X", sha1.Sum(encoded)), team: team, claims: claims}
}

func fsKitAcceptanceAppGroup(t *testing.T, app, module, moduleID string) fsKitAcceptancePrerequisites {
	t.Helper()
	identity := os.Getenv("AFS_FSKIT_SIGNING_IDENTITY")
	if err := validateFSKitSigningIdentity(identity); err != nil {
		t.Fatal(err)
	}
	moduleSignature := fsKitInspectSignedComponent(t, module)
	group, err := fsKitCanonicalAppGroup(moduleSignature.team)
	if err != nil || identity != moduleSignature.identity {
		t.Fatal("AFS_FSKIT_SIGNING_IDENTITY must match the enabled module's exact existing certificate")
	}
	parentInfo := filepath.Join(app, "Contents", "Info.plist")
	if fsKitReadPlistValue(t, parentInfo, "CFBundleIdentifier") != strings.TrimSuffix(moduleID, ".fskit") {
		t.Fatal("native module and containing app identifiers do not match")
	}
	for _, info := range []string{parentInfo, filepath.Join(module, "Contents", "Info.plist")} {
		if fsKitReadPlistValue(t, info, "RepoReachAppGroupIdentifier") != group {
			t.Fatal("native app and filesystem module must declare their exact signed TeamID.rr app group")
		}
	}
	for _, path := range []string{module, app, filepath.Join(app, "Contents", "Helpers", "artifact-fs")} {
		signature := moduleSignature
		if path != module {
			signature = fsKitInspectSignedComponent(t, path)
		}
		if signature.identity != identity || signature.team != moduleSignature.team {
			t.Fatal("native parent, filesystem and engine must share the enabled module's exact certificate and developer team")
		}
		if err := validateFSKitGroupClaims(signature.claims, group, path != module); err != nil {
			t.Fatalf("native app-group prerequisite: %v", err)
		}
		if path == module {
			if err := validateFSKitModuleClaims(signature.claims, moduleSignature.team, moduleID); err != nil {
				t.Fatalf("native module signing prerequisite: %v", err)
			}
		}
	}
	executable := fsKitReadPlistValue(t, parentInfo, "CFBundleExecutable")
	if executable == "" || filepath.Base(executable) != executable || executable == "." || executable == ".." || strings.ContainsAny(executable, "\\\x00") {
		t.Fatal("native parent has an unsafe executable name")
	}
	path := filepath.Join(app, "Contents", "MacOS", executable)
	info, err := os.Lstat(path)
	canonical, canonicalErr := filepath.EvalSymlinks(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 || canonicalErr != nil || canonical != path {
		t.Fatal("native parent resolver must be its exact regular signed executable")
	}
	output := fsKitAcceptanceCommand(t, nil, path, "--resolve-fsbridge-container")
	directory, err := parseFSKitContainerResolution(output, group)
	if err != nil {
		t.Fatalf("native app-group container resolution: %v", err)
	}
	if err := validateFSKitContainerDirectory(directory); err != nil {
		t.Fatalf("native app-group container access: %v", err)
	}
	t.Logf("native acceptance app-group prerequisites verified for team %s and exact existing signing certificate; resolved container %s", moduleSignature.team, directory)
	return fsKitAcceptancePrerequisites{identity: identity, team: moduleSignature.team, group: group, socketDir: directory}
}

func parseFSKitContainerResolution(output []byte, group string) (string, error) {
	if len(output) == 0 || len(output) > 4096 || !utf8.Valid(output) {
		return "", errors.New("resolver JSON is absent or exceeds its bound")
	}
	decoder := json.NewDecoder(bytes.NewReader(output))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return "", errors.New("resolver must return a JSON object")
	}
	values := make(map[string]string, 2)
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok || key != "groupIdentifier" && key != "directory" {
			return "", errors.New("resolver returned an unknown field")
		}
		if _, duplicate := values[key]; duplicate {
			return "", errors.New("resolver returned a duplicate field")
		}
		var value string
		if err := decoder.Decode(&value); err != nil {
			return "", errors.New("resolver fields must be strings")
		}
		values[key] = value
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return "", errors.New("resolver object is incomplete")
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return "", errors.New("resolver returned trailing output")
	}
	directory := values["directory"]
	if len(values) != 2 || values["groupIdentifier"] != group || !filepath.IsAbs(directory) || directory == "/" || filepath.Clean(directory) != directory || strings.ContainsRune(directory, '\x00') {
		return "", errors.New("resolver did not establish the exact signed group and canonical absolute directory")
	}
	return directory, nil
}

func validateFSKitContainerDirectory(directory string) error {
	info, err := os.Lstat(directory)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	canonical, canonicalErr := filepath.EvalSymlinks(directory)
	if !info.IsDir() || info.Mode().Perm() != 0o700 || !ok || stat.Uid != uint32(os.Getuid()) || canonicalErr != nil || canonical != directory {
		return errors.New("resolved container must be a canonical private directory owned by the current user")
	}
	return nil
}

func fsKitAcceptanceCommand(t *testing.T, stdin []byte, program string, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	stdout, stderr, err := fsKitBoundedCommand(ctx, stdin, program, args...)
	if err != nil {
		t.Fatalf("native acceptance prerequisite command %s: %v\n%s", filepath.Base(program), err, auth.RedactString(string(stderr)))
	}
	return stdout
}

func fsKitBoundedCommand(ctx context.Context, stdin []byte, program string, args ...string) ([]byte, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, program, args...)
	command.Stdin = bytes.NewReader(stdin)
	command.WaitDelay = 2 * time.Second
	stdout, stderr := &fsKitInspectorOutput{}, &fsKitInspectorOutput{}
	command.Stdout, command.Stderr = stdout, stderr
	err := command.Run()
	if stdout.truncated || stderr.truncated {
		return nil, nil, errors.New("native acceptance prerequisite command output exceeded its bound")
	}
	return stdout.data, stderr.data, err
}

func TestFSKitAcceptanceSigningIdentity(t *testing.T) {
	const fingerprint = "0123456789ABCDEF0123456789ABCDEF01234567"
	for _, test := range []struct {
		name, identity string
		valid          bool
	}{
		{name: "explicit full fingerprint", identity: fingerprint, valid: true},
		{name: "absent identity"},
		{name: "signing name", identity: "Apple Development: Fixture"},
		{name: "lowercase fingerprint", identity: strings.ToLower(fingerprint)},
		{name: "short fingerprint", identity: fingerprint[:39]},
		{name: "leading whitespace", identity: " " + fingerprint},
		{name: "invalid character", identity: "G" + fingerprint[1:]},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateFSKitSigningIdentity(test.identity); (err == nil) != test.valid {
				t.Fatalf("identity accepted=%v want %v: %v", err == nil, test.valid, err)
			}
		})
	}
}

func TestFSKitAcceptanceGroupClaims(t *testing.T) {
	const group = "FIXTURE123.rr"
	for _, team := range []string{"", "FIXTURE12", "fixture123", "FIXTURE12_", "FIXTURE123\n", "FIXTURE12é"} {
		if actual, err := fsKitCanonicalAppGroup(team); err == nil || actual != "" {
			t.Fatalf("invalid team %q established a group %q: %v", team, actual, err)
		}
	}
	if actual, err := fsKitCanonicalAppGroup("FIXTURE123"); err != nil || actual != group {
		t.Fatalf("canonical group=%q error=%v", actual, err)
	}
	for _, test := range []struct {
		name      string
		claims    map[string]any
		onlyGroup bool
		valid     bool
	}{
		{name: "minimum helper", claims: map[string]any{"com.apple.security.application-groups": []any{group}}, onlyGroup: true, valid: true},
		{name: "profile-bound filesystem", claims: map[string]any{"com.apple.security.application-groups": []any{group}, "com.apple.developer.fskit.fsmodule": true, "com.apple.security.app-sandbox": true}, valid: true},
		{name: "absent group", claims: map[string]any{}},
		{name: "empty groups", claims: map[string]any{"com.apple.security.application-groups": []any{}}},
		{name: "different group", claims: map[string]any{"com.apple.security.application-groups": []any{"OTHERTEAM1.rr"}}},
		{name: "extra group", claims: map[string]any{"com.apple.security.application-groups": []any{group, "OTHERTEAM1.rr"}}},
		{name: "non-string group", claims: map[string]any{"com.apple.security.application-groups": []any{1}}},
		{name: "group not array", claims: map[string]any{"com.apple.security.application-groups": group}},
		{name: "modern group", claims: map[string]any{"com.apple.security.application-groups": []any{"group.com.enoughtools.reporeach"}}},
		{name: "extra helper entitlement", claims: map[string]any{"com.apple.security.application-groups": []any{group}, "com.apple.security.network.client": true}, onlyGroup: true},
		{name: "debug access", claims: map[string]any{"com.apple.security.application-groups": []any{group}, "com.apple.security.get-task-allow": true}},
		{name: "false debugger claim", claims: map[string]any{"com.apple.security.application-groups": []any{group}, "get-task-allow": false}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateFSKitGroupClaims(test.claims, group, test.onlyGroup); (err == nil) != test.valid {
				t.Fatalf("group claims accepted=%v want %v: %v", err == nil, test.valid, err)
			}
		})
	}
}

func TestFSKitContainerResolution(t *testing.T) {
	const group = "FIXTURE123.rr"
	for _, test := range []struct {
		name, output string
		valid        bool
	}{
		{name: "exact resolver result", output: `{"groupIdentifier":"FIXTURE123.rr","directory":"/fixture/container"}`, valid: true},
		{name: "whitespace and key order", output: " {\"directory\":\"/fixture/container\",\"groupIdentifier\":\"FIXTURE123.rr\"}\n", valid: true},
		{name: "missing output"},
		{name: "different group", output: `{"groupIdentifier":"OTHERTEAM1.rr","directory":"/fixture/container"}`},
		{name: "unknown field", output: `{"groupIdentifier":"FIXTURE123.rr","directory":"/fixture/container","extra":true}`},
		{name: "duplicate field", output: `{"groupIdentifier":"FIXTURE123.rr","directory":"/fixture/container","directory":"/fixture/other"}`},
		{name: "missing directory", output: `{"groupIdentifier":"FIXTURE123.rr"}`},
		{name: "missing group", output: `{"directory":"/fixture/container"}`},
		{name: "relative directory", output: `{"groupIdentifier":"FIXTURE123.rr","directory":"fixture/container"}`},
		{name: "traversal", output: `{"groupIdentifier":"FIXTURE123.rr","directory":"/fixture/../container"}`},
		{name: "trailing separator", output: `{"groupIdentifier":"FIXTURE123.rr","directory":"/fixture/container/"}`},
		{name: "root directory", output: `{"groupIdentifier":"FIXTURE123.rr","directory":"/"}`},
		{name: "NUL directory", output: `{"groupIdentifier":"FIXTURE123.rr","directory":"/fixture/container\u0000"}`},
		{name: "non-string directory", output: `{"groupIdentifier":"FIXTURE123.rr","directory":false}`},
		{name: "JSON array", output: `[{"groupIdentifier":"FIXTURE123.rr","directory":"/fixture/container"}]`},
		{name: "trailing object", output: `{"groupIdentifier":"FIXTURE123.rr","directory":"/fixture/container"}{}`},
		{name: "trailing diagnostics", output: `{"groupIdentifier":"FIXTURE123.rr","directory":"/fixture/container"}diagnostic`},
		{name: "truncated object", output: `{"groupIdentifier":"FIXTURE123.rr","directory":"/fixture/container"`},
		{name: "excessive output", output: strings.Repeat(" ", 4097)},
		{name: "invalid UTF8", output: "{\"groupIdentifier\":\"FIXTURE123.rr\",\"directory\":\"/fixture/\xff\"}"},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory, err := parseFSKitContainerResolution([]byte(test.output), group)
			if (err == nil) != test.valid || test.valid && directory != "/fixture/container" || !test.valid && directory != "" {
				t.Fatalf("resolver result directory=%q error=%v valid=%v", directory, err, test.valid)
			}
		})
	}
}

func TestFSKitAcceptanceModuleClaims(t *testing.T) {
	const team, moduleID = "FIXTURE123", "com.enoughtools.reporeach.validation.fskit"
	base := map[string]any{
		"com.apple.security.application-groups": []any{team + ".rr"},
		"com.apple.developer.fskit.fsmodule":    true,
		"com.apple.security.app-sandbox":        true,
		"com.apple.developer.team-identifier":   team,
		"com.apple.application-identifier":      team + "." + moduleID,
	}
	for _, test := range []struct {
		name   string
		change func(map[string]any)
		valid  bool
	}{
		{name: "exact module claims", change: func(map[string]any) {}, valid: true},
		{name: "alternate profile identifier key", change: func(c map[string]any) {
			c["application-identifier"] = c["com.apple.application-identifier"]
			delete(c, "com.apple.application-identifier")
		}, valid: true},
		{name: "older App ID prefix differs from signing team", change: func(c map[string]any) {
			c["com.apple.application-identifier"] = "OLDERID123." + moduleID
		}, valid: true},
		{name: "missing capability", change: func(c map[string]any) { delete(c, "com.apple.developer.fskit.fsmodule") }},
		{name: "sandbox false", change: func(c map[string]any) { c["com.apple.security.app-sandbox"] = false }},
		{name: "untyped capability", change: func(c map[string]any) { c["com.apple.developer.fskit.fsmodule"] = "true" }},
		{name: "other team", change: func(c map[string]any) { c["com.apple.developer.team-identifier"] = "OTHERTEAM1" }},
		{name: "other app", change: func(c map[string]any) { c["com.apple.application-identifier"] = team + ".com.example.fskit" }},
		{name: "wildcard app", change: func(c map[string]any) { c["com.apple.application-identifier"] = team + ".*" }},
		{name: "noncanonical App ID prefix", change: func(c map[string]any) { c["com.apple.application-identifier"] = "legacy." + moduleID }},
		{name: "non-string App ID", change: func(c map[string]any) { c["com.apple.application-identifier"] = true }},
		{name: "duplicate identifier claims", change: func(c map[string]any) { c["application-identifier"] = c["com.apple.application-identifier"] }},
		{name: "missing identifier", change: func(c map[string]any) { delete(c, "com.apple.application-identifier") }},
		{name: "network client", change: func(c map[string]any) { c["com.apple.security.network.client"] = true }},
		{name: "network server", change: func(c map[string]any) { c["com.apple.security.network.server"] = true }},
		{name: "debugger", change: func(c map[string]any) { c["get-task-allow"] = true }},
		{name: "unknown entitlement", change: func(c map[string]any) { c["com.apple.security.files.user-selected.read-write"] = true }},
	} {
		t.Run(test.name, func(t *testing.T) {
			claims := make(map[string]any, len(base))
			for key, value := range base {
				claims[key] = value
			}
			test.change(claims)
			if err := validateFSKitModuleClaims(claims, team, moduleID); (err == nil) != test.valid {
				t.Fatalf("module claims accepted=%v want %v: %v", err == nil, test.valid, err)
			}
		})
	}
}

func TestFSKitContainerDirectory(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	container := filepath.Join(root, "container")
	if err := os.Mkdir(container, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := validateFSKitContainerDirectory(container); err != nil {
		t.Fatalf("owned private real container: %v", err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(container, alias); err != nil {
		t.Fatal(err)
	}
	if err := validateFSKitContainerDirectory(alias); err == nil {
		t.Fatal("symlink accepted as an actual app-group container")
	}
	if err := os.Chmod(container, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := validateFSKitContainerDirectory(container); err == nil {
		t.Fatal("group-accessible container accepted")
	}
	if err := validateFSKitContainerDirectory(filepath.Join(root, "missing")); err == nil {
		t.Fatal("absent container accepted")
	}
	regular := filepath.Join(root, "regular")
	fsKitWrite(t, regular, nil, 0o700)
	if err := validateFSKitContainerDirectory(regular); err == nil {
		t.Fatal("regular file accepted as a container")
	}
}

type fsKitInspectionCandidate struct {
	ModuleID   string `json:"module_id"`
	ModulePath string `json:"module_path"`
}

type fsKitInstalledInspection struct {
	OK                 bool                       `json:"ok"`
	ExpectedModuleID   string                     `json:"expected_module_id"`
	ExpectedModulePath string                     `json:"expected_module_path"`
	ShortName          string                     `json:"short_name"`
	CandidateCount     int                        `json:"candidate_count"`
	Candidates         []fsKitInspectionCandidate `json:"candidates"`
	Unknowns           *[]json.RawMessage         `json:"unknowns"`
	OutputTruncated    *bool                      `json:"output_truncated"`
}

func validateFSKitInstalledInspection(result fsKitInstalledInspection, moduleID, modulePath string) error {
	if !result.OK || result.OutputTruncated == nil || *result.OutputTruncated || result.Unknowns == nil || len(*result.Unknowns) != 0 ||
		result.ExpectedModuleID != moduleID || result.ExpectedModulePath != modulePath || result.ShortName != "reporeach" ||
		result.CandidateCount != 1 || len(result.Candidates) != 1 || result.Candidates[0].ModuleID != moduleID || result.Candidates[0].ModulePath != modulePath {
		return errors.New("public FSKit discovery did not establish the selected module as the sole enabled reporeach filesystem")
	}
	return nil
}

func fsKitInspectInstalledModule(t *testing.T, moduleID, modulePath string) {
	t.Helper()
	canonical, err := filepath.EvalSymlinks(modulePath)
	if err != nil {
		t.Fatalf("canonical installed native module path: %v", err)
	}
	inspector := os.Getenv("AFS_FSKIT_INSPECTOR")
	if inspector == "" {
		architecture := map[string]string{"arm64": "arm64", "amd64": "x86_64"}[runtime.GOARCH]
		if architecture == "" {
			t.Fatal("public FSKit inspector requires Apple Silicon or Intel macOS")
		}
		inspector = filepath.Join(t.TempDir(), "inspect-fskit-module")
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		output, err := fsKitInspectorCommand(ctx, "/usr/bin/xcrun", "swiftc", "-parse-as-library", "-target", architecture+"-apple-macos15.4",
			"native/Tools/inspect-fskit-module.swift", "-o", inspector)
		cancel()
		if err != nil {
			t.Fatalf("build read-only FSKit discovery inspector: %v\n%s", err, auth.RedactString(string(output)))
		}
	} else {
		info, err := os.Stat(inspector)
		if !filepath.IsAbs(inspector) || err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			t.Fatal("AFS_FSKIT_INSPECTOR must name an absolute, regular executable built from the matching inspection source")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	output, err := fsKitInspectorCommand(ctx, inspector, moduleID, canonical)
	if err != nil {
		t.Fatalf("public FSKit installed-module selection could not be established: %v\n%s", err, auth.RedactString(string(output)))
	}
	var result fsKitInstalledInspection
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("invalid public FSKit discovery response: %v", err)
	}
	if err := validateFSKitInstalledInspection(result, moduleID, canonical); err != nil {
		t.Fatalf("%v\n%s", err, auth.RedactString(string(output)))
	}
	t.Logf("public FSKit discovery verified the sole enabled reporeach identifier and exact module path: %s", auth.RedactString(string(output)))
}

type fsKitInspectorOutput struct {
	data      []byte
	truncated bool
}

func (w *fsKitInspectorOutput) Write(p []byte) (int, error) {
	const limit = 64 * 1024
	n := min(len(p), limit-len(w.data))
	w.data = append(w.data, p[:n]...)
	w.truncated = w.truncated || n != len(p)
	return len(p), nil
}

func fsKitInspectorCommand(ctx context.Context, program string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, program, args...)
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
	command.WaitDelay = 2 * time.Second
	output := &fsKitInspectorOutput{}
	// Identical comparable output writers are copied by one exec goroutine.
	command.Stdout, command.Stderr = output, output
	err := command.Run()
	if output.truncated {
		return nil, errors.New("public FSKit inspector output exceeded its bound")
	}
	return output.data, err
}

func TestFSKitInstalledInspectionEvidence(t *testing.T) {
	const identifier, path = "com.enoughtools.reporeach.validation.fskit", "/fixture/RepoReachFSKit.appex"
	empty := []json.RawMessage{}
	falseValue := false
	base := fsKitInstalledInspection{
		OK: true, ExpectedModuleID: identifier, ExpectedModulePath: path, ShortName: "reporeach",
		CandidateCount: 1, Candidates: []fsKitInspectionCandidate{{ModuleID: identifier, ModulePath: path}},
		Unknowns: &empty, OutputTruncated: &falseValue,
	}
	for _, test := range []struct {
		name   string
		change func(*fsKitInstalledInspection)
		valid  bool
	}{
		{name: "verified exact selection", change: func(*fsKitInstalledInspection) {}, valid: true},
		{name: "absent discovery", change: func(r *fsKitInstalledInspection) { r.OK = false }},
		{name: "ambiguous discovery", change: func(r *fsKitInstalledInspection) { r.CandidateCount = 2 }},
		{name: "other module selected", change: func(r *fsKitInstalledInspection) { r.Candidates[0].ModuleID = "com.enoughtools.reporeach.fskit" }},
		{name: "different installed copy", change: func(r *fsKitInstalledInspection) { r.Candidates[0].ModulePath = "/another/copy.appex" }},
		{name: "unknown competing metadata", change: func(r *fsKitInstalledInspection) {
			unknown := []json.RawMessage{json.RawMessage(`{"reason":"unreadable"}`)}
			r.Unknowns = &unknown
		}},
		{name: "missing unknown inventory", change: func(r *fsKitInstalledInspection) { r.Unknowns = nil }},
		{name: "missing output bound", change: func(r *fsKitInstalledInspection) { r.OutputTruncated = nil }},
		{name: "truncated output", change: func(r *fsKitInstalledInspection) { value := true; r.OutputTruncated = &value }},
		{name: "self-test result cannot establish module selection", change: func(r *fsKitInstalledInspection) { r.ExpectedModuleID = ""; r.CandidateCount = 0; r.Candidates = nil }},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := base
			result.Candidates = append([]fsKitInspectionCandidate(nil), base.Candidates...)
			test.change(&result)
			if err := validateFSKitInstalledInspection(result, identifier, path); (err == nil) != test.valid {
				t.Fatalf("selection evidence accepted=%v want %v: %v", err == nil, test.valid, err)
			}
		})
	}
}

type fsKitAcceptanceHarness struct {
	root, state, mount, gh, image string
	prerequisites                 fsKitAcceptancePrerequisites
	server                        *desktopAdoptionServer
	preserve                      bool
}

func (h *fsKitAcceptanceHarness) copyServerImage(t *testing.T) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(executable)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	h.image = filepath.Join(h.root, "test-engine")
	out, err := os.OpenFile(h.image, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil || closeErr != nil {
		t.Fatalf("copy private daemon image: %v, %v", copyErr, closeErr)
	}
	claimsPath := filepath.Join(h.root, "test-engine.entitlements")
	claims := []byte("<?xml version=\"1.0\" encoding=\"UTF-8\"?><plist version=\"1.0\"><dict><key>com.apple.security.application-groups</key><array><string>" + h.prerequisites.group + "</string></array></dict></plist>")
	fsKitWrite(t, claimsPath, claims, 0o600)
	fsKitAcceptanceCommand(t, nil, "/usr/bin/codesign", "--force", "--sign", h.prerequisites.identity, "--options", "runtime", "--timestamp=none", "--identifier", "com.enoughtools.reporeach.acceptance-engine", "--entitlements", claimsPath, h.image)
	signature := fsKitInspectSignedComponent(t, h.image)
	if signature.identity != h.prerequisites.identity || signature.team != h.prerequisites.team {
		t.Fatal("copied acceptance daemon does not use the module's exact existing certificate and developer team")
	}
	if err := validateFSKitGroupClaims(signature.claims, h.prerequisites.group, true); err != nil {
		t.Fatalf("copied acceptance daemon signing claims: %v", err)
	}
}

func (h *fsKitAcceptanceHarness) start(t *testing.T) {
	t.Helper()
	h.server = &desktopAdoptionServer{socket: filepath.Join(h.root, "control.sock"), mountRoot: h.mount, logPath: filepath.Join(h.root, "server.log"), done: make(chan error, 1)}
	s := h.server
	log, err := os.OpenFile(s.logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	// No child test timeout: a failed normal detach must leave its owner alive.
	s.cmd = exec.Command(h.image, "-test.run=^TestDesktopAdoptionServer$", "-test.v", "-test.timeout=0")
	s.cmd.Stdout, s.cmd.Stderr = log, log
	s.cmd.Dir = h.root
	s.cmd.Env = append(os.Environ(), "AFS_E2E_DESKTOP_SERVER=1", "AFS_E2E_DESKTOP_STATE="+h.state, "AFS_E2E_DESKTOP_MOUNT="+h.mount, "AFS_E2E_DESKTOP_SOCKET="+s.socket, "AFS_E2E_DESKTOP_GH="+h.gh, "AFS_E2E_DESKTOP_FSKIT_SOCKET_DIR="+h.prerequisites.socketDir)
	if err := s.cmd.Start(); err != nil {
		_ = log.Close()
		t.Fatal(err)
	}
	t.Logf("private native fixture=%s daemon PID=%d socket=%s", h.root, s.cmd.Process.Pid, s.socket)
	go func() { err := s.cmd.Wait(); _ = log.Close(); s.done <- err }()
	waitForCondition(t, 60*time.Second, "private native daemon readiness", func() (bool, string) {
		select {
		case err := <-s.done:
			s.done <- err
			return false, fmt.Sprintf("daemon exited: %v; inspect %s", err, s.logPath)
		default:
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, code, err := desktop.Request(ctx, s.socket, http.MethodGet, "/v1/status", nil)
		return err == nil && code == http.StatusOK, fmt.Sprintf("status=%d err=%v; log=%s", code, err, s.logPath)
	})
}

func (h *fsKitAcceptanceHarness) rawRequest(t *testing.T, method, path string, body any) ([]byte, int) {
	t.Helper()
	var data []byte
	if body != nil {
		var err error
		data, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	data, code, err := desktop.Request(ctx, h.server.socket, method, path, data)
	if err != nil {
		t.Fatalf("native desktop %s %s: %v; log=%s", method, path, err, h.server.logPath)
	}
	return data, code
}

func (h *fsKitAcceptanceHarness) request(t *testing.T, method, path string, body, response any) {
	t.Helper()
	data, code := h.rawRequest(t, method, path, body)
	if code < 200 || code >= 300 {
		t.Fatalf("native desktop %s %s: %d %s; log=%s", method, path, code, data, h.server.logPath)
	}
	if response != nil {
		if err := json.Unmarshal(data, response); err != nil {
			t.Fatal(err)
		}
	}
}

// Cleanup only asks the owned service to detach normally. A busy/uncertain
// result preserves both the owner and its private resource for investigation.
func (h *fsKitAcceptanceHarness) stop(t *testing.T) (safe bool) {
	t.Helper()
	defer func() {
		if !safe {
			h.preserve = true
		}
	}()
	if h.preserve {
		return false
	}
	s := h.server
	if s == nil || s.stopped || s.cmd == nil || s.cmd.Process == nil {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	_, code, err := desktop.Request(ctx, s.socket, http.MethodPost, "/v1/prepare-quit", nil)
	if err != nil || code != http.StatusOK {
		t.Errorf("normal prepare-quit failed (%d, %v); preserving fixture %s and daemon PID %d. Inspect %s; close fixture users and retry normal prepare-quit. No forced unmount or process kill was attempted", code, err, h.root, s.cmd.Process.Pid, s.logPath)
		return false
	}
	attached, err := h.attached()
	if err != nil || attached {
		t.Errorf("cannot confirm private resource detached: attached=%v err=%v; preserving %s and daemon PID %d", attached, err, h.root, s.cmd.Process.Pid)
		return false
	}
	if err := s.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Errorf("stop detached private daemon PID %d: %v; preserving %s", s.cmd.Process.Pid, err, h.root)
		return false
	}
	select {
	case err := <-s.done:
		s.stopped = true
		if err != nil {
			t.Errorf("private daemon exit: %v; preserving log %s", err, s.logPath)
			return false
		}
		return true
	case <-time.After(30 * time.Second):
		t.Errorf("detached daemon PID %d did not stop; preserving %s without killing it", s.cmd.Process.Pid, h.root)
		return false
	}
}

func (h *fsKitAcceptanceHarness) cleanup(t *testing.T) {
	t.Helper()
	if t.Failed() && h.server != nil {
		h.logFailure(t)
	}
	if !h.stop(t) {
		return
	}
	attached, err := h.attached()
	if err != nil || attached {
		t.Errorf("preserving fixture %s: resource still attached=%v or mount-table error=%v", h.root, attached, err)
		return
	}
	if err := os.RemoveAll(h.root); err != nil {
		t.Errorf("remove detached private fixture %s: %v", h.root, err)
	}
}

// Preserve complete, bounded diagnostic lines before disposable cleanup.
func (h *fsKitAcceptanceHarness) logFailure(t *testing.T) {
	t.Helper()
	file, err := os.Open(h.server.logPath)
	if err != nil {
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return
	}
	const limit = int64(16 * 1024)
	start := max(int64(0), info.Size()-limit)
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return
	}
	data, err := io.ReadAll(io.LimitReader(file, limit))
	if err != nil {
		return
	}
	if start > 0 {
		_, data, _ = bytes.Cut(data, []byte("\n"))
	}
	if end := bytes.LastIndexByte(data, '\n'); end >= 0 {
		data = data[:end+1]
	} else {
		return
	}
	t.Logf("private native daemon diagnostics:\n%s", auth.RedactString(string(data)))
}

type fsKitAcceptanceIdentity struct {
	fsid               [2]int32
	owner              uint32
	root, source, kind string
}

func fsKitAcceptanceMounts() ([]fsKitAcceptanceIdentity, error) {
	count, err := unix.Getfsstat(nil, unix.MNT_NOWAIT)
	if err != nil {
		return nil, err
	}
	stats := make([]unix.Statfs_t, count+16)
	n, err := unix.Getfsstat(stats, unix.MNT_NOWAIT)
	if err != nil {
		return nil, err
	}
	if n >= len(stats) {
		return nil, errors.New("mount table changed beyond acceptance buffer; attachment uncertain")
	}
	identities := make([]fsKitAcceptanceIdentity, 0, n)
	for _, stat := range stats[:n] {
		identities = append(identities, fsKitAcceptanceIdentity{fsid: stat.Fsid.Val, owner: stat.Owner, root: unix.ByteSliceToString(stat.Mntonname[:]), source: unix.ByteSliceToString(stat.Mntfromname[:]), kind: unix.ByteSliceToString(stat.Fstypename[:])})
	}
	return identities, nil
}

func (h *fsKitAcceptanceHarness) sourceMatches(source string) bool {
	expected := filepath.Join(h.state, "FSKit")
	if source == expected {
		return true
	}
	u, err := url.Parse(source)
	return err == nil && u.Scheme == "file" && (u.Host == "" || u.Host == "localhost") && u.User == nil && u.Path == expected && u.RawQuery == "" && u.Fragment == "" && u.Opaque == ""
}

func (h *fsKitAcceptanceHarness) attached() (bool, error) {
	mounts, err := fsKitAcceptanceMounts()
	if err != nil {
		return false, err
	}
	for _, identity := range mounts {
		// Preserve the fixture if any volume is attached anywhere below it,
		// including an unexpected child mount. RemoveAll must not traverse one.
		if identity.root == h.root || strings.HasPrefix(identity.root, h.root+string(os.PathSeparator)) || h.sourceMatches(identity.source) {
			return true, nil
		}
	}
	return false, nil
}

func (h *fsKitAcceptanceHarness) resourceAttached(t *testing.T) bool {
	t.Helper()
	attached, err := h.attached()
	if err != nil {
		t.Fatal(err)
	}
	return attached
}

func (h *fsKitAcceptanceHarness) identity(t *testing.T) fsKitAcceptanceIdentity {
	t.Helper()
	mounts, err := fsKitAcceptanceMounts()
	if err != nil {
		t.Fatal(err)
	}
	for _, identity := range mounts {
		if identity.root == h.mount && h.sourceMatches(identity.source) && identity.fsid != ([2]int32{}) && identity.kind != "" && identity.owner == uint32(os.Getuid()) {
			return identity
		}
	}
	t.Fatalf("kernel has no owned FSKit mount at %s using private resource %s", h.mount, filepath.Join(h.state, "FSKit"))
	return fsKitAcceptanceIdentity{}
}

func (h *fsKitAcceptanceHarness) session(t *testing.T) [32]byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(h.state, "FSKit", "connection.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Compare session capability changes without ever printing its secret bytes.
	return sha256.Sum256(data)
}

func fsKitWrite(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
}

func fsKitReadEqual(t *testing.T, path string, expected []byte) {
	t.Helper()
	if data, err := os.ReadFile(path); err != nil || !bytes.Equal(data, expected) {
		t.Fatalf("native read %s: bytes=%v want=%v error=%v", path, data, expected, err)
	}
}

func fsKitGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", gitArgsWithSafeDirectory(dir, args...)...)
	command.Dir = dir
	// Fixtures and mounted Git are isolated from inherited repository bindings.
	for _, value := range os.Environ() {
		name, _, _ := strings.Cut(value, "=")
		switch name {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_NAMESPACE", "GIT_CONFIG", "GIT_CONFIG_COUNT", "GIT_CONFIG_PARAMETERS":
			continue
		}
		if strings.HasPrefix(name, "GIT_CONFIG_KEY_") || strings.HasPrefix(name, "GIT_CONFIG_VALUE_") {
			continue
		}
		command.Env = append(command.Env, value)
	}
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("fixture native git %v: %v\n%s", args, err, stderr.Bytes())
	}
	return stdout.String()
}

func fsKitCleanGit(t *testing.T, repo string) {
	t.Helper()
	if status := fsKitGit(t, repo, "status", "--porcelain=v1", "--untracked-files=all"); status != "" {
		t.Fatalf("native Git tree is dirty: %q", status)
	}
}

func fsKitRetainedFile(t *testing.T, repo string) {
	t.Helper()
	path, renamed := filepath.Join(repo, "retained.tmp"), filepath.Join(repo, "renamed.tmp")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.Write([]byte("before")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, renamed); err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte("after!"), 0); err != nil {
		t.Fatal(err)
	}
	fsKitReadEqual(t, renamed, []byte("after!"))
	if err := os.Remove(renamed); err != nil {
		t.Fatal(err)
	}
	waitDesktopCataloguePathMissing(t, renamed)
	if _, err := file.WriteAt([]byte("unlinked"), 0); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, len("unlinked"))
	if n, err := file.ReadAt(data, 0); n != len(data) || err != nil || !bytes.Equal(data, []byte("unlinked")) {
		t.Fatalf("retained unlinked read/write: n=%d err=%v bytes=%v", n, err, data)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	fsKitCleanGit(t, repo)
}
