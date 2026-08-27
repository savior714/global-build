// Package publish implements the deterministic candidate publication command:
//
//	global-build publish --repo <path> --run-id <run-id> \
//	    --candidate <full-oid> --admitted-base <full-oid> \
//	    --watch <surface> [--watch <surface> ...]
//
// It proves the live origin/main state with a single narrow non-mutating Git
// helper, performs exactly one non-force push when eligible, and deletes the
// candidate ref only with an expected-old compare-and-swap. It never force
// pushes, never recovers topology, never deletes remote refs, and never
// rewrites unrelated history.
//
// This is the only responsibility of the package. There is no orchestration,
// no queue, no scheduler, and no semantic review.
package publish

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"global-build/internal/gitx"
	"global-build/internal/ownership"
	"global-build/internal/watches"
)

// Stable PUBLISH_RESULT values.
const (
	ResultPublished           = "PUBLISHED"
	ResultAlreadyPublished    = "ALREADY_PUBLISHED"
	ResultStopped             = "STOPPED"
	ResultError               = "ERROR"
	ResultPublishedRefPreserv = "PUBLISHED_REF_PRESERVED"
)

// Stable ERROR_KIND values for publish.
const (
	ErrMalformedArgument      = "MALFORMED_ARGUMENT"
	ErrCandidateValidation    = "CANDIDATE_VALIDATION_FAILED"
	ErrRemoteMainUnresolvable = "REMOTE_MAIN_UNRESOLVABLE"
	ErrMainMoved              = "MAIN_MOVED"
	ErrPushRejectedOrRaced    = "PUSH_REJECTED_OR_RACED"
	ErrRemotePostcondition    = "REMOTE_POSTCONDITION_MISMATCH"
	ErrAmbiguousConcurrent    = "AMBIGUOUS_CONCURRENT_PUBLICATION"
	ErrCandidateRefChanged    = "CANDIDATE_REF_CHANGED"
)

// Exit codes (publish only uses 0/20/40).
const (
	ExitPublished = 0
	ExitStopped   = 20
	ExitError     = 40
)

// candidateRefPrefix is the exact candidate ref namespace.
const candidateRefPrefix = "refs/build-candidates/"

// Config is the injected publish configuration.
type Config struct {
	Repo         string
	RunID        string
	Candidate    string
	AdmittedBase string
	Watches      []string
	Out          io.Writer
	Err          io.Writer
}

// Outcome is the full structured publish result.
type Outcome struct {
	PublishResult string
	ErrorKind     string
	ExitCode      int
	Fields        map[string]string
}

// Run executes the publish command, emits the deterministic stdout keys and
// diagnostics to stderr, and returns the process exit code.
func Run(ctx context.Context, cfg Config) int {
	if cfg.Out == nil {
		cfg.Out = io.Discard
	}
	if cfg.Err == nil {
		cfg.Err = io.Discard
	}
	o := Compute(ctx, cfg)
	emit(cfg.Out, o)
	if o.ErrorKind != "" {
		fmt.Fprintf(cfg.Err, "global-build publish: %s (%s)\n", o.PublishResult, o.ErrorKind)
	}
	return o.ExitCode
}

// Compute performs the deterministic publish decision and returns the structured
// Outcome without emitting. Tests assert against this directly.
func Compute(ctx context.Context, cfg Config) Outcome {
	o := Outcome{Fields: map[string]string{}}

	if cfg.Out == nil {
		cfg.Out = io.Discard
	}
	if cfg.Err == nil {
		cfg.Err = io.Discard
	}

	// --- argument validation (fail closed) --------------------------------
	if cfg.Repo == "" || strings.HasPrefix(cfg.Repo, "-") {
		return malformed(o, "malformed repository path %q", cfg.Repo)
	}
	if !ownership.ValidRunID(cfg.RunID) {
		return malformed(o, "unsafe run-id %q", cfg.RunID)
	}
	if !gitx.IsFullOID(cfg.Candidate) {
		return malformed(o, "candidate %q is not a full git object id", cfg.Candidate)
	}
	if !gitx.IsFullOID(cfg.AdmittedBase) {
		return malformed(o, "admitted-base %q is not a full git object id", cfg.AdmittedBase)
	}
	norm, werr := normalizeWatches(cfg.Watches)
	if werr != nil {
		return malformed(o, "watch surfaces: %v", werr)
	}

	// --- repository resolution --------------------------------------------
	repoDir, rerr := resolveRepo(ctx, cfg.Repo)
	if rerr != nil {
		return malformed(o, "cannot resolve repository: %v", rerr)
	}

	// Require remote `origin` (never silently select another remote).
	if !remoteExists(ctx, repoDir, "origin") {
		o.Fields["ERROR_KIND"] = ErrRemoteMainUnresolvable
		o.PublishResult = ResultError
		o.ExitCode = ExitError
		return o
	}

	o.Fields["RUN_ID"] = cfg.RunID
	o.Fields["ADMITTED_BASE"] = cfg.AdmittedBase
	o.Fields["CANDIDATE"] = cfg.Candidate
	candRef := candidateRefPrefix + cfg.RunID
	o.Fields["CANDIDATE_REF"] = candRef

	// --- preflight: local candidate validation (no network mutation) -------
	if rerr := preflight(ctx, repoDir, cfg, candRef, norm); rerr != nil {
		o.Fields["ERROR_KIND"] = ErrCandidateValidation
		o.PublishResult = ResultError
		o.ExitCode = ExitError
		o.Fields["ERROR_DETAIL"] = rerr.Error()
		return o
	}

	// --- live remote check (non-mutating) ----------------------------------
	remoteBefore, rerr2 := gitx.ReadRemoteBranchOID(ctx, repoDir, "origin", "main")
	if rerr2 != nil {
		o.Fields["ERROR_KIND"] = ErrRemoteMainUnresolvable
		o.PublishResult = ResultError
		o.ExitCode = ExitError
		o.Fields["ERROR_DETAIL"] = rerr2.Error()
		return o
	}
	o.Fields["REMOTE_BEFORE"] = remoteBefore

	// Candidate already live: no push, proceed to exact candidate-ref cleanup.
	if remoteBefore == cfg.Candidate {
		o.Fields["REMOTE_AFTER"] = remoteBefore
		return finishDelete(o, ctx, repoDir, candRef, cfg.Candidate, ResultAlreadyPublished)
	}

	// Candidate eligible for exactly one non-force push attempt.
	if remoteBefore == cfg.AdmittedBase {
		return attemptPush(o, ctx, repoDir, candRef, cfg.Candidate)
	}

	// Otherwise live main has moved to some unrelated commit.
	o.Fields["REMOTE_AFTER"] = remoteBefore
	o.Fields["PUBLISH_RESULT"] = ResultStopped
	o.Fields["ERROR_KIND"] = ErrMainMoved
	o.ExitCode = ExitStopped
	return o
}

