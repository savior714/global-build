// Package runner orchestrates exactly one BUILD attempt per invocation:
//
//	stdin envelope -> disposable detached worktree -> noninteractive OpenCode
//	-> strict result protocol validation -> deterministic candidate checks
//	-> candidate ref -> identity-proved worktree removal.
//
// The mutating target repository is always an explicitly requested locator;
// it is canonicalized and bound to a proven Git identity (toplevel/common-dir)
// before any ownership lease, task worktree, candidate ref, or executor
// invocation. The process working directory is never used to select a
// mutating target.
//
// The runner is stateless: everything it needs lives in process memory for the
// duration of one run. There is no daemon, task database, queue, or registry,
// and there is no publication step in this slice.
package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"global-build/internal/envelope"
	"global-build/internal/gitx"
	"global-build/internal/opencode"
	"global-build/internal/ownership"
	"global-build/internal/result"
	"global-build/internal/watches"
)

// Exit codes of the runner protocol.
const (
	ExitComplete        = 0
	ExitContinuable     = 10
	ExitBlocked         = 20
	ExitMalformedResult = 30
	ExitRunnerError     = 40
)

const (
	candidateRefPrefix = "refs/build-candidates/"
	defaultWallClock   = 90 * time.Minute
	defaultProgressWin = 15 * time.Minute
	maxAttempts        = 2 // initial attempt + at most one transient retry
)

// Config is the injected runtime configuration.
type Config struct {
	WallClock      time.Duration // absolute wall-clock limit (default 90m)
	ProgressWindow time.Duration // no-meaningful-progress watchdog (default 15m)
	CacheRoot      string        // override cache root for tests; empty => os.UserCacheDir()/global-build
	BinPath        string        // resolved OpenCode executable
	Stdout         io.Writer
	Stderr         io.Writer
}

func (c *Config) fillDefaults() {
	if c.WallClock <= 0 {
		c.WallClock = defaultWallClock
	}
	if c.ProgressWindow <= 0 {
		c.ProgressWindow = defaultProgressWin
	}
	if c.Stdout == nil {
		c.Stdout = os.Stdout
	}
	if c.Stderr == nil {
		c.Stderr = os.Stderr
	}
}

// resolveCacheRoot returns the cache root used for both worktree layout and the
// liveness lease. An explicit Config.CacheRoot wins; otherwise the platform user
// cache directory joined with "global-build".
func resolveCacheRoot(cfg Config) string {
	cr := cfg.CacheRoot
	if cr == "" {
		userCache, err := os.UserCacheDir()
		if err != nil || userCache == "" {
			return ""
		}
		cr = filepath.Join(userCache, "global-build")
	}
	// Canonicalize so the lease path matches the canonical form Git reports for
	// worktree registrations (and the cleanup command's canonicalized cacheRoot).
	return gitx.CanonicalPath(cr)
}

// records are the ownership facts held in process memory for this run. The
// repository locator (where the caller requested) and the repository identity
// (what Git proved that locator actually is) are kept separate: downstream
// ownership/worktree validation binds to the identity only.
type records struct {
	runID       string
	repoLocator string // canonical explicitly-requested repository locator (never cwd)
	commonDir   string // canonical git common-dir of the target repository
	repoRoot    string // canonical main work tree root (git operations run here)
	worktree    string // intended worktree path (canonical)
	admittedOID string
	surfaces    []string // normalized WATCH_SURFACES from the envelope
	hashLen     int
	repoID      string // deterministic repo identity (gitx.RepoID(commonDir))
	reason      string // durable ownership marker (lock reason)
	lease       *ownership.Lease
}

// output accumulates stable stdout keys and prints them deterministically:
// the three contract keys always lead, remaining keys keep insertion order.
type output struct {
	w      io.Writer
	order  []string
	fields map[string]string
}

var keyRank = map[string]int{"RUN_RESULT": 0, "RUN_ID": 1, "ADMITTED_BASE": 2}

func newOutput(w io.Writer) *output { return &output{w: w, fields: map[string]string{}} }

