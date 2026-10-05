//go:build !windows

package gitstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/model"
)

func TestRemoteLookupCommandIsReapedBeforeCleanupReturns(t *testing.T) {
	// A single scheduler thread exercises cancellation before the command's
	// context watcher can run independently. Cleanup must itself wait for the
	// lookup, rather than relying on that goroutine winning a scheduling race.
	previous := runtime.GOMAXPROCS(1)
	t.Cleanup(func() { runtime.GOMAXPROCS(previous) })
	for _, action := range []string{"cancel lookup", "fetch success", "fetch failure", "fetch ref"} {
		t.Run(action, func(t *testing.T) {
			tmp := t.TempDir()
			bin := filepath.Join(tmp, "bin")
			if err := os.Mkdir(bin, 0o755); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(tmp, "lookup-pid")
			script := "#!/bin/sh\n" +
				"case \"$*\" in\n" +
				"  'remote get-url origin') printf '%s\\n' \"$$\" > \"$AFS_LOOKUP_PID.tmp\"; mv \"$AFS_LOOKUP_PID.tmp\" \"$AFS_LOOKUP_PID\"; exec sleep 10;;\n" +
				"  fetch*) while [ ! -f \"$AFS_LOOKUP_PID\" ]; do sleep 0.001; done; if [ \"$AFS_FETCH_FAILURE\" = 1 ]; then printf '%s\\n' 'fatal: fixture transport failure' >&2; exit 1; fi;;\n" +
				"esac\n"
			if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("AFS_LOOKUP_PID", marker)
			failure := "0"
			if action == "fetch failure" {
				failure = "1"
			}
			t.Setenv("AFS_FETCH_FAILURE", failure)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			store := New(nil)
			defer store.Close()
			repo := model.RepoConfig{Name: "fixture", GitDir: filepath.Join(tmp, "repo.git"), RemoteURL: "https://example.invalid/owner/repo.git", Branch: "main"}
			started := time.Now()
			var lookupPID int
			if action == "cancel lookup" {
				_, cleanup := store.startRemotesForLogging(ctx, repo)
				defer cleanup()
				lookupPID = waitForRemoteLookupPID(t, marker)
				cleanup()
			} else {
				var err error
				if action == "fetch ref" {
					err = store.FetchRefWithCredentials(ctx, repo, "main")
				} else {
					err = store.Fetch(ctx, repo)
				}
				if (err != nil) != (action == "fetch failure") || errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("%s result = %v", action, err)
				}
				lookupPID = waitForRemoteLookupPID(t, marker)
			}
			if elapsed := time.Since(started); elapsed > time.Second {
				t.Fatalf("lookup cleanup delayed completed operation by %v", elapsed)
			}
			if err := syscall.Kill(lookupPID, 0); !errors.Is(err, syscall.ESRCH) {
				// Own fixture cleanup even when testing the former broken path.
				_ = syscall.Kill(lookupPID, syscall.SIGKILL)
				t.Fatalf("lookup process %d still exists after %s returned: %v", lookupPID, action, err)
			}
		})
	}
}

func waitForRemoteLookupPID(t *testing.T, marker string) int {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		data, err := os.ReadFile(marker)
		if err == nil {
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil || pid <= 0 {
				t.Fatalf("lookup pid = %q, %v", data, err)
			}
			return pid
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("lookup command did not start")
		}
		time.Sleep(time.Millisecond)
	}
}
