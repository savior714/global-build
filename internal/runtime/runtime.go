// Package runtime owns the durable execution policy around the one-shot BUILD
// runner.  It is deliberately executor-agnostic: an executor produces an
// ExecutionOutcome and this package decides which bounded task may continue.
//
// Runtime v0.2.3 separates the Problem Brief publication gate from the
// candidate change-approval gate.  Approval of a candidate is local to that
// candidate; it never becomes a project-wide pause.
package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"global-build/internal/watches"
)

// Version is the canonical Runtime policy version.
const Version = "0.2.3"

// RuntimeVersion is a descriptive alias for adapters that expose the policy
// version as part of their own protocol.
const RuntimeVersion = Version

// State is the execution state of one bounded task.
type State string

const (
	Active         State = "ACTIVE"
	Ready          State = "READY"
	Waiting        State = "WAITING"
	CandidateReady State = "CANDIDATE_READY"
	ApprovalQueue  State = "APPROVAL_QUEUE"
	Published      State = "PUBLISHED"
)

// State-prefixed aliases make the public state vocabulary unambiguous at
// call-sites that also import other packages with ACTIVE/READY concepts.
const (
	StateActive         = Active
	StateReady          = Ready
	StateWaiting        = Waiting
	StateCandidateReady = CandidateReady
	StateApprovalQueue  = ApprovalQueue
	StatePublished      = Published
)

// BriefStatus records the independent Problem Brief publication gate.
type BriefStatus string

const (
	BriefDraft     BriefStatus = "DRAFT"
	BriefPublished BriefStatus = "PUBLISHED"
)

// Gate identifies the gate currently holding a task, when any.
type Gate string

const (
	BriefPublicationGate Gate = "BRIEF_PUBLICATION_GATE"
	ChangeApprovalGate   Gate = "CHANGE_APPROVAL_GATE"
)

// BlockerKind is intentionally narrower than arbitrary status prose.  Only
// these scope-local blockers may participate in a whole-project STOP.
type BlockerKind string

const (
	BlockerUser       BlockerKind = "WAITING_USER"
	BlockerExternal   BlockerKind = "WAITING_EXTERNAL"
	BlockerProof      BlockerKind = "PROOF_BLOCKED"
	BlockerDependency BlockerKind = "WAITING_DEPENDENCY"
)

// RejectKind distinguishes an implementation repair from a semantic return
// to Problem Framer.  They preserve different gates but neither restarts the
// whole task implicitly.
type RejectKind string

const (
	RejectImplementation RejectKind = "IMPLEMENTATION"
	RejectSemantic       RejectKind = "SEMANTIC"
)

// Task is the durable control-plane record for one bounded slice.  Runtime
// never invents a task merely because executor capacity is available.
type Task struct {
	ID              string        `json:"id"`
	Scope           string        `json:"scope"`
	Bounded         bool          `json:"bounded"`
	PositiveValue   bool          `json:"positive_value"`
	ValueReason     string        `json:"value_reason"`
	BriefStatus     BriefStatus   `json:"brief_status"`
	State           State         `json:"state"`
	Gate            Gate          `json:"gate,omitempty"`
	DependsOn       []string      `json:"depends_on,omitempty"`
	WatchSurfaces   []string      `json:"watch_surfaces,omitempty"`
	Blocker         *Blocker      `json:"blocker,omitempty"`
	Candidate       *Candidate    `json:"candidate,omitempty"`
	Admission       *Admission    `json:"admission,omitempty"`
	Continuation    *Continuation `json:"continuation,omitempty"`
	Resume          ResumeState   `json:"resume"`
	LastError       string        `json:"last_error,omitempty"`
	PublishedCommit string        `json:"published_commit,omitempty"`
}

// Candidate is the resumable identity of a prepared implementation.  The
// digests bind approval to the exact semantics and proof that were reviewed.
type Candidate struct {
	Identity      string `json:"identity"`
	AdmittedBase  string `json:"admitted_base"`
	Commit        string `json:"commit"`
	Ref           string `json:"ref"`
	SemanticsHash string `json:"semantics_hash"`
	ProofHash     string `json:"proof_hash"`
	ResumePoint   string `json:"resume_point"`
}

// Admission is the claim-calibrated candidate gate.  Every field is explicit
// so Runtime cannot promote a convenient but under-proven candidate.
type Admission struct {
	DecisionDebtCleared       bool   `json:"decision_debt_cleared"`
	ClaimProofDebtCleared     bool   `json:"claim_proof_debt_cleared"`
	RootCauseApplicable       bool   `json:"root_cause_applicable"`
	RootCauseUnderstood       bool   `json:"root_cause_understood"`
	BoundedChange             bool   `json:"bounded_change"`
	InvariantsPreserved       bool   `json:"invariants_preserved"`
	FocusedProofSufficient    bool   `json:"focused_proof_sufficient"`
	RegressionProofSufficient bool   `json:"regression_proof_sufficient"`
	PublicationPathChecked    bool   `json:"publication_path_checked"`
	RollbackUnderstood        bool   `json:"rollback_understood"`
	MaterialResidualRisk      string `json:"material_residual_risk"`
}

