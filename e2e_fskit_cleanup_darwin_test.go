//go:build darwin

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestFSKitCleanupInventory(t *testing.T) {
	const uid = uint32(501)
	captured := fsKitAcceptanceIdentity{fsid: [2]int32{123, 456}, owner: uid, root: "/private/tmp/rr-fskit-fixture/mount", source: "file:///private/tmp/rr-fskit-fixture/state/FSKit/", kind: "reporeach"}
	unrelated := fsKitAcceptanceIdentity{fsid: [2]int32{777, 888}, owner: uid, root: "/unrelated", source: "/dev/disk-other", kind: "apfs"}
	for _, test := range []struct {
		name       string
		capture    fsKitAcceptanceIdentity
		inventory  []fsKitAcceptanceIdentity
		change     func(*fsKitAcceptanceIdentity)
		owned, bad bool
	}{
		{name: "same complete tuple", capture: captured, inventory: []fsKitAcceptanceIdentity{unrelated, captured}, owned: true},
		{name: "fully detached", capture: captured, inventory: []fsKitAcceptanceIdentity{unrelated}},
		{name: "duplicate captured tuple", capture: captured, inventory: []fsKitAcceptanceIdentity{captured, captured}, bad: true},
		{name: "same FSID moved elsewhere", capture: captured, change: func(m *fsKitAcceptanceIdentity) { m.root, m.source = "/elsewhere", "different-resource" }, bad: true},
		{name: "replacement FSID at same root", capture: captured, change: func(m *fsKitAcceptanceIdentity) { m.fsid[0]++ }, bad: true},
		{name: "source changed", capture: captured, change: func(m *fsKitAcceptanceIdentity) { m.source += "different" }, bad: true},
		{name: "equivalent source spelling changed", capture: captured, change: func(m *fsKitAcceptanceIdentity) { m.source = strings.TrimSuffix(m.source, "/") }, bad: true},
		{name: "owner changed", capture: captured, change: func(m *fsKitAcceptanceIdentity) { m.owner++ }, bad: true},
		{name: "kind changed", capture: captured, change: func(m *fsKitAcceptanceIdentity) { m.kind = "other" }, bad: true},
		{name: "child mount below fixture", capture: captured, change: func(m *fsKitAcceptanceIdentity) {
			m.fsid, m.root, m.source = [2]int32{999, 1000}, "/private/tmp/rr-fskit-fixture/child", "other"
		}, bad: true},
		{name: "source mounted elsewhere", capture: captured, change: func(m *fsKitAcceptanceIdentity) { m.fsid, m.root = [2]int32{999, 1000}, "/elsewhere" }, bad: true},
		{name: "uncaptured attachment", inventory: []fsKitAcceptanceIdentity{captured}, bad: true},
		{name: "uncaptured but fully absent", inventory: []fsKitAcceptanceIdentity{unrelated}},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := &fsKitAcceptanceHarness{root: "/private/tmp/rr-fskit-fixture", state: "/private/tmp/rr-fskit-fixture/state", mount: captured.root, capturedIdentity: test.capture}
			inventory := test.inventory
			if test.change != nil {
				changed := captured
				test.change(&changed)
				inventory = []fsKitAcceptanceIdentity{unrelated, changed}
			}
			owned, err := h.validateCleanupInventory(inventory, uid)
			if owned != test.owned || (err != nil) != test.bad {
				t.Fatalf("inventory owned=%v error=%v; want owned=%v bad=%v", owned, err, test.owned, test.bad)
			}
		})
	}
	for _, change := range []func(*fsKitAcceptanceIdentity){
		func(m *fsKitAcceptanceIdentity) { m.owner++ },
		func(m *fsKitAcceptanceIdentity) { m.root = "/other" },
		func(m *fsKitAcceptanceIdentity) { m.source = "different" },
		func(m *fsKitAcceptanceIdentity) { m.kind = "" },
	} {
		invalid := captured
		change(&invalid)
		h := &fsKitAcceptanceHarness{root: "/private/tmp/rr-fskit-fixture", state: "/private/tmp/rr-fskit-fixture/state", mount: captured.root, capturedIdentity: invalid}
		if _, err := h.validateCleanupInventory(nil, uid); err == nil {
			t.Fatal("invalid captured ownership accepted even with an empty inventory")
		}
	}
}