func (o *output) set(key, val string) {
	if _, ok := o.fields[key]; !ok {
		o.order = append(o.order, key)
	}
	o.fields[key] = val
}

func (o *output) flush() {
	keys := append([]string(nil), o.order...)
	sort.SliceStable(keys, func(i, j int) bool { return keyRank[keys[i]] < keyRank[keys[j]] })
	var b strings.Builder
	for _, k := range keys {
		lines := strings.Split(o.fields[k], "\n")
		b.WriteString(k + ": " + lines[0] + "\n")
		for _, cont := range lines[1:] {
			b.WriteString("  " + cont + "\n")
		}
	}
	fmt.Fprint(o.w, b.String())
}

// terminal is the decided end state of the whole run.
type terminal struct {
	exitCode int
	rerr     *RunError // set only for MALFORMED_RESULT / RUNNER_ERROR paths
}

// Run executes one BUILD attempt for the explicitly requested repository
// locator and returns the exit code. The process working directory is NEVER
// used to select the mutating target: repoLocator must name the exact
// repository the caller intends to mutate, and it is canonicalized and bound
// to a proven Git identity before any mutation can occur.
func Run(ctx context.Context, cfg Config, repoLocator string, input []byte) int {
	cfg.fillDefaults()
	logger := log.New(cfg.Stderr, "global-build ", log.LstdFlags)

	out := newOutput(cfg.Stdout)
	defer out.flush()

	env, rerr := parseEnvelope(input)
	if rerr != nil {
		out.set("RUN_ID", "-")
		out.set("ADMITTED_BASE", "-")
		return reportTerminal(out, logger, terminal{exitCode: ExitRunnerError, rerr: rerr})
	}
	out.set("RUN_ID", env.RunID)
	out.set("ADMITTED_BASE", env.AdmittedBase)

	rec, rerr := resolveRepository(repoLocator, env)
	if rerr != nil {
		return reportTerminal(out, logger, terminal{exitCode: ExitRunnerError, rerr: rerr})
	}
	if rerr := preflight(ctx, cfg, rec, logger); rerr != nil {
		return reportTerminal(out, logger, terminal{exitCode: ExitRunnerError, rerr: rerr})
	}
	if rerr := establishLeaseAndWorktree(ctx, cfg, rec, logger); rerr != nil {
		return reportTerminal(out, logger, terminal{exitCode: ExitRunnerError, rerr: rerr})
	}
	logger.Printf("worktree ready: %s (detached+locked at %s)", rec.worktree, rec.admittedOID)

	term := executeAttempts(ctx, cfg, rec, env, out, logger)
	return reportTerminal(out, logger, term)
}

// reportTerminal maps a terminal state onto the stdout contract and exit code.
func reportTerminal(out *output, logger *log.Logger, t terminal) int {
	switch {
	case t.rerr == nil:
		switch t.exitCode {
		case ExitComplete:
			out.set("RUN_RESULT", "COMPLETE")
		case ExitContinuable:
			out.set("RUN_RESULT", "CONTINUABLE")
		case ExitBlocked:
			out.set("RUN_RESULT", "BLOCKED")
		default:
			out.set("RUN_RESULT", "RUNNER_ERROR")
		}
		return t.exitCode
	case t.rerr.Kind == ErrMalformedResult:
		out.set("RUN_RESULT", "MALFORMED_RESULT")
		logger.Print(t.rerr.Msg)
		return ExitMalformedResult
	default:
		out.set("RUN_RESULT", "RUNNER_ERROR")
		out.set("ERROR_KIND", t.rerr.Kind.ErrorKindString())
		logger.Print(t.rerr.Msg)
		return ExitRunnerError
	}
}

// --- input resolution -------------------------------------------------------

func parseEnvelope(input []byte) (*envelope.Envelope, *RunError) {
	env, err := envelope.Parse(string(input))
	if err != nil {
		return nil, runErr(ErrMalformedInput, "%v", err)
	}
	return env, nil
}

