package runner

import (
	"bytes"
	"context"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"global-build/internal/envelope"
	"global-build/internal/gitx"
	"global-build/internal/ownership"
)

// --- harness -----------------------------------------------------------------

type testEnv struct {
	repoRoot    string
	admittedOID string
	cacheRoot   string
	fakeBin     string
	callsFile   string
	stdinCopy   string
}

func git(t *testing.T, args ...string) string {
	t.Helper()
	out, err := gitx.Run(context.Background(), "", args...)
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(out)
}

func initRepo(t *testing.T) (root, head string) {
	t.Helper()
	return createNamedRepo(t, t.TempDir(), "repo")
}

// createNamedRepo initializes a standalone repository with the same content
// shape as initRepo, under parent/basename — so two repositories can share an
// identical basename while remaining distinct identity domains.
func createNamedRepo(t *testing.T, parent, basename string) (root, head string) {
	t.Helper()
	root = filepath.Join(parent, basename)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, "init", "-q", "-b", "main", root)
	git(t, "-C", root, "config", "user.email", "gb-test@example.com")
	git(t, "-C", root, "config", "user.name", "gb-test")
	git(t, "-C", root, "config", "commit.gpgsign", "false")
	writeFile(t, filepath.Join(root, "docs", "notes.txt"), "base\n")
	writeFile(t, filepath.Join(root, "src", "main.go"), "package main\n")
	git(t, "-C", root, "add", "-A")
	git(t, "-C", root, "commit", "-qm", "initial state")
	return root, git(t, "-C", root, "rev-parse", "HEAD")
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

func setupFake(t *testing.T, scenario string) *testEnv {
	t.Helper()
	root, head := initRepo(t)
	return setupFakeAt(t, scenario, root, head)
}

// setupFakeAt builds the fake-worker harness around an arbitrary repository,
// so tests can exercise explicit targeting across distinct repositories.
func setupFakeAt(t *testing.T, scenario, root, head string) *testEnv {
	t.Helper()
	env := &testEnv{repoRoot: root, admittedOID: head}
	env.cacheRoot = t.TempDir()

	binDir := t.TempDir()
	env.fakeBin = filepath.Join(binDir, "fake-opencode")
	raw, err := os.ReadFile("testdata/fake-opencode.sh")
	if err != nil {
		t.Fatalf("fixture missing: %v", err)
	}
	if err := os.WriteFile(env.fakeBin, raw, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(env.fakeBin, 0o755); err != nil {
		t.Fatal(err)
	}

	env.callsFile = filepath.Join(binDir, "calls.log")
	env.stdinCopy = filepath.Join(binDir, "stdin-copy.txt")
	t.Setenv("GB_FAKE_SCENARIO", scenario)
	t.Setenv("GB_FAKE_CALLS", env.callsFile)
	t.Setenv("GB_FAKE_STDIN_COPY", env.stdinCopy)
	return env
}

func envelopeText(runID, admitted string, surfaces ...string) string {
	surfs := ""
	for _, s := range surfaces {
		surfs += "  - " + s + "\n"
	}
	return "---\nrun_id: " + runID + "\nadmitted_base: " + admitted + "\nwatch_surfaces:\n" + surfs +
		"---\n\nGOAL\nMake the documented change.\n\nSETTLED FACTS\ndocs/notes.txt is the target.\n\nCHANGE BOUNDARY\nOnly listed surfaces may change.\n\nPRIMARY PROOF\ngit diff shows exactly one committed change inside the surfaces.\n\nSTOP CONDITIONS\nStop on completion or contradiction.\n"
}

func (e *testEnv) run(t *testing.T, input string, mutate func(*Config)) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cfg := Config{
		WallClock:      30 * time.Second,
		ProgressWindow: 20 * time.Second,
		CacheRoot:      e.cacheRoot,
		BinPath:        e.fakeBin,
		Stdout:         &stdout,
		Stderr:         &stderr,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	code := Run(context.Background(), cfg, e.repoRoot, []byte(input))
	return code, stdout.String(), stderr.String()
}

func stdoutKeys(t *testing.T, out string) map[string]string {
	t.Helper()
	keys := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if line == "" || strings.HasPrefix(line, "  ") {
			continue
		}
		k, v, ok := strings.Cut(line, ": ")
		if !ok {
			continue
		}
		keys[k] = v
	}
	return keys
}

func (e *testEnv) callCount(t *testing.T) int {
	t.Helper()
	raw, err := os.ReadFile(e.callsFile)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return len(strings.Split(strings.TrimSpace(string(raw)), "\n"))
}

