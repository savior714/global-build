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

### Hard / mechanically enforced contract

The following invariants are enforced by code, not by prompt prose. A violation causes the BUILD to fail closed rather than proceed with weakened authority.

- **Explicit repository identity.** The `--repo` flag is required for mutation. The runner proves the requested path matches the canonical Git toplevel/common-dir identity and that `admitted_base` belongs to that repository. The process working directory is never used as authority.
- **Exact disposable detached worktree.** Each BUILD runs inside a freshly created, locked, detached Git worktree. The runner creates the worktree, pins it to `admitted_base`, and removes it on completion (or leaves it only when identity cannot be proven during cleanup).
- **Repository-owned worker semantics.** The canonical worker definition lives at `internal/opencode/global-build-explore.md` and `internal/opencode/global-build-worker.md`, is embedded into the binary, and cannot be replaced by stale external OpenCode config. Canonical-owned fields (mode, steps, permission, prompt) always win over any pre-existing invocation config.
- **Repository-owned read-only investigator semantics.** The canonical `global-build-explore` subagent is injected into every BUILD invocation with a deny-by-default permission allow-list containing only `list`, `glob`, `grep`, and `read`. Stale or external `global-build-explore` fields cannot replace these canonical operational fields. The legacy built-in `explore` agent is not synthesized when starting from empty config; if an unrelated pre-existing legacy `explore` block exists, its permission is overridden but it cannot alter `global-build-explore`.
- **Single mutation owner.** The primary worker is the sole mutation owner. Subagents never implement the change, create commits, decide final acceptance, or substitute for PRIMARY PROOF.
- **No worker publication.** The generated worker config denies `git push`, all remote topology commands, branch/tag creation and force operations, reset, stash, worktree mutation, clean, gc, prune, and reflog. The worker cannot publish under any invocation path.
- **Bounded Git capability.** Git is default-deny. Only the minimal read/stage/commit operations required for a BUILD are reopened: `git status`, `git diff`, `git log`, `git show`, `git rev-parse`, `git cat-file`, `git describe`, `git for-each-ref`, `git ls-files`, `git check-attr`, `git add`, `git commit`, `git restore`, and `git checkout --`. All dangerous topology/ref/remote commands remain unreachable.
- **Exact one-commit candidate.** After the worker reports COMPLETE, the runner verifies that the admitted_base..candidate range contains exactly one non-merge commit and zero merge commits. A two-commit (or zero-commit, or merge) candidate is rejected before any ref is created.
- **Runner-owned candidate validation.** The runner owns the full candidate validation pipeline: ancestry check, commit-count check, merge-count check, worktree-clean check, and candidate-ref creation. The worker never writes the candidate ref itself.
- **Primary runs PRIMARY PROOF.** The worker must run its own PRIMARY PROOF before declaring COMPLETE. The runner mechanically verifies the protocol text; it does not re-run proof but enforces that the worker claims to have done so.
- **External-directory containment.** The worker's `external_directory` permission is denied globally and allowed only for the exact disposable worktree of the current run. The worker cannot read or write outside that directory.
- **Compatible OpenCode generation admission.** A mutating BUILD performs `opencode --version` before runner admission and fails closed unless the version is exactly OpenCode 1.18.x. Pre-1.18, 1.19+, and 2.x are rejected until they receive a separate compatibility and runtime acceptance pass.
- **Malformed terminal repair.** If the primary reaches a terminal semantic outcome but renders malformed BUILD protocol text, the runner performs exactly one tool-less continuation of that exact session to re-render the already-reached outcome. The finalizer is deny-by-default, uses the same session identity, and never recurses.

### Execution defaults / tuning policy

The following values are current operating defaults derived from empirical BUILD evidence. They may be retuned without redefining the safety model above.

- **Primary steps = 50.** The canonical worker runs up to 50 reasoning steps. This is a capacity bound, not a target; most BUILDs complete well before it.
- **Maximum investigator-call budget = 3.** The worker may delegate up to three `global-build-explore` calls per BUILD. This is a hard cap enforced by the worker prompt, not a mechanical permission.
- **Minimum-useful delegation.** Delegation is an orchestration optimization, not extra mutation authority. Direct reads remain preferred for isolated known-file questions. The worker should delegate only when there are genuinely independent investigation axes (production-code ownership/path discovery, tests/proof/fixtures discovery, existing-pattern/regression-surface discovery) and should launch them in parallel in the same turn.
- **Parallel investigation.** When multiple axes are independent, launch them in parallel rather than serially. The bounded budget and read-only contract keep parallel exploration safe.

These are tuning parameters, not safety invariants. The hard contract above remains enforced regardless of how these values are adjusted.

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
go test -race ./...
go vet ./...
```

GitHub Actions runs the same test suite on pushes and pull requests. Runtime acceptance involving the real installed OpenCode binary remains a separate local/native proof because repository CI cannot establish the user's actual OpenCode installation and host permission behavior.
