// Package freshness implements the deterministic continuation freshness command:
//
//	global-build continuation-check --repo <path> --base <full-oid> \
//	    --watch <surface> [--watch <surface> ...]
//
// It is read-only. It reads the live origin/main with the same narrow
// non-mutating helper the publisher uses and computes control-plane freshness
// facts only: whether the base is unchanged, advanced without overlap, advanced
// with overlap, or has diverged/been rewritten.
//
// It does NOT consume the natural-language CONTINUABLE checkpoint, does NOT
// perform semantic review, and does NOT mutate any ref, worktree, index,
// candidate ref, remote ref, or lease. Ownership of COMPLETED / REMAINING /
// NEXT_ACTION / semantic overlap interpretation remains with the Frontier.
package freshness

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"global-build/internal/gitx"
	"global-build/internal/watches"
)

// Stable FRESHNESS_RESULT values.
const (
	ResultBaseUnchanged        = "BASE_UNCHANGED"
	ResultAdvancedNoOverlap    = "ADVANCED_NO_OVERLAP"
	ResultOverlapReview        = "OVERLAP_REVIEW_REQUIRED"
	ResultHistoryReview       = "HISTORY_REVIEW_REQUIRED"
	ResultError                = "ERROR"
)

// Stable ERROR_KIND values for freshness.
const (
	ErrMalformedArgument       = "MALFORMED_ARGUMENT"
	ErrInvalidBase             = "INVALID_BASE"
	ErrRemoteMainUnresolvable  = "REMOTE_MAIN_UNRESOLVABLE"
	ErrRepoUnresolvable        = "REPO_UNRESOLVABLE"
	ErrGitFailure              = "GIT_FAILURE"
)

// Exit codes (freshness uses 0/10/40).
const (
	ExitUnchanged = 0
	ExitReview    = 10
	ExitError     = 40
)

// Config is the injected continuation-check configuration.
type Config struct {
	Repo    string
	Base    string
	Watches []string
	Out     io.Writer
	Err     io.Writer
}

// Outcome is the full structured freshness result.
type Outcome struct {
	FreshnessResult string
	ErrorKind       string
	ExitCode        int
	Fields          map[string]string
}

// Run executes the continuation-check command, emits the deterministic stdout
// keys and diagnostics to stderr, and returns the process exit code.
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
		fmt.Fprintf(cfg.Err, "global-build continuation-check: %s (%s)\n", o.FreshnessResult, o.ErrorKind)
	}
	return o.ExitCode
}

