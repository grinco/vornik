package api

// Tests for the Self-Healing Workflow Genome v1 candidate-persistence
// wiring on the generate-candidate endpoint. The handler stamps the
// trigger as it always has AND (when a candidate repo is wired)
// persists a workflow_healing_candidates row linked to the architect's
// WorkflowProposal. Candidate persistence is best-effort: a nil repo
// or an insert error must never lose the proposal or the trigger stamp.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// apiStubHealingCandidateRepo is the api-package in-memory candidate
// repo stub. Records inserts so the test can assert the link to the
// proposal; insertErr forces the best-effort failure path.
type apiStubHealingCandidateRepo struct {
	mu        sync.Mutex
	rows      map[string]*persistence.HealingCandidate
	inserted  []*persistence.HealingCandidate
	insertErr error
}

func newAPIStubHealingCandidateRepo() *apiStubHealingCandidateRepo {
	return &apiStubHealingCandidateRepo{rows: map[string]*persistence.HealingCandidate{}}
}

func (s *apiStubHealingCandidateRepo) Insert(_ context.Context, c *persistence.HealingCandidate) error {
	if s.insertErr != nil {
		return s.insertErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.ID == "" {
		c.ID = persistence.GenerateID("whc")
	}
	cp := *c
	s.rows[c.ID] = &cp
	s.inserted = append(s.inserted, &cp)
	return nil
}

func (s *apiStubHealingCandidateRepo) Get(_ context.Context, id string) (*persistence.HealingCandidate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.rows[id]
	if !ok {
		return nil, persistence.ErrNotFound
	}
	return c, nil
}

func (s *apiStubHealingCandidateRepo) List(_ context.Context, _ persistence.HealingCandidateListFilter) ([]*persistence.HealingCandidate, error) {
	return nil, nil
}

func (s *apiStubHealingCandidateRepo) SetStatus(context.Context, string, persistence.HealingCandidateStatus) error {
	return nil
}
func (s *apiStubHealingCandidateRepo) BeginTrial(context.Context, string) (bool, error) {
	return true, nil
}
func (s *apiStubHealingCandidateRepo) Promote(context.Context, string, string) error { return nil }
func (s *apiStubHealingCandidateRepo) Reject(context.Context, string) error          { return nil }

// A WORKFLOW.md the genome hasher can parse so the candidate carries a
// non-empty candidate_genome_hash.
const persistTestWorkflowMD = `---
workflowId: "wf-a"
displayName: "WF A"
description: "Test workflow."
version: "1.0"
entrypoint: "plan"
steps:
  plan:
    type: "agent"
    role: "lead"
    on_success: "complete"
    prompt: "Plan."
terminals:
  complete:
    status: "success"
---

# WF A
`

func TestGenerateCandidate_PersistsCandidateLinkedToProposal(t *testing.T) {
	repo := newAPIStubHealingTriggerRepo()
	_ = repo.Insert(context.Background(), apiOpenTrigger("t-1"))
	candRepo := newAPIStubHealingCandidateRepo()
	arch := &stubArchitect{
		result: &persistence.WorkflowProposal{
			ID:           "wpr-7",
			WorkflowID:   "wf-a",
			ProposalYAML: persistTestWorkflowMD,
			Motivation:   "remove the retry loop",
		},
	}
	opts := append(adminAuthOpts(),
		WithHealingTriggerRepository(repo),
		WithHealingCandidateRepository(candRepo),
		WithWorkflowArchitect(arch),
	)
	s := NewServer(opts...)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/admin/workflow-healing/triggers/t-1/generate-candidate", nil)
	req = withAdminKeyContext(req, "sk-admin")
	rec := httptest.NewRecorder()
	s.AdminHealingTriggerGenerateCandidate(rec, req, "t-1")

	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d, body=%s", rec.Code, rec.Body.String())
	}
	if len(candRepo.inserted) != 1 {
		t.Fatalf("expected 1 candidate persisted, got %d", len(candRepo.inserted))
	}
	c := candRepo.inserted[0]
	if c.ProposalID != "wpr-7" {
		t.Errorf("candidate.ProposalID = %q, want wpr-7 (must link to the proposal)", c.ProposalID)
	}
	if c.TriggerID != "t-1" {
		t.Errorf("candidate.TriggerID = %q, want t-1", c.TriggerID)
	}
	if c.WorkflowID != "wf-a" || c.ProjectID != "proj-x" {
		t.Errorf("candidate scope = (%q,%q), want (proj-x, wf-a)", c.ProjectID, c.WorkflowID)
	}
	if c.CandidateClass != persistence.HealingCandidateArchitect {
		t.Errorf("candidate.CandidateClass = %q, want architect", c.CandidateClass)
	}
	if c.Motivation != "remove the retry loop" {
		t.Errorf("candidate.Motivation = %q, want denormalised proposal motivation", c.Motivation)
	}
	if c.ProposalDiff != persistTestWorkflowMD {
		t.Error("candidate.ProposalDiff should be the denormalised proposal YAML")
	}
	if c.CandidateGenomeHash == "" {
		t.Error("candidate genome hash should be derived from the proposal YAML")
	}
	if c.Status != persistence.HealingCandidateDraft {
		t.Errorf("candidate.Status = %q, want draft", c.Status)
	}
}

