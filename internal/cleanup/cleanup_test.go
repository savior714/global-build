package cleanup

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"global-build/internal/gitx"
	"global-build/internal/ownership"
)

// --- harness ----------------------------------------------------------------

func initRepo(t *testing.T) (repoRoot, admittedOID string) {
	t.Helper()
	repoRoot = filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repoRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, repoRoot, "init", "-q", "-b", "main", repoRoot)
	git(t, repoRoot, "config", "user.email", "gb-test@example.com")
	git(t, repoRoot, "config", "user.name", "gb-test")
	git(t, repoRoot, "config", "commit.gpgsign", "false")
	writeFile(t, filepath.Join(repoRoot, "docs", "notes.txt"), "base\n")
	git(t, repoRoot, "add", "-A")
	git(t, repoRoot, "commit", "-qm", "initial state")
	admittedOID = strings.TrimSpace(git(t, repoRoot, "rev-parse", "HEAD"))
	return repoRoot, admittedOID
}

func git(t *testing.T, repoRoot string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s in %s: %v", strings.Join(args, " "), repoRoot, err)
	}
	return string(out)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func repoIDOf(t *testing.T, repoRoot string) string {
	t.Helper()
	commonDir, err := gitx.CanonicalCommonDir(context.Background(), repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	return gitx.RepoID(commonDir)
}

func cacheRootOf(t *testing.T) string {
	t.Helper()
	return gitx.CanonicalPath(filepath.Join(t.TempDir(), "global-build"))
}

// createOrphan builds a detached, locked worktree in the global-build cache
// layout with an explicit ownership reason and a lease identity file carrying
// leaseBase as the admitted-base. The worktree is checked out at checkoutOID.
// It returns the worktree path; it does NOT hold the lease.
func createOrphan(t *testing.T, repoRoot, cacheRoot, repoID, runID, reason, checkoutOID, leaseBase string) string {
	t.Helper()
	wtParent := filepath.Join(cacheRoot, "worktrees", repoID)
	if err := os.MkdirAll(wtParent, 0o755); err != nil {
		t.Fatal(err)
	}
	wtPath := filepath.Join(wtParent, runID)
	git(t, repoRoot, "worktree", "add", "--detach", "--lock", "--reason", reason, wtPath, checkoutOID)

	lease := ownership.NewLease(cacheRoot, repoID, runID)
	if err := os.MkdirAll(filepath.Dir(lease.Path()), 0o755); err != nil {
		t.Fatal(err)
	}
	id := ownership.LeaseIdentity{Version: ownership.LeaseVersion, RepoID: repoID, RunID: runID, AdmittedBase: leaseBase}
	b, err := json.Marshal(id)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, lease.Path(), string(b))
	return wtPath
}

func runCleanup(t *testing.T, cacheRoot, repoArg string, apply bool) (*Report, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cfg := Config{CacheRoot: cacheRoot, Out: &stdout, Err: &stderr}
	rep, err := Run(context.Background(), cfg, repoArg, apply)
	if err != nil {
		t.Fatalf("cleanup.Run error: %v\nstderr:%s", err, stderr.String())
	}
	return rep, stdout.String(), stderr.String()
}

func eligibleRunIDs(report *Report) []string {
	var out []string
	for _, c := range report.Eligible {
		out = append(out, c.RunID)
	}
	return out
}

func removedRunIDs(report *Report) []string {
	var out []string
	for _, c := range report.Removed {
		out = append(out, c.RunID)
	}
	return out
}

func contains(slice []string, s string) bool {
	for _, x := range slice {
		if x == s {
			return true
		}
	}
	return false
}

// --- A: valid crash-shaped orphan --------------------------------------------

