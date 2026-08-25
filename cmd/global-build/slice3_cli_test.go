package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func initPublishRepo(t *testing.T) (repoRoot, bare, base string) {
	t.Helper()
	parent := t.TempDir()
	bare = filepath.Join(parent, "origin.git")
	repoRoot = filepath.Join(parent, "src")
	mk := func(p string) {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mk(bare)
	mk(repoRoot)
	gitCLI(t, "", "init", "--bare", "-b", "main", bare)
	gitCLI(t, "", "init", "-q", "-b", "main", repoRoot)
	gitCLI(t, repoRoot, "config", "user.email", "gb-test@example.com")
	gitCLI(t, repoRoot, "config", "user.name", "gb-test")
	gitCLI(t, repoRoot, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(repoRoot, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCLI(t, repoRoot, "add", "-A")
	gitCLI(t, repoRoot, "commit", "-qm", "base")
	base = strings.TrimSpace(gitCLI(t, repoRoot, "rev-parse", "HEAD"))
	gitCLI(t, repoRoot, "remote", "add", "origin", bare)
	gitCLI(t, repoRoot, "push", "origin", base+":refs/heads/main")
	return repoRoot, bare, base
}

func makeCandidate(t *testing.T, repoRoot, base, rel, content string) string {
	t.Helper()
	gitCLI(t, repoRoot, "checkout", "-q", "-b", "cand-cli", base)
	path := filepath.Join(repoRoot, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCLI(t, repoRoot, "add", "-A")
	gitCLI(t, repoRoot, "commit", "-qm", "candidate")
	oid := strings.TrimSpace(gitCLI(t, repoRoot, "rev-parse", "HEAD"))
	gitCLI(t, repoRoot, "update-ref", "refs/build-candidates/run-cli-pub", oid)
	gitCLI(t, repoRoot, "checkout", "-q", "main")
	return oid
}

func refExistsCLI(t *testing.T, repoRoot, ref string) bool {
	t.Helper()
	cmd := exec.Command("git", "-C", repoRoot, "rev-parse", "--verify", "--quiet", ref)
	return cmd.Run() == nil
}

func TestCLIPublishSuccess(t *testing.T) {
	repoRoot, bare, base := initPublishRepo(t)
	cand := makeCandidate(t, repoRoot, base, "src/app.go", "package main // c\n")
	code, out, _ := runBin(t, []string{
		"publish", "--repo", repoRoot, "--run-id", "run-cli-pub",
		"--candidate", cand, "--admitted-base", base, "--watch", "src/",
	}, "", nil)
	if code != 0 {
		t.Fatalf("exit=%d want 0\n%s", code, out)
	}
	if !strings.Contains(out, "PUBLISH_RESULT: PUBLISHED") {
		t.Errorf("missing PUBLISH_RESULT: PUBLISHED:\n%s", out)
	}
	if !strings.Contains(out, "CANDIDATE_REF_DELETED: YES") {
		t.Errorf("missing CANDIDATE_REF_DELETED: YES:\n%s", out)
	}
	if got := strings.TrimSpace(gitCLI(t, bare, "rev-parse", "refs/heads/main")); got != cand {
		t.Errorf("bare main=%q want candidate %q", got, cand)
	}
	if refExistsCLI(t, repoRoot, "refs/build-candidates/run-cli-pub") {
		t.Error("candidate ref should be deleted after successful publish")
	}
}

func TestCLIPublishMainMoved(t *testing.T) {
	repoRoot, _, base := initPublishRepo(t)
	gitCLI(t, repoRoot, "checkout", "-q", "-b", "moved", base)
	if err := os.WriteFile(filepath.Join(repoRoot, "moved.txt"), []byte("m\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCLI(t, repoRoot, "add", "-A")
	gitCLI(t, repoRoot, "commit", "-qm", "moved")
	gitCLI(t, repoRoot, "push", "origin", "moved:refs/heads/main")
	gitCLI(t, repoRoot, "checkout", "-q", "main")
	cand := makeCandidate(t, repoRoot, base, "src/app.go", "package main // c\n")

	code, out, _ := runBin(t, []string{
		"publish", "--repo", repoRoot, "--run-id", "run-cli-pub",
		"--candidate", cand, "--admitted-base", base, "--watch", "src/",
	}, "", nil)
	if code != 20 {
		t.Fatalf("exit=%d want 20\n%s", code, out)
	}
	if !strings.Contains(out, "ERROR_KIND: MAIN_MOVED") {
		t.Errorf("missing ERROR_KIND: MAIN_MOVED:\n%s", out)
	}
	if !refExistsCLI(t, repoRoot, "refs/build-candidates/run-cli-pub") {
		t.Error("candidate ref must be preserved on MAIN_MOVED")
	}
}

func TestCLIContinuationCheckNoOverlap(t *testing.T) {
	repoRoot, _, base := initPublishRepo(t)
	gitCLI(t, repoRoot, "checkout", "-q", "-b", "adv", base)
	if err := os.MkdirAll(filepath.Join(repoRoot, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "docs", "notes.md"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCLI(t, repoRoot, "add", "-A")
	gitCLI(t, repoRoot, "commit", "-qm", "adv")
	gitCLI(t, repoRoot, "push", "origin", "adv:refs/heads/main")
	gitCLI(t, repoRoot, "checkout", "-q", "main")

	code, out, _ := runBin(t, []string{
		"continuation-check", "--repo", repoRoot, "--base", base, "--watch", "src/",
	}, "", nil)
	if code != 0 {
		t.Fatalf("exit=%d want 0\n%s", code, out)
	}
	if !strings.Contains(out, "FRESHNESS_RESULT: ADVANCED_NO_OVERLAP") {
		t.Errorf("missing FRESHNESS_RESULT: ADVANCED_NO_OVERLAP:\n%s", out)
	}
}

func TestCLIContinuationCheckOverlap(t *testing.T) {
	repoRoot, _, base := initPublishRepo(t)
	gitCLI(t, repoRoot, "checkout", "-q", "-b", "adv", base)
	if err := os.MkdirAll(filepath.Join(repoRoot, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "src", "app.go"), []byte("package main // x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCLI(t, repoRoot, "add", "-A")
	gitCLI(t, repoRoot, "commit", "-qm", "adv")
	gitCLI(t, repoRoot, "push", "origin", "adv:refs/heads/main")
	gitCLI(t, repoRoot, "checkout", "-q", "main")

	code, out, _ := runBin(t, []string{
		"continuation-check", "--repo", repoRoot, "--base", base, "--watch", "src/",
	}, "", nil)
	if code != 10 {
		t.Fatalf("exit=%d want 10 (overlap)\n%s", code, out)
	}
	if !strings.Contains(out, "FRESHNESS_RESULT: OVERLAP_REVIEW_REQUIRED") {
		t.Errorf("missing FRESHNESS_RESULT: OVERLAP_REVIEW_REQUIRED:\n%s", out)
	}
}

func TestCLIPublishArgValidation(t *testing.T) {
	repoRoot, _, base := initPublishRepo(t)
	cand := makeCandidate(t, repoRoot, base, "src/app.go", "package main // c\n")
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"force rejected", []string{"publish", "--repo", repoRoot, "--run-id", "run-cli-pub", "--candidate", cand, "--admitted-base", base, "--watch", "src/", "--force"}, 40},
		{"no repo", []string{"publish", "--run-id", "run-cli-pub", "--candidate", cand, "--admitted-base", base, "--watch", "src/"}, 40},
		{"missing candidate", []string{"publish", "--repo", repoRoot, "--run-id", "run-cli-pub", "--admitted-base", base, "--watch", "src/"}, 40},
		{"duplicate repo", []string{"publish", "--repo", repoRoot, "--repo", repoRoot, "--run-id", "r", "--candidate", cand, "--admitted-base", base, "--watch", "src/"}, 40},
		{"positional rejected", []string{"publish", "extra", "--repo", repoRoot, "--run-id", "r", "--candidate", cand, "--admitted-base", base, "--watch", "src/"}, 40},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, _, _ := runBin(t, c.args, "", nil)
			if code != c.want {
				t.Errorf("args %v: exit=%d want %d", c.args, code, c.want)
			}
		})
	}
}

func TestCLIContinuationArgValidation(t *testing.T) {
	repoRoot, _, base := initPublishRepo(t)
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"no watch", []string{"continuation-check", "--repo", repoRoot, "--base", base}, 40},
		{"bad base", []string{"continuation-check", "--repo", repoRoot, "--base", "not-an-oid", "--watch", "src/"}, 40},
		{"no repo", []string{"continuation-check", "--base", base, "--watch", "src/"}, 40},
		{"unknown subcommand", []string{"frobnicate", "--repo", repoRoot}, 40},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, _, _ := runBin(t, c.args, "", nil)
			if code != c.want {
				t.Errorf("args %v: exit=%d want %d", c.args, code, c.want)
			}
		})
	}
}