func (e *testEnv) worktreePath(t *testing.T, runID string) string {
	t.Helper()
	commonDir, err := gitx.CanonicalCommonDir(context.Background(), e.repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(e.cacheRoot, "worktrees", gitx.RepoID(commonDir), runID)
}

func assertWorktreeGone(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err == nil {
		t.Errorf("disposable worktree still exists at %s", path)
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat %s: %v", path, err)
	}
}

func refOID(t *testing.T, repoRoot, ref string) (string, bool) {
	cmd := exec.Command("git", "-C", repoRoot, "rev-parse", "--verify", "--quiet", ref)
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}

// --- required cases ----------------------------------------------------------

func TestCompleteHappyPath(t *testing.T) {
	e := setupFake(t, "complete")
	runID := "run-complete-1"
	code, stdout, stderrOut := e.run(t, envelopeText(runID, e.admittedOID, "docs/", "src/main.go"), nil)

	if code != ExitComplete {
		t.Fatalf("exit = %d, want 0\nstdout:\n%s\nstderr:\n%s", code, stdout, stderrOut)
	}
	keys := stdoutKeys(t, stdout)
	if keys["RUN_RESULT"] != "COMPLETE" {
		t.Errorf("RUN_RESULT = %q", keys["RUN_RESULT"])
	}
	if keys["RUN_ID"] != runID || keys["ADMITTED_BASE"] != e.admittedOID {
		t.Errorf("contract keys wrong: %+v", keys)
	}
	candidate := keys["CANDIDATE"]
	if candidate == "" || keys["CANDIDATE_REF"] != "refs/build-candidates/"+runID {
		t.Errorf("candidate keys wrong: %+v", keys)
	}

	refValue, exists := refOID(t, e.repoRoot, "refs/build-candidates/"+runID)
	if !exists || refValue != candidate {
		t.Fatalf("candidate ref missing or wrong: exists=%v value=%q candidate=%q", exists, refValue, candidate)
	}
	// Candidate remains reachable through the exact ref after worktree removal.
	if got := git(t, "-C", e.repoRoot, "cat-file", "-t", refValue+"^{commit}"); got != "commit" {
		t.Errorf("candidate not reachable as commit via ref: %s", got)
	}
	if ancestor, _ := gitx.IsAncestor(context.Background(), e.repoRoot, e.admittedOID, candidate); !ancestor {
		t.Error("candidate is not a descendant of admitted base")
	}
	if keys["CHANGED_SURFACE"] != "docs/notes.txt" {
		t.Errorf("CHANGED_SURFACE = %q", keys["CHANGED_SURFACE"])
	}
	assertWorktreeGone(t, e.worktreePath(t, runID))

	// Frontmatter metadata must never reach the fake worker's stdin.
	stdinBody, err := os.ReadFile(e.stdinCopy)
	if err != nil {
		t.Fatalf("stdin copy missing: %v", err)
	}
	for _, banned := range []string{"run_id", "admitted_base", "watch_surfaces"} {
		if bytes.Contains(stdinBody, []byte(banned)) {
			t.Errorf("worker stdin leaks frontmatter key %q", banned)
		}
	}
	for _, section := range []string{"GOAL", "SETTLED FACTS", "CHANGE BOUNDARY", "PRIMARY PROOF", "STOP CONDITIONS"} {
		if !bytes.Contains(stdinBody, []byte(section)) {
			t.Errorf("worker stdin missing section %q", section)
		}
	}
	if n := e.callCount(t); n != 1 {
		t.Errorf("call count = %d, want 1", n)
	}
}

func TestContinuableDiscardsPartialMutation(t *testing.T) {
	e := setupFake(t, "continuable")
	runID := "run-cont-1"
	code, stdout, stderrOut := e.run(t, envelopeText(runID, e.admittedOID, "docs/"), nil)

	if code != ExitContinuable {
		t.Fatalf("exit = %d, want 10\nstdout:\n%s\nstderr:\n%s", code, stdout, stderrOut)
	}
	keys := stdoutKeys(t, stdout)
	if keys["RUN_RESULT"] != "CONTINUABLE" {
		t.Errorf("RUN_RESULT = %q", keys["RUN_RESULT"])
	}
	if keys["ADMITTED_BASE"] != e.admittedOID {
		t.Errorf("checkpoint ADMITTED_BASE = %q, want %s", keys["ADMITTED_BASE"], e.admittedOID)
	}
	for _, field := range []string{"COMPLETED", "REMAINING", "NEXT_ACTION", "DO_NOT_REOPEN", "VERIFICATION_ALREADY_DONE", "WORKTREE_STATE"} {
		if keys[field] == "" {
			t.Errorf("checkpoint field %s missing in output:\n%s", field, stdout)
		}
	}
	if _, exists := refOID(t, e.repoRoot, "refs/build-candidates/"+runID); exists {
		t.Error("no candidate ref may exist for CONTINUABLE")
	}
	assertWorktreeGone(t, e.worktreePath(t, runID))
	// The partial mutation must be gone with the worktree.
	content, err := os.ReadFile(filepath.Join(e.repoRoot, "docs", "notes.txt"))
	if err != nil || string(content) != "base\n" {
		t.Errorf("main repo mutated by CONTINUABLE attempt: %q (%v)", content, err)
	}
}

func TestBlockedPreservesBlockerEvidence(t *testing.T) {
	e := setupFake(t, "blocked")
	runID := "run-block-1"
	code, stdout, _ := e.run(t, envelopeText(runID, e.admittedOID, "docs/"), nil)

	if code != ExitBlocked {
		t.Fatalf("exit = %d, want 20; stdout:\n%s", code, stdout)
	}
	keys := stdoutKeys(t, stdout)
	if keys["RUN_RESULT"] != "BLOCKED" {
		t.Errorf("RUN_RESULT = %q", keys["RUN_RESULT"])
	}
	if keys["BLOCKER"] == "" || keys["EVIDENCE"] == "" {
		t.Errorf("blocker/evidence not preserved: %+v", keys)
	}
	if _, exists := refOID(t, e.repoRoot, "refs/build-candidates/"+runID); exists {
		t.Error("no candidate ref may exist for BLOCKED")
	}
	assertWorktreeGone(t, e.worktreePath(t, runID))
}

func TestMalformedNoTerminalText(t *testing.T) {
	e := setupFake(t, "no_text")
	runID := "run-mal-1"
	code, stdout, _ := e.run(t, envelopeText(runID, e.admittedOID, "docs/"), nil)

	if code != ExitMalformedResult {
		t.Fatalf("exit = %d, want 30", code)
	}
	keys := stdoutKeys(t, stdout)
	if keys["RUN_RESULT"] != "MALFORMED_RESULT" {
		t.Errorf("RUN_RESULT = %q", keys["RUN_RESULT"])
	}
	if _, hasKind := keys["ERROR_KIND"]; hasKind {
		t.Error("MALFORMED_RESULT must not carry ERROR_KIND")
	}
	if _, exists := refOID(t, e.repoRoot, "refs/build-candidates/"+runID); exists {
		t.Error("no candidate ref may exist for MALFORMED_RESULT")
	}
	assertWorktreeGone(t, e.worktreePath(t, runID))
	if n := e.callCount(t); n != 1 {
		t.Errorf("unclassifiable stream must not be retried; calls = %d", n)
	}
}

func TestGarbageLineFailsClosed(t *testing.T) {
	e := setupFake(t, "garbage_line")
	runID := "run-garbage-1"
	code, stdout, _ := e.run(t, envelopeText(runID, e.admittedOID, "docs/"), nil)

	if code != ExitMalformedResult {
		t.Fatalf("exit = %d, want 30 despite a parseable protocol text being present", code)
	}
	if stdoutKeys(t, stdout)["RUN_RESULT"] != "MALFORMED_RESULT" {
		t.Errorf("stdout: %s", stdout)
	}
	assertWorktreeGone(t, e.worktreePath(t, runID))
}

func TestTruncatedJSONFailsClosed(t *testing.T) {
	e := setupFake(t, "truncated_json")
	runID := "run-trunc-1"
	code, stdout, _ := e.run(t, envelopeText(runID, e.admittedOID, "docs/"), nil)

	if code != ExitMalformedResult {
		t.Fatalf("exit = %d, want 30", code)
	}
	if stdoutKeys(t, stdout)["RUN_RESULT"] != "MALFORMED_RESULT" {
		t.Errorf("pretty-output fallback suspected; stdout: %s", stdout)
	}
	assertWorktreeGone(t, e.worktreePath(t, runID))
}

func TestCandidateOutsideWatchSurfacesFailsClosed(t *testing.T) {
	e := setupFake(t, "outside_surface")
	runID := "run-outside-1"
	code, stdout, _ := e.run(t, envelopeText(runID, e.admittedOID, "docs/"), nil) // src/main.go NOT covered

	if code != ExitRunnerError {
		t.Fatalf("exit = %d, want 40", code)
	}
	keys := stdoutKeys(t, stdout)
	if keys["ERROR_KIND"] != "CANDIDATE_VALIDATION_FAILED" {
		t.Errorf("ERROR_KIND = %q", keys["ERROR_KIND"])
	}
	if _, exists := refOID(t, e.repoRoot, "refs/build-candidates/"+runID); exists {
		t.Error("candidate ref must not be created when surfaces reject the change")
	}
	assertWorktreeGone(t, e.worktreePath(t, runID))
}

func TestDirtyCompleteFailsClosed(t *testing.T) {
	e := setupFake(t, "dirty_complete")
	runID := "run-dirty-1"
	code, stdout, _ := e.run(t, envelopeText(runID, e.admittedOID, "docs/"), nil)

	if code != ExitRunnerError {
		t.Fatalf("exit = %d, want 40", code)
	}
	if stdoutKeys(t, stdout)["ERROR_KIND"] != "CANDIDATE_VALIDATION_FAILED" {
		t.Errorf("stdout: %s", stdout)
	}
	if _, exists := refOID(t, e.repoRoot, "refs/build-candidates/"+runID); exists {
		t.Error("dirty COMPLETE must never create a candidate ref")
	}
	assertWorktreeGone(t, e.worktreePath(t, runID))
}

func TestPreExistingCandidateRefFailsWithoutOverwrite(t *testing.T) {
	e := setupFake(t, "complete")
	runID := "run-prereq-1"
	ref := "refs/build-candidates/" + runID
	git(t, "-C", e.repoRoot, "update-ref", ref, e.admittedOID)

	code, stdout, _ := e.run(t, envelopeText(runID, e.admittedOID, "docs/"), nil)

	if code != ExitRunnerError {
		t.Fatalf("exit = %d, want 40", code)
	}
	if stdoutKeys(t, stdout)["ERROR_KIND"] != "CANDIDATE_VALIDATION_FAILED" {
		t.Errorf("stdout: %s", stdout)
	}
	if val, _ := refOID(t, e.repoRoot, ref); val != e.admittedOID {
		t.Errorf("pre-existing ref was overwritten: now %q", val)
	}
	if n := e.callCount(t); n != 0 {
		t.Errorf("fail-fast preflight must not invoke opencode; calls = %d", n)
	}
}

func TestCleanupIdentityMismatchLeavesWorktree(t *testing.T) {
	e := setupFake(t, "identity_mismatch")
	runID := "run-identity-1"
	wtPath := e.worktreePath(t, runID)

	code, stdout, _ := e.run(t, envelopeText(runID, e.admittedOID, "docs/"), nil)

	if code != ExitRunnerError {
		t.Fatalf("exit = %d, want 40", code)
	}
	if stdoutKeys(t, stdout)["ERROR_KIND"] != "WORKTREE_IDENTITY_MISMATCH" {
		t.Errorf("stdout: %s", stdout)
	}
	moved := wtPath + ".moved"
	if _, err := os.Lstat(moved); err != nil {
		t.Errorf("tampered worktree must be left untouched, but %s is missing: %v", moved, err)
	}
	if _, exists := refOID(t, e.repoRoot, "refs/build-candidates/"+runID); exists {
		t.Error("no candidate ref may be created when identity cannot be proven")
	}
	// Cleanup of the tampered registration is later-slice work; nothing else
	// should have been pruned. The main repo worktree must remain intact.
	if _, err := os.Stat(e.repoRoot); err != nil {
		t.Errorf("main repository damaged: %v", err)
	}
}

func TestWallClockTimeoutNoRetry(t *testing.T) {
	e := setupFake(t, "sleep_forever")
	runID := "run-timeout-1"
	start := time.Now()
	code, stdout, _ := e.run(t, envelopeText(runID, e.admittedOID, "docs/"), func(c *Config) {
		// Budgets stay far below production defaults but above the observed
		// first-exec latency of a freshly spawned child on macOS, so the
		// assertion targets the watchdog contract, not process-start jitter.
		c.WallClock = 2 * time.Second
		c.ProgressWindow = 30 * time.Second
	})
	elapsed := time.Since(start)

	if code != ExitRunnerError {
		t.Fatalf("exit = %d, want 40", code)
	}
	if stdoutKeys(t, stdout)["ERROR_KIND"] != "TIMEOUT" {
		t.Errorf("stdout: %s", stdout)
	}
	if elapsed > 10*time.Second {
		t.Errorf("timeout handling took %s; watchdog not enforced", elapsed)
	}
	if n := e.callCount(t); n != 1 {
		t.Errorf("timeouts must never be retried; calls = %d", n)
	}
	assertWorktreeGone(t, e.worktreePath(t, runID))
}

func TestProgressWatchdogTimeout(t *testing.T) {
	e := setupFake(t, "sleep_forever")
	runID := "run-progress-1"
	code, stdout, _ := e.run(t, envelopeText(runID, e.admittedOID, "docs/"), func(c *Config) {
		c.WallClock = 20 * time.Second
		c.ProgressWindow = 1500 * time.Millisecond
	})

	if code != ExitRunnerError {
		t.Fatalf("exit = %d, want 40", code)
	}
	if stdoutKeys(t, stdout)["ERROR_KIND"] != "TIMEOUT" {
		t.Errorf("progress watchdog did not produce TIMEOUT: %s", stdout)
	}
	if n := e.callCount(t); n != 1 {
		t.Errorf("timeouts must never be retried; calls = %d", n)
	}
	assertWorktreeGone(t, e.worktreePath(t, runID))
}

// TestProgressWatchdogFollowsLatestEvent proves the watchdog clock follows the
// latest meaningful progress event, not process start time. A worker that emits
// periodic progress while sleeping must NOT be killed while progress continues,
// even if the total wall time exceeds any reasonable ProgressWindow.
func TestProgressWatchdogFollowsLatestEvent(t *testing.T) {
	e := setupFake(t, "sustained_progress_no_kill")
	runID := "run-prog-live-1"
	// Budgets are deliberately tight: total wall time of the fake is ~4.5s
	// (30 events * 0.15s), but progress events are emitted every 0.15s so the
	// watchdog should never fire. Use a window smaller than total wall time but
	// larger than the inter-event gap to prove clock follows latest event.
	code, stdout, _ := e.run(t, envelopeText(runID, e.admittedOID, "docs/"), func(c *Config) {
		c.WallClock = 10 * time.Second
		c.ProgressWindow = 500 * time.Millisecond
	})

	if code != ExitComplete {
		t.Fatalf("exit = %d, want 0 (worker emitted continuous progress)\nstdout:\n%s", code, stdout)
	}
	if n := e.callCount(t); n != 1 {
		t.Errorf("calls = %d, want 1", n)
	}
	if _, exists := refOID(t, e.repoRoot, "refs/build-candidates/"+runID); !exists {
		t.Error("candidate ref must be created; watchdog did not kill sustained progress")
	}
	assertWorktreeGone(t, e.worktreePath(t, runID))
}

// TestProgressWatchdogTimeoutAfterStall proves the watchdog fires when progress
// stalls after a final event, even if that final event was recent relative to
// process start. The clock must follow the latest meaningful event, not spawn.
func TestProgressWatchdogTimeoutAfterStall(t *testing.T) {
	e := setupFake(t, "progress_then_stall")
	runID := "run-prog-stall-1"
	start := time.Now()
	code, stdout, _ := e.run(t, envelopeText(runID, e.admittedOID, "docs/"), func(c *Config) {
		c.WallClock = 10 * time.Second
		c.ProgressWindow = 800 * time.Millisecond
	})
	elapsed := time.Since(start)

	if code != ExitRunnerError {
		t.Fatalf("exit = %d, want 40 (stall must trigger timeout)\nstdout:\n%s", code, stdout)
	}
	if stdoutKeys(t, stdout)["ERROR_KIND"] != "TIMEOUT" {
		t.Errorf("progress watchdog did not produce TIMEOUT on stall: %s", stdout)
	}
	if elapsed > 5*time.Second {
		t.Errorf("watchdog took %s to fire; should have fired within ProgressWindow of stall", elapsed)
	}
	if n := e.callCount(t); n != 1 {
		t.Errorf("timeouts must never be retried; calls = %d", n)
	}
	assertWorktreeGone(t, e.worktreePath(t, runID))
}

func TestTwoCommitsCompleteRejected(t *testing.T) {
	e := setupFake(t, "two_commits_complete")
	runID := "run-two-commit-1"
	code, stdout, _ := e.run(t, envelopeText(runID, e.admittedOID, "docs/"), nil)

	if code != ExitRunnerError {
		t.Fatalf("exit = %d, want 40 (two commits must be rejected)\nstdout:\n%s", code, stdout)
	}
	if stdoutKeys(t, stdout)["ERROR_KIND"] != "CANDIDATE_VALIDATION_FAILED" {
		t.Errorf("ERROR_KIND = %q, want CANDIDATE_VALIDATION_FAILED", stdoutKeys(t, stdout)["ERROR_KIND"])
	}
	if _, exists := refOID(t, e.repoRoot, "refs/build-candidates/"+runID); exists {
		t.Error("candidate ref must not be created when commit count != 1")
	}
	assertWorktreeGone(t, e.worktreePath(t, runID))
}

func TestTransientRetryExactlyOnceThenSuccess(t *testing.T) {
	e := setupFake(t, "transient_then_success")
	runID := "run-retry-ok-1"
	code, stdout, _ := e.run(t, envelopeText(runID, e.admittedOID, "docs/"), nil)

	if code != ExitComplete {
		t.Fatalf("exit = %d, want 0 after single retry; stdout:\n%s", code, stdout)
	}
	if n := e.callCount(t); n != 2 {
		t.Errorf("calls = %d, want exactly 2 (one transient retry allowed)", n)
	}
	val, exists := refOID(t, e.repoRoot, "refs/build-candidates/"+runID)
	if !exists || val != stdoutKeys(t, stdout)["CANDIDATE"] {
		t.Errorf("candidate ref not created correctly after retry")
	}
	assertWorktreeGone(t, e.worktreePath(t, runID))
}

func TestNoRetryAfterSubstantiveText(t *testing.T) {
	e := setupFake(t, "substantive_then_transient")
	runID := "run-noretry-1"
	code, _, _ := e.run(t, envelopeText(runID, e.admittedOID, "docs/"), nil)

	if code != ExitMalformedResult {
		t.Fatalf("exit = %d, want 30", code)
	}
	if n := e.callCount(t); n != 1 {
		t.Errorf("substantive text forbids retry; calls = %d", n)
	}
}

func TestNoRetryAfterToolEvent(t *testing.T) {
	e := setupFake(t, "tool_then_transient")
	runID := "run-noretry-tool-1"
	code, _, _ := e.run(t, envelopeText(runID, e.admittedOID, "docs/"), nil)

	if code != ExitMalformedResult {
		t.Fatalf("exit = %d, want 30", code)
	}
	if n := e.callCount(t); n != 1 {
		t.Errorf("tool events forbid retry; calls = %d", n)
	}
}

func TestNoRetryForNonTransientError(t *testing.T) {
	e := setupFake(t, "nontransient_error")
	runID := "run-noretry-auth-1"
	code, _, _ := e.run(t, envelopeText(runID, e.admittedOID, "docs/"), nil)

	if code != ExitMalformedResult {
		t.Fatalf("exit = %d, want 30 (auth failure is not classifiable as transport-transient)", code)
	}
	if n := e.callCount(t); n != 1 {
		t.Errorf("non-transient errors forbid retry; calls = %d", n)
	}
}

func TestEnvelopeRejectionsNeverInvokeWorker(t *testing.T) {
	e := setupFake(t, "complete") // would succeed if ever invoked
	base := e.admittedOID

	cases := map[string]string{
		"unsafe run_id": "---\nrun_id: ../evil\nadmitted_base: " + base + "\nwatch_surfaces:\n  - docs/\n---\nGOAL\ng\nSETTLED FACTS\ns\nCHANGE BOUNDARY\nc\nPRIMARY PROOF\np\nSTOP CONDITIONS\nst\n",
		"bad oid":       "---\nrun_id: ok-x\nadmitted_base: deadbeef\nwatch_surfaces:\n  - docs/\n---\nGOAL\ng\nSETTLED FACTS\ns\nCHANGE BOUNDARY\nc\nPRIMARY PROOF\np\nSTOP CONDITIONS\nst\n",
	}
	for name, input := range cases {
		code, stdout, _ := e.run(t, input, nil)
		if code != ExitRunnerError {
			t.Errorf("%s: exit = %d, want 40", name, code)
		}
		keys := stdoutKeys(t, stdout)
		if keys["ERROR_KIND"] != "MALFORMED_INPUT" {
			t.Errorf("%s: ERROR_KIND = %q", name, keys["ERROR_KIND"])
		}
	}
	if n := e.callCount(t); n != 0 {
		t.Errorf("invalid envelopes must never reach the worker; calls = %d", n)
	}
}

// --- repository binding (explicit target over implicit cwd) -------------------

// assertRepoUntouched proves a repository received zero mutation/admission
// artifacts: no candidate ref, no task worktree in its cache namespace, no
// liveness lease, and an unchanged clean checkout.
func assertRepoUntouched(t *testing.T, root, cacheRoot, runID string) {
	t.Helper()
	ctx := context.Background()

	if _, exists := refOID(t, root, "refs/build-candidates/"+runID); exists {
		t.Errorf("repository %s must not receive candidate ref refs/build-candidates/%s", root, runID)
	}

	commonDir, err := gitx.CanonicalCommonDir(ctx, root)
	if err != nil {
		t.Fatalf("common dir for %s: %v", root, err)
	}
	repoID := gitx.RepoID(commonDir)

	wtParent := filepath.Join(gitx.CanonicalPath(cacheRoot), "worktrees", repoID)
	entries, err := os.ReadDir(wtParent)
	if err == nil && len(entries) > 0 {
		names := make([]string, 0, len(entries))
		for _, en := range entries {
			names = append(names, en.Name())
		}
		t.Errorf("wrong repository %s must not receive task worktrees; found %v under %s", root, names, wtParent)
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read %s: %v", wtParent, err)
	}

	lease := ownership.NewLease(gitx.CanonicalPath(cacheRoot), repoID, runID)
	if _, err := os.Lstat(lease.Path()); err == nil {
		t.Errorf("wrong repository %s must not receive an ownership lease (%s)", root, lease.Path())
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat lease %s: %v", lease.Path(), err)
	}

	if dirty := gitStatusDirty(t, root); dirty {
		t.Errorf("wrong repository %s checkout was mutated", root)
	}
}

func gitStatusDirty(t *testing.T, root string) bool {
	t.Helper()
	out, err := gitx.Run(context.Background(), root, "status", "--porcelain=v1")
	if err != nil {
		t.Fatalf("status for %s: %v", root, err)
	}
	return strings.TrimSpace(out) != ""
}

// TestExplicitRepoWinsOverWrongCWD is the direct prevention of the proven
// incident: the process cwd corresponds to repo B while the mutating BUILD
// explicitly targets repo A. Repo A is mutated through its own disposable
// worktree and repo B receives zero admission artifacts of any kind.
func TestExplicitRepoWinsOverWrongCWD(t *testing.T) {
	e := setupFake(t, "complete") // repo A: the explicit mutating target
	bRoot, _ := initRepo(t)       // repo B: the (wrong) process working directory

	origWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(bRoot); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(origWd) })
	if wd, werr := os.Getwd(); werr != nil || wd == origWd {
		t.Fatalf("test harness could not establish wrong-cwd context (wd=%q): %v", wd, werr)
	}

	runID := "run-explicit-wins-1"
	code, stdout, stderrOut := e.run(t, envelopeText(runID, e.admittedOID, "docs/", "src/main.go"), nil)

	if code != ExitComplete {
		t.Fatalf("exit = %d, want 0\nstdout:\n%s\nstderr:\n%s", code, stdout, stderrOut)
	}

	// The explicitly requested repository A was targeted.
	candidate, exists := refOID(t, e.repoRoot, "refs/build-candidates/"+runID)
	if !exists || candidate == "" {
		t.Fatalf("explicitly requested repository A did not receive its candidate ref\nstdout:\n%s", stdout)
	}
	assertWorktreeGone(t, e.worktreePath(t, runID))

	// The wrong-cwd repository B received zero mutations/admissions.
	assertRepoUntouched(t, bRoot, e.cacheRoot, runID)
}