func TestCleanupValidOrphan(t *testing.T) {
	repoRoot, admittedOID := initRepo(t)
	cacheRoot := cacheRootOf(t)
	repoID := repoIDOf(t, repoRoot)
	runID := "run-orphan-a"

	wt := createOrphan(t, repoRoot, cacheRoot, repoID, runID,
		ownership.BuildReason(repoID, runID, admittedOID), admittedOID, admittedOID)

	// Inspect: zero persistent mutation, classified ELIGIBLE.
	rep, out, _ := runCleanup(t, cacheRoot, repoRoot, false)
	if !contains(eligibleRunIDs(rep), runID) {
		t.Fatalf("orphan not eligible on inspect; eligible=%v\nout=%s", eligibleRunIDs(rep), out)
	}
	if !strings.Contains(out, "ELIGIBLE:") || !strings.Contains(out, "SKIPPED_UNCERTAIN:") {
		t.Errorf("inspect output missing sections:\n%s", out)
	}
	if _, err := os.Lstat(wt); err != nil {
		t.Errorf("inspect must not remove worktree: %v", err)
	}

	// Apply: exact orphan removed, unrelated untouched.
	rep, out, _ = runCleanup(t, cacheRoot, repoRoot, true)
	if !contains(removedRunIDs(rep), runID) {
		t.Errorf("orphan not removed on apply; out=\n%s", out)
	}
	if _, err := os.Lstat(wt); err == nil {
		t.Error("orphan worktree still present after apply")
	}
	lease := ownership.NewLease(cacheRoot, repoID, runID)
	if _, err := os.Lstat(lease.Path()); err == nil {
		t.Error("lease pathname not removed after apply")
	}
}

// --- D: active session -------------------------------------------------------

func TestCleanupActiveSessionSkipped(t *testing.T) {
	repoRoot, admittedOID := initRepo(t)
	cacheRoot := cacheRootOf(t)
	repoID := repoIDOf(t, repoRoot)
	runID := "run-active-d"

	wt := createOrphan(t, repoRoot, cacheRoot, repoID, runID,
		ownership.BuildReason(repoID, runID, admittedOID), admittedOID, admittedOID)

	// Hold the lease as a live session would.
	lease := ownership.NewLease(cacheRoot, repoID, runID)
	heldID := ownership.LeaseIdentity{Version: ownership.LeaseVersion, RepoID: repoID, RunID: runID, AdmittedBase: admittedOID}
	if err := lease.Establish(heldID); err != nil {
		t.Fatal(err)
	}
	defer lease.Release()

	rep, out, _ := runCleanup(t, cacheRoot, repoRoot, false)
	if contains(eligibleRunIDs(rep), runID) {
		t.Fatalf("active session must not be eligible:\n%s", out)
	}
	found := false
	for _, c := range rep.Uncertain {
		if c.RunID == runID && strings.Contains(c.Reason, "ACTIVE_LEASE_HELD") {
			found = true
		}
	}
	if !found {
		t.Errorf("active session not classified ACTIVE_LEASE_HELD: uncertain=%+v", rep.Uncertain)
	}

	// --apply must leave the worktree untouched.
	rep, out, _ = runCleanup(t, cacheRoot, repoRoot, true)
	if contains(removedRunIDs(rep), runID) {
		t.Errorf("active session worktree removed by apply:\n%s", out)
	}
	if _, err := os.Lstat(wt); err != nil {
		t.Errorf("active session worktree should remain: %v", err)
	}
}

// --- E: path-only fake -------------------------------------------------------

func TestCleanupPathOnlyFake(t *testing.T) {
	repoRoot, admittedOID := initRepo(t)
	cacheRoot := cacheRootOf(t)
	repoID := repoIDOf(t, repoRoot)
	runID := "run-fake-e"

	wtParent := filepath.Join(cacheRoot, "worktrees", repoID)
	os.MkdirAll(wtParent, 0o755)
	wtPath := filepath.Join(wtParent, runID)
	// Detached but NOT locked: no valid ownership marker.
	git(t, repoRoot, "worktree", "add", "--detach", wtPath, admittedOID)

	rep, _, _ := runCleanup(t, cacheRoot, repoRoot, false)
	if contains(eligibleRunIDs(rep), runID) {
		t.Error("path-only fake must never be eligible")
	}
	rep, _, _ = runCleanup(t, cacheRoot, repoRoot, true)
	if contains(removedRunIDs(rep), runID) {
		t.Error("path-only fake must never be removed")
	}
	if _, err := os.Lstat(wtPath); err != nil {
		t.Error("path-only fake should remain")
	}
}

