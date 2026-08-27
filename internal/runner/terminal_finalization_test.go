package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMalformedTerminalGetsOneExactSessionToollessFinalization(t *testing.T) {
	e := setupFake(t, "malformed_then_finalized")
	argsLog := filepath.Join(filepath.Dir(e.callsFile), "args.log")
	t.Setenv("GB_FAKE_ARGS_LOG", argsLog)
	runID := "run-finalize-1"

	code, stdout, stderrOut := e.run(t, envelopeText(runID, e.admittedOID, "docs/"), nil)
	if code != ExitComplete {
		t.Fatalf("exit = %d, want 0 after protocol-only finalization\nstdout:\n%s\nstderr:\n%s", code, stdout, stderrOut)
	}
	if n := e.callCount(t); n != 2 {
		t.Fatalf("calls = %d, want exactly 2 (primary + one finalizer)", n)
	}
	if stdoutKeys(t, stdout)["RUN_RESULT"] != "COMPLETE" {
		t.Errorf("stdout:\n%s", stdout)
	}
	if _, exists := refOID(t, e.repoRoot, "refs/build-candidates/"+runID); !exists {
		t.Error("valid finalized COMPLETE must still pass deterministic candidate validation")
	}
	assertWorktreeGone(t, e.worktreePath(t, runID))

	raw, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatalf("args log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 {
		t.Fatalf("argv lines = %d, want 2: %q", len(lines), raw)
	}
	if !strings.Contains(lines[1], "--agent global-build-finalizer") {
		t.Errorf("second invocation did not use finalizer agent: %s", lines[1])
	}
	if !strings.Contains(lines[1], "--session ses_1") {
		t.Errorf("second invocation did not continue exact primary session: %s", lines[1])
	}
}

func TestMalformedFinalizerStillFailsClosedWithoutThirdAttempt(t *testing.T) {
	e := setupFake(t, "malformed_then_malformed")
	runID := "run-finalize-malformed-1"

	code, stdout, _ := e.run(t, envelopeText(runID, e.admittedOID, "docs/"), nil)
	if code != ExitMalformedResult {
		t.Fatalf("exit = %d, want 30", code)
	}
	if stdoutKeys(t, stdout)["RUN_RESULT"] != "MALFORMED_RESULT" {
		t.Errorf("stdout:\n%s", stdout)
	}
	if n := e.callCount(t); n != 2 {
		t.Errorf("calls = %d, want exactly 2; finalization must never recurse", n)
	}
	if _, exists := refOID(t, e.repoRoot, "refs/build-candidates/"+runID); exists {
		t.Error("malformed finalizer output must not create candidate ref")
	}
	assertWorktreeGone(t, e.worktreePath(t, runID))
}

func TestMalformedTerminalWithoutExactSessionDoesNotFinalize(t *testing.T) {
	e := setupFake(t, "malformed_no_session")
	runID := "run-finalize-no-session-1"

	code, stdout, _ := e.run(t, envelopeText(runID, e.admittedOID, "docs/"), nil)
	if code != ExitMalformedResult {
		t.Fatalf("exit = %d, want 30", code)
	}
	if stdoutKeys(t, stdout)["RUN_RESULT"] != "MALFORMED_RESULT" {
		t.Errorf("stdout:\n%s", stdout)
	}
	if n := e.callCount(t); n != 1 {
		t.Errorf("calls = %d, want 1; no exact session identity means no finalization", n)
	}
	assertWorktreeGone(t, e.worktreePath(t, runID))
}
