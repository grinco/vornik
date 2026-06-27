package executor

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/observability"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/playbook"
)

// stubInstinctRepo is a minimal persistence.InstinctRepository for the
// Consumer-A overlay tests. Only List + RecordApplication carry
// behaviour; the rest satisfy the interface as no-ops.
type stubInstinctRepo struct {
	rows         []*persistence.Instinct
	applications []*persistence.InstinctApplication
}

func (s *stubInstinctRepo) Upsert(context.Context, *persistence.Instinct) (string, error) {
	return "", nil
}
func (s *stubInstinctRepo) AddEvidence(context.Context, *persistence.InstinctEvidence) (bool, error) {
	return false, nil
}

func (s *stubInstinctRepo) RecordActionVersion(context.Context, *persistence.InstinctActionVersion) error {
	return nil
}

func (s *stubInstinctRepo) ListActionHistory(context.Context, string, int) ([]*persistence.InstinctActionVersion, error) {
	return nil, nil
}
func (s *stubInstinctRepo) RecomputeConfidence(context.Context, string, persistence.InstinctScorer) error {
	return nil
}
func (s *stubInstinctRepo) Get(context.Context, string) (*persistence.Instinct, error) {
	return nil, nil
}
func (s *stubInstinctRepo) List(_ context.Context, filter persistence.InstinctFilter) ([]*persistence.Instinct, error) {
	// Return only rows matching the requested status so the overlay's
	// active/promoted merge behaves like the real backend.
	var out []*persistence.Instinct
	for _, r := range s.rows {
		if filter.Status != nil && r.Status != *filter.Status {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}
func (s *stubInstinctRepo) CountActiveProjects(context.Context, string) (int, error) {
	return 0, nil
}
func (s *stubInstinctRepo) CountByDomainStatus(context.Context) ([]persistence.InstinctDomainStatusCount, error) {
	return nil, nil
}
func (s *stubInstinctRepo) Retire(context.Context, string) error { return nil }
func (s *stubInstinctRepo) RecordApplication(_ context.Context, app *persistence.InstinctApplication) error {
	s.applications = append(s.applications, app)
	return nil
}
func (s *stubInstinctRepo) ListApplications(context.Context, string, int) ([]*persistence.InstinctApplication, error) {
	return nil, nil
}
func (s *stubInstinctRepo) ListPendingRecoveryApplications(context.Context, int) ([]*persistence.InstinctApplication, error) {
	return nil, nil
}
func (s *stubInstinctRepo) ResolveApplication(context.Context, string, string) error {
	return nil
}
func (s *stubInstinctRepo) ListApplicationCounts(context.Context, []string) (map[string]*persistence.InstinctApplicationCounts, error) {
	return nil, nil
}

var _ persistence.InstinctRepository = (*stubInstinctRepo)(nil)

// stubOutcomeRepo returns canned step-outcome rows for List; the rest of
// the ExecutionStepOutcomeRepository surface is unused here, so we embed
// the interface (nil) and override only List.
type stubOutcomeRepo struct {
	persistence.ExecutionStepOutcomeRepository
	rows []*persistence.ExecutionStepOutcome
}

func (s *stubOutcomeRepo) List(_ context.Context, _ persistence.ExecutionStepOutcomeFilter) ([]*persistence.ExecutionStepOutcome, error) {
	return s.rows, nil
}

func mustTriggerJSON(t *testing.T, role, class string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(map[string]string{"role": role, "error_class": class})
	if err != nil {
		t.Fatalf("marshal trigger: %v", err)
	}
	return b
}

func activeRecoveryInstinct(t *testing.T, id, role, class, action string, conf float64) *persistence.Instinct {
	t.Helper()
	return &persistence.Instinct{
		ID:           id,
		Scope:        persistence.InstinctScopeProject,
		ProjectID:    "proj",
		Domain:       persistence.InstinctDomainRecovery,
		Action:       action,
		Status:       persistence.InstinctStatusActive,
		Confidence:   conf,
		SupportCount: 4,
		Trigger:      mustTriggerJSON(t, role, class),
	}
}

// TestAttachLearnedRemediations_GateOff is the byte-for-byte invariant:
// with the failure_playbooks gate off, the recovery context is untouched
// and the rendered block is identical to the no-overlay baseline, even
// when a wired instinct repo holds a matching instinct.
func TestAttachLearnedRemediations_GateOff(t *testing.T) {
	repo := &stubInstinctRepo{rows: []*persistence.Instinct{
		activeRecoveryInstinct(t, "i1", "scout", "Timeout", "retrying resolved the Timeout failure", 0.9),
	}}
	e := &Executor{
		instinctRepo:      repo,
		instinctPlaybooks: false, // gate OFF
		outcomeRepo:       &stubOutcomeRepo{rows: []*persistence.ExecutionStepOutcome{{ErrorClass: "Timeout", Role: "scout"}}},
		logger:            zerolog.Nop(),
	}
	rc := &RecoveryContext{FailedStep: "research", FailureClass: "agent_error"}
	task := &persistence.Task{ID: "t1", ProjectID: "proj"}
	exec := &persistence.Execution{ID: "exec1"}

	e.attachLearnedRemediations(context.Background(), task, exec, rc)

	if rc.LearnedRemediations != nil {
		t.Fatalf("gate off: LearnedRemediations must stay nil, got %+v", rc.LearnedRemediations)
	}
	if len(repo.applications) != 0 {
		t.Fatalf("gate off: no application rows should be recorded, got %d", len(repo.applications))
	}
	// The rendered block must match the no-overlay baseline exactly.
	withOverlayAttempt := buildRecoveryContextBlock(rc)
	baseline := buildRecoveryContextBlock(&RecoveryContext{FailedStep: "research", FailureClass: "agent_error"})
	if withOverlayAttempt != baseline {
		t.Fatalf("gate off must be byte-for-byte identical.\n got: %q\nwant: %q", withOverlayAttempt, baseline)
	}
}

// TestAttachLearnedRemediations_GateOn populates the overlay when the
// gate is on, a matching instinct exists, and records one lead_recovery
// application per surfaced instinct.
func TestAttachLearnedRemediations_GateOn(t *testing.T) {
	repo := &stubInstinctRepo{rows: []*persistence.Instinct{
		activeRecoveryInstinct(t, "i1", "scout", "Timeout", "retrying resolved the Timeout failure", 0.9),
		activeRecoveryInstinct(t, "i2", "scout", "ParseError", "other class", 0.95),
	}}
	e := &Executor{
		instinctRepo:      repo,
		instinctPlaybooks: true, // gate ON
		outcomeRepo:       &stubOutcomeRepo{rows: []*persistence.ExecutionStepOutcome{{ErrorClass: "Timeout", Role: "scout"}}},
		logger:            zerolog.Nop(),
	}
	rc := &RecoveryContext{FailedStep: "research", FailureClass: "agent_error"}
	task := &persistence.Task{ID: "t1", ProjectID: "proj"}
	exec := &persistence.Execution{ID: "exec1"}

	e.attachLearnedRemediations(context.Background(), task, exec, rc)

	if len(rc.LearnedRemediations) != 1 {
		t.Fatalf("expected 1 matching remediation (Timeout only), got %d: %+v", len(rc.LearnedRemediations), rc.LearnedRemediations)
	}
	if rc.LearnedRemediations[0].InstinctID != "i1" {
		t.Errorf("expected instinct i1, got %s", rc.LearnedRemediations[0].InstinctID)
	}
	if len(repo.applications) != 1 {
		t.Fatalf("expected 1 lead_recovery application row, got %d", len(repo.applications))
	}
	app := repo.applications[0]
	if app.Surface != persistence.InstinctSurfaceLeadRecovery {
		t.Errorf("application surface = %q, want %q", app.Surface, persistence.InstinctSurfaceLeadRecovery)
	}
	if app.Result != persistence.InstinctResultIgnored {
		t.Errorf("application result = %q, want %q (surfaced, not auto-applied)", app.Result, persistence.InstinctResultIgnored)
	}
	if app.TaskID != "t1" {
		t.Errorf("application task_id = %q, want t1", app.TaskID)
	}

	// And it now renders into the lead's recovery block.
	block := buildRecoveryContextBlock(rc)
	if !strings.Contains(block, "similar_failures_previously_resolved_here") {
		t.Errorf("recovery block missing learned overlay header; got:\n%s", block)
	}
	if !strings.Contains(block, "retrying resolved the Timeout failure") {
		t.Errorf("recovery block missing the learned action; got:\n%s", block)
	}
}

// TestAttachLearnedRemediations_NoErrorClassNoOverlay confirms that when
// the failed step's outcome row carries no error class, there is nothing
// to match on and the overlay stays empty (no spurious lookup result).
func TestAttachLearnedRemediations_NoErrorClassNoOverlay(t *testing.T) {
	repo := &stubInstinctRepo{rows: []*persistence.Instinct{
		activeRecoveryInstinct(t, "i1", "scout", "Timeout", "x", 0.9),
	}}
	e := &Executor{
		instinctRepo:      repo,
		instinctPlaybooks: true,
		outcomeRepo:       &stubOutcomeRepo{rows: []*persistence.ExecutionStepOutcome{{ErrorClass: "", Role: "scout"}}},
		logger:            zerolog.Nop(),
	}
	rc := &RecoveryContext{FailedStep: "research"}
	e.attachLearnedRemediations(context.Background(), &persistence.Task{ID: "t1", ProjectID: "proj"}, &persistence.Execution{ID: "exec1"}, rc)
	if rc.LearnedRemediations != nil {
		t.Fatalf("no error class: overlay must stay empty, got %+v", rc.LearnedRemediations)
	}
}

// TestAttachLearnedRemediations_NilRepoNoOverlay confirms a nil instinct
// repo (un-wired deployment) is a clean no-op even with the gate flag on.
func TestAttachLearnedRemediations_NilRepoNoOverlay(t *testing.T) {
	e := &Executor{
		instinctRepo:      nil,
		instinctPlaybooks: true,
		logger:            zerolog.Nop(),
	}
	rc := &RecoveryContext{FailedStep: "research", FailureClass: "agent_error"}
	e.attachLearnedRemediations(context.Background(), &persistence.Task{ID: "t1", ProjectID: "proj"}, &persistence.Execution{ID: "exec1"}, rc)
	if rc.LearnedRemediations != nil {
		t.Fatalf("nil repo: overlay must stay empty")
	}
}

// listErrOutcomeRepo returns an error from List so the overlay's
// fail-soft path (swallow + nil) is exercised.
type listErrOutcomeRepo struct {
	persistence.ExecutionStepOutcomeRepository
}

func (listErrOutcomeRepo) List(context.Context, persistence.ExecutionStepOutcomeFilter) ([]*persistence.ExecutionStepOutcome, error) {
	return nil, context.DeadlineExceeded
}

// recordErrInstinctRepo behaves like stubInstinctRepo but RecordApplication
// errors, so the non-fatal application-write log path is covered.
type recordErrInstinctRepo struct {
	stubInstinctRepo
}

func (r *recordErrInstinctRepo) RecordApplication(context.Context, *persistence.InstinctApplication) error {
	return context.Canceled
}

// TestAttachLearnedRemediations_GuardBranches covers the early-return
// guards: nil task, empty project, nil execution, empty failed step.
func TestAttachLearnedRemediations_GuardBranches(t *testing.T) {
	repo := &stubInstinctRepo{rows: []*persistence.Instinct{activeRecoveryInstinct(t, "i1", "scout", "Timeout", "x", 0.9)}}
	e := &Executor{instinctRepo: repo, instinctPlaybooks: true, outcomeRepo: &stubOutcomeRepo{}, logger: zerolog.Nop()}
	exec := &persistence.Execution{ID: "exec1"}
	goodTask := &persistence.Task{ID: "t1", ProjectID: "proj"}

	cases := []struct {
		name string
		task *persistence.Task
		exec *persistence.Execution
		rc   *RecoveryContext
	}{
		{"nil task", nil, exec, &RecoveryContext{FailedStep: "s"}},
		{"empty project", &persistence.Task{ID: "t", ProjectID: ""}, exec, &RecoveryContext{FailedStep: "s"}},
		{"nil exec", goodTask, nil, &RecoveryContext{FailedStep: "s"}},
		{"empty failed step", goodTask, exec, &RecoveryContext{FailedStep: ""}},
		{"nil rc", goodTask, exec, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Must not panic; rc (when present) must stay empty.
			e.attachLearnedRemediations(context.Background(), tc.task, tc.exec, tc.rc)
			if tc.rc != nil && tc.rc.LearnedRemediations != nil {
				t.Fatalf("guard %q: overlay must stay empty", tc.name)
			}
		})
	}
}

// TestAttachLearnedRemediations_OutcomeRepoErrorFailSoft confirms a
// step-outcome lookup error degrades to no overlay (never blocks recovery).
func TestAttachLearnedRemediations_OutcomeRepoErrorFailSoft(t *testing.T) {
	e := &Executor{
		instinctRepo:      &stubInstinctRepo{rows: []*persistence.Instinct{activeRecoveryInstinct(t, "i1", "scout", "Timeout", "x", 0.9)}},
		instinctPlaybooks: true,
		outcomeRepo:       listErrOutcomeRepo{},
		logger:            zerolog.Nop(),
	}
	rc := &RecoveryContext{FailedStep: "research"}
	e.attachLearnedRemediations(context.Background(), &persistence.Task{ID: "t1", ProjectID: "proj"}, &persistence.Execution{ID: "exec1"}, rc)
	if rc.LearnedRemediations != nil {
		t.Fatalf("outcome repo error: overlay must stay empty (fail-soft)")
	}
}

// TestAttachLearnedRemediations_ApplicationWriteErrorNonFatal confirms a
// RecordApplication failure does not blank the surfaced overlay.
func TestAttachLearnedRemediations_ApplicationWriteErrorNonFatal(t *testing.T) {
	repo := &recordErrInstinctRepo{stubInstinctRepo{rows: []*persistence.Instinct{
		activeRecoveryInstinct(t, "i1", "scout", "Timeout", "retrying resolved the Timeout failure", 0.9),
	}}}
	e := &Executor{
		instinctRepo:      repo,
		instinctPlaybooks: true,
		outcomeRepo:       &stubOutcomeRepo{rows: []*persistence.ExecutionStepOutcome{{ErrorClass: "Timeout", Role: "scout"}}},
		logger:            zerolog.Nop(),
	}
	rc := &RecoveryContext{FailedStep: "research"}
	e.attachLearnedRemediations(context.Background(), &persistence.Task{ID: "t1", ProjectID: "proj"}, &persistence.Execution{ID: "exec1"}, rc)
	if len(rc.LearnedRemediations) != 1 {
		t.Fatalf("application write error must not blank the overlay; got %d", len(rc.LearnedRemediations))
	}
}

// listErrInstinctRepo errors from List so the overlay's lookup-error
// fail-soft path (log + nil) is covered.
type listErrInstinctRepo struct {
	*stubInstinctRepo
}

func (listErrInstinctRepo) List(context.Context, persistence.InstinctFilter) ([]*persistence.Instinct, error) {
	return nil, context.Canceled
}

// TestAttachLearnedRemediations_InstinctListErrorFailSoft confirms a
// lookup error degrades to no overlay rather than aborting recovery.
func TestAttachLearnedRemediations_InstinctListErrorFailSoft(t *testing.T) {
	e := &Executor{
		instinctRepo:      listErrInstinctRepo{&stubInstinctRepo{}},
		instinctPlaybooks: true,
		outcomeRepo:       &stubOutcomeRepo{rows: []*persistence.ExecutionStepOutcome{{ErrorClass: "Timeout", Role: "scout"}}},
		logger:            zerolog.Nop(),
	}
	rc := &RecoveryContext{FailedStep: "research"}
	e.attachLearnedRemediations(context.Background(), &persistence.Task{ID: "t1", ProjectID: "proj"}, &persistence.Execution{ID: "exec1"}, rc)
	if rc.LearnedRemediations != nil {
		t.Fatalf("instinct list error: overlay must stay empty (fail-soft)")
	}
}

// TestAttachLearnedRemediations_NoMatchingInstinct covers the
// len(rems)==0 branch: the failed step has a class, but no instinct
// matches it, so the overlay stays empty and no application is recorded.
func TestAttachLearnedRemediations_NoMatchingInstinct(t *testing.T) {
	repo := &stubInstinctRepo{rows: []*persistence.Instinct{
		activeRecoveryInstinct(t, "i1", "scout", "ParseError", "x", 0.9), // different class
	}}
	e := &Executor{
		instinctRepo:      repo,
		instinctPlaybooks: true,
		outcomeRepo:       &stubOutcomeRepo{rows: []*persistence.ExecutionStepOutcome{{ErrorClass: "Timeout", Role: "scout"}}},
		logger:            zerolog.Nop(),
	}
	rc := &RecoveryContext{FailedStep: "research"}
	e.attachLearnedRemediations(context.Background(), &persistence.Task{ID: "t1", ProjectID: "proj"}, &persistence.Execution{ID: "exec1"}, rc)
	if rc.LearnedRemediations != nil {
		t.Fatalf("no matching instinct: overlay must stay empty")
	}
	if len(repo.applications) != 0 {
		t.Fatalf("no matching instinct: no application rows expected")
	}
}

// TestFailedStepErrorClass_Guards covers nil-repo / empty-id / empty-rows.
func TestFailedStepErrorClass_Guards(t *testing.T) {
	// nil outcome repo
	e1 := &Executor{logger: zerolog.Nop()}
	if c, r := e1.failedStepErrorClass(context.Background(), "e", "s"); c != "" || r != "" {
		t.Fatalf("nil repo must return empty")
	}
	// empty ids
	e2 := &Executor{outcomeRepo: &stubOutcomeRepo{}, logger: zerolog.Nop()}
	if c, _ := e2.failedStepErrorClass(context.Background(), "", "s"); c != "" {
		t.Fatalf("empty exec id must return empty")
	}
	// no rows
	e3 := &Executor{outcomeRepo: &stubOutcomeRepo{rows: nil}, logger: zerolog.Nop()}
	if c, _ := e3.failedStepErrorClass(context.Background(), "e", "s"); c != "" {
		t.Fatalf("no rows must return empty")
	}
}

// TestLearnedRemediationsBlock_EmptyRendersNothing pins that the render
// helper returns "" for no remediations so the gate-off prompt is clean.
func TestLearnedRemediationsBlock_EmptyRendersNothing(t *testing.T) {
	if got := learnedRemediationsBlock(nil); got != "" {
		t.Errorf("expected empty string for nil remediations, got %q", got)
	}
	if got := learnedRemediationsBlock([]playbook.LearnedRemediation{}); got != "" {
		t.Errorf("expected empty string for empty remediations, got %q", got)
	}
}

// TestAttachLearnedRemediations_records_execution_step_ids pins that the
// recorded lead_recovery application carries the execution + failed-step
// IDs, so the RecoveryResolver can later match it against the step outcome.
func TestAttachLearnedRemediations_records_execution_step_ids(t *testing.T) {
	repo := &stubInstinctRepo{rows: []*persistence.Instinct{
		activeRecoveryInstinct(t, "i1", "scout", "Timeout", "retrying resolved the Timeout failure", 0.9),
	}}
	e := &Executor{
		instinctRepo:      repo,
		instinctPlaybooks: true,
		outcomeRepo:       &stubOutcomeRepo{rows: []*persistence.ExecutionStepOutcome{{ErrorClass: "Timeout", Role: "scout"}}},
		logger:            zerolog.Nop(),
	}
	rc := &RecoveryContext{FailedStep: "research"}
	e.attachLearnedRemediations(context.Background(),
		&persistence.Task{ID: "t1", ProjectID: "proj"},
		&persistence.Execution{ID: "exec1"}, rc)

	if len(repo.applications) != 1 {
		t.Fatalf("expected 1 application row, got %d", len(repo.applications))
	}
	app := repo.applications[0]
	if app.ExecutionID != "exec1" {
		t.Errorf("application execution_id = %q, want exec1", app.ExecutionID)
	}
	if app.StepID != "research" {
		t.Errorf("application step_id = %q, want research (the failed step)", app.StepID)
	}
}

// TestSetInstinctMetrics_NilSafe confirms SetInstinctMetrics is a no-op on
// a nil executor and stores the sink on a live one.
func TestSetInstinctMetrics_NilSafe(t *testing.T) {
	var nilE *Executor
	nilE.SetInstinctMetrics(nil) // must not panic

	m := observability.NewInstinctMetrics(prometheus.NewRegistry())
	e := &Executor{}
	e.SetInstinctMetrics(m)
	if e.instinctMetrics != m {
		t.Fatalf("SetInstinctMetrics did not store the metrics sink")
	}
}

// TestAttachLearnedRemediations_MetricEmitted asserts the lead_recovery
// surfacing bumps vornik_instinct_applications_total{lead_recovery,ignored}
// once per surfaced instinct.
func TestAttachLearnedRemediations_MetricEmitted(t *testing.T) {
	repo := &stubInstinctRepo{rows: []*persistence.Instinct{
		activeRecoveryInstinct(t, "i1", "scout", "Timeout", "retrying resolved the Timeout failure", 0.9),
	}}
	m := observability.NewInstinctMetrics(prometheus.NewRegistry())
	e := &Executor{
		instinctRepo:      repo,
		instinctPlaybooks: true,
		instinctMetrics:   m,
		outcomeRepo:       &stubOutcomeRepo{rows: []*persistence.ExecutionStepOutcome{{ErrorClass: "Timeout", Role: "scout"}}},
		logger:            zerolog.Nop(),
	}
	rc := &RecoveryContext{FailedStep: "research"}
	e.attachLearnedRemediations(context.Background(),
		&persistence.Task{ID: "t1", ProjectID: "proj"},
		&persistence.Execution{ID: "exec1"}, rc)

	got := testutil.ToFloat64(m.ApplicationsTotal.WithLabelValues(
		persistence.InstinctSurfaceLeadRecovery, persistence.InstinctResultIgnored))
	if got != 1 {
		t.Fatalf("ApplicationsTotal{lead_recovery,ignored}=%v, want 1", got)
	}
}

// TestAttachLearnedRemediations_MetricNilSafe confirms a surfacing with no
// metrics sink wired does not panic and still records the application row.
func TestAttachLearnedRemediations_MetricNilSafe(t *testing.T) {
	repo := &stubInstinctRepo{rows: []*persistence.Instinct{
		activeRecoveryInstinct(t, "i1", "scout", "Timeout", "retrying resolved the Timeout failure", 0.9),
	}}
	e := &Executor{
		instinctRepo:      repo,
		instinctPlaybooks: true,
		instinctMetrics:   nil, // no sink
		outcomeRepo:       &stubOutcomeRepo{rows: []*persistence.ExecutionStepOutcome{{ErrorClass: "Timeout", Role: "scout"}}},
		logger:            zerolog.Nop(),
	}
	rc := &RecoveryContext{FailedStep: "research"}
	e.attachLearnedRemediations(context.Background(),
		&persistence.Task{ID: "t1", ProjectID: "proj"},
		&persistence.Execution{ID: "exec1"}, rc)
	if len(repo.applications) != 1 {
		t.Fatalf("nil metrics: application row must still be recorded, got %d", len(repo.applications))
	}
}
