package runtime_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	runtimepolicy "global-build/internal/runtime"
)

func boundedTask(id string, deps ...string) runtimepolicy.Task {
	return runtimepolicy.Task{
		ID:            id,
		Scope:         "bounded scope for " + id,
		Bounded:       true,
		PositiveValue: true,
		ValueReason:   "current live evidence shows positive value",
		BriefStatus:   runtimepolicy.BriefPublished,
		DependsOn:     deps,
		WatchSurfaces: []string{"src/"},
	}
}

func candidate(id string) runtimepolicy.Candidate {
	return runtimepolicy.Candidate{
		Identity:      id + "-candidate",
		AdmittedBase:  "base-" + id,
		Commit:        "commit-" + id,
		Ref:           "refs/build-candidates/" + id,
		SemanticsHash: "semantics-" + id,
		ProofHash:     "proof-" + id,
		ResumePoint:   "publish " + id + " candidate",
	}
}

func admitted() runtimepolicy.Admission {
	return runtimepolicy.Admission{
		DecisionDebtCleared:       true,
		ClaimProofDebtCleared:     true,
		RootCauseUnderstood:       true,
		BoundedChange:             true,
		InvariantsPreserved:       true,
		FocusedProofSufficient:    true,
		RegressionProofSufficient: true,
		PublicationPathChecked:    true,
		RollbackUnderstood:        true,
		MaterialResidualRisk:      "bounded residual risk is recorded",
	}
}

func task(t *testing.T, m *runtimepolicy.Manager, id string) runtimepolicy.Task {
	t.Helper()
	got, ok := m.Snapshot().Tasks[id]
	if !ok {
		t.Fatalf("task %q missing", id)
	}
	return got
}

func requireState(t *testing.T, m *runtimepolicy.Manager, id string, want runtimepolicy.State) runtimepolicy.Task {
	t.Helper()
	got := task(t, m, id)
	if got.State != want {
		t.Fatalf("task %q state=%q want %q (task=%+v)", id, got.State, want, got)
	}
	return got
}

func add(t *testing.T, m *runtimepolicy.Manager, id string, deps ...string) {
	t.Helper()
	if err := m.AddTask(boundedTask(id, deps...)); err != nil {
		t.Fatal(err)
	}
}

func prepareAndQueue(t *testing.T, m *runtimepolicy.Manager, id string) runtimepolicy.Candidate {
	t.Helper()
	c := candidate(id)
	if err := m.PrepareCandidate(id, c, admitted()); err != nil {
		t.Fatal(err)
	}
	if err := m.QueueApproval(id); err != nil {
		t.Fatal(err)
	}
	return c
}

func approve(t *testing.T, m *runtimepolicy.Manager, id string, c runtimepolicy.Candidate, f runtimepolicy.FreshnessObservation) {
	t.Helper()
	f.CheckPerformed = true
	err := m.Approve(id, runtimepolicy.Approval{
		CandidateIdentity: c.Identity,
		SemanticsHash:     c.SemanticsHash,
		ProofHash:         c.ProofHash,
	}, f)
	if err != nil {
		t.Fatal(err)
	}
}

// A proves that candidate approval is local and independent positive-value
// work immediately occupies ACTIVE/READY capacity.
func TestApprovalDoesNotIdleIndependentWork(t *testing.T) {
	m := runtimepolicy.New()
	add(t, m, "A")
	add(t, m, "B")
	add(t, m, "C")
	requireState(t, m, "A", runtimepolicy.Active)
	requireState(t, m, "B", runtimepolicy.Ready)
	requireState(t, m, "C", runtimepolicy.Waiting)

	c := prepareAndQueue(t, m, "A")
	_ = c
	requireState(t, m, "A", runtimepolicy.ApprovalQueue)
	requireState(t, m, "B", runtimepolicy.Active)
	requireState(t, m, "C", runtimepolicy.Ready)
	if got := len(m.Plan()); got != 2 {
		t.Fatalf("dispatch plan length=%d want ACTIVE + READY", got)
	}
}

func TestBriefPublicationAndChangeApprovalAreDistinctGates(t *testing.T) {
	m := runtimepolicy.New()
	brief := boundedTask("A")
	brief.BriefStatus = runtimepolicy.BriefDraft
	if err := m.AddTask(brief); err != nil {
		t.Fatal(err)
	}
	got := requireState(t, m, "A", runtimepolicy.Waiting)
	if got.Gate != runtimepolicy.BriefPublicationGate {
		t.Fatalf("initial gate=%q want %q", got.Gate, runtimepolicy.BriefPublicationGate)
	}
	if err := m.PublishBrief("A"); err != nil {
		t.Fatal(err)
	}
	got = requireState(t, m, "A", runtimepolicy.Active)
	if got.Gate != "" {
		t.Fatalf("brief publication left gate=%q", got.Gate)
	}
	prepareAndQueue(t, m, "A")
	got = requireState(t, m, "A", runtimepolicy.ApprovalQueue)
	if got.Gate != runtimepolicy.ChangeApprovalGate {
		t.Fatalf("candidate gate=%q want %q", got.Gate, runtimepolicy.ChangeApprovalGate)
	}
}