// resolveRepository binds the explicitly requested repository to a proven Git
// identity. The caller MUST supply the target repository explicitly; the
// process working directory is never used to select a mutating target, so a
// wrong cwd can never become a valid mutating admission.
//
// Binding chain — every step must describe the same repository before any
// mutation is authorized:
//
//	explicitly requested repository (locator)
//	  -> canonical requested path (symlinks resolved)
//	  -> resolved Git toplevel / common-dir
//	  -> expected repository identity (gitx.RepoID(commonDir))
//	  -> owned task worktree parent identity
func resolveRepository(repoLocator string, env *envelope.Envelope) (*records, *RunError) {
	ctx := context.Background()

	// 1. The locator must be explicit. An empty locator is a hard refusal:
	//    there is no implicit-cwd fallback for mutating execution.
	if repoLocator == "" {
		return nil, runErr(ErrMalformedInput, "a target repository must be specified explicitly; the process working directory is never used as the mutating target")
	}

	// 2. The requested repository path must resolve canonically.
	canon, err := filepath.EvalSymlinks(repoLocator)
	if err != nil {
		return nil, runErr(ErrMalformedInput, "requested repository %q cannot be resolved canonically: %v", repoLocator, err)
	}

	// 3. It must be a valid Git work tree of the supported form.
	if !gitx.IsWorkTree(ctx, canon) {
		return nil, runErr(ErrMalformedInput, "requested repository %q is not inside a git work tree", canon)
	}

	// 4. Resolve the canonical common-dir identity of that repository.
	commonDir, err := gitx.CanonicalCommonDir(ctx, canon)
	if err != nil {
		return nil, runErr(ErrMalformedInput, "cannot resolve canonical common dir for %q: %v", canon, err)
	}

	// 5. Resolve the canonical Git toplevel: the repository actually operated on.
	rootOut, err := gitx.Run(ctx, canon, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, runErr(ErrMalformedInput, "cannot resolve work tree root for %q: %v", canon, err)
	}
	repoRoot, err := filepath.EvalSymlinks(strings.TrimSpace(rootOut))
	if err != nil {
		return nil, runErr(ErrMalformedInput, "cannot canonicalize work tree root for %q: %v", canon, err)
	}

	// 6. Fail closed unless the canonical requested locator and the resolved
	//    toplevel describe the same repository (locator equal to or contained
	//    within the toplevel). Both are realpaths, so this is exact evidence,
	//    not basename branding.
	if !pathWithin(repoRoot, canon) {
		return nil, runErr(ErrMalformedInput, "requested repository %q does not resolve inside its own git toplevel %q", canon, repoRoot)
	}

	// 7. Identity binding: admitted_base must resolve to an exact commit in
	//    THIS repository. An admitted base belonging to another repository is
	//    an identity mismatch and fails before any lease, worktree, ref, or
	//    executor invocation.
	if !gitx.ResolveCommit(ctx, repoRoot, env.AdmittedBase) {
		return nil, runErr(ErrMalformedInput, "admitted_base %s does not resolve to an exact commit in the requested repository %q", env.AdmittedBase, canon)
	}

	ref := candidateRefPrefix + env.RunID
	if !gitx.CheckRefFormat(ctx, ref) {
		return nil, runErr(ErrMalformedInput, "run_id %q does not form a valid git ref name", env.RunID)
	}
	return &records{
		runID:       env.RunID,
		repoLocator: canon,
		commonDir:   commonDir,
		repoRoot:    repoRoot,
		admittedOID: env.AdmittedBase,
		surfaces:    env.WatchSurfaces,
	}, nil
}

// pathWithin reports whether child equals parent or lies strictly inside it.
// Both arguments must already be canonical (realpath) absolute paths.
func pathWithin(parent, child string) bool {
	if parent == child {
		return true
	}
	sep := string(os.PathSeparator)
	return strings.HasPrefix(child, strings.TrimSuffix(parent, sep)+sep)
}

