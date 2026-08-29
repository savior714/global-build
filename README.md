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
- **Repository-owned worker semantics.** The canonical worker definition lives at `internal/opencode/global-build-explore.md` and `internal/opencode/global-build-worker.md`, is embedded into the binary, and cannot be replaced by stale external OpenCode config. Canonical-owned fields (mode, permission, prompt) always win over any pre-existing invocation config.
- **Repository-owned investigator configuration.** The canonical `global-build-explore` agent definition is embedded in the binary and injected into every BUILD invocation via runtime inline config. Stale or external `global-build-explore` fields cannot replace the canonical definition; canonical-owned frontmatter fields always win. The injected permission block declares `list`, `glob`, `grep`, and `read` only (deny-by-default). The legacy built-in `explore` agent's permission is overridden but it cannot alter the canonical `global-build-explore`. The actual read-only runtime enforcement of these permissions is an intended behavioral/guardrail contract under OpenCode 1.18.x auto-approval semantics, not an adversarial runtime sandbox.
- **Exact one-commit candidate.** After the worker reports COMPLETE, the runner verifies that the admitted_base..candidate range contains exactly one non-merge commit and zero merge commits. A two-commit (or zero-commit, or merge) candidate is rejected before any ref is created.
- **Runner-owned candidate validation.** The runner owns the full candidate validation pipeline: ancestry check, commit-count check, merge-count check, worktree-clean check, and candidate-ref creation. The worker never writes the candidate ref itself.
- **COMPLETE protocol requires PRIMARY_PROOF: PASS.** The runner mechanically parses the terminal result and rejects any COMPLETE that lacks a `PRIMARY_PROOF: PASS` field or has a non-PASS value. The runner does not independently execute or verify the proof; it enforces only that the protocol text declares it.
- **External-directory injection.** The runner injects a deny-all/allow-exact-worktree `external_directory` permission for the worker.
- **Compatible OpenCode generation admission.** A mutating BUILD performs `opencode --version` before runner admission and fails closed unless the version is exactly OpenCode 1.18.25. Pre-1.18, 1.18.x other patches, 1.19+, and 2.x are rejected until they receive a separate compatibility and runtime acceptance pass.
- **Malformed terminal repair.** If the primary reaches a terminal semantic outcome but renders malformed BUILD protocol text, the runner performs exactly one tool-less continuation of that exact session to re-render the already-reached outcome. The finalizer is deny-by-default, uses the same session identity, and never recurses.

### Behavioral / authority contract

These are enforced by runner architecture and prompt contract, not by mechanical runtime containment. A cooperative worker follows them; a hostile process could bypass them through shell-side means.

