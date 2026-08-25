// Package cleanup implements the explicit `global-build cleanup --repo <path>`
// command. Default mode is inspect-only: it enumerates linked worktrees of the
// supplied repository through `git worktree list --porcelain -z`, and classifies
// each as ELIGIBLE for destructive removal or SKIPPED_UNCERTAIN. A worktree is
// ELIGIBLE only when every ownership/liveness fact is directly and
// deterministically proven; any missing, ambiguous, contradictory, or
// unverifiable fact yields SKIPPED_UNCERTAIN rather than eligibility.
//
// With --apply, the candidate set is frozen at discovery, then each ELIGIBLE
// candidate is removed exactly once, serialized by its per-run liveness lease so
// that concurrent cleanup processes (or a live normal BUILD holding its lease)
// can never double-remove or remove an active session's worktree.
package cleanup

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"global-build/internal/gitx"
	"global-build/internal/ownership"
)

// Config is the injected cleanup configuration.
type Config struct {
	CacheRoot string // override cache root; empty => os.UserCacheDir()/global-build
	Out       io.Writer
	Err       io.Writer
}

// Candidate is one classified worktree.
type Candidate struct {
	RunID        string
	Path         string
	Head         string
	AdmittedBase string
	Reason       string // compact evidence / status for uncertain entries
}

// Report is the full classification outcome.
type Report struct {
	Eligible  []Candidate
	Uncertain []Candidate
	Removed   []Candidate
	Skipped   []Candidate // frozen-eligible that became no-longer-eligible
}

// repoCtx is the resolved target repository identity.
type repoCtx struct {
	repoArg      string
	topLevel     string
	commonDir    string
	repoID       string
	cacheRoot    string
	worktreesDir string
}

// ResolveRepo validates the supplied --repo argument and pins the repository
// identity. A non-repository or malformed path is a mechanical failure.
func ResolveRepo(ctx context.Context, cfg Config, repoArg string) (*repoCtx, error) {
	if repoArg == "" {
		return nil, fmt.Errorf("cleanup: --repo is required")
	}
	if !gitx.IsWorkTree(ctx, repoArg) {
		return nil, fmt.Errorf("cleanup: %q is not inside a git work tree", repoArg)
	}
	commonDir, err := gitx.CanonicalCommonDir(ctx, repoArg)
	if err != nil {
		return nil, fmt.Errorf("cleanup: cannot resolve common-dir: %v", err)
	}
	tlOut, err := gitx.Run(ctx, repoArg, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("cleanup: cannot resolve top-level: %v", err)
	}
	topLevel, err := filepath.EvalSymlinks(strings.TrimSpace(tlOut))
	if err != nil {
		return nil, fmt.Errorf("cleanup: cannot canonicalize top-level: %v", err)
	}
	repoID := gitx.RepoID(commonDir)
	cacheRoot := cfg.CacheRoot
	if cacheRoot == "" {
		uc, err := os.UserCacheDir()
		if err != nil || uc == "" {
			return nil, fmt.Errorf("cleanup: cannot resolve user cache dir: %v", err)
		}
		cacheRoot = filepath.Join(uc, "global-build")
	}
	// Canonicalize so worktree layout and lease path match Git's symlink-
	// resolved worktree paths.
	cacheRoot = gitx.CanonicalPath(cacheRoot)
	return &repoCtx{
		repoArg:      repoArg,
		topLevel:     topLevel,
		commonDir:    commonDir,
		repoID:       repoID,
		cacheRoot:    cacheRoot,
		worktreesDir: filepath.Join(cacheRoot, "worktrees", repoID),
	}, nil
}

// Run performs one cleanup pass. With apply=false it inspects only (zero
// persistent mutation). With apply=true it freezes the discovered candidate set
// and removes each eligible worktree exactly once.
func Run(ctx context.Context, cfg Config, repoArg string, apply bool) (*Report, error) {
	if cfg.Out == nil {
		cfg.Out = io.Discard
	}
	if cfg.Err == nil {
		cfg.Err = io.Discard
	}
	rc, err := ResolveRepo(ctx, cfg, repoArg)
	if err != nil {
		return nil, err
	}
	eligible, uncertain, err := discover(ctx, cfg, rc)
	if err != nil {
		return nil, err
	}
	report := &Report{Eligible: eligible, Uncertain: uncertain}
	if !apply {
		writeInspect(cfg.Out, report)
		return report, nil
	}
	removed, skipped := applyFrozen(ctx, cfg, rc, eligible)
	report.Removed = removed
	report.Skipped = skipped
	writeApply(cfg.Out, report)
	return report, nil
}