// preflight performs cheap fail-fast checks before any mutation: the cache
// root must be derivable, the run path must be free and unregistered, and the
// candidate ref must not exist.
func preflight(ctx context.Context, cfg Config, rec *records, logger *log.Logger) *RunError {
	cacheRoot := cfg.CacheRoot
	if cacheRoot == "" {
		userCache, err := os.UserCacheDir()
		if err != nil || userCache == "" {
			return runErr(ErrGeneric, "os.UserCacheDir() cannot be resolved: %v", err)
		}
		cacheRoot = filepath.Join(userCache, "global-build")
	}
	wtParent := filepath.Join(cacheRoot, "worktrees", gitx.RepoID(rec.commonDir))
	if err := os.MkdirAll(wtParent, 0o755); err != nil {
		return runErr(ErrGeneric, "cannot create cache directory %s: %v", wtParent, err)
	}
	wtPath := filepath.Join(wtParent, rec.runID)
	if _, err := os.Lstat(wtPath); err == nil {
		return runErr(ErrCandidateValidation, "intended run path already exists: %s", wtPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return runErr(ErrGeneric, "cannot inspect intended run path %s: %v", wtPath, err)
	}
	rec.worktree = gitx.CanonicalPath(wtPath)

	entries, err := gitx.WorktreeListPorcelainZ(ctx, rec.repoRoot)
	if err != nil {
		return runErr(ErrGeneric, "worktree list failed: %v", err)
	}
	for _, e := range entries {
		if e.Path == rec.worktree {
			return runErr(ErrCandidateValidation, "intended run path %s is already registered as a worktree", rec.worktree)
		}
	}

	ref := candidateRefPrefix + rec.runID
	if gitx.RefExists(ctx, rec.repoRoot, ref) {
		return runErr(ErrCandidateValidation, "candidate ref %s already exists; refusing to overwrite", ref)
	}
	hashLen, err := gitx.ObjectFormatLen(ctx, rec.repoRoot)
	if err != nil {
		return runErr(ErrGeneric, "cannot determine object format: %v", err)
	}
	rec.hashLen = hashLen
	return nil
}

// --- attempt execution ------------------------------------------------------

// executeAttempts drives up to maxAttempts invocations plus every terminal
// handling path (result validation, candidate checks, ref creation, cleanup).
func executeAttempts(ctx context.Context, cfg Config, rec *records, env *envelope.Envelope, out *output, logger *log.Logger) terminal {
	malformed := func(format string, args ...any) terminal {
		// Safe cleanup only: identity must still prove ownership.
		if cerr := safeCleanup(ctx, rec, "", logger); cerr != nil {
			logger.Printf("cleanup after malformed result skipped: %v", cerr)
		}
		return terminal{exitCode: ExitMalformedResult, rerr: runErr(ErrMalformedResult, format, args...)}
	}

	for attempt := 1; ; attempt++ {
		trk, corrupt, spawnErr, timeoutKind := invokeWithWatchdog(ctx, cfg, rec, env.Body, logger)

		if spawnErr != nil {
			if cerr := safeCleanup(ctx, rec, "", logger); cerr != nil {
				logger.Printf("cleanup after spawn failure skipped: %v", cerr)
			}
			return terminal{exitCode: ExitRunnerError, rerr: runErr(ErrGeneric, "opencode could not be started: %v", spawnErr)}
		}
		if timeoutKind != "" {
			logger.Printf("attempt terminated by watchdog (%s); no retry is permitted after timeout", timeoutKind)
			if cerr := safeCleanup(ctx, rec, "", logger); cerr != nil {
				// Identity could not be proven: leave untouched, surface it.
				return terminal{exitCode: ExitRunnerError, rerr: cerr}
			}
			return terminal{exitCode: ExitRunnerError, rerr: runErr(ErrTimeout, "watchdog timeout (%s)", timeoutKind)}
		}
		if corrupt {
			// Invalid JSON where valid event data is required: fail closed,
			// never fall back to pretty-output parsing, never retry.
			return malformed("structured NDJSON stream contained invalid JSON")
		}

		text, haveText := trk.TerminalText()
		if !haveText {
			if attempt < maxAttempts && retryEligible(trk) {
				// retryEligible proves the attempt was pre-substantive: zero
				// tool/mutation events and zero substantive model output, so
				// the disposable worktree is still pristine. It must survive:
				// the single retry runs against the very same worktree.
				logger.Print("transient transport failure before substantive execution; retrying once")
				continue
			}
			return malformed("no terminal assistant response could be identified from structured events")
		}

		res, perr := result.Parse(text)
		if perr != nil {
			// Almost-correct protocol text is rejected, never repaired.
			return malformed("final assistant response failed protocol validation: %v", perr)
		}

		switch res.Kind {
		case result.Complete:
			return handleComplete(ctx, rec, env.WatchSurfaces, out, logger)
		case result.Continuable:
			if res.Fields["ADMITTED_BASE"] != rec.admittedOID {
				return malformed("CONTINUABLE ADMITTED_BASE %q does not match the run's admitted base", res.Fields["ADMITTED_BASE"])
			}
			return handleDiscardingState(ctx, rec, res, out, logger, ExitContinuable)
		case result.Blocked:
			return handleDiscardingState(ctx, rec, res, out, logger, ExitBlocked)
		default:
			return malformed("unknown result kind %q", res.Kind)
		}
	}
}

// retryEligible applies the narrow, deterministic transient gate: error event(s)
// clearly matching the transport classifier, with zero substantive model output
// and zero tool/mutation events. Anything less certain is not retried.
func retryEligible(trk *opencode.Tracker) bool {
	if trk.SubstantiveBegan() || trk.ToolEventOccurred() {
		return false
	}
	errs := trk.Errors()
	if len(errs) == 0 {
		return false // nothing classifiable: never guess
	}
	for _, e := range errs {
		if !opencode.IsTransientErrorPayload(e) {
			return false
		}
	}
	return true
}

// invokeWithWatchdog runs one child invocation under the wall-clock and
// progress watchdogs. It returns the tracker, whether the stream was corrupt,
// any spawn failure, and a non-empty watchdog reason when the attempt timed out.
func invokeWithWatchdog(ctx context.Context, cfg Config, rec *records, taskBody string, logger *log.Logger) (trk *opencode.Tracker, corrupt bool, spawnErr error, timeoutKind string) {
	attemptCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	bin := cfg.BinPath
	if bin == "" {
		resolved, err := opencode.Executable(os.Getenv(opencode.EnvBinVar))
		if err != nil {
			return nil, false, err, ""
		}
		bin = resolved
	}

	start := time.Now()
	var invokeErr error
	var mu sync.Mutex // guards liveTracker swaps between the two goroutines
	liveTracker := opencode.NewTracker()

	done := make(chan struct{})
	go func() {
		defer close(done)
		logger.Printf("invoking opencode: %s run --format json --agent %s --dir %s", bin, opencode.AgentName, rec.worktree)
		att, err := opencode.Invoke(attemptCtx, bin, rec.worktree, taskBody, logger.Writer())
		if err != nil || att == nil {
			mu.Lock()
			invokeErr = err
			mu.Unlock()
			return
		}
		corrupt = att.StreamCorrupt
		mu.Lock()
		liveTracker = att.Tracker
		mu.Unlock()
	}()

	watchDone := make(chan string, 1)
	go func() {
		tick := clampDuration(minDuration(cfg.WallClock, cfg.ProgressWindow)/8, 5*time.Millisecond, 500*time.Millisecond)
		ticker := time.NewTicker(tick)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				watchDone <- ""
				return
			case now := <-ticker.C:
				if now.Sub(start) > cfg.WallClock {
					cancel(errWallClock)
					watchDone <- errWallClock.name
					return
				}
				mu.Lock()
				tr := liveTracker
				mu.Unlock()
				if now.Sub(tr.ProgressAt()) > cfg.ProgressWindow {
					cancel(errNoProgress)
					watchDone <- errNoProgress.name
					return
				}
			}
		}
	}()

	<-done
	kind := <-watchDone
	if kind != "" {
		return liveTracker, corrupt, nil, kind
	}
	mu.Lock()
	err := invokeErr
	trkFinal := liveTracker
	mu.Unlock()
	if err != nil {
		return trkFinal, corrupt, err, ""
	}
	return trkFinal, corrupt, nil, ""
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func clampDuration(d, lo, hi time.Duration) time.Duration {
	if d < lo {
		return lo
	}
	if d > hi {
		return hi
	}
	return d
}

