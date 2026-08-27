package gitx

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// --- pure-function tests (no git invocation) -------------------------------

func TestIsFullOID(t *testing.T) {
	sha1 := strings.Repeat("a", 40)
	sha256 := strings.Repeat("b", 64)
	cases := []struct {
		in   string
		want bool
	}{
		{sha1, true},
		{sha256, true},
		{strings.Repeat("a", 39), false}, // one short
		{strings.Repeat("a", 65), false},  // one long
		{"", false},
		{"0", false},
		{strings.Repeat("g", 40), false}, // non-hex char
		{strings.Repeat("A", 40), false}, // uppercase not allowed
		{strings.Repeat("a", 41), false},
	}
	for _, c := range cases {
		if got := IsFullOID(c.in); got != c.want {
			t.Errorf("IsFullOID(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestCanonicalPathNoPanicOnNonexistentTopLevel(t *testing.T) {
	got := CanonicalPath("/nonexistent/deep/path/that/does/not/exist")
	if got == "" {
		t.Errorf("CanonicalPath returned empty for nonexistent path")
	}
}

func TestCanonicalPathResolvesExisting(t *testing.T) {
	dir := t.TempDir()
	got := CanonicalPath(dir)
	if !filepath.IsAbs(got) {
		t.Errorf("CanonicalPath(%q) = %q, want absolute", dir, got)
	}
	if got == "" {
		t.Errorf("CanonicalPath returned empty for existing dir")
	}
}

func TestRepoIDDeterministic(t *testing.T) {
	a := RepoID("/some/common-dir")
	b := RepoID("/some/common-dir")
	if a != b {
		t.Errorf("RepoID not deterministic: %q != %q", a, b)
	}
	if len(a) != 16 {
		t.Errorf("RepoID length = %d, want 16", len(a))
	}
	if RepoID("/other") == a {
		t.Error("RepoID collided for different inputs")
	}
}

func TestZeroOID(t *testing.T) {
	if got := ZeroOID("", 40); got != strings.Repeat("0", 40) {
		t.Errorf("ZeroOID(40) = %q", got)
	}
	if got := ZeroOID("", 64); got != strings.Repeat("0", 64) {
		t.Errorf("ZeroOID(64) = %q", got)
	}
}

func TestParseInt(t *testing.T) {
	cases := []struct {
		in   string
		want int
		fail bool
	}{
		{"0", 0, false},
		{"123", 123, false},
		{"007", 7, false},
		{"", 0, false}, // current behavior: empty parses to 0
		{"12a", 0, true},
		{"-1", 0, true},
	}
	for _, c := range cases {
		got, err := parseInt(c.in)
		if c.fail {
			if err == nil {
				t.Errorf("parseInt(%q) should fail", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseInt(%q) unexpected error: %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("parseInt(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// --- parser unit tests (no git invocation) ---------------------------------

func TestParseWorktreePorcelainZNormal(t *testing.T) {
	raw := strings.Join([]string{
		"worktree /repo/main",
		"HEAD " + strings.Repeat("a", 40),
		"detached",
		"", // entry terminator
	}, "\x00")
	entries, err := parseWorktreePorcelainZ(raw)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	e := entries[0]
	if e.Path != "/repo/main" || e.Head != strings.Repeat("a", 40) || !e.Detached {
		t.Errorf("entry wrong: %+v", e)
	}
}

func TestParseWorktreePorcelainZLockedWithReason(t *testing.T) {
	raw := strings.Join([]string{
		"worktree /repo/wt",
		"HEAD " + strings.Repeat("a", 40),
		"locked owned by global-build run-1",
		"",
	}, "\x00")
	entries, err := parseWorktreePorcelainZ(raw)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	e := entries[0]
	if !e.Locked || e.LockReason != "owned by global-build run-1" {
		t.Errorf("lock not captured: %+v", e)
	}
}

func TestParseWorktreePorcelainZLockedNoReason(t *testing.T) {
	raw := strings.Join([]string{
		"worktree /repo/wt",
		"locked",
		"",
	}, "\x00")
	entries, err := parseWorktreePorcelainZ(raw)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	e := entries[0]
	if !e.Locked {
		t.Errorf("locked token without reason not captured: %+v", e)
	}
	if e.LockReason != "" {
		t.Errorf("unexpected lock reason %q", e.LockReason)
	}
}

func TestParseWorktreePorcelainZBare(t *testing.T) {
	raw := strings.Join([]string{
		"worktree /repo/bare",
		"bare",
		"HEAD " + strings.Repeat("a", 40),
		"",
	}, "\x00")
	entries, err := parseWorktreePorcelainZ(raw)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if len(entries) != 1 || !entries[0].Bare {
		t.Errorf("bare entry not captured: %+v", entries)
	}
}

func TestParseWorktreePorcelainZEmptyEntrySkipped(t *testing.T) {
	raw := strings.Join([]string{
		"worktree /a",
		"",
		"",
		"worktree /b",
		"",
	}, "\x00")
	entries, err := parseWorktreePorcelainZ(raw)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2 (empty entry skipped)", len(entries))
	}
}

func TestParseWorktreePorcelainZPeeledIgnored(t *testing.T) {
	raw := strings.Join([]string{
		"worktree /repo/main",
		"HEAD " + strings.Repeat("a", 40),
		"HEAD " + strings.Repeat("a", 40) + "^{}",
		"",
	}, "\x00")
	entries, err := parseWorktreePorcelainZ(raw)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
}

func TestParseLsRemoteSingle(t *testing.T) {
	out := strings.Repeat("c", 40) + "\trefs/heads/main\n"
	oid, err := parseLsRemote(out, "refs/heads/main", "origin")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if oid != strings.Repeat("c", 40) {
		t.Errorf("oid = %q", oid)
	}
}

func TestParseLsRemotePeeledIgnored(t *testing.T) {
	oid := strings.Repeat("c", 40)
	out := oid + "\trefs/heads/main\n" + oid + "\trefs/heads/main^{}\n"
	got, err := parseLsRemote(out, "refs/heads/main", "origin")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != oid {
		t.Errorf("peeled line caused mismatch: got %q", got)
	}
}

func TestParseLsRemoteAmbiguous(t *testing.T) {
	a, b := strings.Repeat("c", 40), strings.Repeat("d", 40)
	out := a + "\trefs/heads/main\n" + b + "\trefs/heads/main\n"
	_, err := parseLsRemote(out, "refs/heads/main", "origin")
	if err == nil || err.Kind != RemoteRefAmbiguous {
		t.Fatalf("expected Ambiguous, got %v", err)
	}
}

func TestParseLsRemoteNoSuch(t *testing.T) {
	out := strings.Repeat("c", 40) + "\trefs/heads/other\n"
	_, err := parseLsRemote(out, "refs/heads/main", "origin")
	if err == nil || err.Kind != RemoteRefNoSuch {
		t.Fatalf("expected NoSuch, got %v", err)
	}
}

func TestParseLsRemoteUnparseableLine(t *testing.T) {
	out := "not-a-tab-separated-line\n"
	_, err := parseLsRemote(out, "refs/heads/main", "origin")
	if err == nil || err.Kind != RemoteRefUnexpected {
		t.Fatalf("expected Unexpected, got %v", err)
	}
}

func TestParseLsRemoteNonOID(t *testing.T) {
	out := strings.Repeat("z", 40) + "\trefs/heads/main\n"
	_, err := parseLsRemote(out, "refs/heads/main", "origin")
	if err == nil || err.Kind != RemoteRefUnexpected {
		t.Fatalf("expected Unexpected for non-OID, got %v", err)
	}
}

// --- integration tests (real git) ------------------------------------------

func gitInit(t *testing.T, dir string) {
	t.Helper()
	runGit(t, "", "init", "-q", "-b", "main", dir)
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "test")
	runGit(t, dir, "config", "commit.gpgsign", "false")
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			t.Fatalf("git %v: %v\n%s", args, err, ee.Stderr)
		}
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

func commitFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "c")
	return runGit(t, dir, "rev-parse", "HEAD")
}

func TestWorktreeListPorcelainZIntegration(t *testing.T) {
	repo := t.TempDir()
	gitInit(t, repo)
	commitFile(t, repo, "main.go", "package main\n")

	wt := filepath.Join(t.TempDir(), "wt")
	if err := WorktreeAddDetachLock(context.Background(), repo, wt, commitFile(t, repo, "x.go", "x"), "owned by global-build run-1"); err != nil {
		t.Fatalf("add locked worktree: %v", err)
	}

	entries, err := WorktreeListPorcelainZ(context.Background(), repo)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) < 2 {
		t.Fatalf("expected main + locked worktree, got %d", len(entries))
	}
	var locked *WorktreeEntry
	for i := range entries {
		if entries[i].Locked {
			locked = &entries[i]
		}
	}
	if locked == nil {
		t.Fatal("no locked worktree entry found")
	}
	if locked.LockReason != "owned by global-build run-1" {
		t.Errorf("lock reason = %q", locked.LockReason)
	}
}

func TestUpdateRefCASSuccess(t *testing.T) {
	repo := t.TempDir()
	gitInit(t, repo)
	oid := commitFile(t, repo, "main.go", "package main\n")
	ref := "refs/build-candidates/test-success"
	if err := UpdateRefCAS(context.Background(), repo, ref, oid, 40); err != nil {
		t.Fatalf("CAS create failed: %v", err)
	}
	if !RefExists(context.Background(), repo, ref) {
		t.Errorf("ref %s not created", ref)
	}
}

func TestUpdateRefCASConflict(t *testing.T) {
	repo := t.TempDir()
	gitInit(t, repo)
	oid := commitFile(t, repo, "main.go", "package main\n")
	ref := "refs/build-candidates/test-conflict"
	if err := UpdateRefCAS(context.Background(), repo, ref, oid, 40); err != nil {
		t.Fatalf("first CAS create failed: %v", err)
	}
	if err := UpdateRefCAS(context.Background(), repo, ref, oid, 40); err == nil {
		t.Error("conflicting CAS create unexpectedly succeeded")
	}
}

func TestUpdateRefCASMalformedRef(t *testing.T) {
	repo := t.TempDir()
	gitInit(t, repo)
	oid := commitFile(t, repo, "main.go", "package main\n")
	if err := UpdateRefCAS(context.Background(), repo, "refs/heads/bad ref", oid, 40); err == nil {
		t.Error("malformed ref CAS unexpectedly succeeded")
	}
}

func TestIsAncestorBranches(t *testing.T) {
	repo := t.TempDir()
	gitInit(t, repo)
	a := commitFile(t, repo, "a.go", "a")
	b := commitFile(t, repo, "b.go", "b")

	if ok, err := IsAncestor(context.Background(), repo, a, b); err != nil || !ok {
		t.Errorf("a should be ancestor of b (ok=%v err=%v)", ok, err)
	}
	if ok, err := IsAncestor(context.Background(), repo, b, a); err != nil || ok {
		t.Errorf("b should NOT be ancestor of a (ok=%v err=%v)", ok, err)
	}
}

func TestIsAncestorGitFailure(t *testing.T) {
	repo := t.TempDir()
	gitInit(t, repo)
	commitFile(t, repo, "a.go", "a")
	if ok, err := IsAncestor(context.Background(), repo, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", "HEAD"); err == nil {
		t.Errorf("expected error for missing rev, got ok=%v", ok)
	}
}

func TestReadRemoteBranchOIDIntegration(t *testing.T) {
	origin := filepath.Join(t.TempDir(), "origin.git")
	runGit(t, "", "init", "--bare", "-b", "main", origin)
	src := t.TempDir()
	gitInit(t, src)
	oid := commitFile(t, src, "main.go", "package main\n")
	runGit(t, src, "remote", "add", "origin", origin)
	runGit(t, src, "push", "origin", oid+":refs/heads/main")

	got, err := ReadRemoteBranchOID(context.Background(), src, "origin", "main")
	if err != nil {
		t.Fatalf("read remote: %v", err)
	}
	if got != oid {
		t.Errorf("remote oid = %q, want %q", got, oid)
	}

	if _, err := ReadRemoteBranchOID(context.Background(), src, "origin", "no-such"); err == nil || err.Kind != RemoteRefNoSuch {
		t.Errorf("expected NoSuch for missing branch, got %v", err)
	}

	if _, err := ReadRemoteBranchOID(context.Background(), src, "no-remote", "main"); err == nil || err.Kind != RemoteRefGitFailure {
		t.Errorf("expected GitFailure for missing remote, got %v", err)
	}
}

func TestRefExistsAndDeleteRefCAS(t *testing.T) {
	repo := t.TempDir()
	gitInit(t, repo)
	oid := commitFile(t, repo, "main.go", "package main\n")
	ref := "refs/build-candidates/del-test"
	if RefExists(context.Background(), repo, ref) {
		t.Fatal("ref should not exist yet")
	}
	if err := UpdateRefCAS(context.Background(), repo, ref, oid, 40); err != nil {
		t.Fatalf("create: %v", err)
	}
	if !RefExists(context.Background(), repo, ref) {
		t.Fatal("ref should exist after create")
	}
	// CAS delete with the correct expected OID succeeds.
	if err := DeleteRefCAS(context.Background(), repo, ref, oid); err != nil {
		t.Fatalf("delete CAS: %v", err)
	}
	if RefExists(context.Background(), repo, ref) {
		t.Fatal("ref should be gone after delete")
	}
	// CAS delete against a missing ref fails (fail-closed).
	if err := DeleteRefCAS(context.Background(), repo, ref, oid); err == nil {
		t.Error("delete of missing ref unexpectedly succeeded")
	}
}

func TestRevListAndMergeCounts(t *testing.T) {
	repo := t.TempDir()
	gitInit(t, repo)
	base := commitFile(t, repo, "base.go", "base")
	mid := commitFile(t, repo, "mid.go", "mid")
	// create a merge commit: branch off base, then merge.
	runGit(t, repo, "checkout", "-q", base)
	runGit(t, repo, "checkout", "-qb", "feature")
	commitFile(t, repo, "feat.go", "feat")
	runGit(t, repo, "checkout", "-q", "main")
	runGit(t, repo, "merge", "-q", "--no-ff", "feature")

	n, err := RevListCount(context.Background(), repo, base, "main")
	if err != nil {
		t.Fatalf("RevListCount: %v", err)
	}
	if n < 2 {
		t.Errorf("RevListCount(base..main) = %d, want >= 2", n)
	}
	m, err := MergeCountInRange(context.Background(), repo, base, "main")
	if err != nil {
		t.Fatalf("MergeCountInRange: %v", err)
	}
	if m < 1 {
		t.Errorf("MergeCountInRange(base..main) = %d, want >= 1", m)
	}
	_ = mid
}

func TestHeadStateDetachedAndBranch(t *testing.T) {
	repo := t.TempDir()
	gitInit(t, repo)
	oid := commitFile(t, repo, "main.go", "package main\n")
	// On a branch (main): not detached.
	if got, detached, err := HeadState(context.Background(), repo); err != nil || detached {
		t.Errorf("HeadState on branch: oid=%q detached=%v err=%v", got, detached, err)
	}
	// Detach HEAD onto the commit.
	runGit(t, repo, "checkout", "-q", "--detach", oid)
	if got, detached, err := HeadState(context.Background(), repo); err != nil || !detached || got != oid {
		t.Errorf("HeadState detached: oid=%q detached=%v err=%v", got, detached, err)
	}
}

func TestIsWorkTreeAndResolveCommit(t *testing.T) {
	repo := t.TempDir()
	gitInit(t, repo)
	oid := commitFile(t, repo, "main.go", "package main\n")
	if !IsWorkTree(context.Background(), repo) {
		t.Error("repo should be a work tree")
	}
	if !ResolveCommit(context.Background(), repo, oid) {
		t.Error("ResolveCommit should resolve existing commit oid")
	}
	if ResolveCommit(context.Background(), repo, strings.Repeat("0", 40)) {
		t.Error("ResolveCommit should reject absent oid")
	}
}

func TestObjectFormatLenAndCommonDir(t *testing.T) {
	repo := t.TempDir()
	gitInit(t, repo)
	n, err := ObjectFormatLen(context.Background(), repo)
	if err != nil || n != 40 {
		t.Errorf("ObjectFormatLen = (%d, %v), want (40, nil)", n, err)
	}
	cd, err := CanonicalCommonDir(context.Background(), repo)
	if err != nil {
		t.Fatalf("CanonicalCommonDir: %v", err)
	}
	if !filepath.IsAbs(cd) {
		t.Errorf("common dir not absolute: %q", cd)
	}
}

func TestWorktreeLifecycleAddUnlockRemove(t *testing.T) {
	repo := t.TempDir()
	gitInit(t, repo)
	oid := commitFile(t, repo, "main.go", "package main\n")
	wt := filepath.Join(t.TempDir(), "wt2")
	if err := WorktreeAddDetachLock(context.Background(), repo, wt, oid, "owned by global-build run-x"); err != nil {
		t.Fatalf("add locked: %v", err)
	}
	if err := WorktreeUnlock(context.Background(), repo, wt); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	if err := WorktreeRemoveForce(context.Background(), repo, wt); err != nil {
		t.Fatalf("remove: %v", err)
	}
}

func TestDiffNameOnlyZAndStatusClean(t *testing.T) {
	repo := t.TempDir()
	gitInit(t, repo)
	a := commitFile(t, repo, "a.go", "a")
	b := commitFile(t, repo, "b.go", "b")
	diff, err := DiffNameOnlyZ(context.Background(), repo, a, b)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if len(diff) != 1 || diff[0] != "b.go" {
		t.Errorf("diff = %v, want [b.go]", diff)
	}
	clean, _, err := StatusClean(context.Background(), repo)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !clean {
		t.Error("clean repo reported dirty")
	}
}