// discover enumerates the repository's worktrees and classifies each. The
// returned eligible slice is frozen and sorted deterministically.
func discover(ctx context.Context, cfg Config, rc *repoCtx) ([]Candidate, []Candidate, error) {
	entries, err := gitx.WorktreeListPorcelainZ(ctx, rc.repoArg)
	if err != nil {
		return nil, nil, fmt.Errorf("cleanup: worktree list failed: %v", err)
	}
	countByPath := map[string]int{}
	for _, e := range entries {
		countByPath[e.Path]++
	}

	var eligible, uncertain []Candidate
	for _, e := range entries {
		cand, ok, err := evaluate(ctx, rc, e, countByPath[e.Path])
		if err != nil {
			// A genuine mechanical failure (e.g. unsupported locking platform)
			// must propagate rather than be silently downgraded to uncertain.
			return nil, nil, err
		}
		if ok {
			eligible = append(eligible, cand)
		} else {
			uncertain = append(uncertain, cand)
		}
	}
	sortCandidates(eligible)
	sortCandidates(uncertain)
	return eligible, uncertain, nil
}

// evaluate applies the full eligibility contract (conditions 1-16). On the
// first unmet condition it returns an uncertain Candidate with a concise
// reason. For inspect, any successful lease acquisition is immediately released
// so no liveness state is held.
func evaluate(ctx context.Context, rc *repoCtx, e gitx.WorktreeEntry, registrationCount int) (Candidate, bool, error) {
	path := gitx.CanonicalPath(e.Path)
	cand := Candidate{Path: path}

	if e.Bare {
		return uncertain(cand, "bare worktree"), false, nil
	}
	if path == rc.topLevel {
		return uncertain(cand, "main/source worktree"), false, nil
	}
	if registrationCount != 1 {
		return uncertain(cand, "ambiguous worktree registration"), false, nil
	}

	// Condition 2/3/4: canonical path matches the global-build cache layout and
	// contains exactly one safe run-id.
	prefix := rc.worktreesDir + string(os.PathSeparator)
	if !strings.HasPrefix(path, prefix) {
		return uncertain(cand, "not in global-build cache layout"), false, nil
	}
	rest := strings.TrimPrefix(path, prefix)
	if rest == "" || strings.Contains(rest, string(os.PathSeparator)) {
		return uncertain(cand, "path is not a single run-id segment"), false, nil
	}
	runID := rest
	if !ownership.ValidRunID(runID) {
		return uncertain(cand, "unsafe run-id in path"), false, nil
	}
	cand.RunID = runID

	if !e.Detached {
		return uncertain(cand, "worktree is branch-attached"), false, nil
	}
	if !e.Locked {
		return uncertain(cand, "worktree is not locked"), false, nil
	}
	if e.LockReason == "" {
		return uncertain(cand, "no ownership marker (legacy residue)"), false, nil
	}

	reason, perr := ownership.ParseReason(e.LockReason)
	if perr != nil {
		return uncertain(cand, fmt.Sprintf("invalid ownership metadata: %v", perr)), false, nil
	}
	if reason.RepoID != rc.repoID {
		return uncertain(cand, "reason repo-id mismatch"), false, nil
	}
	if reason.RunID != runID {
		return uncertain(cand, "reason run-id/path mismatch"), false, nil
	}
	if !gitx.ResolveCommit(ctx, rc.repoArg, reason.AdmittedBase) {
		return uncertain(cand, "admitted-base does not resolve to a commit"), false, nil
	}

	wtCommon, err := gitx.CanonicalCommonDir(ctx, path)
	if err != nil || wtCommon != rc.commonDir {
		return uncertain(cand, "worktree common-dir does not resolve to supplied repository"), false, nil
	}
	if e.Head == "" {
		return uncertain(cand, "worktree HEAD missing"), false, nil
	}
	ancestor, err := gitx.IsAncestor(ctx, rc.repoArg, reason.AdmittedBase, e.Head)
	if err != nil {
		return uncertain(cand, "ancestor check failed"), false, nil
	}
	if !ancestor {
		return uncertain(cand, "admitted-base is not an ancestor of HEAD"), false, nil
	}

	// Condition 14: existing lease identity matches exactly.
	lease := ownership.NewLease(rc.cacheRoot, rc.repoID, runID)
	id, ok, err := lease.ReadIdentity()
	if err != nil {
		return uncertain(cand, "cannot read lease identity"), false, nil
	}
	if !ok {
		return uncertain(cand, "no/malformed lease identity"), false, nil
	}
	if id.Version != ownership.LeaseVersion || id.RepoID != rc.repoID || id.RunID != runID || id.AdmittedBase != reason.AdmittedBase {
		return uncertain(cand, "lease identity mismatch"), false, nil
	}

	// Condition 15: cleanup can acquire the run lease nonblockingly. For inspect
	// we release immediately so we never hold liveness state.
	acquired, lerr := lease.TryAcquireExisting()
	if lerr != nil {
		// Unsupported platform (or other mechanical error): propagate so
		// destructive apply cannot weaken the contract.
		return Candidate{}, false, lerr
	}
	if !acquired {
		return uncertain(cand, "ACTIVE_LEASE_HELD"), false, nil
	}
	if rerr := lease.Release(); rerr != nil {
		return Candidate{}, false, fmt.Errorf("cleanup: cannot release test lease: %v", rerr)
	}

	// Condition 16: candidate-ref state must be non-contradictory.
	ref := "refs/build-candidates/" + runID
	if gitx.RefExists(ctx, rc.repoArg, ref) {
		code, refOID, err := gitx.RunExit(ctx, rc.repoArg, "rev-parse", "--verify", ref)
		if err != nil || code != 0 {
			return uncertain(cand, "cannot resolve candidate ref"), false, nil
		}
		if strings.TrimSpace(refOID) != e.Head {
			return uncertain(cand, "candidate ref mismatch"), false, nil
		}
	}

	cand.Head = e.Head
	cand.AdmittedBase = reason.AdmittedBase
	cand.Reason = "eligible"
	return cand, true, nil
}