// --- terminal-state handlers -------------------------------------------------

// handleComplete validates the COMPLETE candidate deterministically, creates
// the candidate ref with compare-and-swap semantics, then removes the worktree.
func handleComplete(ctx context.Context, rec *records, surfaces []string, out *output, logger *log.Logger) terminal {
	failCV := func(format string, args ...any) terminal {
		// Candidate validation failed before any ref exists: the disposable
		// worktree may be removed only while identity still proves ownership.
		if cerr := safeCleanup(ctx, rec, "", logger); cerr != nil {
			logger.Printf("post-failure cleanup skipped: %v", cerr)
		}
		return terminal{exitCode: ExitRunnerError, rerr: runErr(ErrCandidateValidation, format, args...)}
	}

	headOID, detached, err := gitx.HeadState(ctx, rec.worktree)
	if err != nil {
		if _, statErr := os.Lstat(rec.worktree); statErr != nil {
			return terminal{exitCode: ExitRunnerError, rerr: runErr(ErrWorktreeIdentityMismatch, "worktree vanished before verification: %v", statErr)}
		}
		return failCV("cannot read worktree HEAD: %v", err)
	}
	if !detached {
		return failCV("worktree HEAD is not detached")
	}

	// Full ownership proof against the measured candidate before trusting it.
	if rerr := proveOwnership(ctx, rec, headOID); rerr != nil {
		return terminal{exitCode: ExitRunnerError, rerr: rerr}
	}

	if !gitx.ResolveCommit(ctx, rec.repoRoot, headOID) {
		return failCV("candidate HEAD %s does not resolve to a commit", headOID)
	}
	if headOID == rec.admittedOID {
		return failCV("candidate equals admitted base; nothing was built")
	}
	ancestor, err := gitx.IsAncestor(ctx, rec.repoRoot, rec.admittedOID, headOID)
	if err != nil {
		return failCV("ancestor check failed: %v", err)
	}
	if !ancestor {
		return failCV("admitted base %s is not an ancestor of candidate %s", rec.admittedOID, headOID)
	}
	clean, raw, err := gitx.StatusClean(ctx, rec.worktree)
	if err != nil {
		return failCV("status check failed: %v", err)
	}
	if !clean {
		return failCV("worktree is dirty at COMPLETE; uncommitted/untracked mutation remains: %q", raw)
	}
	changed, err := gitx.DiffNameOnlyZ(ctx, rec.repoRoot, rec.admittedOID, headOID)
	if err != nil {
		return failCV("changed-path computation failed: %v", err)
	}
	ok, outside := watches.New(surfaces).CoversAll(changed)
	if !ok {
		return failCV("changed paths outside WATCH_SURFACES upper bound: %s", strings.Join(outside, ", "))
	}

	ref := candidateRefPrefix + rec.runID
	if err := gitx.UpdateRefCAS(ctx, rec.repoRoot, ref, headOID, rec.hashLen); err != nil {
		return failCV("candidate ref creation failed (must not overwrite): %v", err)
	}
	refOut, err := gitx.Run(ctx, rec.repoRoot, "rev-parse", "--verify", ref)
	if err != nil || strings.TrimSpace(refOut) != headOID {
		return failCV("candidate ref %s does not resolve to the verified candidate", ref)
	}

	// Re-prove ownership immediately before destructive removal, then remove
	// only this owned worktree and release only its lease.
	if rerr := removeOwnedWorktree(ctx, rec, headOID, logger); rerr != nil {
		return terminal{exitCode: ExitRunnerError, rerr: rerr}
	}

	sort.Strings(changed)
	out.set("CANDIDATE", headOID)
	out.set("CANDIDATE_REF", ref)
	out.set("CHANGED_SURFACE", strings.Join(changed, ","))
	logger.Printf("COMPLETE verified: candidate %s reachable via %s", headOID, ref)
	return terminal{exitCode: ExitComplete}
}

