package service

// Pass-through coverage for the Self-Healing Workflow Genome service
// adapters (the LLD-flagged 0%-coverage delegation paths). The existing
// container_healing_candidates_test.go covers sentinel translation +
// nil guards; this file drives a REAL TrialRunner / Promoter (over
// minimal repo fakes) through the adapters to exercise the happy-path
// projection + delegation.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/workflowhealing"
)

// ---- minimal healing-repo fakes ---------------------------------------

type fakeHealCandRepo struct {
	byID     map[string]*persistence.HealingCandidate
	statuses []persistence.HealingCandidateStatus
	rejected []string
}

func (f *fakeHealCandRepo) Insert(_ context.Context, c *persistence.HealingCandidate) error {
	if f.byID == nil {
		f.byID = map[string]*persistence.HealingCandidate{}
	}
	f.byID[c.ID] = c
	return nil
}
func (f *fakeHealCandRepo) Get(_ context.Context, id string) (*persistence.HealingCandidate, error) {
	if c, ok := f.byID[id]; ok {
		return c, nil
	}
	return nil, persistence.ErrNotFound
}
func (f *fakeHealCandRepo) List(_ context.Context, _ persistence.HealingCandidateListFilter) ([]*persistence.HealingCandidate, error) {
	return nil, nil
}
func (f *fakeHealCandRepo) SetStatus(_ context.Context, id string, status persistence.HealingCandidateStatus) error {
	f.statuses = append(f.statuses, status)
	if c, ok := f.byID[id]; ok {
		c.Status = status
	}
	return nil
}
func (f *fakeHealCandRepo) BeginTrial(_ context.Context, id string) (bool, error) {
	c, ok := f.byID[id]
	if !ok {
		return false, nil
	}
	if c.Status.IsTerminal() || c.Status == persistence.HealingCandidateTrialRunning {
		return false, nil
	}
	c.Status = persistence.HealingCandidateTrialRunning
	f.statuses = append(f.statuses, persistence.HealingCandidateTrialRunning)
	return true, nil
}
func (f *fakeHealCandRepo) Promote(_ context.Context, id, _ string) error {
	if c, ok := f.byID[id]; ok {
		c.Status = persistence.HealingCandidatePromoted
	}
	return nil
}
func (f *fakeHealCandRepo) Reject(_ context.Context, id string) error {
	f.rejected = append(f.rejected, id)
	if c, ok := f.byID[id]; ok {
		c.Status = persistence.HealingCandidateRejected
	}
	return nil
}

type fakeHealTrialRepo struct {
	inserted int
	finished int
}

func (f *fakeHealTrialRepo) Insert(_ context.Context, _ *persistence.HealingTrial) error {
	f.inserted++
	return nil
}
func (f *fakeHealTrialRepo) Get(_ context.Context, _ string) (*persistence.HealingTrial, error) {
	return nil, persistence.ErrNotFound
}
func (f *fakeHealTrialRepo) ListByCandidate(_ context.Context, _ string) ([]*persistence.HealingTrial, error) {
	return nil, nil
}
func (f *fakeHealTrialRepo) Finish(_ context.Context, _ string, _ persistence.HealingTrialVerdict, _, _, _ string) error {
	f.finished++
	return nil
}

func seedDraftCandidate(repo *fakeHealCandRepo, id, diff string) {
	_ = repo.Insert(context.Background(), &persistence.HealingCandidate{
		ID:           id,
		WorkflowID:   "wf",
		ProjectID:    "proj",
		TriggerID:    "trg",
		ProposalID:   "wpr_1",
		ProposalDiff: diff,
		RiskLevel:    persistence.HealingRiskLow,
		Status:       persistence.HealingCandidateDraft,
	})
}

// ---- trial runner adapter projection ----------------------------------

// A static trial on an UNPARSEABLE genome returns a FAILED verdict (NOT
// an error), so RunTrial returns (res, nil) — exactly the happy path the
// adapter projection must handle: map Mode/Verdict + marshal Scorecard.
func TestHealingTrialRunnerAdapter_ProjectsResult(t *testing.T) {
	cands := &fakeHealCandRepo{}
	seedDraftCandidate(cands, "cand1", "this is not a valid workflow markdown")
	runner := workflowhealing.NewTrialRunner(cands, &fakeHealTrialRepo{}, nil, workflowhealing.GateThresholds{}, 0, zerolog.Nop())

	adapter := newHealingTrialRunnerAdapter(runner)
	if adapter == nil {
		t.Fatal("adapter nil for non-nil runner")
	}
	out, err := adapter.RunTrial(context.Background(), "cand1", "static", nil)
	if err != nil {
		t.Fatalf("RunTrial: %v", err)
	}
	if out.Mode != "static" {
		t.Errorf("Mode = %q, want static", out.Mode)
	}
	if out.Verdict != string(persistence.HealingTrialFailed) {
		t.Errorf("Verdict = %q, want failed (unparseable genome)", out.Verdict)
	}
	// Scorecard must be marshaled into ScorecardJSON.
	if out.ScorecardJSON == "" {
		t.Fatal("ScorecardJSON empty; projection did not marshal the scorecard")
	}
	var sc map[string]any
	if err := json.Unmarshal([]byte(out.ScorecardJSON), &sc); err != nil {
		t.Errorf("ScorecardJSON not valid JSON: %v", err)
	}
}

// The UI adapter drops the result and returns only the error — a
// successful (FAILED-verdict) RunTrial yields nil.
func TestHealingTrialRunnerUIAdapter_PassesThroughNilError(t *testing.T) {
	cands := &fakeHealCandRepo{}
	seedDraftCandidate(cands, "cand1", "not a workflow")
	runner := workflowhealing.NewTrialRunner(cands, &fakeHealTrialRepo{}, nil, workflowhealing.GateThresholds{}, 0, zerolog.Nop())

	adapter := newHealingTrialRunnerUIAdapter(runner)
	if err := adapter.RunTrial(context.Background(), "cand1", "static", nil); err != nil {
		t.Errorf("UI RunTrial returned error on a successful trial: %v", err)
	}
}

// ---- promoter adapter delegation --------------------------------------

func TestHealingPromoterAdapters_RejectPassThrough(t *testing.T) {
	cands := &fakeHealCandRepo{}
	seedDraftCandidate(cands, "cand1", "x")
	promoter := workflowhealing.NewPromoter(cands, nil, nil, nil, zerolog.Nop())

	// api adapter
	apiAdapter := newHealingPromoterAdapter(promoter)
	cand, err := apiAdapter.Reject(context.Background(), "cand1")
	if err != nil {
		t.Fatalf("api Reject: %v", err)
	}
	if cand == nil || cand.Status != persistence.HealingCandidateRejected {
		t.Errorf("api Reject status = %v, want rejected", cand)
	}

	// ui adapter (re-seed: previous reject made cand1 terminal)
	seedDraftCandidate(cands, "cand2", "x")
	uiAdapter := newHealingPromoterUIAdapter(promoter)
	cand2, err := uiAdapter.Reject(context.Background(), "cand2")
	if err != nil {
		t.Fatalf("ui Reject: %v", err)
	}
	if cand2 == nil || cand2.Status != persistence.HealingCandidateRejected {
		t.Errorf("ui Reject status = %v, want rejected", cand2)
	}
}
