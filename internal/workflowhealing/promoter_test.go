package workflowhealing

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
)

// --- fakes -----------------------------------------------------------

// fakeProposalRepo is a minimal in-memory WorkflowProposalRepository.
// Only the methods the Promoter touches (Get, Decide) carry behaviour;
// the rest satisfy the interface.
type fakeProposalRepo struct {
	mu        sync.Mutex
	rows      map[string]*persistence.WorkflowProposal
	getErr    error
	decideErr error
	decided   []string // ids that went through Decide
}

func newFakeProposalRepo() *fakeProposalRepo {
	return &fakeProposalRepo{rows: map[string]*persistence.WorkflowProposal{}}
}

func (f *fakeProposalRepo) put(p *persistence.WorkflowProposal) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *p
	f.rows[p.ID] = &cp
}

func (f *fakeProposalRepo) Insert(ctx context.Context, p *persistence.WorkflowProposal) error {
	f.put(p)
	return nil
}

func (f *fakeProposalRepo) Get(ctx context.Context, id string) (*persistence.WorkflowProposal, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.rows[id]
	if !ok {
		return nil, persistence.ErrNotFound
	}
	cp := *p
	return &cp, nil
}

func (f *fakeProposalRepo) List(ctx context.Context, filter persistence.WorkflowProposalFilter) ([]*persistence.WorkflowProposal, error) {
	return nil, nil
}