func TestFSKitCleanupRetry(t *testing.T) {
	for _, test := range []struct {
		name                   string
		successAt, uncertainAt int
		requests, checks       int
		wantError              bool
	}{
		{name: "first normal endpoint succeeds", successAt: 1, requests: 1, checks: 1},
		{name: "transient refusal retries", successAt: 2, requests: 2, checks: 2},
		{name: "third attempt succeeds", successAt: 3, requests: 3, checks: 3},
		{name: "bounded three refusals", requests: 3, checks: 3, wantError: true},
		{name: "uncertain before initial request", uncertainAt: 1, checks: 1, wantError: true},
		{name: "uncertain before retry", uncertainAt: 2, requests: 1, checks: 2, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			deadline, _ := ctx.Deadline()
			var calls []string
			var pauses []time.Duration
			checks, requests, refusals := 0, 0, 0
			err := fsKitPrepareQuitWithRetry(ctx, fsKitCleanupRetryOperations{
				verify: func() error {
					checks++
					calls = append(calls, "verify")
					if checks == test.uncertainAt {
						return errors.New("changed tuple or incomplete inventory")
					}
					return nil
				},
				request: func(received context.Context) ([]byte, int, error) {
					requests++
					calls = append(calls, "request")
					got, ok := received.Deadline()
					if !ok || got != deadline || received != ctx {
						t.Fatal("retry replaced the shared deadline/context")
					}
					if requests == test.successAt {
						return []byte(`{"mounted":false}`), http.StatusOK, nil
					}
					return []byte(`{"error":"ordinary unmount refused"}`), http.StatusBadRequest, nil
				},
				pause: func(received context.Context, delay time.Duration) error {
					calls = append(calls, "pause")
					pauses = append(pauses, delay)
					if received != ctx {
						t.Fatal("backoff replaced the shared context")
					}
					return nil
				},
				refusal: func(attempt, code int, err error, body string) {
					refusals++
					calls = append(calls, "refusal")
					if attempt != requests || code != http.StatusBadRequest || err != nil || !json.Valid([]byte(body)) {
						t.Fatal("ordinary refusal diagnostic was lost")
					}
				},
			})
			if (err != nil) != test.wantError || checks != test.checks || requests != test.requests {
				t.Fatalf("retry result error=%v requests=%d checks=%d; want error=%v requests=%d checks=%d", err, requests, checks, test.wantError, test.requests, test.checks)
			}
			var total time.Duration
			for _, delay := range pauses {
				total += delay
			}
			wantRefusals := requests
			if test.successAt > 0 && test.successAt <= requests {
				wantRefusals--
			}
			if total > 2*time.Second || refusals != wantRefusals {
				t.Fatalf("unbounded backoff or omitted refusal: pauses=%v refusals=%d", pauses, refusals)
			}
			for index, call := range calls {
				if call == "request" && (index == 0 || calls[index-1] != "verify") {
					t.Fatalf("endpoint attempt preceded ownership verification: %v", calls)
				}
			}
		})
	}

	t.Run("known detachment continues endpoint drain", func(t *testing.T) {
		captured := fsKitAcceptanceIdentity{fsid: [2]int32{123, 456}, owner: 501, root: "/private/tmp/rr-fskit-fixture/mount", source: "file:///private/tmp/rr-fskit-fixture/state/FSKit/", kind: "reporeach"}
		h := &fsKitAcceptanceHarness{root: "/private/tmp/rr-fskit-fixture", state: "/private/tmp/rr-fskit-fixture/state", mount: captured.root, capturedIdentity: captured}
		checks, requests := 0, 0
		err := fsKitPrepareQuitWithRetry(context.Background(), fsKitCleanupRetryOperations{
			verify: func() error {
				checks++
				var inventory []fsKitAcceptanceIdentity
				if checks == 1 {
					inventory = []fsKitAcceptanceIdentity{captured}
				}
				_, err := h.validateCleanupInventory(inventory, 501)
				return err
			},
			request: func(context.Context) ([]byte, int, error) {
				requests++
				if requests == 1 {
					return []byte(`{"error":"drain repository folder: still draining"}`), http.StatusBadRequest, nil
				}
				return []byte(`{"mounted":false}`), http.StatusOK, nil
			},
			pause:   func(context.Context, time.Duration) error { return nil },
			refusal: func(int, int, error, string) {},
		})
		if err != nil || checks != 2 || requests != 2 {
			t.Fatalf("fully detached session could not finish ordinary endpoint drain: %v checks=%d requests=%d", err, checks, requests)
		}
	})
}

