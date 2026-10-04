//go:build !windows

package main

import (
	"context"
	"os"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/cli"
)

func TestMain(m *testing.M) {
	// The daemon installs its Git fsmonitor hook using os.Executable. During
	// integration tests that executable is this test binary, so handle the hook
	// through the real CLI rather than accidentally starting another test suite.
	if len(os.Args) > 1 && os.Args[1] == "fsmonitor-hook" {
		os.Exit(cli.Run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}