// preflight proves the local candidate facts before any network mutation.
func preflight(ctx context.Context, repoDir string, cfg Config, candRef string, norm []string) error {
	if !gitx.ResolveCommit(ctx, repoDir, cfg.Candidate) {
		return fmt.Errorf("candidate %s is not an exact commit", cfg.Candidate)
	}
	if !gitx.ResolveCommit(ctx, repoDir, cfg.AdmittedBase) {
		return fmt.Errorf("admitted-base %s is not an exact commit", cfg.AdmittedBase)
	}
	if cfg.Candidate == cfg.AdmittedBase {
		return fmt.Errorf("candidate equals admitted-base; nothing to publish")
	}
	if !gitx.RefExists(ctx, repoDir, candRef) {
		return fmt.Errorf("candidate ref %s does not exist", candRef)
	}
	refOID, ok := refOIDExact(ctx, repoDir, candRef)
	if !ok || refOID != cfg.Candidate {
		return fmt.Errorf("candidate ref %s does not point to candidate %s", candRef, cfg.Candidate)
	}
	ancestor, err := gitx.IsAncestor(ctx, repoDir, cfg.AdmittedBase, cfg.Candidate)
	if err != nil {
		return fmt.Errorf("ancestor check failed: %v", err)
	}
	if !ancestor {
		return fmt.Errorf("admitted-base %s is not an ancestor of candidate %s", cfg.AdmittedBase, cfg.Candidate)
	}
	merges, err := gitx.MergeCountInRange(ctx, repoDir, cfg.AdmittedBase, cfg.Candidate)
	if err != nil {
		return fmt.Errorf("merge check failed: %v", err)
	}
	if merges > 0 {
		return fmt.Errorf("candidate range contains a merge commit")
	}
	count, err := gitx.RevListCount(ctx, repoDir, cfg.AdmittedBase, cfg.Candidate)
	if err != nil {
		return fmt.Errorf("commit count failed: %v", err)
	}
	if count != 1 {
		return fmt.Errorf("candidate range must contain exactly one commit, found %d", count)
	}
	changed, err := gitx.DiffNameOnlyZ(ctx, repoDir, cfg.AdmittedBase, cfg.Candidate)
	if err != nil {
		return fmt.Errorf("changed-path computation failed: %v", err)
	}
	for _, p := range changed {
		if p == "" || strings.Contains(p, "\x00") || strings.HasPrefix(p, "/") {
			return fmt.Errorf("malformed changed path %q", p)
		}
	}
	ok2, outside := watches.New(norm).CoversAll(changed)
	if !ok2 {
		return fmt.Errorf("changed paths outside WATCH_SURFACES upper bound: %s", strings.Join(outside, ", "))
	}
	return nil
}

