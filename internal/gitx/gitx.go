// Package gitx wraps the exact Git plumbing the global-build runner needs.
// Every operation is deterministic and side-effect scoped: no reset --hard,
// no git clean, no repo-wide prune, ever.
package gitx

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// CandidateRefPrefix is the exact candidate ref namespace shared by the runner
// (which creates the candidate ref) and the publisher (which verifies and
// deletes it). Centralizing it here keeps the contract in one place so the two
// sides can never drift.
const CandidateRefPrefix = "refs/build-candidates/"

// WorktreeEntry is one record of `git worktree list --porcelain -z`
// (NUL-safe form).
type WorktreeEntry struct {
	Path      string // absolute, symlink-resolved by Git
	Head      string // oid or empty when unborn
	Detached  bool
	Bare      bool
	Locked    bool
	LockReason string // reason text when locked with --reason, else empty
}

// Run executes git in dir and returns stdout. Non-zero exit becomes an error
// carrying trimmed stderr text.
func Run(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errBuf.String())
		if msg == "" {
			return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return out.String(), nil
}

// RunExit executes git and returns the raw exit code along with any error.
func RunExit(ctx context.Context, dir string, args ...string) (int, string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err := cmd.Run()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			return -1, "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
		}
	}
	return code, out.String(), nil
}

// IsWorkTree reports whether dir is inside a git work tree.
func IsWorkTree(ctx context.Context, dir string) bool {
	code, _, err := RunExit(ctx, dir, "rev-parse", "--is-inside-work-tree")
	return err == nil && code == 0
}

// CanonicalCommonDir resolves the canonical common-dir identity of the
// repository containing dir: the realpath of `git rev-parse --git-common-dir`.
func CanonicalCommonDir(ctx context.Context, dir string) (string, error) {
	out, err := Run(ctx, dir, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", err
	}
	p := strings.TrimSpace(out)
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	return filepath.EvalSymlinks(p)
}

// ResolveCommit verifies that oid resolves to an exact commit object present
// in this repository and returns true only on an exact full-oid match.
func ResolveCommit(ctx context.Context, dir, oid string) bool {
	out, err := Run(ctx, dir, "rev-parse", "--verify", "--quiet", oid+"^{commit}")
	if err != nil {
		return false
	}
	return strings.TrimSpace(out) == oid
}

// ObjectFormatLen returns the hex length of the repository object format
// (sha1 => 40, sha256 => 64).
func ObjectFormatLen(ctx context.Context, dir string) (int, error) {
	out, err := Run(ctx, dir, "rev-parse", "--show-object-format")
	if err != nil {
		return 0, err
	}
	switch f := strings.TrimSpace(out); f {
	case "sha1":
		return 40, nil
	case "sha256":
		return 64, nil
	default:
		return 0, fmt.Errorf("unsupported object format %q", f)
	}
}

// ZeroOID returns the all-zero oid of the repository's object format, used as
// the "ref must not exist" old value for compare-and-swap ref creation.
func ZeroOID(dir string, hashLen int) string {
	return strings.Repeat("0", hashLen)
}

// CheckRefFormat validates a fully qualified refname using git itself.
func CheckRefFormat(ctx context.Context, ref string) bool {
	_, _, err := execGitOutput(ctx, "", "check-ref-format", ref)
	return err == nil
}

func execGitOutput(ctx context.Context, dir string, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err := cmd.Run()
	return out.String(), errBuf.String(), err
}

// WorktreeListPorcelainZ parses `git worktree list --porcelain -z` into
// structured entries. Requires Git >= 2.36 for NUL-safe output.
func WorktreeListPorcelainZ(ctx context.Context, dir string) ([]WorktreeEntry, error) {
	raw, err := Run(ctx, dir, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return nil, err
	}
	return parseWorktreePorcelainZ(raw)
}

