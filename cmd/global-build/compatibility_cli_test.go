package main

import (
	"os"
	"strings"
	"testing"
)

// TestCLIBuildRejectsUnsupportedOpenCodeGenerationBeforeMutation proves the
// compatibility guard runs before the runner can create a lease, worktree, or
// candidate ref. V2 requires a separate acceptance cycle because its permission
// model is not the 1.x contract global-build currently owns.
func TestCLIBuildRejectsUnsupportedOpenCodeGenerationBeforeMutation(t *testing.T) {
	repo, head := initCLIRepoShaped(t)
	cacheDir := t.TempDir()
	env := setupCLIFakeWorker(t, "complete")
	env["GB_FAKE_VERSION"] = "2.0.0"
	env["GLOBAL_BUILD_CACHE_ROOT"] = cacheDir

	runID := "run-cli-v2-refusal"
	code, stdout, stderr := runBinInDir(t, repo,
		[]string{"--repo", repo},
		cliEnvelope(runID, head, "docs/"),
		env)

	if code != 40 {
		t.Fatalf("exit=%d want 40\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "unsupported OpenCode version 2.0.0") {
		t.Fatalf("stderr missing compatibility refusal:\n%s", stderr)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Fatalf("runner must not start or emit a run result on compatibility refusal; stdout:\n%s", stdout)
	}

	assertCLIRepoUntouched(t, repo, cacheDir, runID)
	if callsPath := env["GB_FAKE_CALLS"]; callsPath != "" {
		if b, err := os.ReadFile(callsPath); err == nil && strings.TrimSpace(string(b)) != "" {
			t.Fatalf("worker execution must not start after incompatible version probe; calls:\n%s", b)
		} else if err != nil && !os.IsNotExist(err) {
			t.Fatalf("cannot inspect fake worker calls: %v", err)
		}
	}
}