// handleDiscardingState finalizes CONTINUABLE / BLOCKED outcomes: validated
// checkpoint fields are printed, nothing is preserved, worktree is removed.
func handleDiscardingState(ctx context.Context, rec *records, res *result.Result, out *output, logger *log.Logger, exitCode int) terminal {
	if rerr := removeOwnedWorktree(ctx, rec, "", logger); rerr != nil {
		return terminal{exitCode: ExitRunnerError, rerr: rerr}
	}
	for _, k := range res.Keys {
		out.set(k, res.Fields[k])
	}
	return terminal{exitCode: exitCode}
}

// --- ownership proof ---------------------------------------------------------

// proveOwnership re-queries Git-native worktree metadata (NUL-safe) and proves
// this process still owns the exact worktree. When expectedHead is empty the
// HEAD fact degrades to registration consistency (live HEAD must match the
// registered value); otherwise exact equality is enforced.
func proveOwnership(ctx context.Context, rec *records, expectedHead string) *RunError {
	mismatch := func(format string, args ...any) *RunError {
		return runErr(ErrWorktreeIdentityMismatch, format, args...)
	}

	fi, err := os.Lstat(rec.worktree)
	if err != nil || !fi.IsDir() {
		return mismatch("expected worktree path %s does not exist as a directory: %v", rec.worktree, err)
	}
	if canon := gitx.CanonicalPath(rec.worktree); canon != rec.worktree {
		return mismatch("canonical worktree path %s differs from recorded path %s", canon, rec.worktree)
	}

	entries, err := gitx.WorktreeListPorcelainZ(ctx, rec.repoRoot)
	if err != nil {
		return mismatch("worktree metadata query failed: %v", err)
	}
	var found []gitx.WorktreeEntry
	for _, e := range entries {
		if e.Path == rec.worktree {
			found = append(found, e)
		}
	}
	if len(found) != 1 {
		return mismatch("worktree registration for %s is ambiguous or missing (%d entries)", rec.worktree, len(found))
	}
	entry := found[0]
	if !entry.Detached {
		return mismatch("worktree %s is not in the expected detached state", rec.worktree)
	}

	commonDir, err := gitx.CanonicalCommonDir(ctx, rec.worktree)
	if err != nil || commonDir != rec.commonDir {
		return mismatch("common-dir identity mismatch: recorded %s, observed %s (err=%v)", rec.commonDir, commonDir, err)
	}

	headOID, _, err := gitx.HeadState(ctx, rec.worktree)
	if err != nil {
		return mismatch("cannot read HEAD inside worktree: %v", err)
	}
	if headOID != entry.Head {
		return mismatch("live HEAD %s disagrees with registered HEAD %s", headOID, entry.Head)
	}
	if expectedHead != "" && headOID != expectedHead {
		return mismatch("HEAD %s does not match the expected state %s", headOID, expectedHead)
	}
	return nil
}