// parseWorktreePorcelainZ decodes the NUL-separated porcelain token stream into
// entries. Extracted from WorktreeListPorcelainZ so abnormal token streams
// (peeled lines, empty entries, missing lock reasons) can be unit-tested
// without a live work tree.
func parseWorktreePorcelainZ(raw string) ([]WorktreeEntry, error) {
	tokens := strings.Split(raw, "\x00")
	var entries []WorktreeEntry
	cur := WorktreeEntry{}
	started := false
	for _, tok := range tokens {
		if tok == "" {
			if started {
				entries = append(entries, cur)
				cur = WorktreeEntry{}
				started = false
			}
			continue
		}
		switch {
		case strings.HasPrefix(tok, "worktree "):
			cur.Path = strings.TrimPrefix(tok, "worktree ")
			started = true
		case strings.HasPrefix(tok, "HEAD "):
			cur.Head = strings.TrimPrefix(tok, "HEAD ")
		case tok == "detached":
			cur.Detached = true
		case tok == "bare":
			cur.Bare = true
		case strings.HasPrefix(tok, "locked "):
			cur.Locked = true
			cur.LockReason = strings.TrimPrefix(tok, "locked ")
		case tok == "locked":
			cur.Locked = true
		}
	}
	if started {
		entries = append(entries, cur)
	}
	return entries, nil
}

// WorktreeAddDetach runs `git worktree add --detach <path> <oid>` from the
// main repository.
func WorktreeAddDetach(ctx context.Context, repoDir, path, oid string) error {
	_, err := Run(ctx, repoDir, "worktree", "add", "--detach", path, oid)
	return err
}

// WorktreeAddDetachLock runs `git worktree add --detach --lock --reason <reason>
// <path> <oid>` from the main repository. The documented lock reason is the
// durable ownership marker the explicit cleanup command later verifies.
func WorktreeAddDetachLock(ctx context.Context, repoDir, path, oid, reason string) error {
	_, err := Run(ctx, repoDir, "worktree", "add", "--detach", "--lock", "--reason", reason, path, oid)
	return err
}

// WorktreeUnlock releases the lock on an owned worktree before its removal. It
// is the inverse of the --lock used at creation time.
func WorktreeUnlock(ctx context.Context, repoDir, path string) error {
	_, err := Run(ctx, repoDir, "worktree", "unlock", path)
	return err
}

// WorktreeRemoveForce runs `git worktree remove --force <path>` from the main
// repository. Callers MUST prove ownership first.
func WorktreeRemoveForce(ctx context.Context, repoDir, path string) error {
	_, err := Run(ctx, repoDir, "worktree", "remove", "--force", path)
	return err
}

// UpdateRefCAS creates refname at oid using an expected old value of the
// zero-oid ("must not already exist"). Any non-zero exit (including a lost
// race against a concurrent creator) is an error and nothing is overwritten.
func UpdateRefCAS(ctx context.Context, repoDir, refname, oid string, hashLen int) error {
	old := ZeroOID(repoDir, hashLen)
	code, _, err := RunExit(ctx, repoDir, "update-ref", refname, oid, old)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("update-ref %s failed with exit code %d", refname, code)
	}
	return nil
}

// RefExists reports whether refname currently exists.
func RefExists(ctx context.Context, repoDir, refname string) bool {
	code, _, err := RunExit(ctx, repoDir, "rev-parse", "--verify", "--quiet", refname)
	return err == nil && code == 0
}

// IsAncestor reports whether a is an ancestor of b (or equal). A git exit
// code of 1 means "not an ancestor" and is not a hard error.
func IsAncestor(ctx context.Context, repoDir, a, b string) (bool, error) {
	code, _, err := RunExit(ctx, repoDir, "merge-base", "--is-ancestor", a, b)
	if err != nil {
		return false, err
	}
	switch code {
	case 0:
		return true, nil
	case 1:
		return false, nil
	default:
		return false, fmt.Errorf("merge-base --is-ancestor exited %d", code)
	}
}

