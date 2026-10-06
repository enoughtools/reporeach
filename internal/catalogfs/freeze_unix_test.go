//go:build !windows

package catalogfs

import (
	"context"
	"errors"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/jacobsa/fuse/fuseops"
)

func freezeFixture(t *testing.T) *FileSystem {
	t.Helper()
	fs, err := New(testEntries, func(context.Context, Entry) (*fusefs.ArtifactFuse, error) {
		return nil, errors.New("freeze must not activate a repository")
	})
	if err != nil {
		t.Fatal(err)
	}
	return fs
}

type freezeResult struct {
	release func()
	err     error
}

func receiveFreeze(t *testing.T, result <-chan freezeResult) freezeResult {
	t.Helper()
	select {
	case value := <-result:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("repository write admission did not complete")
		return freezeResult{}
	}
}

func TestFreezeRepositoryWritesDrainsMutationAndPreservesReads(t *testing.T) {
	fs := freezeFixture(t)
	repo := fs.repositories[testEntries[0].ID]
	finishMutation, err := fs.beginMutation(repo)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan freezeResult, 1)
	go func() {
		release, err := fs.FreezeRepositoryWrites(testEntries[0].ID)
		result <- freezeResult{release, err}
	}()
	select {
	case value := <-result:
		finishMutation()
		if value.release != nil {
			value.release()
		}
		t.Fatalf("freeze completed during an active mutation: %v", value.err)
	default:
	}
	finishMutation()
	claim := receiveFreeze(t, result)
	if claim.err != nil {
		t.Fatal(claim.err)
	}
	defer claim.release()
	if release, err := fs.beginMutation(repo); err != syscall.EBUSY || release != nil {
		if release != nil {
			release()
		}
		t.Fatalf("frozen mutation admission: %v", err)
	}
	otherRelease, err := fs.beginMutation(fs.repositories[testEntries[1].ID])
	if err != nil {
		t.Fatalf("unrelated repository mutation: %v", err)
	}
	otherRelease()
	syntheticRelease, err := fs.beginMutation(nil)
	if err != nil {
		t.Fatalf("synthetic metadata mutation: %v", err)
	}
	syntheticRelease()
	root := repoRoot(t, fs, "alice")
	attrs := &fuseops.GetInodeAttributesOp{Inode: root}
	if err := fs.GetInodeAttributes(context.Background(), attrs); err != nil {
		t.Fatalf("frozen repository remains readable: %v", err)
	}
	claim.release()
	finishMutation, err = fs.beginMutation(repo)
	if err != nil {
		t.Fatalf("released repository mutation: %v", err)
	}
	finishMutation()
}