func TestGenerateCandidate_NilCandidateRepoStillStampsTrigger(t *testing.T) {
	repo := newAPIStubHealingTriggerRepo()
	_ = repo.Insert(context.Background(), apiOpenTrigger("t-1"))
	arch := &stubArchitect{
		result: &persistence.WorkflowProposal{ID: "wpr-7", WorkflowID: "wf-a", ProposalYAML: persistTestWorkflowMD},
	}
	// No WithHealingCandidateRepository — candidate repo stays nil.
	opts := append(adminAuthOpts(),
		WithHealingTriggerRepository(repo),
		WithWorkflowArchitect(arch),
	)
	s := NewServer(opts...)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/admin/workflow-healing/triggers/t-1/generate-candidate", nil)
	req = withAdminKeyContext(req, "sk-admin")
	rec := httptest.NewRecorder()
	s.AdminHealingTriggerGenerateCandidate(rec, req, "t-1")

	// Pre-genome behaviour preserved: trigger stamped, 200, no panic.
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d, body=%s", rec.Code, rec.Body.String())
	}
	if repo.lastMarkGen.proposalID != "wpr-7" {
		t.Errorf("trigger should still be stamped with the proposal id; got %+v", repo.lastMarkGen)
	}
}

func TestGenerateCandidate_CandidatePersistFailureDoesNotFailRequest(t *testing.T) {
	repo := newAPIStubHealingTriggerRepo()
	_ = repo.Insert(context.Background(), apiOpenTrigger("t-1"))
	candRepo := newAPIStubHealingCandidateRepo()
	candRepo.insertErr = errors.New("db down")
	arch := &stubArchitect{
		result: &persistence.WorkflowProposal{ID: "wpr-7", WorkflowID: "wf-a", ProposalYAML: persistTestWorkflowMD},
	}
	opts := append(adminAuthOpts(),
		WithHealingTriggerRepository(repo),
		WithHealingCandidateRepository(candRepo),
		WithWorkflowArchitect(arch),
	)
	s := NewServer(opts...)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/admin/workflow-healing/triggers/t-1/generate-candidate", nil)
	req = withAdminKeyContext(req, "sk-admin")
	rec := httptest.NewRecorder()
	s.AdminHealingTriggerGenerateCandidate(rec, req, "t-1")

	// The proposal + trigger stamp are durable; a candidate-insert
	// failure must NOT surface as an error to the operator.
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d, want 200 (candidate persist is best-effort), body=%s", rec.Code, rec.Body.String())
	}
	if repo.lastMarkGen.proposalID != "wpr-7" {
		t.Error("trigger should still be stamped even when candidate persist fails")
	}
}
