package desktop

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func fakeAdoptionGit(t *testing.T, script string) string {
	t.Helper()
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "git"), []byte("#!/bin/sh\nset -eu\n"+script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	return directory
}

func TestAdoptionProbeCancelsInheritedOutputPromptly(t *testing.T) {
	started := filepath.Join(t.TempDir(), "started")
	t.Setenv("AFS_ADOPTION_PROBE_STARTED", started)
	directory := fakeAdoptionGit(t, `
printf '%s' started > "$AFS_ADOPTION_PROBE_STARTED"
# Retain both descriptors after the Git process itself has exited.
(sleep 10) &
exit 0
`)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := probeAdoptionBranch(ctx, directory, "https://example.invalid/owner/repo.git", "")
		done <- err
	}()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("probe exited before its child started: %v", err)
		case <-deadline.C:
			t.Fatal("probe never started")
		case <-ticker.C:
		}
	}
	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation error = %v", err)
		}
		if elapsed := time.Since(start); elapsed >= time.Second {
			t.Fatalf("cancellation took %s; inherited descriptors kept the probe alive", elapsed)
		}
	case <-time.After(time.Second):
		t.Fatal("probe did not cancel promptly while a child retained output")
	}
}

func TestAdoptionProbeBoundsMetadata(t *testing.T) {
	directory := fakeAdoptionGit(t, `
dd if=/dev/zero bs=4096 count=257 2>/dev/null
`)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := probeAdoptionBranch(ctx, directory, "https://example.invalid/owner/repo.git", "")
	if err == nil || !strings.Contains(err.Error(), "too much metadata") {
		t.Fatalf("oversized metadata error = %v", err)
	}
}

func TestAdoptionProbeDoesNotExposeDiagnostics(t *testing.T) {
	const secret = "ghp_adoption_diagnostics_must_remain_private"
	for _, exit := range []string{"exit 0", "exit 1"} {
		t.Run(exit, func(t *testing.T) {
			directory := fakeAdoptionGit(t, `
printf '%s\n' 'Authorization: token ghp_adoption_diagnostics_must_remain_private' >&2
printf '%s\n' 'https://user:ghp_adoption_diagnostics_must_remain_private@example.invalid/repo.git'
`+exit)
			_, err := probeAdoptionBranch(context.Background(), directory, "https://example.invalid/owner/repo.git", "")
			if err == nil || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "Authorization") || strings.Contains(err.Error(), "user:") {
				t.Fatalf("probe returned raw diagnostic data: %v", err)
			}
		})
	}
}

func TestAdoptionProbeUsesLiteralArgumentsAndNativeCredentials(t *testing.T) {
	arguments := filepath.Join(t.TempDir(), "arguments")
	t.Setenv("AFS_ADOPTION_PROBE_ARGUMENTS", arguments)
	t.Setenv("GIT_ALLOW_PROTOCOL", "ssh")
	t.Setenv("GIT_SSH_COMMAND", "ssh -i /native/identity")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "credential.helper")
	t.Setenv("GIT_CONFIG_VALUE_0", "native-helper")
	directory := fakeAdoptionGit(t, `
[ "$GIT_ALLOW_PROTOCOL" = ssh ]
[ "$GIT_SSH_COMMAND" = 'ssh -i /native/identity' ]
[ "$GIT_CONFIG_COUNT" = 1 ]
[ "$GIT_CONFIG_KEY_0" = credential.helper ]
[ "$GIT_CONFIG_VALUE_0" = native-helper ]
[ "$GIT_TERMINAL_PROMPT" = 0 ]
printf '%s\n' "$@" > "$AFS_ADOPTION_PROBE_ARGUMENTS"
printf 'ref: refs/heads/trunk\tHEAD\n'
printf '1234567890123456789012345678901234567890\tHEAD\n'
`)
	remote := "git@example.invalid:owner/repo.git"
	branch, err := probeAdoptionBranch(context.Background(), directory, remote, "")
	if err != nil || branch != "trunk" {
		t.Fatalf("native probe = %q, %v", branch, err)
	}
	actual, err := os.ReadFile(arguments)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{"ls-remote", "--symref", "--upload-pack=git-upload-pack", "--", remote, "HEAD", ""}, "\n")
	if string(actual) != want {
		t.Fatalf("probe arguments = %q, want %q", actual, want)
	}
}

