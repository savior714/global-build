package freshness_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"global-build/internal/freshness"
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
	return cmd.Run() == nil
}

func refValue(t *testing.T, dir, ref string) string {
	return gt(t, dir, "-C", dir, "rev-parse", "--verify", ref)
}

type frEnv struct {
	t      *testing.T
	source string
	bare   string
	base   string
	tip    string
}

func setupFresh(t *testing.T) *frEnv {
	pr := &frEnv{t: t}
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
	pr.tip = pr.base
	gt(t, pr.source, "remote", "add", "origin", pr.bare)
	gt(t, pr.source, "push", "origin", pr.base+":refs/heads/main")
	return pr
}

// advance moves origin/main to a fast-forward descendant of the current tip
// that additionally touches rel, and updates the tracked tip.
func (pr *frEnv) advance(t *testing.T, rel, content string) string {
	gt(t, pr.source, "checkout", "-q", "-b", "advance-"+strings.ReplaceAll(rel, "/", "_"), pr.tip)
	writeFile(t, filepath.Join(pr.source, rel), content)
	gt(t, pr.source, "add", "-A")
	gt(t, pr.source, "commit", "-qm", "advance "+rel)
	oid := gt(t, pr.source, "rev-parse", "HEAD")
	gt(t, pr.source, "push", "origin", "advance-"+strings.ReplaceAll(rel, "/", "_")+":refs/heads/main")
	pr.tip = oid
	gt(t, pr.source, "checkout", "-q", "main")
	return oid
}

// diverge moves origin/main to a commit that is NOT a descendant of base.
func (pr *frEnv) diverge(t *testing.T) string {
	gt(t, pr.source, "checkout", "--orphan", "diverge")
	gt(t, pr.source, "rm", "-rf", ".")
	writeFile(t, filepath.Join(pr.source, "other.txt"), "other\n")
	gt(t, pr.source, "add", "-A")
	gt(t, pr.source, "commit", "-qm", "diverge")
	oid := gt(t, pr.source, "rev-parse", "HEAD")
	gt(t, pr.source, "push", "origin", "diverge:refs/heads/main", "--force")
	gt(t, pr.source, "checkout", "-q", "main")
	return oid
}

func (pr *frEnv) cfg(watches ...string) freshness.Config {
	return freshness.Config{Repo: pr.source, Base: pr.base, Watches: watches}
}

func computeFresh(t *testing.T, cfg freshness.Config) freshness.Outcome {
	t.Helper()
	cfg.Out = io.Discard
	cfg.Err = io.Discard
	return freshness.Compute(context.Background(), cfg)
}

func runFresh(t *testing.T, cfg freshness.Config) (int, string) {
	t.Helper()
	var so bytes.Buffer
	cfg.Out = &so
	cfg.Err = io.Discard
	code := freshness.Run(context.Background(), cfg)
	return code, so.String()
}

// --- N. base unchanged -------------------------------------------------------

func TestFreshnessBaseUnchanged(t *testing.T) {
	pr := setupFresh(t)
	code, stdout := runFresh(t, pr.cfg("src/"))
	if code != freshness.ExitUnchanged {
		t.Fatalf("exit=%d want 0", code)
	}
	if !strings.Contains(stdout, "FRESHNESS_RESULT: BASE_UNCHANGED") {
		t.Errorf("stdout missing BASE_UNCHANGED:\n%s", stdout)
	}
	o := computeFresh(t, pr.cfg("src/"))
	if o.FreshnessResult != freshness.ResultBaseUnchanged {
		t.Errorf("FRESHNESS_RESULT=%q", o.FreshnessResult)
	}
	if o.Fields["NEW_ADMITTED_BASE"] != pr.base {
		t.Errorf("NEW_ADMITTED_BASE=%q want %q", o.Fields["NEW_ADMITTED_BASE"], pr.base)
	}
	if o.Fields["LATEST_MAIN"] != pr.base {
		t.Errorf("LATEST_MAIN=%q want %q", o.Fields["LATEST_MAIN"], pr.base)
	}
}

// --- O. advanced, no overlap ------------------------------------------------

