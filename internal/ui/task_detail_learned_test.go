package ui

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/persistence"
)

// uiStubInstinctRepo is a minimal InstinctRepository for the failed-task
// overlay test. Only List carries behaviour; the rest are no-ops.
type uiStubInstinctRepo struct {
	rows []*persistence.Instinct
}

func (s *uiStubInstinctRepo) Upsert(context.Context, *persistence.Instinct) (string, error) {
	return "", nil
}
func (s *uiStubInstinctRepo) AddEvidence(context.Context, *persistence.InstinctEvidence) (bool, error) {
	return false, nil
}

func (s *uiStubInstinctRepo) RecordActionVersion(context.Context, *persistence.InstinctActionVersion) error {
	return nil
}

func (s *uiStubInstinctRepo) ListActionHistory(context.Context, string, int) ([]*persistence.InstinctActionVersion, error) {
	return nil, nil
}
func (s *uiStubInstinctRepo) RecomputeConfidence(context.Context, string, persistence.InstinctScorer) error {
	return nil
}
func (s *uiStubInstinctRepo) Get(context.Context, string) (*persistence.Instinct, error) {
	return nil, nil
}
func (s *uiStubInstinctRepo) List(_ context.Context, filter persistence.InstinctFilter) ([]*persistence.Instinct, error) {
	var out []*persistence.Instinct
	for _, r := range s.rows {
		if filter.Status != nil && r.Status != *filter.Status {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}
func (s *uiStubInstinctRepo) CountActiveProjects(context.Context, string) (int, error) {
	return 0, nil
}
func (s *uiStubInstinctRepo) CountByDomainStatus(context.Context) ([]persistence.InstinctDomainStatusCount, error) {
	return nil, nil
}
func (s *uiStubInstinctRepo) Retire(context.Context, string) error { return nil }
func (s *uiStubInstinctRepo) RecordApplication(context.Context, *persistence.InstinctApplication) error {
	return nil
}
func (s *uiStubInstinctRepo) ListApplications(context.Context, string, int) ([]*persistence.InstinctApplication, error) {
	return nil, nil
}
func (s *uiStubInstinctRepo) ListPendingRecoveryApplications(context.Context, int) ([]*persistence.InstinctApplication, error) {
	return nil, nil
}
func (s *uiStubInstinctRepo) ResolveApplication(context.Context, string, string) error {
	return nil
}
func (s *uiStubInstinctRepo) ListApplicationCounts(context.Context, []string) (map[string]*persistence.InstinctApplicationCounts, error) {
	return nil, nil
}

var _ persistence.InstinctRepository = (*uiStubInstinctRepo)(nil)

// uiStubOutcomeRepo embeds the interface and overrides only List.
type uiStubOutcomeRepo struct {
	persistence.ExecutionStepOutcomeRepository
	rows []*persistence.ExecutionStepOutcome
}

func (s *uiStubOutcomeRepo) List(context.Context, persistence.ExecutionStepOutcomeFilter) ([]*persistence.ExecutionStepOutcome, error) {
	return s.rows, nil
}

func uiTrigger(t *testing.T, role, class string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(map[string]string{"role": role, "error_class": class})
	if err != nil {
		t.Fatalf("marshal trigger: %v", err)
	}
	return b
}

func uiRecoveryInstinct(t *testing.T, id, role, class string, conf float64) *persistence.Instinct {
	t.Helper()
	return &persistence.Instinct{
		ID:           id,
		ProjectID:    "proj",
		Domain:       persistence.InstinctDomainRecovery,
		Action:       "retrying resolved the " + class + " failure",
		Status:       persistence.InstinctStatusActive,
		Confidence:   conf,
		SupportCount: 4,
		Trigger:      uiTrigger(t, role, class),
	}
}

// TestLearnedRemediationsForTask_GateOff is the byte-for-byte invariant
// on the UI surface: with instinctPlaybooks off the overlay is nil even
// when a matching instinct + outcome row exist.
func TestLearnedRemediationsForTask_GateOff(t *testing.T) {
	s := &Server{
		instinctRepo:      &uiStubInstinctRepo{rows: []*persistence.Instinct{uiRecoveryInstinct(t, "i1", "scout", "Timeout", 0.9)}},
		instinctPlaybooks: false,
		outcomeRepo:       &uiStubOutcomeRepo{rows: []*persistence.ExecutionStepOutcome{{ErrorClass: "Timeout", Role: "scout"}}},
		logger:            zerolog.Nop(),
	}
	got := s.learnedRemediationsForTask(context.Background(), &persistence.Task{ID: "t1", ProjectID: "proj"})
	if got != nil {
		t.Fatalf("gate off: overlay must be nil, got %+v", got)
	}
}

// TestLearnedRemediationsForTask_GateOn collects the distinct error
// classes from the task's step outcomes and merges learned remediations,
// de-duplicated by instinct ID and confidence-ordered.
func TestLearnedRemediationsForTask_GateOn(t *testing.T) {
	s := &Server{
		instinctRepo: &uiStubInstinctRepo{rows: []*persistence.Instinct{
			uiRecoveryInstinct(t, "i-timeout", "scout", "Timeout", 0.7),
			uiRecoveryInstinct(t, "i-parse", "lead", "ParseError", 0.95),
			uiRecoveryInstinct(t, "i-other", "scout", "Refused", 0.99), // not present in outcomes
		}},
		instinctPlaybooks: true,
		outcomeRepo: &uiStubOutcomeRepo{rows: []*persistence.ExecutionStepOutcome{
			{ErrorClass: "Timeout", Role: "scout"},
			{ErrorClass: "ParseError", Role: "lead"},
			{ErrorClass: "Timeout", Role: "scout"}, // duplicate class+role
			{ErrorClass: "", Role: "scout"},        // no class — ignored
		}},
		logger: zerolog.Nop(),
	}
	got := s.learnedRemediationsForTask(context.Background(), &persistence.Task{ID: "t1", ProjectID: "proj"})
	if len(got) != 2 {
		t.Fatalf("expected 2 remediations (Timeout + ParseError), got %d: %+v", len(got), got)
	}
	// Highest confidence first: ParseError (0.95) before Timeout (0.7).
	if got[0].InstinctID != "i-parse" || got[1].InstinctID != "i-timeout" {
		t.Errorf("unexpected order: %s, %s", got[0].InstinctID, got[1].InstinctID)
	}
}

// uiFailingOutcomeRepo errors from List so the overlay's fail-soft path
// is covered.
type uiFailingOutcomeRepo struct {
	persistence.ExecutionStepOutcomeRepository
}

func (uiFailingOutcomeRepo) List(context.Context, persistence.ExecutionStepOutcomeFilter) ([]*persistence.ExecutionStepOutcome, error) {
	return nil, context.DeadlineExceeded
}

// uiErrInstinctRepo errors from List so the per-class lookup-error skip
// path is covered.
type uiErrInstinctRepo struct {
	*uiStubInstinctRepo
}

func (uiErrInstinctRepo) List(context.Context, persistence.InstinctFilter) ([]*persistence.Instinct, error) {
	return nil, context.Canceled
}

// TestLearnedRemediationsForTask_OutcomeErrorFailSoft confirms a step-
// outcome lookup error yields nil (panel skipped) instead of erroring.
func TestLearnedRemediationsForTask_OutcomeErrorFailSoft(t *testing.T) {
	s := &Server{
		instinctRepo:      &uiStubInstinctRepo{},
		instinctPlaybooks: true,
		outcomeRepo:       uiFailingOutcomeRepo{},
		logger:            zerolog.Nop(),
	}
	if got := s.learnedRemediationsForTask(context.Background(), &persistence.Task{ID: "t1", ProjectID: "proj"}); got != nil {
		t.Fatalf("outcome error must yield nil overlay (fail-soft)")
	}
}

// TestLearnedRemediationsForTask_InstinctErrorSkipsClass confirms a
// per-class instinct lookup error is skipped (logged), not propagated.
func TestLearnedRemediationsForTask_InstinctErrorSkipsClass(t *testing.T) {
	s := &Server{
		instinctRepo:      uiErrInstinctRepo{&uiStubInstinctRepo{}},
		instinctPlaybooks: true,
		outcomeRepo:       &uiStubOutcomeRepo{rows: []*persistence.ExecutionStepOutcome{{ErrorClass: "Timeout", Role: "scout"}}},
		logger:            zerolog.Nop(),
	}
	if got := s.learnedRemediationsForTask(context.Background(), &persistence.Task{ID: "t1", ProjectID: "proj"}); got != nil {
		t.Fatalf("instinct lookup error must skip the class, yielding nil overlay")
	}
}

// TestLearnedRemediationsForTask_EqualConfidenceTiebreak exercises the
// instinct-ID tiebreak in sortLearnedRemediations for equal-confidence
// rows so the order is deterministic.
func TestLearnedRemediationsForTask_EqualConfidenceTiebreak(t *testing.T) {
	r1 := uiRecoveryInstinct(t, "zeta", "scout", "Timeout", 0.8)
	r2 := uiRecoveryInstinct(t, "alpha", "lead", "ParseError", 0.8) // same confidence
	s := &Server{
		instinctRepo:      &uiStubInstinctRepo{rows: []*persistence.Instinct{r1, r2}},
		instinctPlaybooks: true,
		outcomeRepo: &uiStubOutcomeRepo{rows: []*persistence.ExecutionStepOutcome{
			{ErrorClass: "Timeout", Role: "scout"},
			{ErrorClass: "ParseError", Role: "lead"},
		}},
		logger: zerolog.Nop(),
	}
	got := s.learnedRemediationsForTask(context.Background(), &persistence.Task{ID: "t1", ProjectID: "proj"})
	if len(got) != 2 {
		t.Fatalf("expected 2 remediations, got %d", len(got))
	}
	// Equal confidence → ascending instinct ID: "alpha" before "zeta".
	if got[0].InstinctID != "alpha" || got[1].InstinctID != "zeta" {
		t.Errorf("equal-confidence tiebreak failed: %s, %s", got[0].InstinctID, got[1].InstinctID)
	}
}

// TestLearnedRemediationsForTask_NilDepsNoOverlay confirms missing repos
// degrade to nil cleanly (no panic), even with the gate on.
func TestLearnedRemediationsForTask_NilDepsNoOverlay(t *testing.T) {
	// nil instinct repo
	s1 := &Server{instinctPlaybooks: true, outcomeRepo: &uiStubOutcomeRepo{}, logger: zerolog.Nop()}
	if got := s1.learnedRemediationsForTask(context.Background(), &persistence.Task{ID: "t", ProjectID: "p"}); got != nil {
		t.Fatalf("nil instinct repo must yield nil overlay")
	}
	// nil outcome repo
	s2 := &Server{instinctPlaybooks: true, instinctRepo: &uiStubInstinctRepo{}, logger: zerolog.Nop()}
	if got := s2.learnedRemediationsForTask(context.Background(), &persistence.Task{ID: "t", ProjectID: "p"}); got != nil {
		t.Fatalf("nil outcome repo must yield nil overlay")
	}
	// empty project
	s3 := &Server{instinctPlaybooks: true, instinctRepo: &uiStubInstinctRepo{}, outcomeRepo: &uiStubOutcomeRepo{}, logger: zerolog.Nop()}
	if got := s3.learnedRemediationsForTask(context.Background(), &persistence.Task{ID: "t", ProjectID: ""}); got != nil {
		t.Fatalf("empty project must yield nil overlay")
	}
}