func TestNativeAdoptionEnvironmentPreservesTrustedGitSettings(t *testing.T) {
	retained := []string{
		"PATH=/native/bin", "GIT_ALLOW_PROTOCOL=https:ssh", "GIT_PROTOCOL_FROM_USER=0",
		"GIT_SSH_COMMAND=ssh -i /native/identity", "GIT_SSH=/native/ssh", "SSH_AUTH_SOCK=/native/agent",
		"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=credential.helper", "GIT_CONFIG_VALUE_0=native-helper",
		"GIT_CONFIG_PARAMETERS='credential.helper=native-helper'", "GIT_CONFIG_GLOBAL=/native/global", "GIT_CONFIG_SYSTEM=/native/system",
	}
	removed := []string{
		"GIT_DIR=/unrelated/repo", "GIT_COMMON_DIR=/unrelated/common", "GIT_WORK_TREE=/unrelated/tree",
		"GIT_INDEX_FILE=/unrelated/index", "GIT_OBJECT_DIRECTORY=/unrelated/objects", "GIT_ALTERNATE_OBJECT_DIRECTORIES=/unrelated/alternates",
		"GIT_PREFIX=unrelated/", "GIT_SHALLOW_FILE=/unrelated/shallow", "GIT_NAMESPACE=unrelated", "GIT_REPLACE_REF_BASE=refs/unrelated/", "GIT_GRAFT_FILE=/unrelated/grafts",
		"GIT_TRACE=/private/trace", "GIT_TRACE_CURL=1", "GIT_TRACE_PACKET=1", "GIT_CURL_VERBOSE=1", "GIT_TERMINAL_PROMPT=1",
	}
	input := append(append([]string(nil), retained...), removed...)
	want := append(append([]string(nil), retained...), "GIT_TERMINAL_PROMPT=0")
	if actual := nativeAdoptionEnvironment(input); !reflect.DeepEqual(actual, want) {
		t.Fatalf("native environment = %q, want %q", actual, want)
	}
}

func TestAdoptionRemoteRejectsCredentialAndTransportBoundaries(t *testing.T) {
	for _, raw := range []string{
		"--upload-pack=/tmp/helper", "ext::helper", "custom::repository", "ftp://example.invalid/owner/repo.git",
		"https://user@example.invalid/owner/repo.git", "https://user:password@example.invalid/owner/repo.git",
		"ssh://git:password@example.invalid/owner/repo.git", "ssh://ghp_secret@example.invalid/owner/repo.git", "ghp_secret@example.invalid:owner/repo.git",
		"oauth2@example.invalid:owner/repo.git", "user:password@example.invalid:owner/repo.git", "@example.invalid:owner/repo.git", "git@example.invalid]:owner/repo.git",
		"git@-example.invalid:owner/repo.git", "ssh://git@-example.invalid/owner/repo.git", "ssh://git@example.invalid:0/owner/repo.git",
		"https://example.invalid/owner/repo.git?", "https://example.invalid/owner/repo.git#", "https://example.invalid/owner/../repo.git",
		"https://example.invalid/owner/%2e%2e/repo.git", "https://example.invalid/owner%2frepo.git", "https://example.invalid/owner%5crepo.git",
		"https://example.invalid/owner/repo%00.git", "ssh://git@example.invalid/owner/repo%0a.git", "git@example.invalid:owner/../repo.git",
		"https://example.invalid/owner/repo.git\n", " https://example.invalid/owner/repo.git", "file://remote.invalid/tmp/repo.git",
	} {
		t.Run(raw, func(t *testing.T) {
			if _, err := parseAdoptionRemote(raw); err == nil {
				t.Fatal("unsafe or malformed source was accepted")
			}
		})
	}
}

func TestAdoptionBranchMetadataRejectsConflictingHead(t *testing.T) {
	oid := strings.Repeat("1", 40)
	for _, output := range []string{
		"ref: refs/heads/main\tHEAD\nref: refs/heads/trunk\tHEAD\n" + oid + "\tHEAD\n",
		"ref: refs/heads/main\tHEAD\n" + oid + "\tHEAD\n" + strings.Repeat("2", 40) + "\tHEAD\n",
		"ref: refs/heads/HEAD\tHEAD\n" + oid + "\tHEAD\n",
	} {
		if _, err := decodeAdoptionBranch(output, ""); err == nil {
			t.Fatalf("conflicting or reserved metadata was accepted: %q", output)
		}
	}
}
