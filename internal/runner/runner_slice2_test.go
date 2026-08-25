package runner

import (
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
	"testing"

	"global-build/internal/gitx"
	"global-build/internal/ownership"
)

// leasePath returns the liveness-lease pathname the runner writes for a run.
func leasePath(t *testing.T, e *testEnv, runID string) string {
	t.Helper()
	commonDir, err := gitx.CanonicalCommonDir(context.Background(), e.repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	repoID := gitx.RepoID(commonDir)
	// The runner canonicalizes the cache root (resolveCacheRoot), so mirror it.
	return ownership.NewLease(gitx.CanonicalPath(e.cacheRoot), repoID, runID).Path()
}

func assertLeaseGone(t *testing.T, e *testEnv, runID string) {
	t.Helper()
	if _, err := os.Lstat(leasePath(t, e, runID)); err == nil {
		t.Errorf("lease pathname %s still present after run", leasePath(t, e, runID))
	}
}

// TestOwnershipMarkerAndLeaseLifecycle (proofs A + P): the disposable worktree is
// created detached, locked, with a parseable Slice-2 ownership marker, and the
// run lease is held for the whole attempt (blocks other acquirers) and released
// only after the worktree is removed.
func TestOwnershipMarkerAndLeaseLifecycle(t *testing.T) {
	e := setupFake(t, "complete")
	runID := "run-marker-1"
	ctx := context.Background()

	commonDir, err := gitx.CanonicalCommonDir(ctx, e.repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	repoID := gitx.RepoID(commonDir)
	rec := &records{
		runID:       runID,
		commonDir:   commonDir,
		repoRoot:    e.repoRoot,
		worktree:    gitx.CanonicalPath(e.worktreePath(t, runID)),
		admittedOID: e.admittedOID,
		hashLen:     40,
	}
	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	cfg := Config{CacheRoot: e.cacheRoot}

	if rerr := establishLeaseAndWorktree(ctx, cfg, rec, logger); rerr != nil {
		t.Fatalf("establish failed: %v", rerr)
	}

	// The worktree must carry the durable ownership marker.
	entries, err := gitx.WorktreeListPorcelainZ(ctx, e.repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	var ent *gitx.WorktreeEntry
	for i := range entries {
		if entries[i].Path == rec.worktree {
			ent = &entries[i]
			break
		}
	}
	if ent == nil {
		t.Fatal("worktree not registered")
	}
	if !ent.Detached {
		t.Error("worktree not detached")
	}
	if !ent.Locked {
		t.Error("worktree not locked")
	}
	wantReason := ownership.BuildReason(repoID, runID, e.admittedOID)
	if ent.LockReason != wantReason {
		t.Errorf("lock reason = %q, want %q", ent.LockReason, wantReason)
	}

	// The run lease must be held (liveness) and block other acquirers.
	observer := ownership.NewLease(e.cacheRoot, repoID, runID)
	acquired, oerr := observer.TryAcquireExisting()
	if oerr != nil {
		t.Fatalf("observer error: %v", oerr)
	}
	if acquired {
		t.Error("lease must be held by the runner while the attempt is active")
	}

	// Bounded, matching lease identity.
	id, ok, ierr := rec.lease.ReadIdentity()
	if ierr != nil || !ok {
		t.Fatalf("read identity: ok=%v err=%v", ok, ierr)
	}
	if id.Version != ownership.LeaseVersion || id.RepoID != repoID || id.RunID != runID || id.AdmittedBase != e.admittedOID {
		t.Errorf("lease identity mismatch: %+v", id)
	}

	// Normal removal contract must release the lease and remove the pathname.
	if rerr := removeOwnedWorktree(ctx, rec, e.admittedOID, logger); rerr != nil {
		t.Fatalf("removeOwnedWorktree failed: %v", rerr)
	}
	if _, err := os.Lstat(rec.worktree); err == nil {
		t.Error("worktree still present after removal")
	}
	if _, err := os.Lstat(leasePath(t, e, runID)); err == nil {
		t.Error("lease pathname not removed after run")
	}
}

// TestCompleteReleasesLease (proof A): COMPLETE removes its own worktree AND its
// lease pathname, and never touches unrelated worktrees.
func TestCompleteReleasesLease(t *testing.T) {
	e := setupFake(t, "complete")
	runID := "run-leasedone-1"

	// An unrelated worktree that must survive the build.
	unrelated := filepath.Join(t.TempDir(), "unrelated-wt")
	git(t, "-C", e.repoRoot, "worktree", "add", "--detach", unrelated, e.admittedOID)

	code, stdout, stderrOut := e.run(t, envelopeText(runID, e.admittedOID, "docs/", "src/main.go"), nil)
	if code != ExitComplete {
		t.Fatalf("exit = %d, want 0\nstdout:\n%s\nstderr:\n%s", code, stdout, stderrOut)
	}
	assertWorktreeGone(t, e.worktreePath(t, runID))
	assertLeaseGone(t, e, runID)
	if _, err := os.Lstat(unrelated); err != nil {
		t.Errorf("unrelated worktree was removed by the build: %v", err)
	}
}

// TestContinuableReleasesLease (proof B): CONTINUABLE still releases the lease.
func TestContinuableReleasesLease(t *testing.T) {
	e := setupFake(t, "continuable")
	runID := "run-leasecont-1"
	code, stdout, _ := e.run(t, envelopeText(runID, e.admittedOID, "docs/"), nil)
	if code != ExitContinuable {
		t.Fatalf("exit = %d, want 10\nstdout:\n%s", code, stdout)
	}
	assertWorktreeGone(t, e.worktreePath(t, runID))
	assertLeaseGone(t, e, runID)
}

// TestBlockedReleasesLease (proof B): BLOCKED still releases the lease.
func TestBlockedReleasesLease(t *testing.T) {
	e := setupFake(t, "blocked")
	runID := "run-leaseblock-1"
	code, stdout, _ := e.run(t, envelopeText(runID, e.admittedOID, "docs/"), nil)
	if code != ExitBlocked {
		t.Fatalf("exit = %d, want 20\nstdout:\n%s", code, stdout)
	}
	assertWorktreeGone(t, e.worktreePath(t, runID))
	assertLeaseGone(t, e, runID)
}
