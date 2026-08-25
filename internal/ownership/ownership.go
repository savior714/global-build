// Package ownership defines the durable, machine-parseable ownership evidence
// and the ephemeral liveness lease that let the explicit cleanup command
// distinguish a runner's own worktree, another live session, a crashed orphan,
// a user-created worktree, and ambiguous legacy residue.
//
// Two independent proofs are required before any destructive action:
//
//  1. A durable ownership marker recorded as the Git worktree lock reason,
//     grammar:  global-build:v1:repo=<repo-id>:run=<run-id>:base=<full-oid>
//  2. A per-run liveness lease (an advisory OS file lock over a deterministic
//     path carrying only bounded identity data). The lock proves process
//     liveness; the leftover pathname alone never does.
//
// Both parsers fail closed on any deviation.
package ownership

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// ReasonVersion is the supported ownership-reason grammar version.
const ReasonVersion = "v1"

// LeaseVersion is the supported liveness-lease identity version.
const LeaseVersion = "v1"

// Reason is the parsed durable ownership marker.
type Reason struct {
	Version      string
	RepoID       string
	RunID        string
	AdmittedBase string
}

var (
	reasonOIDRe  = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)
	reasonRepoRe = regexp.MustCompile(`^[0-9a-f]{16}$`)
	// runIDRe mirrors the envelope run_id grammar (kept local so the ownership
	// surface does not depend on the envelope package).
	runIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

// BuildReason produces the deterministic machine-parseable ownership reason.
func BuildReason(repoID, runID, admittedBase string) string {
	return "global-build:" + ReasonVersion + ":repo=" + repoID + ":run=" + runID + ":base=" + admittedBase
}

// ValidRunID reports whether s is a safe run-id (the same grammar the envelope
// enforces). It rejects empty, over-long, leading-dash, space-containing, ".."
// escape, and ".lock"/"." suffix forms.
func ValidRunID(s string) bool {
	if !runIDRe.MatchString(s) {
		return false
	}
	if strings.Contains(s, "..") || strings.HasSuffix(s, ".lock") || strings.HasSuffix(s, ".") {
		return false
	}
	return true
}

// ParseReason parses the ownership marker and fails closed on any structural
// deviation: unsupported version, duplicate field, missing field, extra field,
// malformed full OID, unsafe run-id, or malformed repo-id.
func ParseReason(s string) (Reason, error) {
	var zero Reason
	parts := strings.Split(s, ":")
	if len(parts) != 5 {
		return zero, fmt.Errorf("ownership: reason has %d colon fields, want exactly 5", len(parts))
	}
	if parts[0] != "global-build" {
		return zero, fmt.Errorf("ownership: reason must start with 'global-build'")
	}
	if parts[1] != ReasonVersion {
		return zero, fmt.Errorf("ownership: unsupported reason version %q", parts[1])
	}
	fields := map[string]string{}
	for _, p := range parts[2:] {
		k, v, ok := strings.Cut(p, "=")
		if !ok {
			return zero, fmt.Errorf("ownership: malformed field %q (missing '=')", p)
		}
		if _, dup := fields[k]; dup {
			return zero, fmt.Errorf("ownership: duplicate field %q", k)
		}
		fields[k] = v
	}
	if len(fields) != 3 {
		return zero, fmt.Errorf("ownership: reason has %d key=value fields, want exactly 3 (repo, run, base)", len(fields))
	}
	repoID, ok1 := fields["repo"]
	runID, ok2 := fields["run"]
	base, ok3 := fields["base"]
	if !ok1 || !ok2 || !ok3 {
		return zero, fmt.Errorf("ownership: reason missing one of repo/run/base fields")
	}
	if !reasonRepoRe.MatchString(repoID) {
		return zero, fmt.Errorf("ownership: malformed repo-id %q", repoID)
	}
	if !ValidRunID(runID) {
		return zero, fmt.Errorf("ownership: unsafe run-id %q", runID)
	}
	if !reasonOIDRe.MatchString(base) {
		return zero, fmt.Errorf("ownership: malformed full OID %q", base)
	}
	return Reason{Version: parts[1], RepoID: repoID, RunID: runID, AdmittedBase: base}, nil
}

// Matches verifies the parsed reason agrees with the expected run identity.
// It fails closed on repo-id mismatch, run-id/path mismatch, or
// admitted-base mismatch.
func (r Reason) Matches(repoID, runID, admittedBase string) error {
	if r.RepoID != repoID {
		return fmt.Errorf("ownership: reason repo-id %q != canonical %q", r.RepoID, repoID)
	}
	if r.RunID != runID {
		return fmt.Errorf("ownership: reason run-id %q != path run-id %q", r.RunID, runID)
	}
	if r.AdmittedBase != admittedBase {
		return fmt.Errorf("ownership: reason admitted-base %q != expected %q", r.AdmittedBase, admittedBase)
	}
	return nil
}

