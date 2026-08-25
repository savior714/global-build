package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"global-build/internal/freshness"
	"global-build/internal/gitx"
	"global-build/internal/ownership"
	"global-build/internal/publish"
)

// runPublish parses `publish [--repo <path>] [--run-id <run-id>]
// [--candidate <oid>] [--admitted-base <oid>] [--watch <surface> ...]` and
// delegates to the publish package. Every flag is a required singleton except
// --watch which is repeatable (and required at least once).
func runPublish(args []string) int {
	var repo, runID, candidate, admittedBase string
	var watches []string
	counts := map[string]int{}
	i := 0
	for i < len(args) {
		a := args[i]
		switch a {
		case "--repo":
			if i+1 >= len(args) || args[i+1] == "" || strings.HasPrefix(args[i+1], "-") {
				fmt.Fprintf(os.Stderr, "global-build: --repo requires a path argument\n")
				return publish.ExitError
			}
			repo, counts["--repo"] = args[i+1], counts["--repo"]+1
			i += 2
		case "--run-id":
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
				fmt.Fprintf(os.Stderr, "global-build: --run-id requires an argument\n")
				return publish.ExitError
			}
			runID, counts["--run-id"] = args[i+1], counts["--run-id"]+1
			i += 2
		case "--candidate":
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
				fmt.Fprintf(os.Stderr, "global-build: --candidate requires an argument\n")
				return publish.ExitError
			}
			candidate, counts["--candidate"] = args[i+1], counts["--candidate"]+1
			i += 2
		case "--admitted-base":
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
				fmt.Fprintf(os.Stderr, "global-build: --admitted-base requires an argument\n")
				return publish.ExitError
			}
			admittedBase, counts["--admitted-base"] = args[i+1], counts["--admitted-base"]+1
			i += 2
		case "--watch":
			if i+1 >= len(args) || args[i+1] == "" || strings.HasPrefix(args[i+1], "-") {
				fmt.Fprintf(os.Stderr, "global-build: --watch requires a surface argument\n")
				return publish.ExitError
			}
			watches = append(watches, args[i+1])
			counts["--watch"]++
			i += 2
		default:
			if strings.HasPrefix(a, "--") {
				fmt.Fprintf(os.Stderr, "global-build: unknown publish option %q\n", a)
				return publish.ExitError
			}
			fmt.Fprintf(os.Stderr, "global-build: unexpected publish argument %q\n", a)
			return publish.ExitError
		}
	}

	// Singleton uniqueness/required checks.
	for _, flag := range []string{"--repo", "--run-id", "--candidate", "--admitted-base"} {
		if counts[flag] == 0 {
			fmt.Fprintf(os.Stderr, "global-build: publish requires %s\n", flag)
			return publish.ExitError
		}
		if counts[flag] > 1 {
			fmt.Fprintf(os.Stderr, "global-build: %s may be given exactly once\n", flag)
			return publish.ExitError
		}
	}
	if counts["--watch"] == 0 {
		fmt.Fprintf(os.Stderr, "global-build: publish requires at least one --watch\n")
		return publish.ExitError
	}
	if repo == "" || strings.HasPrefix(repo, "-") {
		fmt.Fprintf(os.Stderr, "global-build: malformed repository path %q\n", repo)
		return publish.ExitError
	}
	if !ownership.ValidRunID(runID) {
		fmt.Fprintf(os.Stderr, "global-build: unsafe run-id %q\n", runID)
		return publish.ExitError
	}
	if !gitx.IsFullOID(candidate) || !gitx.IsFullOID(admittedBase) {
		fmt.Fprintf(os.Stderr, "global-build: --candidate and --admitted-base must be full git object ids\n")
		return publish.ExitError
	}

	cfg := publish.Config{
		Repo:         repo,
		RunID:        runID,
		Candidate:    candidate,
		AdmittedBase: admittedBase,
		Watches:      watches,
		Out:          os.Stdout,
		Err:          os.Stderr,
	}
	return publish.Run(context.Background(), cfg)
}

// runContinuationCheck parses `continuation-check [--repo <path>] [--base <oid>]
// [--watch <surface> ...]` and delegates to the freshness package. It is
// read-only.
func runContinuationCheck(args []string) int {
	var repo, base string
	var watches []string
	counts := map[string]int{}
	i := 0
	for i < len(args) {
		a := args[i]
		switch a {
		case "--repo":
			if i+1 >= len(args) || args[i+1] == "" || strings.HasPrefix(args[i+1], "-") {
				fmt.Fprintf(os.Stderr, "global-build: --repo requires a path argument\n")
				return freshness.ExitError
			}
			repo, counts["--repo"] = args[i+1], counts["--repo"]+1
			i += 2
		case "--base":
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
				fmt.Fprintf(os.Stderr, "global-build: --base requires an argument\n")
				return freshness.ExitError
			}
			base, counts["--base"] = args[i+1], counts["--base"]+1
			i += 2
		case "--watch":
			if i+1 >= len(args) || args[i+1] == "" || strings.HasPrefix(args[i+1], "-") {
				fmt.Fprintf(os.Stderr, "global-build: --watch requires a surface argument\n")
				return freshness.ExitError
			}
			watches = append(watches, args[i+1])
			counts["--watch"]++
			i += 2
		default:
			if strings.HasPrefix(a, "--") {
				fmt.Fprintf(os.Stderr, "global-build: unknown continuation-check option %q\n", a)
				return freshness.ExitError
			}
			fmt.Fprintf(os.Stderr, "global-build: unexpected continuation-check argument %q\n", a)
			return freshness.ExitError
		}
	}

	for _, flag := range []string{"--repo", "--base"} {
		if counts[flag] == 0 {
			fmt.Fprintf(os.Stderr, "global-build: continuation-check requires %s\n", flag)
			return freshness.ExitError
		}
		if counts[flag] > 1 {
			fmt.Fprintf(os.Stderr, "global-build: %s may be given exactly once\n", flag)
			return freshness.ExitError
		}
	}
	if counts["--watch"] == 0 {
		fmt.Fprintf(os.Stderr, "global-build: continuation-check requires at least one --watch\n")
		return freshness.ExitError
	}
	if repo == "" || strings.HasPrefix(repo, "-") {
		fmt.Fprintf(os.Stderr, "global-build: malformed repository path %q\n", repo)
		return freshness.ExitError
	}
	if !gitx.IsFullOID(base) {
		fmt.Fprintf(os.Stderr, "global-build: --base must be a full git object id\n")
		return freshness.ExitError
	}

	cfg := freshness.Config{
		Repo:    repo,
		Base:    base,
		Watches: watches,
		Out:     os.Stdout,
		Err:     os.Stderr,
	}
	return freshness.Run(context.Background(), cfg)
}