- **Single mutation owner.** The primary worker is the intended sole mutation owner. Canonical task config admits only `global-build-explore` as a subagent, and explore is configured read-only. Subagents never implement the change, create commits, decide final acceptance, or substitute for PRIMARY PROOF. This is an orchestration contract, not an adversarial process sandbox.
- **Primary executes PRIMARY PROOF.** The worker is expected to genuinely run its own PRIMARY PROOF before declaring COMPLETE. The runner does not independently execute or verify the proof; it only mechanically enforces that the protocol text contains `PRIMARY_PROOF: PASS`. A cooperative worker follows this contract; a hostile process could bypass it through shell-side means.
- **No worker publication.** The worker is contractually forbidden to publish. The canonical/supported publication path is the separate `global-build publish` command. The runner itself does not perform publication during BUILD. Command-pattern denies for `git push*` and related topology commands are guardrails; in OpenCode 1.18.x non-interactive `run`, the session is auto-approved so agent allow/deny command-pattern rules are not re-checked at the tool layer (only OpenCode's own small internal banned-command list — `curl`, `wget`, `nc`, `telnet`, browsers, … — is always enforced). Arbitrary shell-side remote effects are not mechanically impossible.

### Guardrails

Defense-in-depth patterns that declare intent but are not a complete security sandbox under current OpenCode 1.18.x semantics.

- **General local shell capability; Git authority as guardrails.** Ordinary local shell execution is BUILD *capability*, not authority, so the worker's `bash` permission is broadly `allow` (covering `go test`/`go vet`, `npm`/`pnpm`/`yarn`, `pytest`, `cargo`, `make`, repo-local scripts, codegen/proof, etc.) rather than default-denied. On top of that broad allow, the canonical worker lists the dangerous Git operations as `deny` (`git merge*`, `git rebase*`, `git cherry-pick*`, `git fetch*`, `git pull*`, `git push*`, `git branch*`, `git tag*`, `git update-ref*`, `git remote*`, `git clone*`, `git reset*`, `git stash*`, `git worktree*`, `git clean*`, `git gc*`, `git prune*`, `git reflog*`, `git filter-branch*`, `git replace*`, `sudo*`). These are last-match-wins guardrails and declared intent: with broad shell access a determined process could invoke Git by another path (e.g. an absolute `/usr/bin/git` or a shell wrapper), and in OpenCode 1.18.x non-interactive `run` the session is auto-approved so the patterns are not re-enforced at the tool layer. The runner owns the canonical/supported publication path and candidate validation/acceptance; Git command-pattern denies are guardrails. The runner architecture does NOT mechanically prevent arbitrary shell-side publication/topology side effects during worker execution.
- **External-directory containment (guardrail).** Under current OpenCode 1.18.x semantics the injected `external_directory` permission block is a configured guardrail rather than a hard runtime sandbox. The primary containment against off-worktree read/write is the disposable detached worktree architecture and exact one-commit candidate validation, not the injected permission pattern.

### Worker capability, authority boundary, guardrail, deterministic invariant, execution default

These five concepts are distinct. Do not collapse them:

- **Worker capability (what the worker may use).** `edit`; broad `bash` (ordinary local build/test/lint/typecheck/codegen/proof commands); `webfetch`; `websearch`; `task` limited to `global-build-explore`; and `external_directory` only for the exact disposable worktree. `question` is denied because the runner is noninteractive and has no live human response path.
- **Authority boundary (what the worker may never do).** Publish to any remote; rewrite or move refs/branches/tags/worktrees; run a second mutation owner; or decide final acceptance. These are enforced by the runner architecture (disposable worktree, runner-owned candidate validation, separate `publish` command) and the worker prompt contract — not by shell command matching.
- **Guardrail (defense-in-depth, not a sandbox).** The worker's Git command-pattern `deny` list and the broad `bash` `allow` are declared intent. OpenCode 1.18.x non-interactive `run` auto-approves the session, so these patterns are not re-checked at the tool layer; only OpenCode's own small internal banned-command list is always enforced. Command-pattern matching is **not** a complete security sandbox.
- **Deterministic acceptance invariant.** Exactly one non-merge commit on `admitted_base..candidate`; runner-owned ancestry/merge/clean checks; candidate ref created by the runner; OpenCode version exactly 1.18.25; worker response mechanically parsed against the strict result protocol.
- **Execution default.** Noninteractive prepared-task runner; one attempt; publication is a separate command and never part of a BUILD.

### Execution defaults / tuning policy

The following values are current operating defaults derived from empirical BUILD evidence. They may be retuned without redefining the safety model above.

- **Minimum-useful delegation.** Delegation is an orchestration optimization, not extra mutation authority. Direct reads remain preferred for isolated known-file questions. The worker should delegate only when there are genuinely independent investigation axes (production-code ownership/path discovery, tests/proof/fixtures discovery, existing-pattern/regression-surface discovery) and should launch them in parallel in the same turn.
- **Parallel investigation.** When multiple axes are independent, launch them in parallel rather than serially. The read-only contract keeps parallel exploration safe.

These are tuning parameters, not safety invariants. The mechanical invariants above remain enforced regardless of how these values are adjusted.

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