func TestFreshnessAdvancedNoOverlap(t *testing.T) {
	pr := setupFresh(t)
	latest := pr.advance(t, "docs/notes.txt", "changed\n")
	o := computeFresh(t, pr.cfg("src/"))
	if o.FreshnessResult != freshness.ResultAdvancedNoOverlap {
		t.Fatalf("FRESHNESS_RESULT=%q (LATEST=%q)", o.FreshnessResult, o.Fields["LATEST_MAIN"])
	}
	if o.ExitCode != freshness.ExitUnchanged {
		t.Errorf("exit=%d want 0", o.ExitCode)
	}
	if o.Fields["LATEST_MAIN"] != latest {
		t.Errorf("LATEST_MAIN=%q want %q", o.Fields["LATEST_MAIN"], latest)
	}
	if o.Fields["NEW_ADMITTED_BASE"] != latest {
		t.Errorf("NEW_ADMITTED_BASE=%q want %q", o.Fields["NEW_ADMITTED_BASE"], latest)
	}
	if o.Fields["OVERLAPPING_SURFACE"] != "NONE" {
		t.Errorf("OVERLAPPING_SURFACE=%q want NONE", o.Fields["OVERLAPPING_SURFACE"])
	}
	if !strings.Contains(o.Fields["CHANGED_SURFACE"], "docs/notes.txt") {
		t.Errorf("CHANGED_SURFACE=%q should include docs/notes.txt", o.Fields["CHANGED_SURFACE"])
	}
}

// --- P. overlap review required ---------------------------------------------

func TestFreshnessOverlapReview(t *testing.T) {
	pr := setupFresh(t)
	pr.advance(t, "src/app.go", "package main // changed\n")
	o := computeFresh(t, pr.cfg("src/"))
	if o.FreshnessResult != freshness.ResultOverlapReview {
		t.Fatalf("FRESHNESS_RESULT=%q", o.FreshnessResult)
	}
	if o.ExitCode != freshness.ExitReview {
		t.Errorf("exit=%d want 10", o.ExitCode)
	}
	if o.Fields["OVERLAPPING_SURFACE"] != "src/app.go" {
		t.Errorf("OVERLAPPING_SURFACE=%q want src/app.go", o.Fields["OVERLAPPING_SURFACE"])
	}
}

// --- Q. history review required (divergence) --------------------------------

func TestFreshnessHistoryReview(t *testing.T) {
	pr := setupFresh(t)
	pr.diverge(t)
	o := computeFresh(t, pr.cfg("src/"))
	if o.FreshnessResult != freshness.ResultHistoryReview {
		t.Fatalf("FRESHNESS_RESULT=%q", o.FreshnessResult)
	}
	if o.ExitCode != freshness.ExitReview {
		t.Errorf("exit=%d want 10", o.ExitCode)
	}
	if o.Fields["NEW_ADMITTED_BASE"] != "NONE" {
		t.Errorf("NEW_ADMITTED_BASE=%q want NONE (divergence)", o.Fields["NEW_ADMITTED_BASE"])
	}
}

// --- R. invalid base (full OID, unresolvable) -------------------------------

func TestFreshnessInvalidBase(t *testing.T) {
	pr := setupFresh(t)
	cfg := pr.cfg("src/")
	cfg.Base = "0000000000000000000000000000000000000000"
	o := computeFresh(t, cfg)
	if o.FreshnessResult != freshness.ResultError {
		t.Fatalf("FRESHNESS_RESULT=%q", o.FreshnessResult)
	}
	if o.Fields["ERROR_KIND"] != freshness.ErrInvalidBase {
		t.Errorf("ERROR_KIND=%q want %q", o.Fields["ERROR_KIND"], freshness.ErrInvalidBase)
	}
	if o.ExitCode != freshness.ExitError {
		t.Errorf("exit=%d want 40", o.ExitCode)
	}
}

// --- S. repo unresolvable ---------------------------------------------------

func TestFreshnessRepoUnresolvable(t *testing.T) {
	o := computeFresh(t, freshness.Config{Repo: t.TempDir(), Base: strings.Repeat("a", 40), Watches: []string{"src/"}})
	if o.FreshnessResult != freshness.ResultError {
		t.Fatalf("FRESHNESS_RESULT=%q", o.FreshnessResult)
	}
	if o.Fields["ERROR_KIND"] != freshness.ErrRepoUnresolvable {
		t.Errorf("ERROR_KIND=%q want %q", o.Fields["ERROR_KIND"], freshness.ErrRepoUnresolvable)
	}
	if o.ExitCode != freshness.ExitError {
		t.Errorf("exit=%d want 40", o.ExitCode)
	}
}

// --- T. remote main unresolvable (no origin) --------------------------------