// --- F: invalid ownership metadata (cleanup level) ---------------------------

func TestCleanupInvalidMetadataVariants(t *testing.T) {
	repoRoot, admittedOID := initRepo(t)
	cacheRoot := cacheRootOf(t)
	repoID := repoIDOf(t, repoRoot)

	cases := map[string]string{
		"unsupported version": "global-build:v0:repo=" + repoID + ":run=run-f1:base=" + admittedOID,
		"wrong repo-id":       "global-build:v1:repo=" + strings.Repeat("0", 16) + ":run=run-f2:base=" + admittedOID,
		"wrong run-id":        "global-build:v1:repo=" + repoID + ":run=../evil:base=" + admittedOID,
		"malformed base":      "global-build:v1:repo=" + repoID + ":run=run-f3:base=zzzz",
		"duplicate field":     "global-build:v1:repo=" + repoID + ":repo=" + repoID + ":run=run-f4",
		"missing field":       "global-build:v1:repo=" + repoID + ":run=run-f5",
	}
	for name, reason := range cases {
		t.Run(name, func(t *testing.T) {
			runID := "run-" + strings.ToLower(strings.ReplaceAll(name, " ", "-"))
			wt := createOrphan(t, repoRoot, cacheRoot, repoID, runID, reason, admittedOID, admittedOID)
			rep, _, _ := runCleanup(t, cacheRoot, repoRoot, false)
			if contains(eligibleRunIDs(rep), runID) {
				t.Errorf("%s: invalid metadata must not be eligible", name)
			}
			rep, _, _ = runCleanup(t, cacheRoot, repoRoot, true)
			if contains(removedRunIDs(rep), runID) {
				t.Errorf("%s: invalid metadata must never be removed", name)
			}
			if _, err := os.Lstat(wt); err != nil {
				t.Errorf("%s: worktree should remain: %v", name, err)
			}
		})
	}
}

// --- G: branch-attached worktree ---------------------------------------------

func TestCleanupBranchAttached(t *testing.T) {
	repoRoot, admittedOID := initRepo(t)
	cacheRoot := cacheRootOf(t)
	repoID := repoIDOf(t, repoRoot)
	runID := "run-branch-g"

	wtParent := filepath.Join(cacheRoot, "worktrees", repoID)
	os.MkdirAll(wtParent, 0o755)
	wtPath := filepath.Join(wtParent, runID)
	git(t, repoRoot, "worktree", "add", "-b", "orphan-branch", wtPath, admittedOID)

	rep, _, _ := runCleanup(t, cacheRoot, repoRoot, false)
	if contains(eligibleRunIDs(rep), runID) {
		t.Error("branch-attached worktree must never be eligible")
	}
}

// --- H: base ancestry contradiction ------------------------------------------

func TestCleanupAncestryContradiction(t *testing.T) {
	repoRoot, admittedOID := initRepo(t)
	cacheRoot := cacheRootOf(t)
	repoID := repoIDOf(t, repoRoot)
	runID := "run-anc-h"

	// Advance main so a descendant commit exists.
	writeFile(t, filepath.Join(repoRoot, "docs", "more.txt"), "more\n")
	git(t, repoRoot, "add", "-A")
	git(t, repoRoot, "commit", "-qm", "second")
	mainHead := strings.TrimSpace(git(t, repoRoot, "rev-parse", "HEAD"))

	// Orphan checked out at admittedOID, but reason base claims mainHead (which
	// is NOT an ancestor of admittedOID).
	wt := createOrphan(t, repoRoot, cacheRoot, repoID, runID,
		ownership.BuildReason(repoID, runID, mainHead), admittedOID, mainHead)

	rep, _, _ := runCleanup(t, cacheRoot, repoRoot, false)
	if contains(eligibleRunIDs(rep), runID) {
		t.Error("ancestry contradiction must not be eligible")
	}
	rep, _, _ = runCleanup(t, cacheRoot, repoRoot, true)
	if contains(removedRunIDs(rep), runID) {
		t.Error("ancestry contradiction must never be removed")
	}
	if _, err := os.Lstat(wt); err != nil {
		t.Error("worktree should remain")
	}
}