func TestFSKitCleanupDeadline(t *testing.T) {
	for _, stage := range []string{"before verification", "during verification", "during request", "during backoff"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if stage == "before verification" {
				cancel()
			}
			checks, requests, pauses := 0, 0, 0
			err := fsKitPrepareQuitWithRetry(ctx, fsKitCleanupRetryOperations{
				verify: func() error {
					checks++
					if stage == "during verification" {
						cancel()
					}
					return nil
				},
				request: func(context.Context) ([]byte, int, error) {
					requests++
					if stage == "during request" {
						cancel()
					}
					return []byte(`{"error":"refused"}`), http.StatusBadRequest, nil
				},
				pause: func(context.Context, time.Duration) error {
					pauses++
					cancel()
					return ctx.Err()
				},
				refusal: func(int, int, error, string) {},
			})
			if !errors.Is(err, context.Canceled) || checks > 1 || requests > 1 || pauses > 1 {
				t.Fatalf("expired shared context permitted more work: error=%v checks=%d requests=%d pauses=%d", err, checks, requests, pauses)
			}
			if stage == "before verification" && checks != 0 || (stage == "before verification" || stage == "during verification") && requests != 0 {
				t.Fatal("request or verification started after deadline cancellation")
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := fsKitCleanupPause(ctx, 2*time.Second); !errors.Is(err, context.Canceled) || time.Since(start) > time.Second {
		t.Fatalf("real backoff did not honor cancellation promptly: %v", err)
	}
}

func TestFSKitCleanupDiagnostics(t *testing.T) {
	ordinary := `{"error":"unmount repository folder: still mounted"}`
	if got := fsKitCleanupRefusalDiagnostic([]byte(ordinary)); got != ordinary {
		t.Fatal("ordinary JSON refusal was not retained")
	}
	credential := `{"error":"request https://synthetic-user:synthetic-password@example.invalid/private?token=synthetic-token"}`
	got := fsKitCleanupRefusalDiagnostic([]byte(credential))
	if !json.Valid([]byte(got)) || strings.Contains(got, "synthetic-password") || strings.Contains(got, "synthetic-token") {
		t.Fatal("refusal JSON did not retain safe bounded credential redaction")
	}
	for _, body := range [][]byte{nil, []byte("not JSON"), []byte(strings.Repeat(" ", 4097))} {
		if got := fsKitCleanupRefusalDiagnostic(body); len(got) > 4096 || !strings.HasPrefix(got, "<") {
			t.Fatal("unbounded or malformed refusal data was printed")
		}
	}
	if got := fsKitCleanupErrorDiagnostic(errors.New("https://synthetic-user:synthetic-password@example.invalid/private")); strings.Contains(got, "synthetic-password") {
		t.Fatal("transport error exposed synthetic credential")
	}
	if got := fsKitCleanupErrorDiagnostic(errors.New(strings.Repeat("x", 5000))); got != "<oversized error omitted>" {
		t.Fatal("oversized transport error was not omitted")
	}
}