// attemptPush performs exactly one ordinary non-force push and resolves the
// outcome from the push exit code and the re-read live main.
func attemptPush(o Outcome, ctx context.Context, repoDir, candRef, candidate string) Outcome {
	code, _ := doPush(ctx, repoDir, candidate)
	remoteAfter, rerr := gitx.ReadRemoteBranchOID(ctx, repoDir, "origin", "main")
	if rerr != nil {
		// If we cannot re-read the remote after the push, treat the outcome as
		// unsafe and preserve the candidate ref.
		o.Fields["REMOTE_AFTER"] = ""
		o.Fields["PUBLISH_RESULT"] = ResultError
		o.Fields["ERROR_KIND"] = ErrRemoteMainUnresolvable
		o.ExitCode = ExitError
		return o
	}
	o.Fields["REMOTE_AFTER"] = remoteAfter

	switch {
	case code == 0 && remoteAfter == candidate:
		return finishDelete(o, ctx, repoDir, candRef, candidate, ResultPublished)
	case code == 0 && remoteAfter != candidate:
		o.Fields["PUBLISH_RESULT"] = ResultError
		o.Fields["ERROR_KIND"] = ErrRemotePostcondition
		o.ExitCode = ExitError
	case code != 0 && remoteAfter != candidate:
		o.Fields["PUBLISH_RESULT"] = ResultStopped
		o.Fields["ERROR_KIND"] = ErrPushRejectedOrRaced
		o.ExitCode = ExitStopped
	case code != 0 && remoteAfter == candidate:
		o.Fields["PUBLISH_RESULT"] = ResultError
		o.Fields["ERROR_KIND"] = ErrAmbiguousConcurrent
		o.ExitCode = ExitError
	}
	return o
}

// finishDelete deletes the candidate ref via expected-old CAS and classifies
// the result. On CAS failure the ref is preserved and the result is
// PUBLISHED_REF_PRESERVED.
func finishDelete(o Outcome, ctx context.Context, repoDir, candRef, candidate, successResult string) Outcome {
	if err := gitx.DeleteRefCAS(ctx, repoDir, candRef, candidate); err != nil {
		o.Fields["PUBLISH_RESULT"] = ResultPublishedRefPreserv
		o.Fields["ERROR_KIND"] = ErrCandidateRefChanged
		o.ExitCode = ExitError
		return o
	}
	if gitx.RefExists(ctx, repoDir, candRef) {
		o.Fields["PUBLISH_RESULT"] = ResultPublishedRefPreserv
		o.Fields["ERROR_KIND"] = ErrCandidateRefChanged
		o.ExitCode = ExitError
		return o
	}
	o.Fields["CANDIDATE_REF_DELETED"] = "YES"
	o.Fields["PUBLISH_RESULT"] = successResult
	o.ExitCode = ExitPublished
	return o
}

// doPush performs exactly one ordinary push. It never uses a leading '+',
// --force, or --force-with-lease. The returned code is the git push exit code.
func doPush(ctx context.Context, repoDir, candidate string) (int, string) {
	code, out, err := gitx.RunExit(ctx, repoDir, "push", "--porcelain", "origin", candidate+":refs/heads/main")
	if err != nil {
		return -1, out
	}
	return code, out
}

// refOIDExact returns the exact OID a ref points to.
func refOIDExact(ctx context.Context, repoDir, ref string) (string, bool) {
	out, err := gitx.Run(ctx, repoDir, "rev-parse", "--verify", ref)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(out), true
}

func remoteExists(ctx context.Context, repoDir, remote string) bool {
	code, _, err := gitx.RunExit(ctx, repoDir, "remote", "get-url", remote)
	return err == nil && code == 0
}

func resolveRepo(ctx context.Context, repoArg string) (string, error) {
	commonDir, err := gitx.CanonicalCommonDir(ctx, repoArg)
	if err != nil {
		return "", fmt.Errorf("cannot resolve canonical common dir: %v", err)
	}
	return commonDir, nil
}

func malformed(o Outcome, format string, args ...any) Outcome {
	o.Fields["ERROR_KIND"] = ErrMalformedArgument
	o.PublishResult = ResultError
	o.ExitCode = ExitError
	o.Fields["ERROR_DETAIL"] = fmt.Sprintf(format, args...)
	return o
}

// emit writes the deterministic stdout keys in a fixed order, including only
// present (non-empty) values.
func emit(out io.Writer, o Outcome) {
	order := []string{
		"PUBLISH_RESULT",
		"ERROR_KIND",
		"RUN_ID",
		"ADMITTED_BASE",
		"CANDIDATE",
		"REMOTE_BEFORE",
		"REMOTE_AFTER",
		"CANDIDATE_REF",
		"CANDIDATE_REF_DELETED",
	}
	var b strings.Builder
	for _, k := range order {
		v, ok := o.Fields[k]
		if !ok || v == "" {
			continue
		}
		b.WriteString(k + ": " + v + "\n")
	}
	fmt.Fprint(out, b.String())
}

// normalizeWatches validates and normalizes watch surfaces to the same exact
// semantics as the runner envelope (exact path or trailing-slash directory
// prefix). It fails closed on any malformed surface.
func normalizeWatches(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, fmt.Errorf("zero watch surfaces supplied")
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		n, err := watches.Normalize(s)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

// SortedJoin is a deterministic changed-path serialization (exported for tests).
func SortedJoin(paths []string) string {
	cp := append([]string(nil), paths...)
	sort.Strings(cp)
	return strings.Join(cp, ",")
}