// --- O: legacy residue without an ownership marker ---------------------------

func TestCleanupLegacyWithoutMarker(t *testing.T) {
	repoRoot, admittedOID := initRepo(t)
	cacheRoot := cacheRootOf(t)
	repoID := repoIDOf(t, repoRoot)

	t.Run("locked but no reason", func(t *testing.T) {
		runID := "run-legacy-noreason"
		wtParent := filepath.Join(cacheRoot, "worktrees", repoID)
		os.MkdirAll(wtParent, 0o755)
		wtPath := filepath.Join(wtParent, runID)
		// Locked with no ownership reason string (legacy/foreign lock).
		git(t, repoRoot, "worktree", "add", "--detach", "--lock", wtPath, admittedOID)

		rep, _, _ := runCleanup(t, cacheRoot, repoRoot, false)
		if contains(eligibleRunIDs(rep), runID) {
			t.Error("locked-without-reason worktree must never be eligible")
		}
		rep, _, _ = runCleanup(t, cacheRoot, repoRoot, true)
		if contains(removedRunIDs(rep), runID) {
			t.Error("locked-without-reason worktree must never be removed")
		}
		if _, err := os.Lstat(wtPath); err != nil {
			t.Error("worktree should remain")
		}
	})

	t.Run("foreign reason", func(t *testing.T) {
		runID := "run-legacy-foreign"
		wtParent := filepath.Join(cacheRoot, "worktrees", repoID)
		os.MkdirAll(wtParent, 0o755)
		wtPath := filepath.Join(wtParent, runID)
		git(t, repoRoot, "worktree", "add", "--detach", "--lock", "--reason", "not-global-build-ownership", wtPath, admittedOID)

		rep, _, _ := runCleanup(t, cacheRoot, repoRoot, false)
		if contains(eligibleRunIDs(rep), runID) {
			t.Error("foreign-reason worktree must never be eligible")
		}
		rep, _, _ = runCleanup(t, cacheRoot, repoRoot, true)
		if contains(removedRunIDs(rep), runID) {
			t.Error("foreign-reason worktree must never be removed")
		}
	})
}

// --- I: candidate-ref relationship -------------------------------------------

