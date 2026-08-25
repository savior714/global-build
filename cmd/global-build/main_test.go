package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"global-build/internal/gitx"
	"global-build/internal/ownership"
)

var binPath string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "gb-bin")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	binPath = filepath.Join(dir, "global-build")
	cmd := exec.Command("go", "build", "-o", binPath, ".")
	cmd.Env = append(os.Environ(), "GOTOOLCHAIN=go1.27.0")
	if out, berr := cmd.CombinedOutput(); berr != nil {
		fmt.Fprintf(os.Stderr, "build failed: %v\n%s\n", berr, out)
		os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func runBin(t *testing.T, args []string, stdin string, env map[string]string) (int, string, string) {
	t.Helper()
	cmd := exec.Command(binPath, args...)
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			t.Fatalf("exec error: %v", err)
		}
	}
	return code, stdout.String(), stderr.String()
}

func initCLITestRepo(t *testing.T) (repoRoot, admittedOID string) {
	t.Helper()
	repoRoot = filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repoRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	gitCLI(t, "", "init", "-q", "-b", "main", repoRoot)
	gitCLI(t, repoRoot, "config", "user.email", "gb-test@example.com")
	gitCLI(t, repoRoot, "config", "user.name", "gb-test")
	gitCLI(t, repoRoot, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(repoRoot, "notes.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCLI(t, repoRoot, "add", "-A")
	gitCLI(t, repoRoot, "commit", "-qm", "initial")
	admittedOID = strings.TrimSpace(gitCLI(t, repoRoot, "rev-parse", "HEAD"))
	return repoRoot, admittedOID
}

func gitCLI(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return string(out)
}

func createCLIOrphan(t *testing.T, cacheDir, repoRoot, repoID, runID, reason, admittedOID string) string {
	t.Helper()
	wtParent := filepath.Join(cacheDir, "worktrees", repoID)
	if err := os.MkdirAll(wtParent, 0o755); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(wtParent, runID)
	gitCLI(t, repoRoot, "worktree", "add", "--detach", "--lock", "--reason", reason, wt, admittedOID)

	// Write the bounded, unheld lease identity.
	lease := ownership.NewLease(cacheDir, repoID, runID)
	id := ownership.LeaseIdentity{Version: ownership.LeaseVersion, RepoID: repoID, RunID: runID, AdmittedBase: admittedOID}
	if err := lease.Establish(id); err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	return wt
}

func TestCLICleanupInspectAndApply(t *testing.T) {
	repoRoot, admittedOID := initCLITestRepo(t)
	cacheDir := t.TempDir()
	commonDir, err := gitx.CanonicalCommonDir(context.Background(), repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	repoID := gitx.RepoID(commonDir)
	runID := "run-cli-1"
	reason := ownership.BuildReason(repoID, runID, admittedOID)
	wt := createCLIOrphan(t, cacheDir, repoRoot, repoID, runID, reason, admittedOID)
	env := map[string]string{"GLOBAL_BUILD_CACHE_ROOT": cacheDir}

	// Inspect-only must classify and print ELIGIBLE, with zero removal.
	code, out, _ := runBin(t, []string{"cleanup", "--repo", repoRoot}, "", env)
	if code != 0 {
		t.Fatalf("inspect exit=%d want 0", code)
	}
	if !strings.Contains(out, "ELIGIBLE:") || !strings.Contains(out, runID) {
		t.Errorf("inspect missing eligible orphan:\n%s", out)
	}
	if _, err := os.Lstat(wt); err != nil {
		t.Errorf("inspect must not remove the worktree: %v", err)
	}

	// Apply must remove exactly the orphan.
	code, out, _ = runBin(t, []string{"cleanup", "--repo", repoRoot, "--apply"}, "", env)
	if code != 0 {
		t.Fatalf("apply exit=%d want 0\nout=%s", code, out)
	}
	if !strings.Contains(out, "REMOVED:") || !strings.Contains(out, runID) {
		t.Errorf("apply missing removed orphan:\n%s", out)
	}
	if _, err := os.Lstat(wt); err == nil {
		t.Error("orphan worktree not removed by CLI apply")
	}
}

func TestCLICleanupArgValidation(t *testing.T) {
	repoRoot, _ := initCLITestRepo(t)
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"no repo", []string{"cleanup"}, 40},
		{"duplicate repo", []string{"cleanup", "--repo", repoRoot, "--repo", repoRoot}, 40},
		{"bogus flag", []string{"cleanup", "--repo", repoRoot, "--bogus"}, 40},
		{"force-all rejected", []string{"cleanup", "--force-all", "--repo", repoRoot}, 40},
		{"apply without subcommand", []string{"--apply"}, 40},
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

func TestCLIBuildPathIntact(t *testing.T) {
	// No subcommand + empty stdin must still run the BUILD path (and fail closed
	// on a malformed envelope), proving the normal path is intact.
	code, _, _ := runBin(t, []string{}, "", nil)
	if code != 40 {
		t.Errorf("normal BUILD path exit=%d want 40", code)
	}
}