// TestRequestedRepoIdentityMismatchFailsClosed proves that an admitted base
// belonging to a different repository than the explicitly requested one fails
// closed BEFORE any lease persistence, worktree creation, candidate ref, or
// executor invocation.
func TestRequestedRepoIdentityMismatchFailsClosed(t *testing.T) {
	e := setupFake(t, "complete") // repo A: the explicit requested target
	bRoot, bHead := initRepo(t)   // repo B: owns the admitted base

	// Give repo B a unique commit: two fresh repos created within the same
	// second can otherwise share an identical root commit oid.
	writeFile(t, filepath.Join(bRoot, "docs", "notes.txt"), "unique-to-b\n")
	git(t, "-C", bRoot, "add", "-A")
	git(t, "-C", bRoot, "commit", "-qm", "unique commit in B")
	bHead = git(t, "-C", bRoot, "rev-parse", "HEAD")
	if gitx.ResolveCommit(context.Background(), e.repoRoot, bHead) {
		t.Fatalf("precondition broken: repo B head %s resolves in repo A", bHead)
	}

	runID := "run-mismatch-1"
	code, stdout, stderrOut := e.run(t, envelopeText(runID, bHead, "docs/"), nil)

	if code != ExitRunnerError {
		t.Fatalf("exit = %d, want 40\nstdout:\n%s\nstderr:\n%s", code, stdout, stderrOut)
	}
	if keys := stdoutKeys(t, stdout); keys["ERROR_KIND"] != "MALFORMED_INPUT" {
		t.Errorf("ERROR_KIND = %q, want MALFORMED_INPUT\nstdout:\n%s", keys["ERROR_KIND"], stdout)
	}
	if !strings.Contains(stderrOut, "does not resolve to an exact commit in the requested repository") {
		t.Errorf("failure must name the identity mismatch; stderr:\n%s", stderrOut)
	}

	// Neither repository received any artifact; the worker never ran.
	assertRepoUntouched(t, e.repoRoot, e.cacheRoot, runID)
	assertRepoUntouched(t, bRoot, e.cacheRoot, runID)
	if n := e.callCount(t); n != 0 {
		t.Errorf("identity mismatch must fail before the executor; calls = %d", n)
	}
}