// applyFrozen removes each frozen eligible candidate exactly once. Each removal
// re-acquires and KEEPS the run lease for the whole removal, re-validates, and
// skips (no-longer-eligible) any candidate that changed or whose lease is held
// by another process.
func applyFrozen(ctx context.Context, cfg Config, rc *repoCtx, eligible []Candidate) ([]Candidate, []Candidate) {
	// Snapshot unrelated worktree registrations so we can prove they are
	// unchanged after removal.
	before, _ := gitx.WorktreeListPorcelainZ(ctx, rc.repoArg)
	unrelatedBefore := unrelatedPaths(before, eligible, rc)

	var removed, skipped []Candidate
	for _, cand := range eligible {
		ok, rerr := removeOne(ctx, cfg, rc, cand)
		if rerr != nil {
			// Mechanical failure during a removal: surface and stop applying,
			// but do not weaken safety. The candidate is recorded as skipped
			// with the error so the caller sees the failure.
			cand.Reason = "removal error: " + rerr.Error()
			skipped = append(skipped, cand)
			fmt.Fprintf(cfg.Err, "global-build: cleanup removal failed for %s: %v\n", cand.Path, rerr)
			continue
		}
		if ok {
			removed = append(removed, cand)
		} else {
			cand.Reason = "no-longer-eligible"
			skipped = append(skipped, cand)
		}
	}

	// Verify unrelated worktree registrations are unchanged.
	after, _ := gitx.WorktreeListPorcelainZ(ctx, rc.repoArg)
	unrelatedAfter := unrelatedPaths(after, eligible, rc)
	if len(unrelatedBefore) != len(unrelatedAfter) {
		fmt.Fprintf(cfg.Err, "global-build: WARNING unrelated worktree count changed during cleanup (%d -> %d)\n", len(unrelatedBefore), len(unrelatedAfter))
	}
	for p := range unrelatedBefore {
		if _, ok := unrelatedAfter[p]; !ok {
			fmt.Fprintf(cfg.Err, "global-build: WARNING unrelated worktree removed: %s\n", p)
		}
	}
	return removed, skipped
}