// safeCleanup removes the disposable worktree only while every deterministic
// identity fact agrees. Any doubt leaves the worktree untouched.
func safeCleanup(ctx context.Context, rec *records, expectedHead string, logger *log.Logger) *RunError {
	return removeOwnedWorktree(ctx, rec, expectedHead, logger)
}

// establishLeaseAndWorktree creates the liveness lease (exclusive advisory lock
// held for the whole attempt) and the detached, locked disposable worktree whose
// lock reason is the durable ownership marker.
func establishLeaseAndWorktree(ctx context.Context, cfg Config, rec *records, logger *log.Logger) *RunError {
	rec.repoID = gitx.RepoID(rec.commonDir)
	rec.reason = ownership.BuildReason(rec.repoID, rec.runID, rec.admittedOID)

	cacheRoot := resolveCacheRoot(cfg)
	if cacheRoot == "" {
		return runErr(ErrGeneric, "cannot resolve cache root for liveness lease")
	}
	lease := ownership.NewLease(cacheRoot, rec.repoID, rec.runID)
	id := ownership.LeaseIdentity{
		Version:      ownership.LeaseVersion,
		RepoID:       rec.repoID,
		RunID:        rec.runID,
		AdmittedBase: rec.admittedOID,
	}
	if err := lease.Establish(id); err != nil {
		return runErr(ErrGeneric, "cannot establish run lease: %v", err)
	}
	rec.lease = lease

	if err := gitx.WorktreeAddDetachLock(ctx, rec.repoRoot, rec.worktree, rec.admittedOID, rec.reason); err != nil {
		_ = lease.Release()
		_ = lease.Remove()
		rec.lease = nil
		return runErr(ErrGeneric, "worktree add failed: %v", err)
	}
	return nil
}

