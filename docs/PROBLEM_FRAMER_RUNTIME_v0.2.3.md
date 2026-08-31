# Problem Framer Runtime v0.2.3

**Status: FINAL — Approval-Gated Autonomous Continuation**

This document is the canonical execution-policy artifact for `global-build`.
It supersedes Runtime v0.2.2 where the rules below differ. Problem Framer Core
SKILL_v0.6.1 remains the semantic authority and is unchanged. Runtime owns
scheduling, retrieval, verification, concurrency, blocking, continuation,
closure, and publication mechanics.

## Boundary and gates

Runtime keeps two different gates:

- `BRIEF_PUBLICATION_GATE` makes a Problem Brief canonical. Crossing it is not
  approval to publish an implementation candidate.
- `CHANGE_APPROVAL_GATE` holds a completed, proof-admitted candidate before it
  is published to canonical `main`.

After the brief is published, repository/history inspection, authority
investigation, root-cause analysis, semantically equivalent implementation
choice, local mutation, focused/regression testing, repair, candidate commit
preparation, proof generation, and publication-readiness validation continue
without a user approval request. A user decision is reserved for a material
semantic fork, genuinely user-held information, an irreversible/high-impact
external action, or a completed candidate at `CHANGE_APPROVAL_GATE`.

The normal user decision vocabulary is therefore `APPROVE`, `REJECT`, and
`CHANGE DIRECTION`. Questions such as what to investigate next, whether to run
tests, or which equivalent implementation to use are Runtime Decision Debt
and are resolved autonomously from current authority and evidence.

## Execution states

Each bounded task has exactly one of these execution states:

`ACTIVE`, `READY`, `WAITING`, `CANDIDATE_READY`, `APPROVAL_QUEUE`, or
`PUBLISHED`.

`APPROVAL_QUEUE` is scope-local. If candidate A is waiting for change
approval, independent positive-value task B may be `ACTIVE` and task C may be
`READY` when its bounded value is genuine. A task that semantically depends on
A is `WAITING` with `WAITING_DEPENDENCY`; it must not run before A is
published.

Runtime maintains at most one `ACTIVE` dispatch and approximately one `READY`
dispatch. Spare capacity never creates work: `READY` is used only for a task
explicitly admitted as bounded and positive-value with a stated value reason.

External, user, and proof blockers are scope-local. An external closure track
does not own or idle an independent internal advance frontier. Blocker reports
preserve what is blocked, what is done, what can continue, the minimum needed
condition and owner, and the exact resume point. `USER ACTION: NONE` is used
when the user is not the blocker owner.

## Candidate admission and approval

`CANDIDATE_READY` requires the decision debt and claim-calibrated proof debt to
be sufficiently cleared:

- root cause understood where applicable;
- bounded change and preserved invariants;
- sufficient focused and relevant regression proof;
- publication path checked;
- rollback/reversal understood; and
- material residual risk stated.

The candidate record stores an identity, admitted base, commit/ref, semantic
and proof hashes, and an exact resume point. Approval binds to that identity,
semantics and proof. `APPROVE` resumes from that stored candidate/resume point.

Only causally relevant invalidation reopens minimum revalidation: a watched
surface or authoritative source changed. A different `main`/SHA alone is
information, not failure; unrelated advancement does not restart discovery or
invalidate the candidate.

`REJECT` has two bounded meanings:

- implementation rejection returns to local repair while preserving valid
  investigation/proof state;
- semantic rejection returns to Problem Framer semantic revision while
  preserving the valid investigation/proof resume markers.

Neither rejection automatically restarts the entire task.

## Frontier reconstruction and stop

After task closure or publication, Runtime reconstructs the next frontier from
current live repository/authority evidence. Historical `NEXT` lists, old
queues, old first failures and remembered frontier state are not execution
authority. `PUBLISHED` work remains closed unless direct contradictory or
causally invalidating evidence exists.

Whole-project `STOP` is legal only when:

```text
ACTIVE = NONE
READY  = NONE
```

and every remaining useful bounded slice is genuinely classified as
`WAITING_USER`, `WAITING_EXTERNAL`, or `PROOF_BLOCKED`, after adequate
claim-aware termination/coverage checks. One failed query, zero results, one
inaccessible source, repeated similar results, or generic “no new
information” is not completion evidence by itself.

## Repository enforcement

The policy is executable through `internal/runtime`. The one-shot BUILD runner
remains responsible for disposable worktrees, candidate validation and strict
worker protocol parsing. Runtime consumes its structured `CONTINUABLE`,
`COMPLETE`, and `BLOCKED` outcomes through the package adapter, persists the
candidate/resume state as a versioned JSON document, and exposes the same
transitions through:

```sh
global-build runtime --state /absolute/path/runtime.json apply < event.json
global-build runtime --state /absolute/path/runtime.json snapshot
global-build runtime --state /absolute/path/runtime.json stop-check
```

The `runtime` command has no publication authority. A successful Runtime
publication transition records the result of the existing separate
`global-build publish` authority; it does not push to a remote. No executor
name or executor-specific launcher is part of this policy. State replacement
is atomic and every command takes a nonblocking state sidecar lock, so a
concurrent writer fails closed rather than losing a transition.