func TestCleanupCandidateRefRelationships(t *testing.T) {
	repoRoot, admittedOID := initRepo(t)
	cacheRoot := cacheRootOf(t)
	repoID := repoIDOf(t, repoRoot)

	t.Run("ref absent eligible", func(t *testing.T) {
		runID := "run-ref-absent"
		createOrphan(t, repoRoot, cacheRoot, repoID, runID,
			ownership.BuildReason(repoID, runID, admittedOID), admittedOID, admittedOID)
		rep, _, _ := runCleanup(t, cacheRoot, repoRoot, false)
		if !contains(eligibleRunIDs(rep), runID) {
			t.Error("ref-absent orphan should be eligible")
		}
	})

	t.Run("matching ref eligible and survives", func(t *testing.T) {
		runID := "run-ref-match"
		createOrphan(t, repoRoot, cacheRoot, repoID, runID,
			ownership.BuildReason(repoID, runID, admittedOID), admittedOID, admittedOID)
		// Candidate ref equals worktree HEAD (admittedOID).
		git(t, repoRoot, "update-ref", "refs/build-candidates/"+runID, admittedOID)

		rep, _, _ := runCleanup(t, cacheRoot, repoRoot, false)
		if !contains(eligibleRunIDs(rep), runID) {
			t.Error("matching-ref orphan should be eligible")
		}
		// Apply removes the worktree but preserves the candidate ref.
		rep, _, _ = runCleanup(t, cacheRoot, repoRoot, true)
		if !contains(removedRunIDs(rep), runID) {
			t.Error("matching-ref orphan should be removed")
		}
		if !refExists(t, repoRoot, "refs/build-candidates/"+runID) {
			t.Error("matching candidate ref must survive worktree cleanup")
		}
	})

	t.Run("mismatched ref uncertain", func(t *testing.T) {
		runID := "run-ref-mismatch"
		createOrphan(t, repoRoot, cacheRoot, repoID, runID,
			ownership.BuildReason(repoID, runID, admittedOID), admittedOID, admittedOID)
		// Create a second commit and point the candidate ref at it so the ref
		// exists but resolves to a different OID than the worktree HEAD.
		writeFile(t, filepath.Join(repoRoot, "docs", "other.txt"), "other\n")
		git(t, repoRoot, "add", "-A")
		git(t, repoRoot, "commit", "-qm", "other")
		otherHead := strings.TrimSpace(git(t, repoRoot, "rev-parse", "HEAD"))
		git(t, repoRoot, "update-ref", "refs/build-candidates/"+runID, otherHead)
		defer git(t, repoRoot, "update-ref", "-d", "refs/build-candidates/"+runID)

		rep, _, _ := runCleanup(t, cacheRoot, repoRoot, false)
		if contains(eligibleRunIDs(rep), runID) {
			t.Error("mismatched-ref orphan must be uncertain")
		}
		rep, _, _ = runCleanup(t, cacheRoot, repoRoot, true)
		if contains(removedRunIDs(rep), runID) {
			t.Error("mismatched-ref orphan must never be removed")
		}
	})
}

func refExists(t *testing.T, repoRoot, ref string) bool {
	t.Helper()
	cmd := exec.Command("git", "-C", repoRoot, "rev-parse", "--verify", "--quiet", ref)
	return cmd.Run() == nil
}

// --- J: missing / prunable worktree -------------------------------------------

func TestCleanupMissingWorktreeUncertain(t *testing.T) {
	repoRoot, admittedOID := initRepo(t)
	cacheRoot := cacheRootOf(t)
	repoID := repoIDOf(t, repoRoot)
	runID := "run-missing-j"

	wt := createOrphan(t, repoRoot, cacheRoot, repoID, runID,
		ownership.BuildReason(repoID, runID, admittedOID), admittedOID, admittedOID)
	// Simulate a prunable (removed-on-disk) registration.
	if err := os.RemoveAll(wt); err != nil {
		t.Fatal(err)
	}

	rep, out, _ := runCleanup(t, cacheRoot, repoRoot, false)
	if contains(eligibleRunIDs(rep), runID) {
		t.Error("missing worktree must not be eligible")
	}
	if strings.Contains(out, "prune") {
		t.Errorf("cleanup output unexpectedly mentions prune:\n%s", out)
	}
}

// --- K: repository isolation --------------------------------------------------

func TestCleanupRepositoryIsolation(t *testing.T) {
	repoA, admittedA := initRepo(t)
	repoB, admittedB := initRepo(t)
	cacheRoot := cacheRootOf(t)
	repoID_B := repoIDOf(t, repoB)
	runID := "run-iso-k"

	// An orphan belonging to repository B only.
	wtB := createOrphan(t, repoB, cacheRoot, repoID_B, runID,
		ownership.BuildReason(repoID_B, runID, admittedB), admittedB, admittedB)

	// Cleanup targeting repository A must never list or remove B's worktree.
	rep, out, _ := runCleanup(t, cacheRoot, repoA, false)
	if len(rep.Eligible) != 0 {
		t.Errorf("cleanup of A must not find B worktrees: %+v", rep.Eligible)
	}
	rep, _, _ = runCleanup(t, cacheRoot, repoA, true)
	if len(rep.Removed) != 0 {
		t.Errorf("cleanup of A must not remove B worktrees: %+v", rep.Removed)
	}
	if _, err := os.Lstat(wtB); err != nil {
		t.Errorf("B's orphan worktree was affected by A's cleanup: %v\nout=%s", err, out)
	}
	leaseB := ownership.NewLease(cacheRoot, repoID_B, runID)
	if _, err := os.Lstat(leaseB.Path()); err != nil {
		t.Errorf("B's lease was affected by A's cleanup: %v", err)
	}
	_ = admittedA
}

