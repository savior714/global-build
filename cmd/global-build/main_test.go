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
	// No subcommand + no --repo must fail closed on the missing explicit
	// target repository (exit 40) — the working directory is never silently
	// adopted as the mutating target.
	code, _, stderr := runBin(t, []string{}, "", nil)
	if code != 40 {
		t.Errorf("normal BUILD path exit=%d want 40", code)
	}
	if !strings.Contains(stderr, "--repo") {
		t.Errorf("refusal must point at the required explicit contract; stderr:\n%s", stderr)
	}
}

// --- CLI-level repository-binding proofs --------------------------------------

func initCLIRepoShaped(t *testing.T) (repoRoot, admittedOID string) {
	// Same content shape the runner's fake worker mutates (docs/notes.txt), so
	// a COMPLETE attempt can really commit inside a disposable worktree.
	t.Helper()
	repoRoot = filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repoRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	gitCLI(t, "", "init", "-q", "-b", "main", repoRoot)
	gitCLI(t, repoRoot, "config", "user.email", "gb-test@example.com")
	gitCLI(t, repoRoot, "config", "user.name", "gb-test")
	gitCLI(t, repoRoot, "config", "commit.gpgsign", "false")
	if err := os.MkdirAll(filepath.Join(repoRoot, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "docs", "notes.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCLI(t, repoRoot, "add", "-A")
	gitCLI(t, repoRoot, "commit", "-qm", "initial")
	admittedOID = strings.TrimSpace(gitCLI(t, repoRoot, "rev-parse", "HEAD"))
	return repoRoot, admittedOID
}

// runBinInDir executes the built binary with an explicit child working
// directory, so tests can place the process cwd in one repository while
// explicitly targeting another.
func runBinInDir(t *testing.T, dir string, args []string, stdin string, env map[string]string) (int, string, string) {
	t.Helper()
	cmd := exec.Command(binPath, args...)
	cmd.Dir = dir
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

func cliEnvelope(runID, admitted string, surfaces ...string) string {
	surfs := ""
	for _, s := range surfaces {
		surfs += "  - " + s + "\n"
	}
	return "---\nrun_id: " + runID + "\nadmitted_base: " + admitted + "\nwatch_surfaces:\n" + surfs +
		"---\n\nGOAL\nMake the documented change.\n\nSETTLED FACTS\ndocs/notes.txt is the target.\n\nCHANGE BOUNDARY\nOnly listed surfaces may change.\n\nPRIMARY PROOF\ngit diff shows exactly one committed change inside the surfaces.\n\nSTOP CONDITIONS\nStop on completion or contradiction.\n"
}

// setupCLIFakeWorker installs the shared fake OpenCode binary used by runner
// tests and returns the env entries the CLI needs to pick it up.
func setupCLIFakeWorker(t *testing.T, scenario string) map[string]string {
	t.Helper()
	binDir := t.TempDir()
	fakeBin := filepath.Join(binDir, "fake-opencode")
	raw, err := os.ReadFile(filepath.Join("..", "..", "internal", "runner", "testdata", "fake-opencode.sh"))
	if err != nil {
		t.Fatalf("fixture missing: %v", err)
	}
	if err := os.WriteFile(fakeBin, raw, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(fakeBin, 0o755); err != nil {
		t.Fatal(err)
	}
	return map[string]string{
		"GLOBAL_BUILD_OPENCODE_BIN": fakeBin,
		"GB_FAKE_SCENARIO":          scenario,
		"GB_FAKE_CALLS":             filepath.Join(binDir, "calls.log"),
		"GB_FAKE_STDIN_COPY":        filepath.Join(binDir, "stdin-copy.txt"),
	}
}

// assertCLIRepoUntouched proves at the process boundary that a repository was
// never targeted: no build-candidate refs, only its own registration, a clean
// checkout, and no lease/worktree artifacts under its cache namespace.
func assertCLIRepoUntouched(t *testing.T, root, cacheDir, runID string) {
	t.Helper()
	ctx := context.Background()

	out := gitCLI(t, root, "for-each-ref", "--format=%(refname)", "refs/build-candidates/")
	if strings.TrimSpace(out) != "" {
		t.Errorf("repository %s must not receive build-candidate refs; got:\n%s", root, out)
	}

	wtOut := gitCLI(t, root, "worktree", "list", "--porcelain")
	if !strings.HasPrefix(strings.TrimSpace(wtOut), "worktree ") {
		t.Fatalf("cannot parse worktree list for %s:\n%s", root, wtOut)
	}
	if count := strings.Count(wtOut, "\nworktree ") + 1; count != 1 {
		t.Errorf("repository %s must keep exactly its own worktree registration; saw %d:\n%s", root, count, wtOut)
	}

	status := gitCLI(t, root, "status", "--porcelain=v1")
	if strings.TrimSpace(status) != "" {
		t.Errorf("repository %s checkout was mutated:\n%s", root, status)
	}

	commonDir, err := gitx.CanonicalCommonDir(ctx, root)
	if err != nil {
		t.Fatalf("common dir: %v", err)
	}
	wtParent := filepath.Join(gitx.CanonicalPath(cacheDir), "worktrees", gitx.RepoID(commonDir))
	if _, err := os.Stat(filepath.Join(wtParent, runID)); err == nil {
		t.Errorf("repository %s must not receive task worktree %s", root, filepath.Join(wtParent, runID))
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat %s: %v", filepath.Join(wtParent, runID), err)
	}
	lease := ownership.NewLease(gitx.CanonicalPath(cacheDir), gitx.RepoID(commonDir), runID)
	if _, err := os.Lstat(lease.Path()); err == nil {
		t.Errorf("repository %s must not receive lease %s", root, lease.Path())
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat lease %s: %v", lease.Path(), err)
	}
}

// TestCLIBuildExplicitRepoWinsOverWrongCWD is the binary-boundary reproduction
// of the proven incident: the process cwd sits inside repo B while the
// mutating BUILD explicitly targets repo A. Repo B must receive zero mutation
// or admission artifacts; repo A must be the sole target.
func TestCLIBuildExplicitRepoWinsOverWrongCWD(t *testing.T) {
	repoA, headA := initCLIRepoShaped(t) // explicit mutating target
	repoB, _ := initCLIRepoShaped(t)     // process cwd (wrong repository)

	cacheDir := t.TempDir()
	env := setupCLIFakeWorker(t, "complete")
	env["GLOBAL_BUILD_CACHE_ROOT"] = cacheDir

	runID := "run-cli-cwd-1"
	code, stdout, stderr := runBinInDir(t, repoB,
		[]string{"--repo", repoA},
		cliEnvelope(runID, headA, "docs/"),
		env)

	if code != 0 {
		t.Fatalf("exit=%d want 0\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "RUN_RESULT: COMPLETE") {
		t.Errorf("missing RUN_RESULT: COMPLETE\nstdout:\n%s", stdout)
	}

	candidateRef := "refs/build-candidates/" + runID
	if !refExistsCLI(t, repoA, candidateRef) {
		t.Errorf("explicitly requested repo A must hold candidate ref %s", candidateRef)
	}

	assertCLIRepoUntouched(t, repoB, cacheDir, runID)

	// And A's disposable worktree is gone after COMPLETE.
	commonA, err := gitx.CanonicalCommonDir(context.Background(), repoA)
	if err != nil {
		t.Fatal(err)
	}
	wtA := filepath.Join(gitx.CanonicalPath(cacheDir), "worktrees", gitx.RepoID(commonA), runID)
	if _, err := os.Lstat(wtA); err == nil {
		t.Errorf("disposable worktree still exists at %s", wtA)
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat %s: %v", wtA, err)
	}
}

// TestCLIBuildWithoutRepoInsideRepoRefuses proves the old incident shape is
// dead: invoking the mutating BUILD from inside a repository WITHOUT --repo
// refuses (fail closed) instead of silently adopting that cwd as the target.
func TestCLIBuildWithoutRepoInsideRepoRefuses(t *testing.T) {
	repoB, headB := initCLIRepoShaped(t) // would have been silently targeted before

	cacheDir := t.TempDir()
	env := setupCLIFakeWorker(t, "complete") // would succeed if ever invoked
	env["GLOBAL_BUILD_CACHE_ROOT"] = cacheDir

	runID := "run-cli-norepo-1"
	code, stdout, stderr := runBinInDir(t, repoB,
		nil, // BUILD mode, no --repo: the exact pre-fix incident invocation shape
		cliEnvelope(runID, headB, "docs/"),
		env)

	if code != 40 {
		t.Fatalf("exit=%d want 40\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "--repo") {
		t.Errorf("refusal must name the required explicit contract; stderr:\n%s", stderr)
	}
	assertCLIRepoUntouched(t, repoB, cacheDir, runID)
}

// TestCLIBuildArgValidation pins the narrow BUILD-mode flag surface: exactly
// one --repo, nothing else.
func TestCLIBuildArgValidation(t *testing.T) {
	repoA, _ := initCLIRepoShaped(t)
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"duplicate repo", []string{"--repo", repoA, "--repo", repoA}, 40},
		{"stray apply flag", []string{"--apply"}, 40},
		{"missing value", []string{"--repo"}, 40},
		{"flag-like path rejected", []string{"--repo", "-weird"}, 40},
		{"positional word", []string{"extra", "--repo", repoA}, 40},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, _, stderr := runBin(t, c.args, "", nil)
			if code != c.want {
				t.Errorf("args %v: exit=%d want %d\nstderr:\n%s", c.args, code, c.want, stderr)
			}
		})
	}
}