// B proves that semantic dependency, unlike unrelated approval work, waits.
func TestDependentWorkWaitsForApprovedPublication(t *testing.T) {
	m := runtimepolicy.New()
	add(t, m, "A")
	add(t, m, "B", "A")
	requireState(t, m, "A", runtimepolicy.Active)
	b := requireState(t, m, "B", runtimepolicy.Waiting)
	if b.Blocker == nil || b.Blocker.Kind != runtimepolicy.BlockerDependency || b.Blocker.DependencyID != "A" {
		t.Fatalf("dependency blocker=%+v", b.Blocker)
	}

	c := prepareAndQueue(t, m, "A")
	requireState(t, m, "B", runtimepolicy.Waiting)
	if got := task(t, m, "B").Blocker.DependencyID; got != "A" {
		t.Fatalf("dependency changed while A awaited approval: %q", got)
	}
	approve(t, m, "A", c, runtimepolicy.FreshnessObservation{})
	if err := m.RecordPublished("A", runtimepolicy.Publication{
		CandidateIdentity: c.Identity,
		Commit:            c.Commit,
		Success:           true,
	}); err != nil {
		t.Fatal(err)
	}
	requireState(t, m, "B", runtimepolicy.Active)
}

func TestApproveResumesStoredCandidateWithoutDiscoveryRestart(t *testing.T) {
	m := runtimepolicy.New()
	add(t, m, "A")
	c := prepareAndQueue(t, m, "A")
	approve(t, m, "A", c, runtimepolicy.FreshnessObservation{})
	got := requireState(t, m, "A", runtimepolicy.Active)
	if got.Candidate == nil || got.Candidate.Identity != c.Identity {
		t.Fatalf("candidate identity was not preserved: %+v", got.Candidate)
	}
	if got.Resume.Point != c.ResumePoint || !got.Resume.InvestigationPreserved {
		t.Fatalf("resume state=%+v candidate=%+v", got.Resume, got.Candidate)
	}
	if got.Resume.LastAction != "publish stored candidate" {
		t.Fatalf("approve restarted or changed continuation: %q", got.Resume.LastAction)
	}
}

