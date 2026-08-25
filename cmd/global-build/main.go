// Command global-build is a thin, stateless, deterministic runner that performs
// exactly one BUILD attempt per invocation.
//
// Input is stdin-only and ephemeral (frontmatter envelope + five task body
// sections). The target repository is the repository containing the process
// working directory. Diagnostics go to stderr; stable result keys go to stdout.
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
	envWallClock      = "GLOBAL_BUILD_WALL_CLOCK"       // Go duration, default 90m
	envProgressWindow = "GLOBAL_BUILD_PROGRESS_WINDOW"  // Go duration, default 15m
	envCacheRoot      = "GLOBAL_BUILD_CACHE_ROOT"       // override cache root for tests
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
			// No-subcommand stdin BUILD mode is the only other mode. Stray
			// flags such as `--apply` outside a known subcommand are rejected.
			fmt.Fprintf(os.Stderr, "global-build: unknown subcommand %q (use 'cleanup', 'publish', 'continuation-check', or stdin BUILD mode)\n", args[0])
			return runner.ExitRunnerError
		}
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

	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "global-build: cannot determine working directory: %v\n", err)
		return runner.ExitRunnerError
	}

	return runner.Run(context.Background(), cfg, cwd, input)
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
