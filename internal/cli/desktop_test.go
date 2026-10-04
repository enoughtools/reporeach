package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/desktop"
)

func TestDesktopRequestFailuresAreJSON(t *testing.T) {
	for _, args := range [][]string{
		{"--socket", filepath.Join(t.TempDir(), "missing.sock")},
		{"--method", "DELETE"},
		{"--path", "https://example.com/"},
		{"--body", "token=do-not-log"},
	} {
		var stdout, stderr bytes.Buffer
		all := append([]string{"desktop", "request"}, args...)
		if code := Run(context.Background(), all, &stdout, &stderr); code != 1 {
			t.Fatalf("request %v code = %d", args, code)
		}
		var response map[string]string
		if err := json.Unmarshal(stdout.Bytes(), &response); err != nil || response["error"] == "" {
			t.Fatalf("request %v response = %q, error = %v", args, stdout.Bytes(), err)
		}
		if stderr.Len() != 0 || bytes.Contains(stdout.Bytes(), []byte("do-not-log")) {
			t.Fatalf("request emitted unsafe or non-JSON diagnostics: %q / %q", stdout.Bytes(), stderr.Bytes())
		}
	}
}

func TestDesktopServeLeavesInheritedGitCredentialEnvironmentUntouched(t *testing.T) {
	root := t.TempDir()
	gh := filepath.Join(root, "gh")
	if err := os.WriteFile(gh, []byte("#!/bin/sh\nexit 4\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_COUNT", "7")
	t.Setenv("GIT_CONFIG_VALUE_1", "existing-helper")
	t.Setenv("GH_TELEMETRY", "true")
	var stdout, stderr bytes.Buffer
	// Rejecting a relative state path exercises cleanup after environment setup
	// without starting a listener or touching any real authentication state.
	code := Run(context.Background(), []string{"desktop", "serve", "--gh", gh, "--state-dir", "relative", "--socket", filepath.Join(root, "engine.sock"), "--mount-root", filepath.Join(root, "mount")}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("serve code = %d", code)
	}
	if os.Getenv("GIT_CONFIG_COUNT") != "7" || os.Getenv("GIT_CONFIG_VALUE_1") != "existing-helper" || os.Getenv("GH_TELEMETRY") != "true" {
		t.Fatal("desktop serve leaked process-scoped credential configuration")
	}
}

func TestDesktopServeKeepsNativeGitCredentialsWhileRunning(t *testing.T) {
	// Unix sockets have a short path limit on macOS; Go's full test-name
	// directory would exceed it before the service can start.
	root, err := os.MkdirTemp("", "rr-cli-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	home := filepath.Join(root, "native-home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	globalConfig := filepath.Join(home, ".gitconfig")
	profile := filepath.Join(root, "native-helper")
	if err := os.WriteFile(profile, []byte("#!/bin/sh\nif [ \"$1\" = get ]; then\n  printf 'username=native-profile\\npassword=native-fixture-password\\n'\nfi\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	gh := filepath.Join(root, "gh")
	if err := os.WriteFile(gh, []byte("#!/bin/sh\nexit 4\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("GIT_CONFIG_GLOBAL", globalConfig)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.preloadindex")
	t.Setenv("GIT_CONFIG_VALUE_0", "false")
	t.Setenv("SSH_AUTH_SOCK", filepath.Join(root, "native-agent.sock"))
	t.Setenv("GH_TELEMETRY", "true")
	t.Setenv("GIT_TERMINAL_PROMPT", "1")
	// Configure only the disposable HOME/profile. The helper contains no real
	// credential; its fixed response proves which profile Git actually calls.
	configure := exec.Command("git", "config", "--file", globalConfig, "credential.helper", "!'"+strings.ReplaceAll(profile, "'", "'\\''")+"'")
	if output, err := configure.CombinedOutput(); err != nil {
		t.Fatalf("configure fixture Git profile: %v %s", err, output)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	state, mount, socket := filepath.Join(root, "state"), filepath.Join(root, "mount"), filepath.Join(root, "service.sock")
	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- Run(ctx, []string{"desktop", "serve", "--gh", gh, "--state-dir", state, "--socket", socket, "--mount-root", mount}, &stdout, &stderr)
	}()
	defer func() {
		cancel()
		select {
		case code := <-done:
			if code != 0 {
				t.Errorf("desktop service exited with %d: %s", code, stderr.Bytes())
			}
		case <-time.After(10 * time.Second):
			t.Error("desktop service did not stop")
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, status, err := desktop.Request(ctx, socket, "GET", "/v1/status", nil); err == nil && status == 200 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("desktop service did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, host := range []string{"github.com", "git.example.invalid"} {
		credential := exec.Command("git", "credential", "fill")
		credential.Stdin = strings.NewReader("protocol=https\nhost=" + host + "\n\n")
		output, err := credential.Output()
		if err != nil || !bytes.Contains(output, []byte("username=native-profile\n")) || !bytes.Contains(output, []byte("password=native-fixture-password\n")) {
			t.Fatalf("native Git profile for %s was replaced: %v", host, err)
		}
	}
	if os.Getenv("GIT_CONFIG_COUNT") != "1" || os.Getenv("GIT_CONFIG_KEY_0") != "core.preloadindex" || os.Getenv("GIT_CONFIG_VALUE_0") != "false" || os.Getenv("SSH_AUTH_SOCK") != filepath.Join(root, "native-agent.sock") {
		t.Fatal("running service replaced native Git settings or SSH agent")
	}
	if os.Getenv("GIT_TERMINAL_PROMPT") != "0" || os.Getenv("GH_TELEMETRY") != "false" {
		t.Fatal("running service did not disable terminal prompts and telemetry")
	}
}