// ResumeState lets APPROVE and repair resume the existing work instead of
// restarting discovery.  It is control-plane state, not a liveness lease.
type ResumeState struct {
	Point                  string `json:"point,omitempty"`
	LastAction             string `json:"last_action,omitempty"`
	InvestigationPreserved bool   `json:"investigation_preserved"`
	ProofPreserved         bool   `json:"proof_preserved"`
	FreshnessReason        string `json:"freshness_reason,omitempty"`
}

// Blocker is a scope-local report.  UserAction is NONE unless the user is
// actually the owner of the minimum needed condition.
type Blocker struct {
	Kind            BlockerKind `json:"kind"`
	Owner           string      `json:"owner"`
	WhatBlocked     string      `json:"what_blocked"`
	Done            string      `json:"done,omitempty"`
	CanContinue     string      `json:"can_continue,omitempty"`
	Needed          string      `json:"needed"`
	Evidence        string      `json:"evidence"`
	ResumePoint     string      `json:"resume_point"`
	UserAction      string      `json:"user_action"`
	CoverageChecked bool        `json:"coverage_checked"`
	DependencyID    string      `json:"dependency_id,omitempty"`
}

// Continuation is the structured form of a one-shot runner CONTINUABLE
// result.  It is stored and re-dispatched without asking the user to choose a
// next task or equivalent implementation.
type Continuation struct {
	Completed               string `json:"completed"`
	Remaining               string `json:"remaining"`
	NextAction              string `json:"next_action"`
	DoNotReopen             string `json:"do_not_reopen"`
	VerificationAlreadyDone string `json:"verification_already_done"`
	WorktreeState           string `json:"worktree_state"`
}

// ExecutionOutcome is the adapter boundary from runner/executor to Runtime.
type ExecutionOutcome struct {
	Kind         State
	Candidate    Candidate
	Admission    Admission
	Continuation Continuation
	Blocker      Blocker
}

// Approval must match the stored candidate's semantic and proof hashes.
type Approval struct {
	CandidateIdentity string `json:"candidate_identity"`
	SemanticsHash     string `json:"semantics_hash"`
	ProofHash         string `json:"proof_hash"`
}

// FreshnessObservation contains only facts needed to determine causal
// invalidation.  A different LatestMain with no overlapping changed path is
// information, not an automatic reset.
type FreshnessObservation struct {
	CheckPerformed   bool     `json:"check_performed"`
	LatestMain       string   `json:"latest_main"`
	ChangedPaths     []string `json:"changed_paths,omitempty"`
	AuthorityChanged bool     `json:"authority_changed"`
}

// FrontierEvidence is a fresh live reconstruction input.  It is not a replay
// of a historical NEXT list or task queue.
type FrontierEvidence struct {
	RepositoryRevision string `json:"repository_revision"`
	AuthorityRevision  string `json:"authority_revision"`
	CoverageChecked    bool   `json:"coverage_checked"`
	Tasks              []Task `json:"tasks"`
}

// Publication records the result of the separate publication authority.
type Publication struct {
	CandidateIdentity string `json:"candidate_identity"`
	Commit            string `json:"commit"`
	Success           bool   `json:"success"`
	Error             string `json:"error,omitempty"`
}

// StopDecision is the claim-calibrated project-level termination result.
type StopDecision struct {
	Stop   bool   `json:"stop"`
	Reason string `json:"reason"`
}

// State is the durable runtime document.  The state version is checked on
// load so an older or unknown policy cannot silently drive execution.
type StateDocument struct {
	Version                     string          `json:"version"`
	Revision                    uint64          `json:"revision"`
	LastRepositoryRev           string          `json:"last_repository_revision,omitempty"`
	LastAuthorityRev            string          `json:"last_authority_revision,omitempty"`
	FrontierCoverageChecked     bool            `json:"frontier_coverage_checked"`
	NeedsFrontierReconstruction bool            `json:"needs_frontier_reconstruction"`
	Tasks                       map[string]Task `json:"tasks"`
}

// Manager serializes task transitions in one process.  Durable callers should
// use Save after each accepted transition; file replacement is atomic.
type Manager struct {
	mu    sync.Mutex
	state StateDocument
}

// New returns an empty v0.2.3 runtime.  No task is created implicitly.
func New() *Manager {
	return &Manager{state: StateDocument{Version: Version, Tasks: map[string]Task{}}}
}

// Load reads a runtime document and fails closed on an unknown policy version.
func Load(r io.Reader) (*Manager, error) {
	if r == nil {
		return nil, errors.New("runtime: nil state reader")
	}
	var doc StateDocument
	if err := json.NewDecoder(r).Decode(&doc); err != nil {
		return nil, fmt.Errorf("runtime: decode state: %w", err)
	}
	if doc.Version != Version {
		return nil, fmt.Errorf("runtime: unsupported state version %q (want %q)", doc.Version, Version)
	}
	if doc.Tasks == nil {
		doc.Tasks = map[string]Task{}
	}
	m := &Manager{state: doc}
	if err := m.validate(); err != nil {
		return nil, err
	}
	return m, nil
}

