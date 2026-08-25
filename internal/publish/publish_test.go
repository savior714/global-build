package publish_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"global-build/internal/gitx"
	"global-build/internal/publish"
)

// --- harness -----------------------------------------------------------------

func gt(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			t.Fatalf("git %v in %s: %v\n%s", args, dir, err, ee.Stderr)
		}
		t.Fatalf("git %v in %s: %v", args, dir, err)
	}
	return strings.TrimSpace(string(out))
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

func refExists(t *testing.T, dir, ref string) bool {
	cmd := exec.Command("git", "-C", dir, "rev-parse", "--verify", "--quiet", ref)
	err := cmd.Run()
	return err == nil
}

func refValue(t *testing.T, dir, ref string) string {
	return gt(t, dir, "-C", dir, "rev-parse", "--verify", ref)
}

func writeHook(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

type pubEnv struct {
	t      *testing.T
	source string
	bare   string
	base   string
	runID  string
}

func setupPublish(t *testing.T) *pubEnv {
	pr := &pubEnv{t: t, runID: "run-pub-1"}
	pr.bare = filepath.Join(t.TempDir(), "origin.git")
	pr.source = filepath.Join(t.TempDir(), "src")
	gt(t, "", "init", "--bare", "-b", "main", pr.bare)
	gt(t, "", "init", "-q", "-b", "main", pr.source)
	gt(t, pr.source, "config", "user.email", "x@example.com")
	gt(t, pr.source, "config", "user.name", "x")
	gt(t, pr.source, "config", "commit.gpgsign", "false")
	writeFile(t, filepath.Join(pr.source, "src", "main.go"), "package main\n")
	gt(t, pr.source, "add", "-A")
	gt(t, pr.source, "commit", "-qm", "base")
	pr.base = gt(t, pr.source, "rev-parse", "HEAD")
	gt(t, pr.source, "remote", "add", "origin", pr.bare)
	gt(t, pr.source, "push", "origin", pr.base+":refs/heads/main")
	return pr
}

// makeCandidate creates a one-commit candidate child of base on a throwaway
// branch, sets the candidate ref, and leaves main at base.
func (pr *pubEnv) makeCandidate(t *testing.T, rel, content string) string {
	gt(t, pr.source, "checkout", "-q", "-b", "cand-"+pr.runID, pr.base)
	writeFile(t, filepath.Join(pr.source, rel), content)
	gt(t, pr.source, "add", "-A")
	gt(t, pr.source, "commit", "-qm", "candidate")
	cand := gt(t, pr.source, "rev-parse", "HEAD")
	gt(t, pr.source, "update-ref", "refs/build-candidates/"+pr.runID, cand)
	gt(t, pr.source, "checkout", "-q", "main")
	return cand
}

func (pr *pubEnv) makeMultiCommitCandidate(t *testing.T, rel, content string) string {
	gt(t, pr.source, "checkout", "-q", "-b", "cand-"+pr.runID, pr.base)
	writeFile(t, filepath.Join(pr.source, "a.txt"), "a\n")
	gt(t, pr.source, "add", "-A")
	gt(t, pr.source, "commit", "-qm", "c1")
	writeFile(t, filepath.Join(pr.source, rel), content)
	gt(t, pr.source, "add", "-A")
	gt(t, pr.source, "commit", "-qm", "c2")
	cand := gt(t, pr.source, "rev-parse", "HEAD")
	gt(t, pr.source, "update-ref", "refs/build-candidates/"+pr.runID, cand)
	gt(t, pr.source, "checkout", "-q", "main")
	return cand
}

func (pr *pubEnv) makeMergeCandidate(t *testing.T, rel, content string) string {
	gt(t, pr.source, "checkout", "-q", "-b", "cand-"+pr.runID, pr.base)
	writeFile(t, filepath.Join(pr.source, "a.txt"), "a\n")
	gt(t, pr.source, "add", "-A")
	gt(t, pr.source, "commit", "-qm", "c1")
	gt(t, pr.source, "checkout", "-q", "-b", "side", pr.base)
	writeFile(t, filepath.Join(pr.source, "b.txt"), "b\n")
	gt(t, pr.source, "add", "-A")
	gt(t, pr.source, "commit", "-qm", "c-side")
	gt(t, pr.source, "checkout", "-q", "cand-"+pr.runID)
	gt(t, pr.source, "merge", "-q", "side")
	writeFile(t, filepath.Join(pr.source, rel), content)
	gt(t, pr.source, "add", "-A")
	gt(t, pr.source, "commit", "-qm", "c2")
	cand := gt(t, pr.source, "rev-parse", "HEAD")
	gt(t, pr.source, "update-ref", "refs/build-candidates/"+pr.runID, cand)
	gt(t, pr.source, "checkout", "-q", "main")
	return cand
}

func (pr *pubEnv) createMoved(t *testing.T) string {
	gt(t, pr.source, "checkout", "-q", "-b", "moved", pr.base)
	writeFile(t, filepath.Join(pr.source, "moved.txt"), "moved\n")
	gt(t, pr.source, "add", "-A")
	gt(t, pr.source, "commit", "-qm", "moved")
	moved := gt(t, pr.source, "rev-parse", "HEAD")
	gt(t, pr.source, "push", "origin", "moved:refs/heads/moved")
	gt(t, pr.source, "checkout", "-q", "main")
	return moved
}

func (pr *pubEnv) createOther(t *testing.T) string {
	gt(t, pr.source, "checkout", "-q", "-b", "other", pr.base)
	writeFile(t, filepath.Join(pr.source, "other.txt"), "other\n")
	gt(t, pr.source, "add", "-A")
	gt(t, pr.source, "commit", "-qm", "other")
	other := gt(t, pr.source, "rev-parse", "HEAD")
	gt(t, pr.source, "checkout", "-q", "main")
	return other
}

func (pr *pubEnv) prePushHook(t *testing.T, content string) {
	writeHook(t, filepath.Join(pr.source, ".git", "hooks", "pre-push"), "#!/bin/sh\ncat >/dev/null\n"+content+"\n")
}

func (pr *pubEnv) postReceiveHook(t *testing.T, content string) {
	writeHook(t, filepath.Join(pr.bare, "hooks", "post-receive"), "#!/bin/sh\ncat >/dev/null\n"+content+"\n")
}

func (pr *pubEnv) cfg(cand, watch string) publish.Config {
	return publish.Config{
		Repo:         pr.source,
		RunID:        pr.runID,
		Candidate:    cand,
		AdmittedBase: pr.base,
		Watches:      []string{watch},
	}
}

func computeOut(t *testing.T, cfg publish.Config) (publish.Outcome, string) {
	t.Helper()
	var stdout bytes.Buffer
	cfg.Out = &stdout
	o := publish.Compute(context.Background(), cfg)
	return o, stdout.String()
}

// runPublish exercises the full Run path (emits deterministic stdout/stderr).
func runPublish(t *testing.T, cfg publish.Config) (int, string, string) {
	t.Helper()
	var so, se bytes.Buffer
	cfg.Out = &so
	cfg.Err = &se
	code := publish.Run(context.Background(), cfg)
	return code, so.String(), se.String()
}

// --- A. successful publication ------------------------------------------------

func TestPublishSuccessful(t *testing.T) {
	pr := setupPublish(t)
	// Unrelated ref must survive publication untouched.
	gt(t, pr.source, "update-ref", "refs/heads/feature", pr.base)

	cand := pr.makeCandidate(t, "src/app.go", "package main // changed\n")
	code, stdout, stderr := runPublish(t, pr.cfg(cand, "src/"))

	if code != publish.ExitPublished {
		t.Fatalf("exit=%d want 0\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "PUBLISH_RESULT: PUBLISHED") {
		t.Errorf("stdout missing PUBLISH_RESULT: PUBLISHED:\n%s", stdout)
	}
	if !strings.Contains(stdout, "CANDIDATE_REF_DELETED: YES") {
		t.Errorf("stdout missing CANDIDATE_REF_DELETED: YES:\n%s", stdout)
	}
	if !strings.Contains(stdout, "REMOTE_AFTER: "+cand) {
		t.Errorf("stdout missing REMOTE_AFTER=%s:\n%s", cand, stdout)
	}
	// Bare origin main now equals candidate.
	if got := refValue(t, pr.bare, "refs/heads/main"); got != cand {
		t.Errorf("bare main=%q want candidate %q", got, cand)
	}
	// Candidate ref deleted locally.
	if refExists(t, pr.source, "refs/build-candidates/"+pr.runID) {
		t.Error("candidate ref was not deleted")
	}
	// Unrelated ref unchanged.
	if got := refValue(t, pr.source, "refs/heads/feature"); got != pr.base {
		t.Errorf("unrelated ref changed: %q want %q", got, pr.base)
	}
}

// --- B. already published -----------------------------------------------------

func TestPublishAlreadyPublished(t *testing.T) {
	pr := setupPublish(t)
	cand := pr.makeCandidate(t, "src/app.go", "package main // changed\n")
	// Make live main already equal the candidate.
	gt(t, pr.source, "push", "origin", cand+":refs/heads/main")

	o, _ := computeOut(t, pr.cfg(cand, "src/"))
	if o.ExitCode != publish.ExitPublished {
		t.Fatalf("exit=%d want 0 (%+v)", o.ExitCode, o.Fields)
	}
	if o.Fields["PUBLISH_RESULT"] != publish.ResultAlreadyPublished {
		t.Errorf("PUBLISH_RESULT=%q", o.Fields["PUBLISH_RESULT"])
	}
	if o.Fields["CANDIDATE_REF_DELETED"] != "YES" {
		t.Errorf("CANDIDATE_REF_DELETED=%q", o.Fields["CANDIDATE_REF_DELETED"])
	}
	if refExists(t, pr.source, "refs/build-candidates/"+pr.runID) {
		t.Error("candidate ref should have been CAS-deleted")
	}
}

// --- C. main moved -----------------------------------------------------------

func TestPublishMainMoved(t *testing.T) {
	pr := setupPublish(t)
	moved := pr.createMoved(t)
	// Live main is neither admitted-base nor candidate.
	gt(t, "", "--git-dir="+pr.bare, "update-ref", "refs/heads/main", moved)
	cand := pr.makeCandidate(t, "src/app.go", "package main // changed\n")

	o, _ := computeOut(t, pr.cfg(cand, "src/"))
	if o.ExitCode != publish.ExitStopped {
		t.Fatalf("exit=%d want 20 (%+v)", o.ExitCode, o.Fields)
	}
	if o.Fields["PUBLISH_RESULT"] != publish.ResultStopped || o.Fields["ERROR_KIND"] != publish.ErrMainMoved {
		t.Errorf("result=%q kind=%q", o.Fields["PUBLISH_RESULT"], o.Fields["ERROR_KIND"])
	}
	if !refExists(t, pr.source, "refs/build-candidates/"+pr.runID) {
		t.Error("candidate ref must be preserved on MAIN_MOVED")
	}
	if got := refValue(t, pr.bare, "refs/heads/main"); got != moved {
		t.Errorf("bare main unexpectedly changed: %q", got)
	}
}

// --- D. race / non-fast-forward ----------------------------------------------

func TestPublishRaceNonFastForward(t *testing.T) {
	pr := setupPublish(t)
	moved := pr.createMoved(t)
	cand := pr.makeCandidate(t, "src/app.go", "package main // changed\n")
	// pre-push hook advances live main to a non-ancestor of candidate, allowing
	// the push to proceed (exit 0) but then be rejected as non-fast-forward.
	pr.prePushHook(t, "git --git-dir="+pr.bare+" update-ref refs/heads/main "+moved)

	o, _ := computeOut(t, pr.cfg(cand, "src/"))
	if o.ExitCode != publish.ExitStopped {
		t.Fatalf("exit=%d want 20 (%+v)", o.ExitCode, o.Fields)
	}
	if o.Fields["PUBLISH_RESULT"] != publish.ResultStopped || o.Fields["ERROR_KIND"] != publish.ErrPushRejectedOrRaced {
		t.Errorf("result=%q kind=%q", o.Fields["PUBLISH_RESULT"], o.Fields["ERROR_KIND"])
	}
	if !refExists(t, pr.source, "refs/build-candidates/"+pr.runID) {
		t.Error("candidate ref must be preserved on rejected push")
	}
}

// --- E. candidate-ref mismatch ------------------------------------------------

func TestPublishCandidateRefMismatch(t *testing.T) {
	pr := setupPublish(t)
	cand := pr.makeCandidate(t, "src/app.go", "package main // changed\n")
	other := pr.createOther(t)
	// Point the candidate ref at a different commit than --candidate.
	gt(t, pr.source, "update-ref", "refs/build-candidates/"+pr.runID, other)

	o, _ := computeOut(t, pr.cfg(cand, "src/"))
	if o.ExitCode != publish.ExitError {
		t.Fatalf("exit=%d want 40 (%+v)", o.ExitCode, o.Fields)
	}
	if o.Fields["ERROR_KIND"] != publish.ErrCandidateValidation {
		t.Errorf("ERROR_KIND=%q", o.Fields["ERROR_KIND"])
	}
	// No network mutation: bare main still base, ref still 'other'.
	if got := refValue(t, pr.bare, "refs/heads/main"); got != pr.base {
		t.Errorf("bare main changed: %q", got)
	}
	if got := refValue(t, pr.source, "refs/build-candidates/"+pr.runID); got != other {
		t.Errorf("candidate ref changed: %q want %q", got, other)
	}
}

// --- F. missing candidate ref ------------------------------------------------

func TestPublishMissingCandidateRef(t *testing.T) {
	pr := setupPublish(t)
	cand := pr.makeCandidate(t, "src/app.go", "package main // changed\n")
	// Remove the candidate ref entirely.
	gt(t, pr.source, "update-ref", "-d", "refs/build-candidates/"+pr.runID)

	o, _ := computeOut(t, pr.cfg(cand, "src/"))
	if o.ExitCode != publish.ExitError {
		t.Fatalf("exit=%d want 40 (%+v)", o.ExitCode, o.Fields)
	}
	if o.Fields["ERROR_KIND"] != publish.ErrCandidateValidation {
		t.Errorf("ERROR_KIND=%q", o.Fields["ERROR_KIND"])
	}
	if got := refValue(t, pr.bare, "refs/heads/main"); got != pr.base {
		t.Errorf("bare main changed: %q", got)
	}
}

// --- G. multi-commit candidate ------------------------------------------------

func TestPublishMultiCommitRejected(t *testing.T) {
	pr := setupPublish(t)
	cand := pr.makeMultiCommitCandidate(t, "src/app.go", "package main // changed\n")

	o, _ := computeOut(t, pr.cfg(cand, "src/"))
	if o.ExitCode != publish.ExitError {
		t.Fatalf("exit=%d want 40 (%+v)", o.ExitCode, o.Fields)
	}
	if o.Fields["ERROR_KIND"] != publish.ErrCandidateValidation {
		t.Errorf("ERROR_KIND=%q", o.Fields["ERROR_KIND"])
	}
	if got := refValue(t, pr.bare, "refs/heads/main"); got != pr.base {
		t.Errorf("bare main changed (network mutation): %q", got)
	}
}

// --- H. merge candidate ------------------------------------------------------

func TestPublishMergeRejected(t *testing.T) {
	pr := setupPublish(t)
	cand := pr.makeMergeCandidate(t, "src/app.go", "package main // changed\n")

	o, _ := computeOut(t, pr.cfg(cand, "src/"))
	if o.ExitCode != publish.ExitError {
		t.Fatalf("exit=%d want 40 (%+v)", o.ExitCode, o.Fields)
	}
	if o.Fields["ERROR_KIND"] != publish.ErrCandidateValidation {
		t.Errorf("ERROR_KIND=%q", o.Fields["ERROR_KIND"])
	}
	if got := refValue(t, pr.bare, "refs/heads/main"); got != pr.base {
		t.Errorf("bare main changed (network mutation): %q", got)
	}
}

// --- I. changed path outside WATCH -------------------------------------------

func TestPublishOutsideWatchRejected(t *testing.T) {
	pr := setupPublish(t)
	// Watch src/ but change docs/.
	cand := pr.makeCandidate(t, "docs/notes.txt", "changed\n")

	o, _ := computeOut(t, pr.cfg(cand, "src/"))
	if o.ExitCode != publish.ExitError {
		t.Fatalf("exit=%d want 40 (%+v)", o.ExitCode, o.Fields)
	}
	if o.Fields["ERROR_KIND"] != publish.ErrCandidateValidation {
		t.Errorf("ERROR_KIND=%q", o.Fields["ERROR_KIND"])
	}
	if got := refValue(t, pr.bare, "refs/heads/main"); got != pr.base {
		t.Errorf("bare main changed (network mutation): %q", got)
	}
}

// --- J. push success but remote postcondition mismatch -----------------------

func TestPublishPostconditionMismatch(t *testing.T) {
	pr := setupPublish(t)
	moved := pr.createMoved(t)
	cand := pr.makeCandidate(t, "src/app.go", "package main // changed\n")
	// post-receive rewrites live main away from candidate after a successful push.
	pr.postReceiveHook(t, "git --git-dir="+pr.bare+" update-ref refs/heads/main "+moved)

	o, _ := computeOut(t, pr.cfg(cand, "src/"))
	if o.ExitCode != publish.ExitError {
		t.Fatalf("exit=%d want 40 (%+v)", o.ExitCode, o.Fields)
	}
	if o.Fields["PUBLISH_RESULT"] != publish.ResultError || o.Fields["ERROR_KIND"] != publish.ErrRemotePostcondition {
		t.Errorf("result=%q kind=%q", o.Fields["PUBLISH_RESULT"], o.Fields["ERROR_KIND"])
	}
	if !refExists(t, pr.source, "refs/build-candidates/"+pr.runID) {
		t.Error("candidate ref must be preserved on postcondition mismatch")
	}
}

// --- K. nonzero push while remote == candidate (ambiguous) -------------------

func TestPublishAmbiguousConcurrent(t *testing.T) {
	pr := setupPublish(t)
	cand := pr.makeCandidate(t, "src/app.go", "package main // changed\n")
	// Pre-load the candidate object into the bare repo (push objects are only
	// persisted after the pre-push hook runs, so the hook's update-ref would
	// otherwise fail with a nonexistent-object error).
	gt(t, pr.source, "push", "origin", cand+":refs/heads/__probe")
	// pre-push hook makes live main == candidate but aborts our push.
	pr.prePushHook(t, "git --git-dir="+pr.bare+" update-ref refs/heads/main "+cand+"\nexit 1")

	o, _ := computeOut(t, pr.cfg(cand, "src/"))
	if o.ExitCode != publish.ExitError {
		t.Fatalf("exit=%d want 40 (%+v)", o.ExitCode, o.Fields)
	}
	if o.Fields["PUBLISH_RESULT"] != publish.ResultError || o.Fields["ERROR_KIND"] != publish.ErrAmbiguousConcurrent {
		t.Errorf("result=%q kind=%q", o.Fields["PUBLISH_RESULT"], o.Fields["ERROR_KIND"])
	}
	if !refExists(t, pr.source, "refs/build-candidates/"+pr.runID) {
		t.Error("candidate ref must be preserved on ambiguous concurrent publication")
	}
}

// --- L. candidate ref changes before deletion (CAS protects) -----------------

func TestPublishCandidateRefChanged(t *testing.T) {
	pr := setupPublish(t)
	other := pr.createOther(t)
	cand := pr.makeCandidate(t, "src/app.go", "package main // changed\n")
	// pre-push hook mutates the local candidate ref to a different value during
	// the (otherwise successful) push, so the expected-old CAS must refuse.
	pr.prePushHook(t, "git update-ref refs/build-candidates/"+pr.runID+" "+other)

	o, _ := computeOut(t, pr.cfg(cand, "src/"))
	if o.ExitCode != publish.ExitError {
		t.Fatalf("exit=%d want 40 (%+v)", o.ExitCode, o.Fields)
	}
	if o.Fields["PUBLISH_RESULT"] != publish.ResultPublishedRefPreserv || o.Fields["ERROR_KIND"] != publish.ErrCandidateRefChanged {
		t.Errorf("result=%q kind=%q", o.Fields["PUBLISH_RESULT"], o.Fields["ERROR_KIND"])
	}
	// Ref preserved and NOT rewritten to candidate.
	if got := refValue(t, pr.source, "refs/build-candidates/"+pr.runID); got != other {
		t.Errorf("candidate ref value=%q want preserved %q", got, other)
	}
}

// --- M. contract: push invocation contains no force primitive ----------------

func TestPublishNoForcePrimitive(t *testing.T) {
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	fakeDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "push.log")
	script := "#!/bin/sh\nif [ \"$1\" = \"push\" ]; then printf '%s\\n' \"$*\" >> " +
		singleQuote(logPath) + "\nfi\nexec " + singleQuote(realGit) + " \"$@\"\n"
	gitBin := filepath.Join(fakeDir, "git")
	if err := os.WriteFile(gitBin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	origPATH := os.Getenv("PATH")
	t.Setenv("PATH", fakeDir+string(os.PathListSeparator)+origPATH)
	defer os.Setenv("PATH", origPATH)

	pr := setupPublish(t)
	cand := pr.makeCandidate(t, "src/app.go", "package main // changed\n")

	// Clear any setup-push noise, then run exactly the publish push.
	if err := os.WriteFile(logPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := publish.Run(context.Background(), publish.Config{
		Repo: pr.source, RunID: pr.runID, Candidate: cand, AdmittedBase: pr.base,
		Watches: []string{"src/"}, Out: &stdout, Err: &stderr,
	})
	if code != 0 {
		t.Fatalf("publish exit=%d\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(log)), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected exactly one push invocation, got %d: %q", len(lines), string(log))
	}
	pushLine := lines[0]
	for _, bad := range []string{"--force", "--force-with-lease", "+", "force"} {
		if strings.Contains(pushLine, bad) {
			t.Errorf("push invocation contains forbidden primitive %q: %s", bad, pushLine)
		}
	}
	if !strings.Contains(pushLine, "origin") {
		t.Errorf("push line missing origin: %s", pushLine)
	}
	if !strings.Contains(pushLine, cand+":refs/heads/main") {
		t.Errorf("push line missing expected refspec: %s", pushLine)
	}
}

func singleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ensure gitx import is referenced for ref-existence helpers used above.
var _ = gitx.IsFullOID