// LeaseIdentity is the bounded identity stored in the liveness lease. It MUST
// contain only identity data: protocol version, repo-id, run-id,
// admitted-base. It must never contain task prompt, semantic progress,
// checkpoint state, or publication state.
type LeaseIdentity struct {
	Version      string `json:"version"`
	RepoID       string `json:"repo_id"`
	RunID        string `json:"run_id"`
	AdmittedBase string `json:"admitted_base"`
}

// errLeaseHeld is returned by a nonblocking exclusive acquire when another
// process already holds the advisory lock.
var errLeaseHeld = errors.New("ownership: lease held by another process")

// errLeaseMissing is returned when the lease pathname does not exist, so no
// liveness evidence can be opened (missing evidence must never be manufactured).
var errLeaseMissing = errors.New("ownership: lease pathname missing")

// Lease is the deterministic per-run liveness lease. Its path is derived from
// the user cache root + global-build + repo-id + run-id. The advisory OS lock
// held on the underlying file is the liveness proof; the pathname alone is not.
type Lease struct {
	path string
	f    *os.File
}

// NewLease derives the lease path deterministically. cacheRoot is the same root
// the runner uses (already ending in "global-build"); leaseDir becomes
// <cacheRoot>/<repoID> and the path <cacheRoot>/<repoID>/<runID>.
func NewLease(cacheRoot, repoID, runID string) *Lease {
	return &Lease{path: joinPath(cacheRoot, repoID, runID)}
}

// Path returns the deterministic lease pathname.
func (l *Lease) Path() string { return l.path }

// Establish creates the lease file (idempotently, parent dirs created), writes
// the bounded identity, and acquires the exclusive advisory lock, holding it for
// the lifetime of the BUILD attempt. On platforms without safe nonblocking
// advisory locking this returns an error (callers must fail closed).
func (l *Lease) Establish(id LeaseIdentity) error {
	if err := mkdirAll(dirOf(l.path)); err != nil {
		return err
	}
	f, err := os.OpenFile(l.path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if err := encodeJSON(f, id); err != nil {
		f.Close()
		return err
	}
	if err := flockAcquireExclusiveNB(uintptr(f.Fd())); err != nil {
		f.Close()
		return err
	}
	l.f = f
	return nil
}

// ReadIdentity opens an EXISTING lease and parses its identity. It returns
// ok=false (without error) when the pathname is missing or the content is
// malformed or empty, so callers treat missing/malformed identity as uncertain
// rather than inventing evidence.
func (l *Lease) ReadIdentity() (LeaseIdentity, bool, error) {
	var zero LeaseIdentity
	f, ok, err := l.openExisting()
	if err != nil {
		return zero, false, err
	}
	if !ok {
		return zero, false, nil
	}
	defer f.Close()
	var id LeaseIdentity
	if err := decodeJSON(f, &id); err != nil {
		return zero, false, nil
	}
	return id, true, nil
}

// AcquireExisting opens an EXISTING lease (never creating one) and takes a
// nonblocking exclusive advisory lock, holding it. Returns errLeaseMissing when
// the pathname does not exist, errLeaseHeld when another process holds it, or a
// platform error.
func (l *Lease) AcquireExisting() error {
	f, ok, err := l.openExisting()
	if err != nil {
		return err
	}
	if !ok {
		return errLeaseMissing
	}
	if err := flockAcquireExclusiveNB(uintptr(f.Fd())); err != nil {
		f.Close()
		return err
	}
	l.f = f
	return nil
}

// TryAcquireExisting attempts AcquireExisting and collapses the "not held by us"
// outcomes (missing or held by another) into acquired=false without error, so
// callers can classify the candidate as uncertain. A genuine platform error is
// still returned.
func (l *Lease) TryAcquireExisting() (bool, error) {
	err := l.AcquireExisting()
	if err == nil {
		return true, nil
	}
	if errors.Is(err, errLeaseHeld) || errors.Is(err, errLeaseMissing) {
		return false, nil
	}
	return false, err
}

// Release unlocks and closes the lease handle. It is safe to call when no lock
// is held.
func (l *Lease) Release() error {
	if l.f == nil {
		return nil
	}
	ferr := flockRelease(uintptr(l.f.Fd()))
	cerr := l.f.Close()
	l.f = nil
	if ferr != nil {
		return ferr
	}
	return cerr
}

// Remove deletes only this run's lease pathname. It must be called only after
// Release (or when known safe) and only ever targets the exact derived path.
func (l *Lease) Remove() error {
	if l.path == "" {
		return nil
	}
	return os.Remove(l.path)
}
