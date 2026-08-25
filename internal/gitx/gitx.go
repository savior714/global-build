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
