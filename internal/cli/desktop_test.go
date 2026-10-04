package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
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

func TestDesktopServeRestoresProcessCredentialEnvironment(t *testing.T) {
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