func TestFreshnessRemoteUnresolvable(t *testing.T) {
	pr := &frEnv{t: t}
	pr.source = filepath.Join(t.TempDir(), "src")
	gt(t, "", "init", "-q", "-b", "main", pr.source)
	gt(t, pr.source, "config", "user.email", "x@example.com")
	gt(t, pr.source, "config", "user.name", "x")
	gt(t, pr.source, "config", "commit.gpgsign", "false")
	writeFile(t, filepath.Join(pr.source, "src", "main.go"), "package main\n")
	gt(t, pr.source, "add", "-A")
	gt(t, pr.source, "commit", "-qm", "base")
	pr.base = gt(t, pr.source, "rev-parse", "HEAD")

	o := computeFresh(t, pr.cfg("src/"))
	if o.FreshnessResult != freshness.ResultError {
		t.Fatalf("FRESHNESS_RESULT=%q", o.FreshnessResult)
	}
	if o.Fields["ERROR_KIND"] != freshness.ErrRemoteMainUnresolvable {
		t.Errorf("ERROR_KIND=%q want %q", o.Fields["ERROR_KIND"], freshness.ErrRemoteMainUnresolvable)
	}
	if o.ExitCode != freshness.ExitError {
		t.Errorf("exit=%d want 40", o.ExitCode)
	}
}

// --- U. read-only / no mutation ---------------------------------------------

func TestFreshnessReadOnly(t *testing.T) {
	pr := setupFresh(t)
	latest := pr.advance(t, "docs/notes.txt", "changed\n")
	// Seed a candidate ref that must remain untouched.
	gt(t, pr.source, "update-ref", "refs/build-candidates/run-fresh-1", pr.base)
	headBefore := refValue(t, pr.source, "HEAD")

	o := computeFresh(t, pr.cfg("src/"))
	if o.FreshnessResult != freshness.ResultAdvancedNoOverlap {
		t.Fatalf("FRESHNESS_RESULT=%q", o.FreshnessResult)
	}

	// origin/main unchanged by the read-only command.
	if got := refValue(t, pr.bare, "refs/heads/main"); got != latest {
		t.Errorf("origin/main mutated by freshness: %q want %q", got, latest)
	}
	// local HEAD unchanged.
	if got := refValue(t, pr.source, "HEAD"); got != headBefore {
		t.Errorf("local HEAD mutated: %q want %q", got, headBefore)
	}
	// candidate ref preserved, unchanged.
	if !refExists(t, pr.source, "refs/build-candidates/run-fresh-1") {
		t.Error("candidate ref removed by freshness (must be read-only)")
	}
	if got := refValue(t, pr.source, "refs/build-candidates/run-fresh-1"); got != pr.base {
		t.Errorf("candidate ref changed: %q want %q", got, pr.base)
	}
}

// --- V. multiple overlapping surfaces ---------------------------------------

func TestFreshnessMultipleOverlap(t *testing.T) {
	pr := setupFresh(t)
	pr.advance(t, "src/app.go", "package main // a\n")
	pr.advance(t, "src/lib.go", "package main // b\n")
	pr.advance(t, "docs/notes.txt", "docs\n")
	o := computeFresh(t, pr.cfg("src/", "docs/"))
	if o.FreshnessResult != freshness.ResultOverlapReview {
		t.Fatalf("FRESHNESS_RESULT=%q", o.FreshnessResult)
	}
	if o.Fields["OVERLAPPING_SURFACE"] != "docs/notes.txt,src/app.go,src/lib.go" {
		t.Errorf("OVERLAPPING_SURFACE=%q want sorted union docs/notes.txt,src/app.go,src/lib.go", o.Fields["OVERLAPPING_SURFACE"])
	}
}

// --- W. malformed: zero watch surfaces --------------------------------------

func TestFreshnessZeroWatches(t *testing.T) {
	pr := setupFresh(t)
	o := computeFresh(t, pr.cfg())
	if o.FreshnessResult != freshness.ResultError {
		t.Fatalf("FRESHNESS_RESULT=%q", o.FreshnessResult)
	}
	if o.Fields["ERROR_KIND"] != freshness.ErrMalformedArgument {
		t.Errorf("ERROR_KIND=%q want %q", o.Fields["ERROR_KIND"], freshness.ErrMalformedArgument)
	}
	if o.ExitCode != freshness.ExitError {
		t.Errorf("exit=%d want 40", o.ExitCode)
	}
}