// Compute performs the deterministic freshness classification and returns the
// structured Outcome without emitting. Tests assert against this directly.
func Compute(ctx context.Context, cfg Config) Outcome {
	o := Outcome{Fields: map[string]string{}}

	if cfg.Out == nil {
		cfg.Out = io.Discard
	}
	if cfg.Err == nil {
		cfg.Err = io.Discard
	}

	if cfg.Repo == "" || strings.HasPrefix(cfg.Repo, "-") {
		o.set(ResultError, ErrMalformedArgument, ExitError)
		o.Fields["ERROR_DETAIL"] = fmt.Sprintf("malformed repository path %q", cfg.Repo)
		return o
	}
	if !gitx.IsFullOID(cfg.Base) {
		o.set(ResultError, ErrMalformedArgument, ExitError)
		o.Fields["ERROR_DETAIL"] = fmt.Sprintf("base %q is not a full git object id", cfg.Base)
		return o
	}
	norm, werr := normalizeWatches(cfg.Watches)
	if werr != nil {
		o.set(ResultError, ErrMalformedArgument, ExitError)
		o.Fields["ERROR_DETAIL"] = fmt.Sprintf("watch surfaces: %v", werr)
		return o
	}

	repoDir, rerr := resolveRepo(ctx, cfg.Repo)
	if rerr != nil {
		o.set(ResultError, ErrRepoUnresolvable, ExitError)
		o.Fields["ERROR_DETAIL"] = rerr.Error()
		return o
	}

	if !gitx.ResolveCommit(ctx, repoDir, cfg.Base) {
		o.set(ResultError, ErrInvalidBase, ExitError)
		o.Fields["ERROR_DETAIL"] = fmt.Sprintf("base %s does not resolve to an exact commit", cfg.Base)
		return o
	}

	o.Fields["BASE"] = cfg.Base

	latest, rerr2 := gitx.ReadRemoteBranchOID(ctx, repoDir, "origin", "main")
	if rerr2 != nil {
		o.set(ResultError, ErrRemoteMainUnresolvable, ExitError)
		o.Fields["ERROR_DETAIL"] = rerr2.Error()
		return o
	}
	o.Fields["LATEST_MAIN"] = latest

	// Base unchanged: live main already equals base.
	if latest == cfg.Base {
		o.Fields["NEW_ADMITTED_BASE"] = cfg.Base
		o.set(ResultBaseUnchanged, "", ExitUnchanged)
		return o
	}

	// Determine ancestry. IsAncestor returns true when base is an ancestor of
	// (or equal to) latest; equality was handled above, so true => strict
	// ancestor.
	ancestor, err := gitx.IsAncestor(ctx, repoDir, cfg.Base, latest)
	if err != nil {
		o.set(ResultError, ErrGitFailure, ExitError)
		o.Fields["ERROR_DETAIL"] = fmt.Sprintf("ancestor check failed: %v", err)
		return o
	}

	o.Fields["NEW_ADMITTED_BASE"] = latest

	if !ancestor {
		// base is not an ancestor of latest: divergence or rewrite.
		o.Fields["NEW_ADMITTED_BASE"] = "NONE"
		o.set(ResultHistoryReview, "", ExitReview)
		return o
	}

	// latest is a strict descendant of base: inspect changed paths.
	changed, err := gitx.DiffNameOnlyZ(ctx, repoDir, cfg.Base, latest)
	if err != nil {
		o.set(ResultError, ErrGitFailure, ExitError)
		o.Fields["ERROR_DETAIL"] = fmt.Sprintf("changed-path computation failed: %v", err)
		return o
	}
	o.Fields["CHANGED_SURFACE"] = sortedJoin(changed)

	set := watches.New(norm)
	var overlapping []string
	for _, p := range changed {
		if p == "" || strings.Contains(p, "\x00") || strings.HasPrefix(p, "/") {
			o.set(ResultError, ErrGitFailure, ExitError)
			o.Fields["ERROR_DETAIL"] = fmt.Sprintf("malformed changed path %q", p)
			return o
		}
		if set.Contains(p) {
			overlapping = append(overlapping, p)
		}
	}

	if len(overlapping) == 0 {
		o.Fields["OVERLAPPING_SURFACE"] = "NONE"
		o.set(ResultAdvancedNoOverlap, "", ExitUnchanged)
		return o
	}

	o.Fields["OVERLAPPING_SURFACE"] = sortedJoin(overlapping)
	o.set(ResultOverlapReview, "", ExitReview)
	return o
}

func resolveRepo(ctx context.Context, repoArg string) (string, error) {
	commonDir, err := gitx.CanonicalCommonDir(ctx, repoArg)
	if err != nil {
		return "", fmt.Errorf("cannot resolve canonical common dir: %v", err)
	}
	return commonDir, nil
}

// set records both the structured Outcome fields and the deterministic
// FRESHNESS_RESULT / ERROR_KIND emit fields so callers (tests, future tooling)
// can read either representation.
func (o *Outcome) set(result, errKind string, exit int) {
	if result != "" {
		o.FreshnessResult = result
		o.Fields["FRESHNESS_RESULT"] = result
	}
	if errKind != "" {
		o.ErrorKind = errKind
		o.Fields["ERROR_KIND"] = errKind
	}
	o.ExitCode = exit
}

// emit writes the deterministic stdout keys in a fixed order, including only
// present (non-empty) values.
func emit(out io.Writer, o Outcome) {
	order := []string{
		"FRESHNESS_RESULT",
		"ERROR_KIND",
		"BASE",
		"LATEST_MAIN",
		"NEW_ADMITTED_BASE",
		"CHANGED_SURFACE",
		"OVERLAPPING_SURFACE",
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

func sortedJoin(paths []string) string {
	cp := append([]string(nil), paths...)
	sort.Strings(cp)
	return strings.Join(cp, ",")
}