func (f *fakeProposalRepo) Decide(ctx context.Context, id string, status persistence.WorkflowProposalStatus, decidedBy, notes string) error {
	if f.decideErr != nil {
		return f.decideErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.rows[id]
	if !ok {
		return persistence.ErrNotFound
	}
	p.Status = status
	f.decided = append(f.decided, id)
	return nil
}

func (f *fakeProposalRepo) MarkApplied(ctx context.Context, id, appliedCommit string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p, ok := f.rows[id]; ok {
		p.Status = persistence.WorkflowProposalStatusApplied
		p.AppliedCommit = appliedCommit
	}
	return nil
}

func (f *fakeProposalRepo) MarkRolledBack(ctx context.Context, id, rollbackCommit string) error {
	return nil
}

func (f *fakeProposalRepo) UpdateProposalYAML(ctx context.Context, id, newYAML, editedBy string) error {
	return nil
}

// fakeApplier records its calls and applies the proposal in the repo so
// the end-to-end state is observable.
type fakeApplier struct {
	repo      *fakeProposalRepo
	applyErr  error
	appliedID string
	appliedBy string
	calls     int
}

func (a *fakeApplier) Apply(ctx context.Context, proposalID, decidedBy string) (*persistence.WorkflowProposal, error) {
	a.calls++
	if a.applyErr != nil {
		return nil, a.applyErr
	}
	a.appliedID = proposalID
	a.appliedBy = decidedBy
	// Mirror the real applier: only approved proposals apply.
	p, err := a.repo.Get(ctx, proposalID)
	if err != nil {
		return nil, err
	}
	if p.Status != persistence.WorkflowProposalStatusApproved {
		return nil, errors.New("applier: proposal not approved")
	}
	_ = a.repo.MarkApplied(ctx, proposalID, "deadbeef")
	return a.repo.Get(ctx, proposalID)
}

// countingMetrics records promotion calls.
type countingMetrics struct{ promotions int }

func (m *countingMetrics) RecordPromotion() { m.promotions++ }

// --- helpers ---------------------------------------------------------

func seedPromoCandidate(t *testing.T, repo *fakeCandidateRepo, status persistence.HealingCandidateStatus, proposalID string) *persistence.HealingCandidate {
	t.Helper()
	c := &persistence.HealingCandidate{
		ID:         "whc_test",
		TriggerID:  "trg_1",
		ProjectID:  "proj_1",
		WorkflowID: "wf_1",
		ProposalID: proposalID,
		Status:     status,
	}
	repo.put(c)
	return c
}

func newPromoterUnderTest(cr *fakeCandidateRepo, pr *fakeProposalRepo, ap ProposalApplier, m Metrics) *Promoter {
	return NewPromoter(cr, pr, ap, m, zerolog.Nop())
}

// --- Promote: happy path ---------------------------------------------

func TestPromote_ApprovesAppliesAndPromotes(t *testing.T) {
	cr := newFakeCandidateRepo()
	pr := newFakeProposalRepo()
	pr.put(&persistence.WorkflowProposal{ID: "wpr_1", WorkflowID: "wf_1", Status: persistence.WorkflowProposalStatusPending})
	seedPromoCandidate(t, cr, persistence.HealingCandidateTrialPassed, "wpr_1")
	ap := &fakeApplier{repo: pr}
	m := &countingMetrics{}

	got, err := newPromoterUnderTest(cr, pr, ap, m).Promote(context.Background(), "whc_test", "op@x")
	if err != nil {
		t.Fatalf("Promote: unexpected error: %v", err)
	}
	if got.Status != persistence.HealingCandidatePromoted {
		t.Errorf("candidate status = %q, want promoted", got.Status)
	}
	if got.PromotedBy != "op@x" {
		t.Errorf("promoted_by = %q, want op@x", got.PromotedBy)
	}
	if len(pr.decided) != 1 || pr.decided[0] != "wpr_1" {
		t.Errorf("expected proposal wpr_1 to be approved, got decided=%v", pr.decided)
	}
	if ap.calls != 1 || ap.appliedID != "wpr_1" || ap.appliedBy != "op@x" {
		t.Errorf("applier not called correctly: calls=%d id=%q by=%q", ap.calls, ap.appliedID, ap.appliedBy)
	}
	if m.promotions != 1 {
		t.Errorf("promotion metric = %d, want 1", m.promotions)
	}
}

// An already-approved proposal skips Decide but still applies.
func TestPromote_AlreadyApprovedProposalSkipsDecide(t *testing.T) {
	cr := newFakeCandidateRepo()
	pr := newFakeProposalRepo()
	pr.put(&persistence.WorkflowProposal{ID: "wpr_1", WorkflowID: "wf_1", Status: persistence.WorkflowProposalStatusApproved})
	seedPromoCandidate(t, cr, persistence.HealingCandidateTrialPassed, "wpr_1")
	ap := &fakeApplier{repo: pr}

	if _, err := newPromoterUnderTest(cr, pr, ap, nil).Promote(context.Background(), "whc_test", "op@x"); err != nil {
		t.Fatalf("Promote: unexpected error: %v", err)
	}
	if len(pr.decided) != 0 {
		t.Errorf("approved proposal should not be re-decided; got %v", pr.decided)
	}
	if ap.calls != 1 {
		t.Errorf("applier calls = %d, want 1", ap.calls)
	}
}

// --- Promote: gate failures (nothing promotes without trial_passed) --

func TestPromote_RefusesUntriedCandidate(t *testing.T) {
	for _, status := range []persistence.HealingCandidateStatus{
		persistence.HealingCandidateDraft,
		persistence.HealingCandidateTrialRunning,
		persistence.HealingCandidateTrialFailed,
	} {
		cr := newFakeCandidateRepo()
		pr := newFakeProposalRepo()
		pr.put(&persistence.WorkflowProposal{ID: "wpr_1", Status: persistence.WorkflowProposalStatusPending})
		seedPromoCandidate(t, cr, status, "wpr_1")
		ap := &fakeApplier{repo: pr}

		_, err := newPromoterUnderTest(cr, pr, ap, nil).Promote(context.Background(), "whc_test", "op@x")
		if !errors.Is(err, ErrCandidateNotPromotable) {
			t.Errorf("status %q: err = %v, want ErrCandidateNotPromotable", status, err)
		}
		if ap.calls != 0 {
			t.Errorf("status %q: applier must NOT run on a non-promotable candidate", status)
		}
	}
}

func TestPromote_RefusesTerminalCandidate(t *testing.T) {
	for _, status := range []persistence.HealingCandidateStatus{
		persistence.HealingCandidatePromoted,
		persistence.HealingCandidateRejected,
	} {
		cr := newFakeCandidateRepo()
		pr := newFakeProposalRepo()
		seedPromoCandidate(t, cr, status, "wpr_1")
		ap := &fakeApplier{repo: pr}

		_, err := newPromoterUnderTest(cr, pr, ap, nil).Promote(context.Background(), "whc_test", "op@x")
		if !errors.Is(err, ErrCandidateTerminal) {
			t.Errorf("status %q: err = %v, want ErrCandidateTerminal", status, err)
		}
		if ap.calls != 0 {
			t.Errorf("status %q: applier must NOT run on a terminal candidate", status)
		}
	}
}

func TestPromote_CandidateNotFound(t *testing.T) {
	cr := newFakeCandidateRepo()
	pr := newFakeProposalRepo()
	ap := &fakeApplier{repo: pr}
	_, err := newPromoterUnderTest(cr, pr, ap, nil).Promote(context.Background(), "missing", "op@x")
	if !errors.Is(err, ErrCandidateNotFound) {
		t.Fatalf("err = %v, want ErrCandidateNotFound", err)
	}
}

func TestPromote_NoProposalLinked(t *testing.T) {
	cr := newFakeCandidateRepo()
	pr := newFakeProposalRepo()
	seedPromoCandidate(t, cr, persistence.HealingCandidateTrialPassed, "")
	ap := &fakeApplier{repo: pr}
	_, err := newPromoterUnderTest(cr, pr, ap, nil).Promote(context.Background(), "whc_test", "op@x")
	if !errors.Is(err, ErrNoProposalLinked) {
		t.Fatalf("err = %v, want ErrNoProposalLinked", err)
	}
	if ap.calls != 0 {
		t.Errorf("applier must not run without a linked proposal")
	}
}

func TestPromote_ApplierNotWired(t *testing.T) {
	cr := newFakeCandidateRepo()
	seedPromoCandidate(t, cr, persistence.HealingCandidateTrialPassed, "wpr_1")
	// applier + proposals nil.
	_, err := NewPromoter(cr, nil, nil, nil, zerolog.Nop()).Promote(context.Background(), "whc_test", "op@x")
	if !errors.Is(err, ErrApplierNotWired) {
		t.Fatalf("err = %v, want ErrApplierNotWired", err)
	}
}

// A proposal in a non-promotable state (applied/rejected) is refused
// before any apply runs.
func TestPromote_ProposalInWrongState(t *testing.T) {
	cr := newFakeCandidateRepo()
	pr := newFakeProposalRepo()
	pr.put(&persistence.WorkflowProposal{ID: "wpr_1", Status: persistence.WorkflowProposalStatusApplied})
	seedPromoCandidate(t, cr, persistence.HealingCandidateTrialPassed, "wpr_1")
	ap := &fakeApplier{repo: pr}
	_, err := newPromoterUnderTest(cr, pr, ap, nil).Promote(context.Background(), "whc_test", "op@x")
	if err == nil {
		t.Fatal("expected an error for an already-applied proposal")
	}
	if ap.calls != 0 {
		t.Errorf("applier must not run on a wrong-state proposal")
	}
}

// Apply failure does NOT flip the candidate to promoted.
func TestPromote_ApplyFailureLeavesCandidateUnpromoted(t *testing.T) {
	cr := newFakeCandidateRepo()
	pr := newFakeProposalRepo()
	pr.put(&persistence.WorkflowProposal{ID: "wpr_1", Status: persistence.WorkflowProposalStatusPending})
	seedPromoCandidate(t, cr, persistence.HealingCandidateTrialPassed, "wpr_1")
	ap := &fakeApplier{repo: pr, applyErr: errors.New("git commit failed")}
	m := &countingMetrics{}

	_, err := newPromoterUnderTest(cr, pr, ap, m).Promote(context.Background(), "whc_test", "op@x")
	if err == nil {
		t.Fatal("expected apply error to propagate")
	}
	got, _ := cr.Get(context.Background(), "whc_test")
	if got.Status == persistence.HealingCandidatePromoted {
		t.Error("candidate must NOT be promoted when apply fails")
	}
	if m.promotions != 0 {
		t.Errorf("promotion metric must not increment on apply failure; got %d", m.promotions)
	}
}

// Decide failure stops the flow before apply.
func TestPromote_DecideFailureStopsBeforeApply(t *testing.T) {
	cr := newFakeCandidateRepo()
	pr := newFakeProposalRepo()
	pr.put(&persistence.WorkflowProposal{ID: "wpr_1", Status: persistence.WorkflowProposalStatusPending})
	pr.decideErr = errors.New("decide blew up")
	seedPromoCandidate(t, cr, persistence.HealingCandidateTrialPassed, "wpr_1")
	ap := &fakeApplier{repo: pr}

	if _, err := newPromoterUnderTest(cr, pr, ap, nil).Promote(context.Background(), "whc_test", "op@x"); err == nil {
		t.Fatal("expected decide error to propagate")
	}
	if ap.calls != 0 {
		t.Errorf("applier must not run when approve fails")
	}
}

// --- Reject ----------------------------------------------------------

func TestReject_FlipsCandidate(t *testing.T) {
	cr := newFakeCandidateRepo()
	seedPromoCandidate(t, cr, persistence.HealingCandidateTrialPassed, "wpr_1")
	got, err := NewPromoter(cr, nil, nil, nil, zerolog.Nop()).Reject(context.Background(), "whc_test")
	if err != nil {
		t.Fatalf("Reject: unexpected error: %v", err)
	}
	if got.Status != persistence.HealingCandidateRejected {
		t.Errorf("status = %q, want rejected", got.Status)
	}
}

func TestReject_RefusesTerminal(t *testing.T) {
	cr := newFakeCandidateRepo()
	seedPromoCandidate(t, cr, persistence.HealingCandidatePromoted, "wpr_1")
	_, err := NewPromoter(cr, nil, nil, nil, zerolog.Nop()).Reject(context.Background(), "whc_test")
	if !errors.Is(err, ErrCandidateTerminal) {
		t.Fatalf("err = %v, want ErrCandidateTerminal", err)
	}
}

func TestReject_NotFound(t *testing.T) {
	cr := newFakeCandidateRepo()
	_, err := NewPromoter(cr, nil, nil, nil, zerolog.Nop()).Reject(context.Background(), "missing")
	if !errors.Is(err, ErrCandidateNotFound) {
		t.Fatalf("err = %v, want ErrCandidateNotFound", err)
	}
}

// Reject works even with no proposal/applier wired (operator "no").
func TestReject_DoesNotTouchProposal(t *testing.T) {
	cr := newFakeCandidateRepo()
	seedPromoCandidate(t, cr, persistence.HealingCandidateDraft, "wpr_1")
	if _, err := NewPromoter(cr, nil, nil, nil, zerolog.Nop()).Reject(context.Background(), "whc_test"); err != nil {
		t.Fatalf("Reject should succeed without a proposal repo: %v", err)
	}
}

// --- remaining error branches ----------------------------------------

func TestPromote_NilCandidatesRepo(t *testing.T) {
	if _, err := NewPromoter(nil, nil, nil, nil, zerolog.Nop()).Promote(context.Background(), "x", "op@x"); err == nil {
		t.Fatal("expected error when candidates repo is nil")
	}
}

func TestReject_NilCandidatesRepo(t *testing.T) {
	if _, err := NewPromoter(nil, nil, nil, nil, zerolog.Nop()).Reject(context.Background(), "x"); err == nil {
		t.Fatal("expected error when candidates repo is nil")
	}
}

// A non-NotFound candidate-load error is wrapped, not turned into
// ErrCandidateNotFound.
func TestPromote_CandidateLoadError(t *testing.T) {
	cr := newFakeCandidateRepo()
	cr.getErr = errors.New("db unreachable")
	_, err := NewPromoter(cr, newFakeProposalRepo(), &fakeApplier{}, nil, zerolog.Nop()).
		Promote(context.Background(), "whc_test", "op@x")
	if err == nil || errors.Is(err, ErrCandidateNotFound) {
		t.Fatalf("err = %v, want a wrapped load error (not ErrCandidateNotFound)", err)
	}
}

func TestReject_CandidateLoadError(t *testing.T) {
	cr := newFakeCandidateRepo()
	cr.getErr = errors.New("db unreachable")
	_, err := NewPromoter(cr, nil, nil, nil, zerolog.Nop()).Reject(context.Background(), "whc_test")
	if err == nil || errors.Is(err, ErrCandidateNotFound) {
		t.Fatalf("err = %v, want a wrapped load error", err)
	}
}

// Proposal-load failure aborts before apply.
func TestPromote_ProposalLoadError(t *testing.T) {
	cr := newFakeCandidateRepo()
	pr := newFakeProposalRepo()
	pr.getErr = errors.New("proposal db down")
	seedPromoCandidate(t, cr, persistence.HealingCandidateTrialPassed, "wpr_1")
	ap := &fakeApplier{repo: pr}
	if _, err := newPromoterUnderTest(cr, pr, ap, nil).Promote(context.Background(), "whc_test", "op@x"); err == nil {
		t.Fatal("expected proposal-load error to propagate")
	}
	if ap.calls != 0 {
		t.Error("applier must not run when the proposal can't be loaded")
	}
}

// The candidate-stamp failure AFTER a successful apply surfaces the
// error (the workflow change is already live; the operator reconciles).
func TestPromote_CandidateStampFailureAfterApply(t *testing.T) {
	cr := newFakeCandidateRepo()
	cr.promoteErr = errors.New("stamp failed")
	pr := newFakeProposalRepo()
	pr.put(&persistence.WorkflowProposal{ID: "wpr_1", Status: persistence.WorkflowProposalStatusPending})
	seedPromoCandidate(t, cr, persistence.HealingCandidateTrialPassed, "wpr_1")
	ap := &fakeApplier{repo: pr}
	if _, err := newPromoterUnderTest(cr, pr, ap, nil).Promote(context.Background(), "whc_test", "op@x"); err == nil {
		t.Fatal("expected candidate-stamp failure to surface")
	}
	if ap.calls != 1 {
		t.Errorf("apply should have run once before the stamp failed; calls=%d", ap.calls)
	}
}

func TestReject_StampFailure(t *testing.T) {
	cr := &fakeCandidateRepo{rows: map[string]*persistence.HealingCandidate{}}
	seedPromoCandidate(t, cr, persistence.HealingCandidateTrialPassed, "wpr_1")
	cr.setErr = nil
	// Force Reject to fail by swapping the row out from under it is hard;
	// instead use the rejectErr path via a wrapper repo.
	wrap := &rejectErrRepo{fakeCandidateRepo: cr, err: errors.New("reject db error")}
	if _, err := NewPromoter(wrap, nil, nil, nil, zerolog.Nop()).Reject(context.Background(), "whc_test"); err == nil {
		t.Fatal("expected reject stamp error to surface")
	}
}

// rejectErrRepo forces Reject to error while delegating everything else.
type rejectErrRepo struct {
	*fakeCandidateRepo
	err error
}

func (r *rejectErrRepo) Reject(ctx context.Context, id string) error { return r.err }
