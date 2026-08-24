package runner

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"global-build/internal/gitx"
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
	root = filepath.Join(t.TempDir(), "repo")
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
