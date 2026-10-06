package gitstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDiscardUnreachableObjectsRequiresFreshRemoteProof(t *testing.T) {
	const object = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const remote = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	for _, test := range []struct {
		name        string
		fsck, proof string
		accepted    bool
	}{
		{"no-unreachable", "", "", true},
		{"published-ancestor", "unreachable commit " + object + "\n", remote + "\n" + object + "\n", true},
		{"local-blob", "unreachable blob " + object + "\n", remote + "\n", false},
		{"local-commit", "unreachable commit " + object + "\n", remote + "\n", false},
		{"local-tree", "unreachable tree " + object + "\n", remote + "\n", false},
		{"local-tag", "unreachable tag " + object + "\n", remote + "\n", false},
		{"malformed-fsck", "unreachable commit HEAD\n", object + "\n", false},
		{"unknown-type", "unreachable invented " + object + "\n", object + "\n", false},
		{"extra-fsck-fields", "unreachable commit " + object + " ignored\n", object + "\n", false},
		{"malformed-proof", "unreachable commit " + object + "\n", object + " path\n", false},
		{"incomplete-proof", "unreachable commit " + object + "\n", "?" + object + "\n", false},
		{"oversized-record", strings.Repeat("a", 2048) + "\n", "", false},
		{"too-many-candidates", "", "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			bin := t.TempDir()
			fsckFile, proofFile := filepath.Join(bin, "fsck"), filepath.Join(bin, "proof")
			fsck := test.fsck
			if test.name == "too-many-candidates" {
				var output strings.Builder
				for i := 1; i <= maxDiscardUnreachableObjects+1; i++ {
					fmt.Fprintf(&output, "unreachable blob %040x\n", i)
				}
				fsck = output.String()
			}
			if err := os.WriteFile(fsckFile, []byte(fsck), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(proofFile, []byte(test.proof), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(bin, "git"), []byte(`#!/bin/sh
if [ "$GIT_NO_LAZY_FETCH" != 1 ]; then exit 91; fi
case "$1" in
fsck) exec /bin/cat "$DISCARD_TEST_FSCK" ;;
rev-list) exec /bin/cat "$DISCARD_TEST_PROOF" ;;
esac
exit 92
`), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("GIT_NO_LAZY_FETCH", "0")
			env := []string{"GIT_NO_LAZY_FETCH=1", "DISCARD_TEST_FSCK=" + fsckFile, "DISCARD_TEST_PROOF=" + proofFile}
			err := verifyUnreachableObjects(context.Background(), bin, env, []string{remote})
			if (err == nil) != test.accepted {
				t.Fatalf("accepted=%v; want %v, error=%v", err == nil, test.accepted, err)
			}
		})
	}
}

func TestDiscardMetadataLinesCancellationAndErrorRedaction(t *testing.T) {
	for _, mode := range []string{"cancellation", "redaction"} {
		t.Run(mode, func(t *testing.T) {
			bin := t.TempDir()
			script := "#!/bin/sh\nexec sleep 20\n"
			if mode == "redaction" {
				script = "#!/bin/sh\necho 'https://user:discard-secret@example.invalid/repo' >&2\nexit 97\n"
			}
			if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
			defer cancel()
			started := time.Now()
			err := discardMetadataLines(ctx, bin, []string{"GIT_NO_LAZY_FETCH=1"}, func(string) error { return nil }, "fsck")
			if err == nil {
				t.Fatal("failed metadata command was accepted")
			}
			if mode == "redaction" && strings.Contains(err.Error(), "discard-secret") {
				t.Fatalf("metadata error exposed credentials: %v", err)
			}
			if mode == "cancellation" && (!errors.Is(err, context.DeadlineExceeded) || time.Since(started) > 3*time.Second) {
				t.Fatalf("metadata cancellation lost the deadline or failed to stop: %v", err)
			}
		})
	}
}

func TestDiscardRemoteSelectionRejectsOlderGitMismatchWithoutURLDisclosure(t *testing.T) {
	for _, name := range []string{"matching", "mismatch"} {
		t.Run(name, func(t *testing.T) {
			bin := t.TempDir()
			if err := os.WriteFile(filepath.Join(bin, "git"), []byte(`#!/bin/sh
# Simulate an older Git selecting its existing first origin URL despite reset.
if [ "$1" != ls-remote ] || [ "$2" != --get-url ]; then exit 92; fi
case "$3" in
origin) printf '%s\n' "$DISCARD_TEST_SELECTED" ;;
https://requested.example.invalid/requested.git) printf '%s\n' "$DISCARD_TEST_EXPECTED" ;;
*) exit 93 ;;
esac
`), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			selected := "https://expanded.example.invalid/requested.git"
			if name == "mismatch" {
				selected = "https://native:old-origin-secret@wrong.example.invalid/origin.git"
			}
			env := []string{"DISCARD_TEST_SELECTED=" + selected, "DISCARD_TEST_EXPECTED=https://expanded.example.invalid/requested.git"}
			err := verifyDiscardRemoteSelection(context.Background(), bin, env, "https://requested.example.invalid/requested.git")
			if name == "matching" && err != nil {
				t.Fatalf("matching older Git origin was refused: %v", err)
			}
			if name == "mismatch" && (err == nil || strings.Contains(err.Error(), "old-origin-secret") || strings.Contains(err.Error(), "wrong.example.invalid")) {
				t.Fatalf("wrong source was accepted or its native URL disclosed: %v", err)
			}
		})
	}
}