// --- L: concurrent apply -----------------------------------------------------

func TestCleanupConcurrentApply(t *testing.T) {
	repoRoot, admittedOID := initRepo(t)
	cacheRoot := cacheRootOf(t)
	repoID := repoIDOf(t, repoRoot)
	runID := "run-concurrent-l"

	createOrphan(t, repoRoot, cacheRoot, repoID, runID,
		ownership.BuildReason(repoID, runID, admittedOID), admittedOID, admittedOID)

	var wg sync.WaitGroup
	results := make([]*Report, 2)
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var stdout, stderr bytes.Buffer
			cfg := Config{CacheRoot: cacheRoot, Out: &stdout, Err: &stderr}
			rep, err := Run(context.Background(), cfg, repoRoot, true)
			results[i] = rep
			errs[i] = err
		}(i)
	}
	wg.Wait()

	removedCount := 0
	for i, rep := range results {
		if errs[i] != nil {
			t.Fatalf("concurrent cleanup %d errored: %v", i, errs[i])
		}
		if contains(removedRunIDs(rep), runID) {
			removedCount++
		}
	}
	if removedCount != 1 {
		t.Fatalf("exactly one concurrent apply must remove the orphan; removedCount=%d", removedCount)
	}
	wt := filepath.Join(cacheRoot, "worktrees", repoID, runID)
	if _, err := os.Lstat(wt); err == nil {
		t.Error("orphan worktree still present after concurrent apply")
	}
}

// --- M: frozen candidate set --------------------------------------------------

func TestCleanupFrozenCandidateSet(t *testing.T) {
	repoRoot, admittedOID := initRepo(t)
	cacheRoot := cacheRootOf(t)
	repoID := repoIDOf(t, repoRoot)
	runID1 := "run-frozen-1"

	createOrphan(t, repoRoot, cacheRoot, repoID, runID1,
		ownership.BuildReason(repoID, runID1, admittedOID), admittedOID, admittedOID)

	// Discover (freeze) the candidate set.
	cfg := Config{CacheRoot: cacheRoot, Out: &bytes.Buffer{}, Err: &bytes.Buffer{}}
	rc, err := ResolveRepo(context.Background(), cfg, repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	eligible, _, err := discover(context.Background(), cfg, rc)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(eligibleRunIDs(&Report{Eligible: eligible}), runID1) {
		t.Fatal("frozen set missing the first orphan")
	}

	// A second valid orphan appears AFTER discovery.
	runID2 := "run-frozen-2"
	createOrphan(t, repoRoot, cacheRoot, repoID, runID2,
		ownership.BuildReason(repoID, runID2, admittedOID), admittedOID, admittedOID)

	// Apply only the frozen set.
	removed, _ := applyFrozen(context.Background(), cfg, rc, eligible)
	if !contains(removedRunIDs(&Report{Removed: removed}), runID1) {
		t.Error("frozen orphan not removed")
	}
	if contains(removedRunIDs(&Report{Removed: removed}), runID2) {
		t.Error("post-discovery orphan must not be absorbed into this invocation")
	}
	if _, err := os.Lstat(filepath.Join(cacheRoot, "worktrees", repoID, runID2)); err != nil {
		t.Error("late orphan should remain after frozen-set apply")
	}
}