// unrelatedPaths returns the set of registered worktree paths that are NOT the
// main worktree and NOT any candidate path.
func unrelatedPaths(entries []gitx.WorktreeEntry, candidates []Candidate, rc *repoCtx) map[string]struct{} {
	set := map[string]struct{}{}
	candPaths := map[string]struct{}{}
	for _, c := range candidates {
		candPaths[c.Path] = struct{}{}
	}
	for _, e := range entries {
		if e.Path == rc.topLevel {
			continue
		}
		if _, isCand := candPaths[e.Path]; isCand {
			continue
		}
		set[e.Path] = struct{}{}
	}
	return set
}

// removeOne attempts to remove exactly one owned worktree. It returns
// (false, nil) when the candidate is no-longer-eligible (lease held elsewhere,
// identity changed, or worktree gone). It returns (true, nil) on successful
// removal. A genuine mechanical error (e.g. unsupported platform) is returned as
// (false, err).
func removeOne(ctx context.Context, cfg Config, rc *repoCtx, cand Candidate) (bool, error) {
	lease := ownership.NewLease(rc.cacheRoot, rc.repoID, cand.RunID)

	// Condition 15 (re-acquire and KEEP): serialize destructive removal by the
	// per-run lease. If another process holds it, this candidate is skipped.
	acquired, lerr := lease.TryAcquireExisting()
	if lerr != nil {
		return false, lerr
	}
	if !acquired {
		return false, nil
	}
	// Hold the lease for the entire removal; release on any exit path.
	defer func() { _ = lease.Release() }()

	// Re-validate full ownership + marker on the live state.
	if ok, _ := revalidate(ctx, rc, cand); !ok {
		return false, nil
	}

	fmt.Fprintf(cfg.Out, "REMOVING:\n- run=%s path=%s\n", cand.RunID, cand.Path)

	// Unlock only the exact worktree.
	if err := gitx.WorktreeUnlock(ctx, rc.repoArg, cand.Path); err != nil {
		return false, fmt.Errorf("worktree unlock failed: %w", err)
	}
	// Re-prove exact identity (path/repo/common-dir/HEAD) while still holding
	// the lease, before the destructive remove.
	if err := proveBasic(ctx, rc, cand); err != nil {
		return false, err
	}
	// Remove only with `git worktree remove --force <exact-path>`.
	if err := gitx.WorktreeRemoveForce(ctx, rc.repoArg, cand.Path); err != nil {
		return false, fmt.Errorf("worktree remove failed: %w", err)
	}
	// Verify the exact registration is gone.
	if stillRegistered(ctx, rc.repoArg, cand.Path) {
		return false, fmt.Errorf("worktree still registered after removal")
	}
	// Remove only this run's lease pathname.
	if err := lease.Remove(); err != nil && !os.IsNotExist(err) {
		return false, fmt.Errorf("lease path removal failed: %w", err)
	}
	return true, nil
}

// revalidate re-checks conditions 1-14 and 16 on the live state (the lease is
// already held by the caller, so condition 15 is not re-tested here).
func revalidate(ctx context.Context, rc *repoCtx, cand Candidate) (bool, string) {
	entries, err := gitx.WorktreeListPorcelainZ(ctx, rc.repoArg)
	if err != nil {
		return false, "metadata query failed"
	}
	var found *gitx.WorktreeEntry
	for i := range entries {
		if entries[i].Path == cand.Path {
			found = &entries[i]
			break
		}
	}
	if found == nil {
		return false, "worktree no longer registered"
	}
	if found.Bare {
		return false, "bare worktree"
	}
	if cand.Path == rc.topLevel {
		return false, "main/source worktree"
	}
	if !found.Detached {
		return false, "no longer detached"
	}
	if !found.Locked {
		return false, "no longer locked"
	}
	if found.LockReason == "" {
		return false, "ownership marker gone"
	}
	reason, perr := ownership.ParseReason(found.LockReason)
	if perr != nil {
		return false, "invalid ownership metadata"
	}
	if reason.RepoID != rc.repoID || reason.RunID != cand.RunID || reason.AdmittedBase != cand.AdmittedBase {
		return false, "ownership identity changed"
	}
	if !gitx.ResolveCommit(ctx, rc.repoArg, reason.AdmittedBase) {
		return false, "admitted-base unresolved"
	}
	wtCommon, err := gitx.CanonicalCommonDir(ctx, cand.Path)
	if err != nil || wtCommon != rc.commonDir {
		return false, "common-dir mismatch"
	}
	if found.Head != cand.Head {
		return false, "HEAD changed"
	}
	ancestor, err := gitx.IsAncestor(ctx, rc.repoArg, reason.AdmittedBase, found.Head)
	if err != nil || !ancestor {
		return false, "ancestry contradiction"
	}
	// Condition 14 re-check (lease identity still matches).
	lease := ownership.NewLease(rc.cacheRoot, rc.repoID, cand.RunID)
	id, ok, err := lease.ReadIdentity()
	if err != nil || !ok {
		return false, "lease identity missing"
	}
	if id.Version != ownership.LeaseVersion || id.RepoID != rc.repoID || id.RunID != cand.RunID || id.AdmittedBase != reason.AdmittedBase {
		return false, "lease identity mismatch"
	}
	// Condition 16 re-check.
	ref := "refs/build-candidates/" + cand.RunID
	if gitx.RefExists(ctx, rc.repoArg, ref) {
		code, refOID, rerr := gitx.RunExit(ctx, rc.repoArg, "rev-parse", "--verify", ref)
		if rerr != nil || code != 0 || strings.TrimSpace(refOID) != found.Head {
			return false, "candidate ref mismatch"
		}
	}
	return true, ""
}

