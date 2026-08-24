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
	"time"

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
