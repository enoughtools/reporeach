package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/catalogfs"
	"github.com/cloudflare/artifact-fs/internal/fusefs"
)

func TestRecoverMountHealthyOwnerIsNoOp(t *testing.T) {
	for _, recovered := range []bool{false, true} {
		name := "existing owner"
		if recovered {
			name = "retry after lost recovery response"
		}
		t.Run(name, func(t *testing.T) {
			s := newDesktopTestService(t, "exit 4")
			seedDesktopCatalogue(t, s, catalogueRepository("octocat", "repo"))
			s.dependencyReady = func() bool { return true }
			mount := newDesktopFakeMount()
			mountCalls, recoveryCalls := 0, 0
			s.mountCatalogue = func(context.Context, string, *catalogfs.FileSystem) (fusefs.MountedFS, error) {
				mountCalls++
				return mount, nil
			}
			s.recoverMountCatalogue = func(context.Context) error {
				recoveryCalls++
				return nil
			}
			var err error
			if recovered {
				err = s.RecoverMount(context.Background())
			} else {
				err = s.Mount(context.Background())
			}
			if err != nil {
				t.Fatal(err)
			}
			before := s.Status()
			statePath := filepath.Join(s.opts.StateDir, "catalogue.json")
			beforeState, err := readState(statePath, s.opts.MountRoot)
			if err != nil {
				t.Fatal(err)
			}
			for request := 0; request < 3; request++ {
				if err := s.RecoverMount(context.Background()); err != nil {
					t.Fatalf("healthy recovery retry %d: %v", request, err)
				}
			}
			wantRecoveryCalls := 0
			if recovered {
				wantRecoveryCalls = 1
			}
			mount.mu.Lock()
			unmountCalls := mount.unmountCalls
			mount.mu.Unlock()
			s.mu.Lock()
			owner, maintenance := s.mounted, s.maintenance
			s.mu.Unlock()
			if owner != mount || mountCalls != 1 || recoveryCalls != wantRecoveryCalls || unmountCalls != 0 || maintenance {
				t.Fatalf("retry changed a healthy owner: sameOwner=%v mount=%d recovery=%d unmount=%d maintenance=%v", owner == mount, mountCalls, recoveryCalls, unmountCalls, maintenance)
			}
			if after := s.Status(); !reflect.DeepEqual(after, before) {
				t.Fatalf("healthy retry changed status: before=%+v after=%+v", before, after)
			}
			afterState, err := readState(statePath, s.opts.MountRoot)
			if err != nil || !reflect.DeepEqual(afterState, beforeState) {
				t.Fatalf("healthy retry changed persisted intent: before=%+v after=%+v error=%v", beforeState, afterState, err)
			}
		})
	}
}

func TestRecoverMountRejectsRunningRepositoryOperation(t *testing.T) {
	s := newDesktopTestService(t, "exit 4")
	s.dependencyReady = func() bool { return true }
	s.mu.Lock()
	s.ops = []Operation{{ID: "download", RepositoryID: "octocat/repo", Action: "keep", Status: "running"}}
	s.mu.Unlock()
	recoveryCalls, mountCalls := 0, 0
	s.recoverMountCatalogue = func(context.Context) error { recoveryCalls++; return nil }
	s.mountCatalogue = func(context.Context, string, *catalogfs.FileSystem) (fusefs.MountedFS, error) {
		mountCalls++
		return nil, errors.New("unexpected mount")
	}
	before := s.Status()
	err := s.RecoverMount(context.Background())
	if err == nil || !strings.Contains(err.Error(), "finish repository operations") {
		t.Fatalf("running operation recovery error = %v", err)
	}
	s.mu.Lock()
	maintenance := s.maintenance
	s.mu.Unlock()
	if maintenance || recoveryCalls != 0 || mountCalls != 0 || !reflect.DeepEqual(s.Status(), before) {
		t.Fatalf("rejected recovery changed service: maintenance=%v recovery=%d mount=%d status=%+v", maintenance, recoveryCalls, mountCalls, s.Status())
	}
}

func TestRecoverMountHTTPReturnsFullStatusAndAcceptsRetry(t *testing.T) {
	s := newDesktopTestService(t, "exit 4")
	seedDesktopCatalogue(t, s, catalogueRepository("octocat", "repo"), catalogueRepository("team", "other"))
	s.dependencyReady = func() bool { return true }
	s.mu.Lock()
	s.state.Account = &Account{Login: "octocat", AvatarURL: "https://example.invalid/avatar.png"}
	s.ops = []Operation{{ID: "previous", RepositoryID: "octocat/repo", Action: "keep", Status: "complete", DownloadedBytes: 512}}
	s.mountRecoveryAvailable = true
	s.mu.Unlock()
	mount := newDesktopFakeMount()
	recoveryCalls, mountCalls := 0, 0
	s.recoverMountCatalogue = func(context.Context) error { recoveryCalls++; return nil }
	s.mountCatalogue = func(context.Context, string, *catalogfs.FileSystem) (fusefs.MountedFS, error) {
		mountCalls++
		return mount, nil
	}
	for request := 0; request < 2; request++ {
		response := httptest.NewRecorder()
		s.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/mount/recover", nil))
		if response.Code != http.StatusOK {
			t.Fatalf("recovery response = %d, %s", response.Code, response.Body.String())
		}
		var status Status
		if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
			t.Fatal(err)
		}
		if want := s.Status(); !reflect.DeepEqual(status, want) || !status.Mounted || status.MountRecoveryAvailable || len(status.Repositories) != 2 || len(status.Organizations) != 2 || len(status.Operations) != 1 || status.Account == nil {
			t.Fatalf("recovery did not return the complete current status: got=%+v want=%+v", status, want)
		}
	}
	mount.mu.Lock()
	unmountCalls := mount.unmountCalls
	mount.mu.Unlock()
	if recoveryCalls != 1 || mountCalls != 1 || unmountCalls != 0 {
		t.Fatalf("HTTP retry replaced a healthy session: recovery=%d mount=%d unmount=%d", recoveryCalls, mountCalls, unmountCalls)
	}
}

func TestRecoverMountHTTPMarksOnlyRecoveryErrors(t *testing.T) {
	for _, path := range []string{"/v1/mount/recover", "/v1/mount"} {
		t.Run(path, func(t *testing.T) {
			s := newDesktopTestService(t, "exit 4")
			s.dependencyReady = func() bool { return true }
			backendErr := errors.New("fixture backend refuses the change")
			s.recoverMountCatalogue = func(context.Context) error { return backendErr }
			s.mountCatalogue = func(context.Context, string, *catalogfs.FileSystem) (fusefs.MountedFS, error) {
				return nil, backendErr
			}
			response := httptest.NewRecorder()
			s.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, path, nil))
			if response.Code != http.StatusBadRequest {
				t.Fatalf("backend failure response = %d, %s", response.Code, response.Body.String())
			}
			var body map[string]json.RawMessage
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			var message string
			if err := json.Unmarshal(body["error"], &message); err != nil || !strings.Contains(message, backendErr.Error()) {
				t.Fatalf("backend error missing from response: %s, error=%v", response.Body.String(), err)
			}
			marker, marked := body["recoveryError"]
			if path == "/v1/mount/recover" {
				if !marked || string(marker) != "true" {
					t.Fatalf("recovery failure was not marked: %s", response.Body.String())
				}
			} else if marked {
				t.Fatalf("ordinary mount failure was marked as recovery: %s", response.Body.String())
			}
		})
	}
}