func TestRelevantInvalidationReopensMinimumRevalidation(t *testing.T) {
	m := runtimepolicy.New()
	add(t, m, "A")
	c := prepareAndQueue(t, m, "A")
	approve(t, m, "A", c, runtimepolicy.FreshnessObservation{ChangedPaths: []string{"src/app.go"}})
	got := requireState(t, m, "A", runtimepolicy.Active)
	if got.Candidate == nil || got.Resume.LastAction != "minimum revalidation of candidate" {
		t.Fatalf("relevant invalidation did not preserve candidate and reopen minimally: %+v", got)
	}
	if !got.Resume.InvestigationPreserved || !got.Resume.ProofPreserved {
		t.Fatalf("revalidation lost valid investigation/proof: %+v", got.Resume)
	}
	if err := m.RecordPublished("A", runtimepolicy.Publication{
		CandidateIdentity: c.Identity,
		Commit:            c.Commit,
		Success:           true,
	}); err == nil {
		t.Fatal("candidate must not publish before minimum revalidation is complete")
	}
	if err := m.RecordRevalidated("A", true); err != nil {
		t.Fatal(err)
	}
	if err := m.RecordPublished("A", runtimepolicy.Publication{
		CandidateIdentity: c.Identity,
		Commit:            c.Commit,
		Success:           true,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUnrelatedMainMovementDoesNotResetApproval(t *testing.T) {
	m := runtimepolicy.New()
	add(t, m, "A")
	c := prepareAndQueue(t, m, "A")
	approve(t, m, "A", c, runtimepolicy.FreshnessObservation{
		LatestMain:   "unrelated-new-main",
		ChangedPaths: []string{"docs/notes.md"},
	})
	got := requireState(t, m, "A", runtimepolicy.Active)
	if got.Resume.LastAction != "publish stored candidate" || got.Resume.FreshnessReason != "" {
		t.Fatalf("unrelated SHA movement reset candidate: %+v", got.Resume)
	}
}

func TestPublishedTaskPromotesNextLiveFrontier(t *testing.T) {
	m := runtimepolicy.New()
	add(t, m, "A")
	add(t, m, "B")
	add(t, m, "C")
	requireState(t, m, "B", runtimepolicy.Ready)

	a := prepareAndQueue(t, m, "A")
	_ = a
	requireState(t, m, "B", runtimepolicy.Active)
	// Candidate preparation and approval for B are autonomous runtime work;
	// the user is never asked to select an equivalent implementation.
	b := prepareAndQueue(t, m, "B")
	approve(t, m, "B", b, runtimepolicy.FreshnessObservation{})
	if err := m.RecordPublished("B", runtimepolicy.Publication{
		CandidateIdentity: b.Identity,
		Commit:            b.Commit,
		Success:           true,
	}); err != nil {
		t.Fatal(err)
	}
	requireState(t, m, "B", runtimepolicy.Published)
	if got := task(t, m, "C").State; got != runtimepolicy.Active {
		t.Fatalf("next frontier state=%q want ACTIVE after B publication", got)
	}

	// Reconstruct from current live evidence adds D without replaying an old
	// queue and keeps CLOSED/PUBLISHED B intact.
	if err := m.Reconstruct(runtimepolicy.FrontierEvidence{
		RepositoryRevision: "repo-now",
		AuthorityRevision:  "authority-now",
		CoverageChecked:    true,
		Tasks:              []runtimepolicy.Task{boundedTask("D")},
	}); err != nil {
		t.Fatal(err)
	}
	if task(t, m, "B").State != runtimepolicy.Published {
		t.Fatal("reconstruction resurrected published work")
	}
	if got := task(t, m, "D").State; got != runtimepolicy.Ready {
		t.Fatalf("new live frontier task=%q want READY while C is ACTIVE", got)
	}
}

func TestExternalBlockerIsScopeLocal(t *testing.T) {
	m := runtimepolicy.New()
	add(t, m, "A")
	add(t, m, "B")
	if err := m.RecordBlocked("A", runtimepolicy.Blocker{
		Kind:            runtimepolicy.BlockerExternal,
		Owner:           "EXTERNAL",
		WhatBlocked:     "authority call",
		Done:            "local analysis",
		CanContinue:     "B can continue",
		Needed:          "authority becomes available",
		Evidence:        "provider unavailable",
		ResumePoint:     "retry authority call",
		UserAction:      "should be ignored",
		CoverageChecked: true,
	}); err != nil {
		t.Fatal(err)
	}
	requireState(t, m, "A", runtimepolicy.Waiting)
	requireState(t, m, "B", runtimepolicy.Active)
	if blocker := task(t, m, "A").Blocker; blocker.UserAction != "NONE" {
		t.Fatalf("non-user blocker escalated user action: %+v", blocker)
	}
}

func TestTrueStopRequiresNoActiveOrReadyWork(t *testing.T) {
	m := runtimepolicy.New()
	add(t, m, "A")
	decision := m.StopDecision()
	if decision.Stop {
		t.Fatalf("STOP reported while useful task was active: %+v", decision)
	}
	if err := m.RecordBlocked("A", runtimepolicy.Blocker{
		Kind:            runtimepolicy.BlockerProof,
		Owner:           "PROOF",
		WhatBlocked:     "regression proof",
		Needed:          "required proof artifact",
		Evidence:        "proof environment unavailable",
		ResumePoint:     "rerun regression proof",
		CoverageChecked: true,
	}); err != nil {
		t.Fatal(err)
	}
	decision = m.StopDecision()
	if !decision.Stop {
		t.Fatalf("STOP not reported for genuinely proof-blocked project: %+v", decision)
	}

	// A second live task must reopen continuation instead of allowing a stale
	// STOP decision to end the project.
	add(t, m, "B")
	if decision = m.StopDecision(); decision.Stop {
		t.Fatalf("STOP ignored newly reconstructed useful work: %+v", decision)
	}
}

func TestPublicationRequiresLiveFrontierReconstructionBeforeStop(t *testing.T) {
	m := runtimepolicy.New()
	add(t, m, "A")
	c := prepareAndQueue(t, m, "A")
	approve(t, m, "A", c, runtimepolicy.FreshnessObservation{})
	if err := m.RecordPublished("A", runtimepolicy.Publication{
		CandidateIdentity: c.Identity,
		Commit:            c.Commit,
		Success:           true,
	}); err != nil {
		t.Fatal(err)
	}
	if decision := m.StopDecision(); decision.Stop {
		t.Fatalf("publication without live frontier reconstruction produced STOP: %+v", decision)
	}
	if err := m.Reconstruct(runtimepolicy.FrontierEvidence{
		RepositoryRevision: "repo-now",
		CoverageChecked:    true,
	}); err != nil {
		t.Fatal(err)
	}
	if decision := m.StopDecision(); !decision.Stop {
		t.Fatalf("STOP not reported after covered empty live frontier: %+v", decision)
	}
}

func TestSingleExternalFailureDoesNotPretendProjectIsStopped(t *testing.T) {
	m := runtimepolicy.New()
	add(t, m, "A")
	if err := m.RecordBlocked("A", runtimepolicy.Blocker{
		Kind:        runtimepolicy.BlockerExternal,
		Owner:       "EXTERNAL",
		WhatBlocked: "one authority query",
		Needed:      "retry the authority query",
		Evidence:    "source was inaccessible once",
		ResumePoint: "retry authority query",
	}); err == nil {
		t.Fatal("a single unverified external failure must not support a stop claim")
	}
	if decision := m.StopDecision(); decision.Stop {
		t.Fatalf("failed blocker admission should not produce STOP: %+v", decision)
	}
}

func TestOrdinaryContinuationAndEquivalentChoicesDoNotEscalate(t *testing.T) {
	m := runtimepolicy.New()
	add(t, m, "A")
	if err := m.RecordContinuation("A", runtimepolicy.Continuation{
		Completed:               "investigation",
		Remaining:               "implementation and proof",
		NextAction:              "implement the equivalent bounded solution",
		DoNotReopen:             "settled semantic envelope",
		VerificationAlreadyDone: "root-cause test",
		WorktreeState:           "clean checkpoint",
	}); err != nil {
		t.Fatal(err)
	}
	got := requireState(t, m, "A", runtimepolicy.Active)
	if got.Blocker != nil || got.Resume.LastAction != "implement the equivalent bounded solution" {
		t.Fatalf("ordinary continuation escalated: %+v", got)
	}
}

func TestApplyExecutionBridgesOneShotRunnerOutcomes(t *testing.T) {
	m := runtimepolicy.New()
	add(t, m, "A")
	if err := m.ApplyExecution("A", runtimepolicy.ExecutionOutcome{
		Kind: runtimepolicy.Active,
		Continuation: runtimepolicy.Continuation{
			Remaining:  "verification",
			NextAction: "run focused verification",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if got := task(t, m, "A").Resume.Point; got != "run focused verification" {
		t.Fatalf("continuation resume point=%q", got)
	}
	if err := m.ApplyExecution("A", runtimepolicy.ExecutionOutcome{
		Kind:      runtimepolicy.CandidateReady,
		Candidate: candidate("A"),
		Admission: admitted(),
	}); err != nil {
		t.Fatal(err)
	}
	requireState(t, m, "A", runtimepolicy.ApprovalQueue)
}

func TestStateRoundTripPreservesCandidateResumeState(t *testing.T) {
	m := runtimepolicy.New()
	add(t, m, "A")
	c := prepareAndQueue(t, m, "A")
	var buf bytes.Buffer
	if err := m.SaveTo(&buf); err != nil {
		t.Fatal(err)
	}
	loaded, err := runtimepolicy.Load(&buf)
	if err != nil {
		t.Fatal(err)
	}
	got := task(t, loaded, "A")
	if got.State != runtimepolicy.ApprovalQueue || got.Candidate == nil || got.Candidate.Identity != c.Identity {
		t.Fatalf("round-trip lost approval candidate: %+v", got)
	}
}

func TestSaveFileUsesAtomicDurableState(t *testing.T) {
	m := runtimepolicy.New()
	add(t, m, "A")
	path := filepath.Join(t.TempDir(), "runtime.json")
	if err := m.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := runtimepolicy.LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.Snapshot().Version; got != runtimepolicy.Version {
		t.Fatalf("version=%q", got)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	contents := string(mustRead(t, path))
	if !strings.Contains(contents, `"version": "0.2.3"`) || !strings.Contains(contents, `"tasks"`) {
		t.Fatalf("state file is not a runtime document: %s", contents)
	}
}

func TestFileLockExcludesConcurrentRuntimeWriters(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "runtime.json")
	first, err := runtimepolicy.AcquireFileLock(statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	if second, err := runtimepolicy.AcquireFileLock(statePath); err == nil {
		second.Release()
		t.Fatal("second runtime writer acquired an already-owned state lock")
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	third, err := runtimepolicy.AcquireFileLock(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := third.Release(); err != nil {
		t.Fatal(err)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