// LoadFile is the durable-file convenience wrapper.
func LoadFile(path string) (*Manager, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Load(f)
}

// Save atomically replaces path with the current state document.
func (m *Manager) Save(path string) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("runtime: empty state path")
	}
	m.mu.Lock()
	doc := cloneDocument(m.state)
	m.mu.Unlock()
	if err := validateDocument(doc); err != nil {
		return err
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("runtime: encode state: %w", err)
	}
	b = append(b, '\n')
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".global-build-runtime-*")
	if err != nil {
		return fmt.Errorf("runtime: create state temp: %w", err)
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return fmt.Errorf("runtime: chmod state temp: %w", err)
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return fmt.Errorf("runtime: write state: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("runtime: sync state: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("runtime: close state temp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("runtime: replace state: %w", err)
	}
	return nil
}

// SaveTo serializes the validated state to a caller-owned writer.  It is
// useful for adapters that already provide their own atomic storage and for
// deterministic in-memory verification.
func (m *Manager) SaveTo(w io.Writer) error {
	if w == nil {
		return errors.New("runtime: nil state writer")
	}
	doc := m.Snapshot()
	if err := validateDocument(doc); err != nil {
		return err
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}

// Snapshot returns a copy suitable for inspection or serialization.
func (m *Manager) Snapshot() StateDocument {
	m.mu.Lock()
	defer m.mu.Unlock()
	return cloneDocument(m.state)
}

// AddTask admits a fresh bounded task.  The caller must explicitly provide a
// positive-value reason; a task is never created as scheduler filler.
func (m *Manager) AddTask(task Task) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := validateTaskIdentity(task); err != nil {
		return err
	}
	if _, exists := m.state.Tasks[task.ID]; exists {
		return fmt.Errorf("runtime: task %q already exists", task.ID)
	}
	if task.BriefStatus == "" {
		task.BriefStatus = BriefDraft
	}
	// Incoming execution state is not authority for a new task.  Admission
	// starts behind the brief gate and scheduler promotion is deterministic.
	task.State = Waiting
	if task.BriefStatus == BriefPublished {
		task.Gate = ""
		task.Blocker = nil
	} else {
		task.Gate = BriefPublicationGate
		task.Blocker = briefBlocker(task.ID)
	}
	m.state.Tasks[task.ID] = task
	m.bumpLocked()
	m.reconcileLocked()
	return nil
}

// PublishBrief closes only BRIEF_PUBLICATION_GATE.  It does not approve a
// candidate and never substitutes for CHANGE_APPROVAL_GATE.
func (m *Manager) PublishBrief(taskID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, err := m.taskLocked(taskID)
	if err != nil {
		return err
	}
	t.BriefStatus = BriefPublished
	if t.Gate == BriefPublicationGate || (t.Blocker != nil && t.Blocker.Kind == BlockerUser) {
		t.Gate = ""
		t.Blocker = nil
	}
	m.writeTaskLocked(t)
	m.bumpLocked()
	m.reconcileLocked()
	return nil
}

// Reconstruct merges fresh live frontier evidence.  Existing execution state
// and PUBLISHED work are preserved; historical state supplied by the source
// cannot resurrect or reprioritize an existing task.
func (m *Manager) Reconstruct(evidence FrontierEvidence) error {
	if strings.TrimSpace(evidence.RepositoryRevision) == "" && strings.TrimSpace(evidence.AuthorityRevision) == "" {
		return errors.New("runtime: frontier reconstruction requires a live repository or authority revision")
	}
	if !evidence.CoverageChecked {
		return errors.New("runtime: frontier reconstruction requires claim-aware coverage confirmation")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	seen := make(map[string]struct{}, len(evidence.Tasks))
	for _, incoming := range evidence.Tasks {
		if err := validateTaskIdentity(incoming); err != nil {
			return err
		}
		if _, duplicate := seen[incoming.ID]; duplicate {
			return fmt.Errorf("runtime: frontier contains duplicate task %q", incoming.ID)
		}
		seen[incoming.ID] = struct{}{}
	}
	for _, incoming := range evidence.Tasks {
		current, exists := m.state.Tasks[incoming.ID]
		if !exists {
			if incoming.BriefStatus == "" {
				incoming.BriefStatus = BriefDraft
			}
			incoming.State = Waiting
			if incoming.BriefStatus == BriefPublished {
				incoming.Gate = ""
				incoming.Blocker = nil
			} else {
				incoming.Gate = BriefPublicationGate
				incoming.Blocker = briefBlocker(incoming.ID)
			}
			m.state.Tasks[incoming.ID] = incoming
			continue
		}
		if current.State == Published || current.State == Active || current.State == CandidateReady || current.State == ApprovalQueue {
			continue
		}
		// Live source may refresh the frontier definition, but never imports its
		// historical state or replaces a task-local blocker.
		current.Scope = incoming.Scope
		current.Bounded = incoming.Bounded
		current.PositiveValue = incoming.PositiveValue
		current.ValueReason = incoming.ValueReason
		current.DependsOn = append([]string(nil), incoming.DependsOn...)
		current.WatchSurfaces = append([]string(nil), incoming.WatchSurfaces...)
		if incoming.BriefStatus != "" {
			current.BriefStatus = incoming.BriefStatus
		}
		if current.BriefStatus == BriefPublished && current.Gate == BriefPublicationGate {
			current.Gate = ""
			current.Blocker = nil
		}
		m.state.Tasks[incoming.ID] = current
	}
	m.state.LastRepositoryRev = evidence.RepositoryRevision
	m.state.LastAuthorityRev = evidence.AuthorityRevision
	m.state.FrontierCoverageChecked = true
	m.state.NeedsFrontierReconstruction = false
	m.bumpLocked()
	m.reconcileLocked()
	return nil
}

// Reconcile reruns promotion using the already admitted task set.  It does
// not create work and does not consume a historical NEXT list.
func (m *Manager) Reconcile() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.bumpLocked()
	m.reconcileLocked()
}

// ApplyExecution bridges the one-shot runner result into Runtime policy.
// COMPLETE becomes a candidate approval queue; CONTINUABLE remains active
// with a stored resume point; BLOCKED becomes a scope-local wait.
func (m *Manager) ApplyExecution(taskID string, outcome ExecutionOutcome) error {
	switch outcome.Kind {
	case CandidateReady:
		if err := m.PrepareCandidate(taskID, outcome.Candidate, outcome.Admission); err != nil {
			return err
		}
		return m.QueueApproval(taskID)
	case Waiting:
		return m.RecordBlocked(taskID, outcome.Blocker)
	case Active:
		return m.RecordContinuation(taskID, outcome.Continuation)
	default:
		return fmt.Errorf("runtime: unsupported execution outcome %q", outcome.Kind)
	}
}

// RecordContinuation preserves valid investigation/proof and leaves the task
// active.  No user escalation is generated for ordinary incomplete work.
func (m *Manager) RecordContinuation(taskID string, c Continuation) error {
	if strings.TrimSpace(c.NextAction) == "" || strings.TrimSpace(c.Remaining) == "" {
		return errors.New("runtime: continuation requires remaining work and next action")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	t, err := m.taskLocked(taskID)
	if err != nil {
		return err
	}
	if t.State != Active {
		return fmt.Errorf("runtime: task %q is %s, continuation requires ACTIVE", taskID, t.State)
	}
	t.Resume = ResumeState{
		Point:                  c.NextAction,
		LastAction:             c.NextAction,
		InvestigationPreserved: true,
		ProofPreserved:         strings.TrimSpace(c.VerificationAlreadyDone) != "",
	}
	continuation := c
	t.Continuation = &continuation
	t.LastError = ""
	m.writeTaskLocked(t)
	m.bumpLocked()
	return nil
}

// PrepareCandidate enforces the candidate admission debt before making the
// candidate resumable.  QueueApproval is a separate explicit transition.
func (m *Manager) PrepareCandidate(taskID string, candidate Candidate, admission Admission) error {
	if err := candidate.validate(); err != nil {
		return err
	}
	if err := admission.validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	t, err := m.taskLocked(taskID)
	if err != nil {
		return err
	}
	if t.State != Active {
		return fmt.Errorf("runtime: task %q is %s, candidate preparation requires ACTIVE", taskID, t.State)
	}
	t.Candidate = &candidate
	t.Admission = &admission
	t.Gate = ""
	t.Blocker = nil
	t.Resume.Point = candidate.ResumePoint
	t.Resume.LastAction = "queue candidate for approval"
	t.Resume.InvestigationPreserved = true
	t.Resume.ProofPreserved = true
	t.LastError = ""
	t.State = CandidateReady
	t.Continuation = nil
	m.writeTaskLocked(t)
	m.bumpLocked()
	m.reconcileLocked()
	return nil
}

// QueueApproval moves a fully admitted candidate to the change gate.
func (m *Manager) QueueApproval(taskID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, err := m.taskLocked(taskID)
	if err != nil {
		return err
	}
	if t.State != CandidateReady || t.Candidate == nil || t.Admission == nil {
		return fmt.Errorf("runtime: task %q is not a complete CANDIDATE_READY record", taskID)
	}
	t.State = ApprovalQueue
	t.Gate = ChangeApprovalGate
	t.Blocker = nil
	t.Resume.LastAction = "await candidate publication approval"
	m.writeTaskLocked(t)
	m.bumpLocked()
	m.reconcileLocked()
	return nil
}

// Approve resumes from the stored candidate/resume point.  A causally relevant
// watch-surface or authority change reopens only minimum revalidation; an
// unrelated main SHA movement leaves the candidate intact.
func (m *Manager) Approve(taskID string, approval Approval, freshness FreshnessObservation) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, err := m.taskLocked(taskID)
	if err != nil {
		return err
	}
	if t.State != ApprovalQueue || t.Gate != ChangeApprovalGate || t.Candidate == nil {
		return fmt.Errorf("runtime: task %q is not awaiting CHANGE_APPROVAL_GATE", taskID)
	}
	if approval.CandidateIdentity != t.Candidate.Identity || approval.SemanticsHash != t.Candidate.SemanticsHash || approval.ProofHash != t.Candidate.ProofHash {
		return fmt.Errorf("runtime: approval does not bind to stored candidate semantics and proof")
	}
	if !freshness.CheckPerformed {
		return errors.New("runtime: approval requires a causally relevant freshness check")
	}
	if err := freshness.validate(); err != nil {
		return err
	}
	relevant := t.causallyInvalidated(freshness)
	t.Gate = ""
	t.Blocker = nil
	t.Resume.FreshnessReason = ""
	if relevant {
		t.Resume.LastAction = "minimum revalidation of candidate"
		t.Resume.FreshnessReason = "causally relevant watch surface or authority changed"
	} else {
		t.Resume.LastAction = "publish stored candidate"
		t.Resume.Point = t.Candidate.ResumePoint
	}
	// An independent task may be ACTIVE while this approval arrives.  Keep
	// the resumed candidate READY until the single active slot is available.
	if m.hasActiveOtherLocked(taskID) {
		if m.hasReadyOtherLocked(taskID) {
			t.State = Waiting
		} else {
			t.State = Ready
		}
	} else {
		t.State = Active
	}
	t.Resume.InvestigationPreserved = true
	t.Resume.ProofPreserved = true
	m.writeTaskLocked(t)
	m.bumpLocked()
	m.reconcileLocked()
	return nil
}

// Reject preserves the useful part of the work.  Implementation rejection
// returns to local repair; semantic rejection returns to the brief gate while
// retaining the investigation/proof resume markers.
func (m *Manager) Reject(taskID string, kind RejectKind, note string) error {
	if kind != RejectImplementation && kind != RejectSemantic {
		return fmt.Errorf("runtime: unknown rejection kind %q", kind)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	t, err := m.taskLocked(taskID)
	if err != nil {
		return err
	}
	if t.State != ApprovalQueue {
		return fmt.Errorf("runtime: task %q is not awaiting approval", taskID)
	}
	t.LastError = strings.TrimSpace(note)
	t.Resume.InvestigationPreserved = true
	t.Resume.ProofPreserved = true
	if kind == RejectSemantic {
		t.BriefStatus = BriefDraft
		t.State = Waiting
		t.Gate = BriefPublicationGate
		t.Blocker = briefBlocker(taskID)
		t.Resume.LastAction = "revise Problem Brief semantics"
	} else {
		t.Gate = ""
		t.Blocker = nil
		t.Resume.LastAction = "repair rejected implementation"
		if m.hasActiveOtherLocked(taskID) {
			if m.hasReadyOtherLocked(taskID) {
				t.State = Waiting
			} else {
				t.State = Ready
			}
		} else {
			t.State = Active
		}
	}
	m.writeTaskLocked(t)
	m.bumpLocked()
	m.reconcileLocked()
	return nil
}

// RecordPublished closes the candidate task and immediately promotes the next
// live frontier already represented in the runtime.  A failed publication is
// a local external wait, not a project-wide stop.
func (m *Manager) RecordPublished(taskID string, publication Publication) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, err := m.taskLocked(taskID)
	if err != nil {
		return err
	}
	if t.Candidate == nil || publication.CandidateIdentity != t.Candidate.Identity {
		return fmt.Errorf("runtime: publication does not match stored candidate")
	}
	if t.State != Active && t.State != Ready {
		return fmt.Errorf("runtime: task %q has not resumed from CHANGE_APPROVAL_GATE", taskID)
	}
	if t.Resume.LastAction != "publish stored candidate" {
		return fmt.Errorf("runtime: task %q requires candidate revalidation before publication", taskID)
	}
	if !publication.Success {
		evidence := strings.TrimSpace(publication.Error)
		if evidence == "" {
			evidence = "publication authority returned an unsuccessful outcome"
		}
		t.State = Waiting
		t.Gate = ""
		t.Blocker = &Blocker{
			Kind:        BlockerExternal,
			Owner:       "EXTERNAL",
			WhatBlocked: "candidate publication",
			CanContinue: "independent tasks may continue",
			Needed:      "resolve publication failure and retry the stored candidate",
			Evidence:    evidence,
			ResumePoint: t.Candidate.ResumePoint,
			UserAction:  "NONE",
		}
		t.Resume.LastAction = "retry stored candidate publication"
		m.writeTaskLocked(t)
		m.bumpLocked()
		m.reconcileLocked()
		return nil
	}
	if strings.TrimSpace(publication.Commit) == "" {
		return errors.New("runtime: successful publication requires a commit")
	}
	t.State = Published
	t.Gate = ""
	t.Blocker = nil
	t.PublishedCommit = publication.Commit
	t.Continuation = nil
	t.Resume.LastAction = "published; reconstruct live frontier"
	m.state.NeedsFrontierReconstruction = true
	m.writeTaskLocked(t)
	m.bumpLocked()
	m.reconcileLocked()
	return nil
}

// RecordRevalidated closes the minimum revalidation step opened by a causal
// freshness change. It preserves the candidate identity and returns the task
// to the same publication continuation; it never starts discovery again.
func (m *Manager) RecordRevalidated(taskID string, proofSufficient bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, err := m.taskLocked(taskID)
	if err != nil {
		return err
	}
	if t.Candidate == nil || (t.State != Active && t.State != Ready) || t.Resume.LastAction != "minimum revalidation of candidate" {
		return fmt.Errorf("runtime: task %q is not awaiting minimum candidate revalidation", taskID)
	}
	if !proofSufficient {
		return errors.New("runtime: revalidation proof is insufficient")
	}
	t.Resume.LastAction = "publish stored candidate"
	t.Resume.FreshnessReason = "revalidated after causally relevant change"
	m.writeTaskLocked(t)
	m.bumpLocked()
	m.reconcileLocked()
	return nil
}

// RecordBlocked records a validated scope-local blocker.  It rejects unknown
// blocker kinds so generic prose cannot accidentally authorize whole-project
// STOP.
func (m *Manager) RecordBlocked(taskID string, blocker Blocker) error {
	if blocker.Kind != BlockerUser && blocker.Kind != BlockerExternal && blocker.Kind != BlockerProof {
		return fmt.Errorf("runtime: BLOCKED outcome must be USER, EXTERNAL, or PROOF scoped")
	}
	if strings.TrimSpace(blocker.WhatBlocked) == "" || strings.TrimSpace(blocker.Needed) == "" || strings.TrimSpace(blocker.Evidence) == "" || strings.TrimSpace(blocker.ResumePoint) == "" {
		return errors.New("runtime: blocker requires what, needed, evidence, and resume point")
	}
	if (blocker.Kind == BlockerExternal || blocker.Kind == BlockerProof) && !blocker.CoverageChecked {
		return errors.New("runtime: external/proof blocker requires claim-aware coverage check before it can support STOP")
	}
	if blocker.UserAction == "" || blocker.Owner != "USER" {
		blocker.UserAction = "NONE"
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	t, err := m.taskLocked(taskID)
	if err != nil {
		return err
	}
	if t.State != Active && t.State != Ready {
		return fmt.Errorf("runtime: task %q is %s, blocker requires ACTIVE or READY", taskID, t.State)
	}
	t.State = Waiting
	t.Gate = ""
	t.Blocker = &blocker
	t.Resume.Point = blocker.ResumePoint
	t.Resume.LastAction = "resume after scope-local blocker clears"
	t.Resume.InvestigationPreserved = true
	m.writeTaskLocked(t)
	m.bumpLocked()
	m.reconcileLocked()
	return nil
}

// Plan returns the current ACTIVE and READY dispatch records in deterministic
// order.  Approval queue records are deliberately absent from the plan.
func (m *Manager) Plan() []Task {
	s := m.Snapshot()
	ids := make([]string, 0, len(s.Tasks))
	for id, t := range s.Tasks {
		if t.State == Active || t.State == Ready {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	out := make([]Task, 0, len(ids))
	for _, id := range ids {
		out = append(out, s.Tasks[id])
	}
	return out
}

// StopDecision reports whole-project STOP only after active/ready capacity is
// empty and every remaining positive-value slice is a genuine user, external,
// or proof blocker (including a dependency chain ending in one of those).
func (m *Manager) StopDecision() StopDecision {
	s := m.Snapshot()
	if s.NeedsFrontierReconstruction {
		return StopDecision{Reason: "current live frontier reconstruction is required before whole-project STOP"}
	}
	for _, t := range s.Tasks {
		if t.State == Active || t.State == Ready {
			return StopDecision{Reason: fmt.Sprintf("task %q remains %s", t.ID, t.State)}
		}
	}
	for _, t := range s.Tasks {
		if !t.useful() || t.State == Published {
			continue
		}
		if !stopBlocked(t.ID, s.Tasks, map[string]bool{}) {
			return StopDecision{Reason: fmt.Sprintf("task %q has no admissible terminal blocker", t.ID)}
		}
	}
	return StopDecision{Stop: true, Reason: "no ACTIVE or READY positive-value work remains; all useful slices are scope-locally blocked"}
}

func (m *Manager) taskLocked(id string) (*Task, error) {
	t, ok := m.state.Tasks[id]
	if !ok {
		return nil, fmt.Errorf("runtime: unknown task %q", id)
	}
	return &t, nil
}

func (m *Manager) hasActiveOtherLocked(id string) bool {
	for otherID, t := range m.state.Tasks {
		if otherID != id && t.State == Active {
			return true
		}
	}
	return false
}

func (m *Manager) hasReadyOtherLocked(id string) bool {
	for otherID, t := range m.state.Tasks {
		if otherID != id && t.State == Ready {
			return true
		}
	}
	return false
}

// taskLocked returns a copy, so transitions must write it back.  This helper
// is intentionally centralized to make accidental unsaved mutation obvious.
func (m *Manager) writeTaskLocked(t *Task) {
	m.state.Tasks[t.ID] = *t
}

func (m *Manager) bumpLocked() { m.state.Revision++ }

func (m *Manager) reconcileLocked() {
	ids := sortedTaskIDs(m.state.Tasks)
	for _, id := range ids {
		t := m.state.Tasks[id]
		if t.State == Published || t.State == ApprovalQueue || t.State == CandidateReady {
			continue
		}
		if t.BriefStatus != BriefPublished {
			t.State = Waiting
			t.Gate = BriefPublicationGate
			t.Blocker = briefBlocker(t.ID)
			m.writeTaskLocked(&t)
			continue
		}
		if t.Blocker != nil && t.Blocker.Kind != BlockerDependency {
			t.State = Waiting
			m.writeTaskLocked(&t)
			continue
		}
		if dependency := firstUnpublishedDependency(t, m.state.Tasks); dependency != "" {
			t.State = Waiting
			t.Gate = ""
			t.Blocker = &Blocker{
				Kind:         BlockerDependency,
				Owner:        "DEPENDENCY",
				WhatBlocked:  "dependent task cannot run before its prerequisite is published",
				CanContinue:  "independent tasks may continue",
				Needed:       "publish prerequisite " + dependency,
				Evidence:     "prerequisite state=" + string(m.state.Tasks[dependency].State),
				ResumePoint:  "reconstruct after prerequisite publication",
				UserAction:   "NONE",
				DependencyID: dependency,
			}
			m.writeTaskLocked(&t)
			continue
		}
		if t.Blocker != nil && t.Blocker.Kind == BlockerDependency {
			t.Blocker = nil
		}
		if t.State == Waiting || t.State == Ready {
			t.Gate = ""
		}
		m.writeTaskLocked(&t)
	}

	active := ""
	for _, id := range ids {
		if m.state.Tasks[id].State == Active {
			active = id
			break
		}
	}
	if active == "" {
		for _, id := range ids {
			if m.scheduleableLocked(m.state.Tasks[id]) && m.state.Tasks[id].State == Ready {
				t := m.state.Tasks[id]
				t.State = Active
				m.writeTaskLocked(&t)
				active = id
				break
			}
		}
		if active == "" {
			for _, id := range ids {
				if m.scheduleableLocked(m.state.Tasks[id]) && m.state.Tasks[id].State == Waiting {
					t := m.state.Tasks[id]
					t.State = Active
					m.writeTaskLocked(&t)
					active = id
					break
				}
			}
		}
	}
	if active != "" {
		ready := false
		for _, id := range ids {
			if m.state.Tasks[id].State == Ready {
				ready = true
				break
			}
		}
		if !ready {
			for _, id := range ids {
				if id == active {
					continue
				}
				t := m.state.Tasks[id]
				if m.scheduleableLocked(t) && t.State == Waiting {
					t.State = Ready
					m.writeTaskLocked(&t)
					break
				}
			}
		}
	}
}

func (m *Manager) scheduleableLocked(t Task) bool {
	return t.useful() && t.BriefStatus == BriefPublished && t.Blocker == nil && firstUnpublishedDependency(t, m.state.Tasks) == ""
}

func (m *Manager) validate() error { return validateDocument(m.state) }

func (t Task) useful() bool {
	return t.Bounded && t.PositiveValue && strings.TrimSpace(t.Scope) != "" && strings.TrimSpace(t.ValueReason) != ""
}

func validateTaskIdentity(t Task) error {
	if strings.TrimSpace(t.ID) == "" {
		return errors.New("runtime: task id is required")
	}
	if strings.ContainsAny(t.ID, "\r\n") {
		return fmt.Errorf("runtime: task id %q contains a newline", t.ID)
	}
	if strings.TrimSpace(t.Scope) == "" {
		return fmt.Errorf("runtime: task %q scope is required", t.ID)
	}
	if t.BriefStatus != "" && t.BriefStatus != BriefDraft && t.BriefStatus != BriefPublished {
		return fmt.Errorf("runtime: task %q has unknown brief status %q", t.ID, t.BriefStatus)
	}
	if len(t.WatchSurfaces) == 0 {
		return fmt.Errorf("runtime: task %q requires at least one watch surface", t.ID)
	}
	for _, surface := range t.WatchSurfaces {
		if _, err := watches.Normalize(surface); err != nil {
			return fmt.Errorf("runtime: task %q watch surface: %v", t.ID, err)
		}
	}
	if t.Bounded && t.PositiveValue && strings.TrimSpace(t.ValueReason) == "" {
		return fmt.Errorf("runtime: task %q positive value requires a reason", t.ID)
	}
	return nil
}

func (c Candidate) validate() error {
	for name, value := range map[string]string{
		"identity": c.Identity, "admitted_base": c.AdmittedBase, "commit": c.Commit,
		"ref": c.Ref, "semantics_hash": c.SemanticsHash, "proof_hash": c.ProofHash,
		"resume_point": c.ResumePoint,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("runtime: candidate %s is required", name)
		}
	}
	return nil
}

func (a Admission) validate() error {
	checks := map[string]bool{
		"decision debt": a.DecisionDebtCleared, "claim proof debt": a.ClaimProofDebtCleared,
		"bounded change": a.BoundedChange,
		"invariants":     a.InvariantsPreserved, "focused proof": a.FocusedProofSufficient,
		"regression proof": a.RegressionProofSufficient, "publication path": a.PublicationPathChecked,
		"rollback": a.RollbackUnderstood,
	}
	for name, ok := range checks {
		if !ok {
			return fmt.Errorf("runtime: candidate admission debt not cleared: %s", name)
		}
	}
	if a.RootCauseApplicable && !a.RootCauseUnderstood {
		return errors.New("runtime: candidate admission debt not cleared: root cause")
	}
	if strings.TrimSpace(a.MaterialResidualRisk) == "" {
		return errors.New("runtime: material residual risk must be stated")
	}
	return nil
}

func (t Task) causallyInvalidated(observation FreshnessObservation) bool {
	if observation.AuthorityChanged {
		return true
	}
	set := watches.New(t.WatchSurfaces)
	for _, path := range observation.ChangedPaths {
		if set.Contains(path) {
			return true
		}
	}
	return false
}

func (o FreshnessObservation) validate() error {
	for _, path := range o.ChangedPaths {
		if path == "" || strings.Contains(path, "\x00") || strings.HasPrefix(path, "/") {
			return fmt.Errorf("runtime: malformed freshness changed path %q", path)
		}
	}
	return nil
}

func briefBlocker(id string) *Blocker {
	return &Blocker{
		Kind:        BlockerUser,
		Owner:       "USER",
		WhatBlocked: "Problem Brief is not canonical",
		Needed:      "publish the Problem Brief or change direction",
		Evidence:    "task " + id + " has not crossed BRIEF_PUBLICATION_GATE",
		ResumePoint: "publish Problem Brief, then reconstruct frontier",
		UserAction:  "APPROVE / REJECT / CHANGE DIRECTION",
	}
}

func firstUnpublishedDependency(t Task, tasks map[string]Task) string {
	deps := append([]string(nil), t.DependsOn...)
	sort.Strings(deps)
	for _, dep := range deps {
		if prerequisite, ok := tasks[dep]; !ok || prerequisite.State != Published {
			return dep
		}
	}
	return ""
}

func stopBlocked(id string, tasks map[string]Task, seen map[string]bool) bool {
	if seen[id] {
		return false
	}
	seen[id] = true
	t, ok := tasks[id]
	if !ok {
		return false
	}
	if !t.useful() || t.State == Published {
		return true
	}
	if t.State == ApprovalQueue || t.Gate == BriefPublicationGate {
		return true
	}
	if t.Blocker != nil {
		switch t.Blocker.Kind {
		case BlockerUser:
			return true
		case BlockerExternal, BlockerProof:
			return t.Blocker.CoverageChecked
		case BlockerDependency:
			return stopBlocked(t.Blocker.DependencyID, tasks, seen)
		}
	}
	return false
}

func sortedTaskIDs(tasks map[string]Task) []string {
	ids := make([]string, 0, len(tasks))
	for id := range tasks {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func cloneDocument(in StateDocument) StateDocument {
	out := in
	out.Tasks = make(map[string]Task, len(in.Tasks))
	for id, task := range in.Tasks {
		taskCopy := task
		taskCopy.DependsOn = append([]string(nil), task.DependsOn...)
		taskCopy.WatchSurfaces = append([]string(nil), task.WatchSurfaces...)
		if task.Blocker != nil {
			b := *task.Blocker
			taskCopy.Blocker = &b
		}
		if task.Candidate != nil {
			c := *task.Candidate
			taskCopy.Candidate = &c
		}
		if task.Admission != nil {
			a := *task.Admission
			taskCopy.Admission = &a
		}
		if task.Continuation != nil {
			c := *task.Continuation
			taskCopy.Continuation = &c
		}
		out.Tasks[id] = taskCopy
	}
	return out
}

func validateDocument(doc StateDocument) error {
	if doc.Version != Version {
		return fmt.Errorf("runtime: unsupported state version %q", doc.Version)
	}
	activeCount, readyCount := 0, 0
	for id, task := range doc.Tasks {
		if id != task.ID {
			return fmt.Errorf("runtime: task map key %q does not match task id %q", id, task.ID)
		}
		if err := validateTaskIdentity(task); err != nil {
			return err
		}
		switch task.State {
		case Active:
			activeCount++
		case Ready:
			readyCount++
		case Waiting, CandidateReady, ApprovalQueue, Published:
		default:
			return fmt.Errorf("runtime: task %q has unknown state %q", id, task.State)
		}
		if (task.State == CandidateReady || task.State == ApprovalQueue) && (task.Candidate == nil || task.Admission == nil) {
			return fmt.Errorf("runtime: task %q in %s lacks candidate admission record", id, task.State)
		}
		if task.Candidate != nil {
			if err := task.Candidate.validate(); err != nil {
				return err
			}
		}
		if task.Admission != nil {
			if err := task.Admission.validate(); err != nil {
				return err
			}
		}
	}
	if activeCount > 1 {
		return fmt.Errorf("runtime: state has %d ACTIVE tasks; want at most one", activeCount)
	}
	if readyCount > 1 {
		return fmt.Errorf("runtime: state has %d READY tasks; want at most one", readyCount)
	}
	return nil
}