// TestWorktreeParentIdentityBoundToRequestedRepo proves the owned task
// worktree resolves to exactly the Git common-dir/repo identity of the
// explicitly requested repository — not to cwd, basename, or any other hint.
func TestWorktreeParentIdentityBoundToRequestedRepo(t *testing.T) {
	e := setupFake(t, "complete")
	runID := "run-wt-identity-1"
	ctx := context.Background()

	env, perr := envelope.Parse(envelopeText(runID, e.admittedOID, "docs/", "src/main.go"))
	if perr != nil {
		t.Fatal(perr)
	}
	rec, rerr := resolveRepository(e.repoRoot, env)
	if rerr != nil {
		t.Fatalf("resolveRepository failed: %v", rerr)
	}

	// Locator vs identity separation: the locator is the canonical requested
	// path; the identity comes from what Git proved about it.
	if want := gitx.CanonicalPath(e.repoRoot); rec.repoLocator != want {
		t.Errorf("repoLocator = %q, want canonical requested path %q", rec.repoLocator, want)
	}
	if rec.commonDir == "" || rec.repoRoot == "" {
		t.Fatalf("identity facts missing: commonDir=%q repoRoot=%q", rec.commonDir, rec.repoRoot)
	}

	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	cfg := Config{CacheRoot: e.cacheRoot}
	if rerr := preflight(ctx, cfg, rec, logger); rerr != nil {
		t.Fatalf("preflight failed: %v", rerr)
	}
	if rerr := establishLeaseAndWorktree(ctx, cfg, rec, logger); rerr != nil {
		t.Fatalf("establish failed: %v", rerr)
	}

	// The worktree parent directory is derived from the requested repo's identity.
	wantParent := filepath.Join(gitx.CanonicalPath(e.cacheRoot), "worktrees", gitx.RepoID(rec.commonDir))
	if got := filepath.Dir(rec.worktree); got != wantParent {
		t.Errorf("worktree parent = %q, want identity-bound %q", got, wantParent)
	}

	// The created worktree resolves to the SAME common-dir as the requested repo.
	wtCommonDir, err := gitx.CanonicalCommonDir(ctx, rec.worktree)
	if err != nil || wtCommonDir != rec.commonDir {
		t.Fatalf("worktree common-dir mismatch: got %q (err=%v), want %q", wtCommonDir, err, rec.commonDir)
	}

	if rerr := removeOwnedWorktree(ctx, rec, e.admittedOID, logger); rerr != nil {
		t.Fatalf("removal failed: %v", rerr)
	}
	assertWorktreeGone(t, rec.worktree)
}