// DiffNameOnlyZ lists changed paths between two commits (NUL-separated,
// rename detection off — the smallest deterministic change set).
func DiffNameOnlyZ(ctx context.Context, repoDir, from, to string) ([]string, error) {
	raw, err := Run(ctx, repoDir, "diff", "--name-only", "-z", from, to)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, p := range strings.Split(raw, "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths, nil
}

// StatusClean runs `git status --porcelain=v1 -z` inside worktreeDir and
// reports whether there are no staged, unstaged or untracked changes.
func StatusClean(ctx context.Context, worktreeDir string) (bool, string, error) {
	raw, err := Run(ctx, worktreeDir, "status", "--porcelain=v1", "-z")
	if err != nil {
		return false, "", err
	}
	return strings.TrimSpace(raw) == "", raw, nil
}

// HeadState returns the HEAD oid of the repository/worktree at dir and
// whether HEAD is detached.
func HeadState(ctx context.Context, dir string) (oid string, detached bool, err error) {
	code, _, symErr := RunExit(ctx, dir, "symbolic-ref", "-q", "HEAD")
	if symErr != nil {
		return "", false, symErr
	}
	detached = code != 0
	out, err := Run(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return "", detached, err
	}
	return strings.TrimSpace(out), detached, nil
}

// CanonicalPath best-effort canonicalizes an absolute filesystem path the way
// git prints worktree paths (symlinks resolved in every existing component).
func CanonicalPath(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	// Resolve the deepest existing ancestor, then rejoin the remainder.
	parent, base := filepath.Split(filepath.Clean(p))
	if parent == "" {
		return filepath.Clean(p)
	}
	parent = strings.TrimSuffix(parent, string(os.PathSeparator))
	resolvedParent := CanonicalPath(parent)
	return filepath.Join(resolvedParent, base)
}

// RepoID derives a stable, filesystem-safe identifier from the canonical
// common-dir path. Path namespace alone is not ownership proof; this id is
// only used to namespace cache directories.
func RepoID(commonDir string) string {
	sum := sha256.Sum256([]byte(commonDir))
	return hex.EncodeToString(sum[:])[:16]
}

// IsFullOID reports whether s is a full git object id in either supported
// object format (sha1 => 40 hex, sha256 => 64 hex).
func IsFullOID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}

// DeleteRefCAS deletes refname only when its current value equals expectedOID,
// using `git update-ref -d <ref> <oldvalue>`. Any mismatch (ref missing,
// pointing elsewhere, or concurrent change) leaves the ref untouched and
// returns an error. The ref is never rewritten.
func DeleteRefCAS(ctx context.Context, repoDir, refname, expectedOID string) error {
	code, _, err := RunExit(ctx, repoDir, "update-ref", "-d", refname, expectedOID)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("delete-ref %s CAS (expected %s) failed", refname, expectedOID)
	}
	return nil
}

// RevListCount returns the number of commits reachable from `to` but not from
// `from` (i.e. the size of from..to), using `git rev-list --count`.
func RevListCount(ctx context.Context, repoDir, from, to string) (int, error) {
	out, err := Run(ctx, repoDir, "rev-list", "--count", from+".."+to)
	if err != nil {
		return 0, err
	}
	n, err := parseInt(strings.TrimSpace(out))
	if err != nil {
		return 0, fmt.Errorf("rev-list --count: %w", err)
	}
	return n, nil
}

// MergeCountInRange returns the number of merge commits in from..to, using
// `git rev-list --count --merges from..to`. A non-zero result means the range
// contains at least one merge commit.
func MergeCountInRange(ctx context.Context, repoDir, from, to string) (int, error) {
	out, err := Run(ctx, repoDir, "rev-list", "--count", "--merges", from+".."+to)
	if err != nil {
		return 0, err
	}
	n, err := parseInt(strings.TrimSpace(out))
	if err != nil {
		return 0, fmt.Errorf("rev-list --merges: %w", err)
	}
	return n, nil
}

// RemoteRefErrorKind classifies a non-mutating remote ref read failure.
type RemoteRefErrorKind string