// proveOwnershipMarker verifies the live worktree carries exactly the expected
// global-build ownership lock reason and that it agrees with the run identity.
func proveOwnershipMarker(ctx context.Context, rec *records) *RunError {
	entries, err := gitx.WorktreeListPorcelainZ(ctx, rec.repoRoot)
	if err != nil {
		return runErr(ErrWorktreeIdentityMismatch, "worktree metadata query failed: %v", err)
	}
	var found *gitx.WorktreeEntry
	for i := range entries {
		if entries[i].Path == rec.worktree {
			found = &entries[i]
			break
		}
	}
	if found == nil {
		return runErr(ErrWorktreeIdentityMismatch, "worktree %s is not registered", rec.worktree)
	}
	if !found.Locked {
		return runErr(ErrWorktreeIdentityMismatch, "worktree %s is not locked", rec.worktree)
	}
	reason, perr := ownership.ParseReason(found.LockReason)
	if perr != nil {
		return runErr(ErrWorktreeIdentityMismatch, "lock reason is not valid global-build ownership metadata: %v", perr)
	}
	if merr := reason.Matches(rec.repoID, rec.runID, rec.admittedOID); merr != nil {
		return runErr(ErrWorktreeIdentityMismatch, "%v", merr)
	}
	return nil
}

// removeOwnedWorktree performs the exact Slice-2 destructive removal contract:
// prove ownership + marker, unlock only the exact worktree, re-prove identity,
// remove only with `git worktree remove --force <exact-path>`, verify the
// registration is gone, then remove only this run's lease pathname and release
// the lease. Any doubt leaks nothing and leaves the worktree untouched.
func removeOwnedWorktree(ctx context.Context, rec *records, expectedHead string, logger *log.Logger) *RunError {
	if rerr := proveOwnership(ctx, rec, expectedHead); rerr != nil {
		return rerr
	}
	if rerr := proveOwnershipMarker(ctx, rec); rerr != nil {
		return rerr
	}
	if err := gitx.WorktreeUnlock(ctx, rec.repoRoot, rec.worktree); err != nil {
		return runErr(ErrGeneric, "worktree unlock failed: %v", err)
	}
	// Re-read metadata and re-prove exact identity (path/repo/common-dir/HEAD).
	if rerr := proveOwnership(ctx, rec, expectedHead); rerr != nil {
		return rerr
	}
	if err := gitx.WorktreeRemoveForce(ctx, rec.repoRoot, rec.worktree); err != nil {
		return runErr(ErrGeneric, "verified candidate preserved but worktree removal failed: %v", err)
	}
	if stillRegistered(ctx, rec.repoRoot, rec.worktree) {
		return runErr(ErrGeneric, "worktree %s still registered after removal", rec.worktree)
	}
	if rec.lease != nil {
		if err := rec.lease.Remove(); err != nil && !os.IsNotExist(err) {
			logger.Printf("lease path removal warning: %v", err)
		}
		if err := rec.lease.Release(); err != nil {
			logger.Printf("lease release warning: %v", err)
		}
	}
	logger.Printf("disposable worktree removed: %s", rec.worktree)
	return nil
}

// stillRegistered reports whether a worktree path is still present in the
// repository's registration after removal.
func stillRegistered(ctx context.Context, repoDir, path string) bool {
	entries, err := gitx.WorktreeListPorcelainZ(ctx, repoDir)
	if err != nil {
		return true // uncertain: treat as still present to avoid a false success
	}
	for _, e := range entries {
		if e.Path == path {
			return true
		}
	}
	return false
}