// TestDistinctSameBasenameRepositoriesNotConfused proves identity comes from
// canonical Git/path evidence, never from basename branding: two repositories
// with the identical basename "repo" remain distinct mutating domains.
func TestDistinctSameBasenameRepositoriesNotConfused(t *testing.T) {
	rootA, headA := createNamedRepo(t, filepath.Join(t.TempDir(), "one"), "repo")
	rootB, _ := createNamedRepo(t, filepath.Join(t.TempDir(), "two"), "repo")
	e := setupFakeAt(t, "complete", rootA, headA)

	runID := "run-distinct-1"
	code, stdout, stderrOut := e.run(t, envelopeText(runID, headA, "docs/", "src/main.go"), nil)

	if code != ExitComplete {
		t.Fatalf("exit = %d, want 0\nstdout:\n%s\nstderr:\n%s", code, stdout, stderrOut)
	}
	if _, exists := refOID(t, rootA, "refs/build-candidates/"+runID); !exists {
		t.Errorf("requested repo A must hold the candidate ref\nstdout:\n%s", stdout)
	}
	assertRepoUntouched(t, rootB, e.cacheRoot, runID)
}

// TestEmptyLocatorFailsClosed pins the fail-closed boundary: with no explicit
// locator there is no implicit-cwd fallback for mutating execution.
func TestEmptyLocatorFailsClosed(t *testing.T) {
	e := setupFake(t, "complete") // would succeed if ever invoked
	runID := "run-empty-locator-1"

	var stdout, stderr bytes.Buffer
	cfg := Config{
		WallClock: 30 * time.Second,
		CacheRoot: e.cacheRoot,
		BinPath:   e.fakeBin,
		Stdout:    &stdout,
		Stderr:    &stderr,
	}
	code := Run(context.Background(), cfg, "", []byte(envelopeText(runID, e.admittedOID, "docs/")))

	if code != ExitRunnerError {
		t.Fatalf("exit = %d, want 40", code)
	}
	keys := stdoutKeys(t, stdout.String())
	if keys["RUN_RESULT"] != "RUNNER_ERROR" || keys["ERROR_KIND"] != "MALFORMED_INPUT" {
		t.Errorf("stdout:\n%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "working directory is never used") {
		t.Errorf("refusal must state the no-cwd rule; stderr:\n%s", stderr.String())
	}
	assertRepoUntouched(t, e.repoRoot, e.cacheRoot, runID)
	if n := e.callCount(t); n != 0 {
		t.Errorf("empty locator must never reach the worker; calls = %d", n)
	}
}

// --- representative non-Git PRIMARY PROOF capability -------------------------

// TestNonGitProofSucceeds proves the worker can execute ordinary non-Git shell
// commands (build/test/lint/proof) as required by PRIMARY PROOF. The fake
// simulates a Go test passing and the runner must accept the COMPLETE outcome.
func TestNonGitProofSucceeds(t *testing.T) {
	e := setupFake(t, "non_git_proof")
	runID := "run-non-git-proof-1"
	code, stdout, stderrOut := e.run(t, envelopeText(runID, e.admittedOID, "docs/"), nil)

	if code != ExitComplete {
		t.Fatalf("exit = %d, want 0\nstdout:\n%s\nstderr:\n%s", code, stdout, stderrOut)
	}
	keys := stdoutKeys(t, stdout)
	if keys["RUN_RESULT"] != "COMPLETE" {
		t.Errorf("RUN_RESULT = %q", keys["RUN_RESULT"])
	}
	if _, exists := refOID(t, e.repoRoot, "refs/build-candidates/"+runID); !exists {
		t.Error("candidate ref must be created for non-Git PRIMARY PROOF success")
	}
	assertWorktreeGone(t, e.worktreePath(t, runID))
	if n := e.callCount(t); n != 1 {
		t.Errorf("calls = %d, want 1", n)
	}
}

// TestFailingNonGitProofProducesBlocked proves that when PRIMARY PROOF fails
// (e.g. go test exits non-zero), the worker reports BLOCKED and no candidate
// ref is created. This validates that the worker does not coerce failure into
// a false COMPLETE when shell proof commands fail.
func TestFailingNonGitProofProducesBlocked(t *testing.T) {
	e := setupFake(t, "failing_test_blocked")
	runID := "run-failing-proof-1"
	code, stdout, _ := e.run(t, envelopeText(runID, e.admittedOID, "docs/"), nil)

	if code != ExitBlocked {
		t.Fatalf("exit = %d, want 20\nstdout:\n%s", code, stdout)
	}
	keys := stdoutKeys(t, stdout)
	if keys["RUN_RESULT"] != "BLOCKED" {
		t.Errorf("RUN_RESULT = %q", keys["RUN_RESULT"])
	}
	if keys["BLOCKER"] == "" {
		t.Error("BLOCKED must carry a BLOCKER field")
	}
	if keys["EVIDENCE"] == "" {
		t.Error("BLOCKED must carry EVIDENCE")
	}
	if _, exists := refOID(t, e.repoRoot, "refs/build-candidates/"+runID); exists {
		t.Error("no candidate ref may exist for BLOCKED")
	}
	assertWorktreeGone(t, e.worktreePath(t, runID))
}