const (
	// RemoteRefNoSuch means the queried ref does not exist on the remote.
	RemoteRefNoSuch RemoteRefErrorKind = "NO_SUCH_REF"
	// RemoteRefAmbiguous means the query returned more than one ref line.
	RemoteRefAmbiguous RemoteRefErrorKind = "AMBIGUOUS"
	// RemoteRefUnexpected means the output could not be parsed into exactly one
	// valid OID/ref line.
	RemoteRefUnexpected RemoteRefErrorKind = "UNEXPECTED_OUTPUT"
	// RemoteRefGitFailure means git itself could not be executed or returned an
	// unexpected non-zero exit code.
	RemoteRefGitFailure RemoteRefErrorKind = "GIT_FAILURE"
)

// RemoteRefError is the typed, fail-closed result of ReadRemoteBranchOID.
type RemoteRefError struct {
	Kind RemoteRefErrorKind
	Msg  string
}

func (e *RemoteRefError) Error() string { return e.Msg }

// ReadRemoteBranchOID is the single narrow, non-mutating helper shared by the
// publisher and the continuation freshness check. It reads the live OID of
// refs/heads/<branch> at <remote> via `git ls-remote --exit-code`, performs no
// local ref mutation, supports both object formats, and fails closed: exactly
// one <oid>\t<ref> line carrying a valid full OID is required.
func ReadRemoteBranchOID(ctx context.Context, repoDir, remote, branch string) (string, *RemoteRefError) {
	ref := "refs/heads/" + branch
	code, out, err := RunExit(ctx, repoDir, "ls-remote", "--exit-code", remote, ref)
	if err != nil {
		return "", &RemoteRefError{Kind: RemoteRefGitFailure, Msg: fmt.Sprintf("git ls-remote failed: %v", err)}
	}
	switch code {
	case 0:
		// proceed to parse
	case 2:
		return "", &RemoteRefError{Kind: RemoteRefNoSuch, Msg: fmt.Sprintf("remote %s has no ref %s", remote, ref)}
	default:
		return "", &RemoteRefError{Kind: RemoteRefGitFailure, Msg: fmt.Sprintf("git ls-remote exited %d for %s %s", code, remote, ref)}
	}

	return parseLsRemote(out, ref, remote)
}

// parseLsRemote decodes `git ls-remote` output into the fail-closed result for
// a single branch ref. Peeled "<oid>\t<ref>^{}" lines are ignored (they never
// match the exact branch ref), non-OID and malformed lines are rejected, and
// exactly one matching OID is required. Extracted from ReadRemoteBranchOID so
// the collection/peeled/ambiguous logic can be unit-tested without a remote.
func parseLsRemote(out, ref, remote string) (string, *RemoteRefError) {
	var oids []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Expected: "<oid>\t<ref>"; ls-remote may also emit a peeled
		// "<oid>\t<ref>^{}" line for annotated tags, but for an exact branch
		// ref we expect exactly one primary line.
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			return "", &RemoteRefError{Kind: RemoteRefUnexpected, Msg: fmt.Sprintf("unparseable ls-remote line %q", line)}
		}
		oid := parts[0]
		if !IsFullOID(oid) {
			return "", &RemoteRefError{Kind: RemoteRefUnexpected, Msg: fmt.Sprintf("ls-remote returned non-OID %q", oid)}
		}
		if parts[1] == ref {
			oids = append(oids, oid)
		}
	}
	return classifyLsRemote(oids, remote, ref)
}

// classifyLsRemote turns the collected matching OIDs into the fail-closed
// result: exactly one match is required. Extracted from ReadRemoteBranchOID so
// ambiguous / no-match / peeled-line outcomes can be unit-tested directly.
func classifyLsRemote(oids []string, remote, ref string) (string, *RemoteRefError) {
	if len(oids) == 0 {
		return "", &RemoteRefError{Kind: RemoteRefNoSuch, Msg: fmt.Sprintf("remote %s has no ref %s", remote, ref)}
	}
	if len(oids) > 1 {
		return "", &RemoteRefError{Kind: RemoteRefAmbiguous, Msg: fmt.Sprintf("ls-remote returned %d matches for %s", len(oids), ref)}
	}
	return oids[0], nil
}

func parseInt(s string) (int, error) {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("not an integer: %q", s)
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}
