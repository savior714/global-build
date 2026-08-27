# global-build

`global-build` is a thin, stateless cross-repository BUILD runner. It executes one prepared mutation task inside an isolated detached Git worktree, validates the worker result mechanically, and produces a candidate commit without giving the worker publication authority.

## Canonical operating contract

### Mutating BUILD

A mutating BUILD must name its target repository explicitly:

```sh
global-build --repo /absolute/path/to/repository < task.md
```

The process working directory is never used as repository authority. Before mutation, the runner binds the requested path to its canonical Git toplevel/common-dir identity and proves that `admitted_base` belongs to that repository.

The stdin envelope contains frontmatter plus exactly five body sections:

```text
---
run_id: <collision-resistant-id>
admitted_base: <full-git-oid>
watch_surfaces:
  - <exact-path-or-directory-prefix>
---

GOAL
...

SETTLED FACTS
...

CHANGE BOUNDARY
...

PRIMARY PROOF
...

STOP CONDITIONS
...
```

The worker sees only the five body sections. `run_id`, `admitted_base`, and `watch_surfaces` remain runner-owned control-plane data.

### Worker authority

The canonical worker is repository-owned at `internal/opencode/global-build-worker.md` and embedded into the binary. Invocation-specific OpenCode config may supply unrelated fields such as model/variant, but canonical-owned fields always win.

Current worker invariants:

- primary agent
- `steps: 50`
- `webfetch`, `websearch`, `question`, and `doom_loop`: denied
- `task`: deny-by-default; only the built-in read-only `explore` subagent is admitted
- delegation is bounded to at most three Explore calls per BUILD, using the minimum useful number and parallel dispatch only for genuinely independent investigation axes
- delegated investigation must be read-only and non-overlapping; the primary remains the sole mutation owner and runs the final PRIMARY PROOF itself
- every Explore prompt must use only OpenCode's dedicated `list`, `glob`, `grep`, and `read` tools; Bash/shell is forbidden even for read-only discovery
- unsafe Git publication/topology commands: denied
- `external_directory`: denied globally and allowed only for the exact disposable worktree of the current run
- worker never pushes or publishes

The delegation policy is an orchestration optimization, not extra mutation authority. Direct reads remain preferred for isolated known-file questions. When delegation is appropriate, the worker should ask separate Explore agents for narrow evidence such as production-code ownership, tests/proof/fixtures, or existing patterns/regression surfaces, and must not duplicate the delegated investigation itself. Explore prompts explicitly avoid Bash because current OpenCode 1.x Explore can otherwise choose shell-based `ls`/`rg`-style discovery and trigger an unnecessary permission path even though dedicated read/search tools are available. `general`, `scout`, and other subagents remain outside the worker contract.

### OpenCode compatibility

The accepted runtime contract is the OpenCode 1.x legacy agent/permission generation. A mutating BUILD performs `opencode --version` before runner admission and fails closed unless the version is OpenCode 1.x at `>= 1.18`.

The latest explicit runtime acceptance as of 2026-08-27 used OpenCode `1.18.23`. OpenCode V2 has a different permission/configuration model and is intentionally rejected until it receives a separate compatibility and runtime acceptance pass.

This is a generation guard, not a patch pin: compatible newer 1.x minors remain allowed.

### Result protocol

The worker final response is mechanically parsed and must contain only protocol lines.

COMPLETE:

```text
RESULT: COMPLETE
PRIMARY_PROOF: PASS
```

CONTINUABLE:

```text
RESULT: CONTINUABLE
ADMITTED_BASE: <starting-detached-head-oid>
COMPLETED: <completed work>
REMAINING: <remaining work>
NEXT_ACTION: <next action>
DO_NOT_REOPEN: <settled facts>
VERIFICATION_ALREADY_DONE: <proof already run>
WORKTREE_STATE: <remaining worktree state>
```

BLOCKED:

```text
RESULT: BLOCKED
BLOCKER: <one-line blocker>
EVIDENCE: <direct evidence>
```

Markdown fences, prose before/after the protocol, unknown fields, duplicate fields, and missing required fields are rejected. Intentional multi-line field values must use two-space-indented continuation lines; unindented prose is never inferred as part of the previous field.

Exit codes:

| Code | Meaning |
| ---: | --- |
| `0` | COMPLETE |
| `10` | CONTINUABLE |
| `20` | BLOCKED |
| `30` | MALFORMED_RESULT |
| `40` | RUNNER_ERROR |

## Publication and continuation

BUILD and publication are separate authorities.

- `global-build continuation-check ...` is read-only and classifies whether the admitted base is unchanged, advanced without overlap, requires overlap review, or requires history review.
- `global-build publish ...` promotes a proven candidate to `origin/main` without force, fails closed on races/main movement/postcondition mismatch, and deletes the candidate ref with compare-and-swap semantics.
- `global-build cleanup --repo <path>` is inspect-only by default. Add `--apply` only to remove worktrees whose ownership and liveness are directly proven.

Do not use merge/rebase/cherry-pick/force as normal publication recovery for a BUILD candidate.

## Development and verification

The module keeps a Go `1.26` compatibility floor in `go.mod`; current development and CI use Go `1.27.0`.

Primary repository proof:

```sh
go test ./...
```

GitHub Actions runs the same test suite on pushes and pull requests. Runtime acceptance involving the real installed OpenCode binary remains a separate local/native proof because repository CI cannot establish the user's actual OpenCode installation and host permission behavior.