func TestFrozenRepositoryMutationsRejectWithoutWaiting(t *testing.T) {
	fs := freezeFixture(t)
	claim, err := fs.FreezeRepositoryWrites(testEntries[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	defer claim()
	repo := fs.repositories[testEntries[0].ID]
	const count = 32
	result := make(chan freezeResult, count)
	for range count {
		go func() {
			release, err := fs.beginMutation(repo)
			result <- freezeResult{release, err}
		}()
	}
	for range count {
		value := receiveFreeze(t, result)
		if value.release != nil {
			value.release()
		}
		if value.err != syscall.EBUSY || value.release != nil {
			t.Fatalf("frozen mutation should reject before release: %v", value.err)
		}
	}
}

func TestConcurrentRepositoryFreezersHaveOneExclusiveClaim(t *testing.T) {
	fs := freezeFixture(t)
	const count = 32
	start := make(chan struct{})
	result := make(chan freezeResult, count)
	for range count {
		go func() {
			<-start
			release, err := fs.FreezeRepositoryWrites(testEntries[0].ID)
			result <- freezeResult{release, err}
		}()
	}
	close(start)
	var claims []func()
	for range count {
		value := receiveFreeze(t, result)
		if value.err == nil {
			claims = append(claims, value.release)
		} else if value.err != syscall.EBUSY || value.release != nil {
			t.Errorf("competing freeze: %v", value.err)
		}
	}
	for _, release := range claims {
		defer release()
	}
	if len(claims) != 1 || claims[0] == nil {
		t.Fatalf("exclusive claims: %d", len(claims))
	}
	var releases sync.WaitGroup
	for range count {
		releases.Add(1)
		go func() {
			defer releases.Done()
			claims[0]()
		}()
	}
	releases.Wait()
	next, err := fs.FreezeRepositoryWrites(testEntries[0].ID)
	if err != nil {
		t.Fatalf("new freeze after concurrent release: %v", err)
	}
	defer next()
	claims[0]()
	if release, err := fs.beginMutation(fs.repositories[testEntries[0].ID]); err != syscall.EBUSY || release != nil {
		if release != nil {
			release()
		}
		t.Fatalf("old release removed a newer claim: %v", err)
	}
}

func TestRepositoryWriteFreezeFollowsRetainedID(t *testing.T) {
	fs := freezeFixture(t)
	id := testEntries[0].ID
	repo := fs.repositories[id]
	if err := fs.SetEntries(nil); err != nil {
		t.Fatal(err)
	}
	claim, err := fs.FreezeRepositoryWrites(id)
	if err != nil {
		t.Fatalf("retained identity: %v", err)
	}
	defer claim()
	if release, err := fs.beginMutation(repo); err != syscall.EBUSY || release != nil {
		if release != nil {
			release()
		}
		t.Fatalf("hidden repository's retained handle admission: %v", err)
	}
	entries := []Entry{{ID: id, Owner: "renamed", Name: "project"}, {ID: "replacement", Owner: "alice", Name: "project"}}
	if err := fs.SetEntries(entries); err != nil {
		t.Fatal(err)
	}
	if fs.repositories[id] != repo {
		t.Fatal("catalogue refresh replaced the frozen repository identity")
	}
	if release, err := fs.beginMutation(fs.repositories[id]); err != syscall.EBUSY || release != nil {
		if release != nil {
			release()
		}
		t.Fatalf("renamed frozen identity: %v", err)
	}
	release, err := fs.beginMutation(fs.repositories["replacement"])
	if err != nil {
		t.Fatalf("replacement at the original path inherited freeze: %v", err)
	}
	release()
}

func TestRepositoryWriteFreezeRejectsUnknownAndDetachedIdentities(t *testing.T) {
	fs := freezeFixture(t)
	if release, err := fs.FreezeRepositoryWrites("unknown"); err != syscall.ENOENT || release != nil {
		if release != nil {
			release()
		}
		t.Fatalf("unknown identity: %v", err)
	}
	repo := fs.repositories[testEntries[0].ID]
	fs.Destroy()
	if release, err := fs.FreezeRepositoryWrites(testEntries[0].ID); err != syscall.ESTALE || release != nil {
		if release != nil {
			release()
		}
		t.Fatalf("detached identity: %v", err)
	}
	for _, owner := range []*repository{repo, nil} {
		if release, err := fs.beginMutation(owner); err != syscall.ESTALE || release != nil {
			if release != nil {
				release()
			}
			t.Fatalf("detached mutation admission: %v", err)
		}
	}
}

func TestRepositoryWriteFreezeDoesNotPublishAfterDetachDuringDrain(t *testing.T) {
	fs := freezeFixture(t)
	finishMutation, err := fs.beginMutation(fs.repositories[testEntries[0].ID])
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan freezeResult, 1)
	go func() {
		release, err := fs.FreezeRepositoryWrites(testEntries[0].ID)
		result <- freezeResult{release, err}
	}()
	fs.Destroy()
	finishMutation()
	value := receiveFreeze(t, result)
	if value.release != nil {
		value.release()
	}
	if value.err != syscall.ESTALE || value.release != nil {
		t.Fatalf("freeze after detach: %v", value.err)
	}
}
