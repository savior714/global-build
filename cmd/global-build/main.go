// Command global-build is a thin, stateless, deterministic runner that performs
// exactly one BUILD attempt per invocation.
//
// Input is stdin-only and ephemeral (frontmatter envelope + five task body
// sections). Diagnostics go to stderr; stable result keys go to stdout.
//
// Durable repository-targeting rule: every mutating mode (BUILD, publish,
// cleanup) requires an explicit target repository via --repo <path>. The
// process working directory is never used to select a mutating target; the
// requested path is canonicalized and bound to a proven Git identity before
// any mutation.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"global-build/internal/cleanup"
	"global-build/internal/opencode"
	"global-build/internal/runner"
)

// Environment overrides (used by tests and local installations; normal runs
// need none of them).
const (
	envWallClock      = "GLOBAL_BUILD_WALL_CLOCK"      // Go duration, default 90m
	envProgressWindow = "GLOBAL_BUILD_PROGRESS_WINDOW" // Go duration, default 15m
	envCacheRoot      = "GLOBAL_BUILD_CACHE_ROOT"      // override cache root for tests
)

func main() {
	os.Exit(run())
}

func run() int {
	args := os.Args[1:]
	if len(args) > 0 {
		switch args[0] {
		case "cleanup":
			return runCleanup(args[1:])
		case "publish":
			return runPublish(args[1:])
		case "continuation-check":
			return runContinuationCheck(args[1:])
		default:
			if !strings.HasPrefix(args[0], "-") {
				// No-subcommand stdin BUILD mode is the only other mode.
				// Positional words are never valid there.
				fmt.Fprintf(os.Stderr, "global-build: unknown subcommand %q (use 'cleanup', 'publish', 'continuation-check', or stdin BUILD mode)\n", args[0])
				return runner.ExitRunnerError
			}
			// Flag-led stdin BUILD mode (e.g. `--repo <path>`): parsed below,
			// where anything other than exactly one --repo fails closed.
		}
	}

	// Mutating BUILD admission requires an explicitly requested target
	// repository. There is no implicit-cwd fallback: a missing or malformed
	// --repo fails closed before the envelope is even read.
	repo, err := parseBuildArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "global-build: %v\n", err)
		return runner.ExitRunnerError
	}
	if repo == "" {
		fmt.Fprintf(os.Stderr, "global-build: a mutating BUILD requires an explicit target repository; pass --repo <path> (the working directory is never used as the mutating target)\n")
		return runner.ExitRunnerError
	}
	if strings.HasPrefix(repo, "-") {
		fmt.Fprintf(os.Stderr, "global-build: malformed repository path %q\n", repo)
		return runner.ExitRunnerError
	}

	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "global-build: cannot read stdin: %v\n", err)
		return runner.ExitRunnerError
	}

	bin, err := opencode.Executable(os.Getenv(opencode.EnvBinVar))
	if err != nil {
		fmt.Fprintf(os.Stderr, "global-build: %v\n", err)
		return runner.ExitRunnerError
	}
	if _, err := opencode.CheckCompatibility(context.Background(), bin); err != nil {
		fmt.Fprintf(os.Stderr, "global-build: %v\n", err)
		return runner.ExitRunnerError
	}

	cfg := runner.Config{
		BinPath:   bin,
		CacheRoot: os.Getenv(envCacheRoot),
	}
	if v := os.Getenv(envWallClock); v != "" {
		d, perr := time.ParseDuration(v)
		if perr != nil {
			fmt.Fprintf(os.Stderr, "global-build: invalid %s %q: %v\n", envWallClock, v, perr)
			return runner.ExitRunnerError
		}
		cfg.WallClock = d
	}
	if v := os.Getenv(envProgressWindow); v != "" {
		d, perr := time.ParseDuration(v)
		if perr != nil {
			fmt.Fprintf(os.Stderr, "global-build: invalid %s %q: %v\n", envProgressWindow, v, perr)
			return runner.ExitRunnerError
		}
		cfg.ProgressWindow = d
	}

	return runner.Run(context.Background(), cfg, repo, input)
}

// parseBuildArgs extracts the explicit target repository for the stdin BUILD
// mode. Only --repo <path> is accepted (exactly once); any other flag or
// positional argument is rejected so no implicit target can slip through.
func parseBuildArgs(args []string) (string, error) {
	var repo string
	repoCount := 0
	i := 0
	for i < len(args) {
		a := args[i]
		switch a {
		case "--repo":
			if i+1 >= len(args) {
				return "", fmt.Errorf("--repo requires a path argument")
			}
			if repoCount > 0 {
				return "", fmt.Errorf("--repo may be given exactly once")
			}
			repo = args[i+1]
			repoCount++
			i += 2
		default:
			if strings.HasPrefix(a, "--") {
				return "", fmt.Errorf("unknown BUILD option %q (BUILD mode accepts only --repo <path>)", a)
			}
			return "", fmt.Errorf("unexpected BUILD argument %q (BUILD mode accepts only --repo <path>)", a)
		}
	}
	return repo, nil
}

// runCleanup parses `cleanup [--repo <path>] [--apply]` and delegates to the
// cleanup package. Discovery is inspect-only unless --apply is given.
func runCleanup(args []string) int {
	var repo string
	apply := false
	repoCount := 0

	i := 0
	for i < len(args) {
		a := args[i]
		switch a {
		case "--repo":
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "global-build: --repo requires a path argument\n")
				return runner.ExitRunnerError
			}
			if repoCount > 0 {
				fmt.Fprintf(os.Stderr, "global-build: --repo may be given exactly once\n")
				return runner.ExitRunnerError
			}
			repo = args[i+1]
			repoCount++
			i += 2
		case "--apply":
			apply = true
			i++
		case "--force-all":
			fmt.Fprintf(os.Stderr, "global-build: --force-all is not supported\n")
			return runner.ExitRunnerError
		default:
			if strings.HasPrefix(a, "--") {
				fmt.Fprintf(os.Stderr, "global-build: unknown cleanup option %q\n", a)
				return runner.ExitRunnerError
			}
			fmt.Fprintf(os.Stderr, "global-build: unexpected cleanup argument %q\n", a)
			return runner.ExitRunnerError
		}
	}

	if repoCount == 0 {
		fmt.Fprintf(os.Stderr, "global-build: cleanup requires --repo <path>\n")
		return runner.ExitRunnerError
	}
	if repo == "" || strings.HasPrefix(repo, "-") {
		fmt.Fprintf(os.Stderr, "global-build: malformed repository path %q\n", repo)
		return runner.ExitRunnerError
	}

	cfg := cleanup.Config{
		CacheRoot: os.Getenv(envCacheRoot),
		Out:       os.Stdout,
		Err:       os.Stderr,
	}
	if err := runCleanupOnly(context.Background(), cfg, repo, apply); err != nil {
		fmt.Fprintf(os.Stderr, "global-build: cleanup failed: %v\n", err)
		return runner.ExitRunnerError
	}
	return 0
}

// runCleanupOnly is the testable cleanup entry point.
func runCleanupOnly(ctx context.Context, cfg cleanup.Config, repo string, apply bool) error {
	_, err := cleanup.Run(ctx, cfg, repo, apply)
	return err
}