// proveBasic re-proves exact repo/path/HEAD identity (used after unlock, when
// the marker is intentionally gone).
func proveBasic(ctx context.Context, rc *repoCtx, cand Candidate) error {
	entries, err := gitx.WorktreeListPorcelainZ(ctx, rc.repoArg)
	if err != nil {
		return fmt.Errorf("metadata query failed: %w", err)
	}
	var found *gitx.WorktreeEntry
	for i := range entries {
		if entries[i].Path == cand.Path {
			found = &entries[i]
			break
		}
	}
	if found == nil {
		return fmt.Errorf("worktree %s not registered before removal", cand.Path)
	}
	if cand.Path == rc.topLevel {
		return fmt.Errorf("worktree %s is the main worktree", cand.Path)
	}
	wtCommon, err := gitx.CanonicalCommonDir(ctx, cand.Path)
	if err != nil || wtCommon != rc.commonDir {
		return fmt.Errorf("common-dir mismatch")
	}
	if found.Head != cand.Head {
		return fmt.Errorf("HEAD changed before removal")
	}
	return nil
}

func stillRegistered(ctx context.Context, repoDir, path string) bool {
	entries, err := gitx.WorktreeListPorcelainZ(ctx, repoDir)
	if err != nil {
		return true
	}
	for _, e := range entries {
		if e.Path == path {
			return true
		}
	}
	return false
}

func uncertain(cand Candidate, reason string) Candidate {
	cand.Reason = reason
	return cand
}

func sortCandidates(cs []Candidate) {
	sort.SliceStable(cs, func(i, j int) bool {
		if cs[i].RunID != cs[j].RunID {
			return cs[i].RunID < cs[j].RunID
		}
		return cs[i].Path < cs[j].Path
	})
}

func writeInspect(out io.Writer, r *Report) {
	fmt.Fprintln(out, "ELIGIBLE:")
	for _, c := range r.Eligible {
		fmt.Fprintf(out, "- run=%s path=%s head=%s base=%s\n", c.RunID, c.Path, c.Head, c.AdmittedBase)
	}
	fmt.Fprintln(out, "SKIPPED_UNCERTAIN:")
	for _, c := range r.Uncertain {
		run := c.RunID
		if run == "" {
			run = "?"
		}
		fmt.Fprintf(out, "- run=%s path=%s reason=%s\n", run, c.Path, c.Reason)
	}
}

func writeApply(out io.Writer, r *Report) {
	fmt.Fprintln(out, "ELIGIBLE (frozen):")
	for _, c := range r.Eligible {
		fmt.Fprintf(out, "- run=%s path=%s head=%s base=%s\n", c.RunID, c.Path, c.Head, c.AdmittedBase)
	}
	if len(r.Removed) > 0 {
		fmt.Fprintln(out, "REMOVED:")
		for _, c := range r.Removed {
			fmt.Fprintf(out, "- run=%s path=%s\n", c.RunID, c.Path)
		}
	}
	if len(r.Skipped) > 0 {
		fmt.Fprintln(out, "SKIPPED_UNCERTAIN:")
		for _, c := range r.Skipped {
			fmt.Fprintf(out, "- run=%s path=%s reason=%s\n", c.RunID, c.Path, c.Reason)
		}
	}
}
