package repotest

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// seedLiftInstinct creates a parent instincts row via the given
// InstinctRepository and returns its ID. instinct_lift.instinct_id is an
// FK to instincts(id), so every RunInstinctLiftSuite case needs a real
// parent row before it can upsert a snapshot.
func seedLiftInstinct(t *testing.T, instincts persistence.InstinctRepository) string {
	t.Helper()
	ctx := context.Background()
	id, err := instincts.Upsert(ctx, &persistence.Instinct{
		ProjectID: uniqueID("proj"), Domain: persistence.InstinctDomainRecovery,
		TriggerKey: uniqueID("tk"), Action: "a",
	})
	if err != nil {
		t.Fatalf("seedLiftInstinct: Upsert: %v", err)
	}
	return id
}

// seedGlobalLiftInstinct is seedLiftInstinct's global-scope sibling:
// ProjectID "" / Scope "global", used by
// testLiftArchitectComplementCrossProject to document that the
// instinct under test is the kind whose treatment set is expected to
// span multiple projects' workflows. Note the Architect* queries
// themselves never read Instinct.ProjectID or .Scope (they key
// entirely off workflow_proposals.instinct_ids / workflow_id / kind),
// so this only matters for making the fixture's intent legible — the
// SQL behaves identically either way.
func seedGlobalLiftInstinct(t *testing.T, instincts persistence.InstinctRepository) string {
	t.Helper()
	ctx := context.Background()
	id, err := instincts.Upsert(ctx, &persistence.Instinct{
		Scope: persistence.InstinctScopeGlobal, Domain: persistence.InstinctDomainRecovery,
		TriggerKey: uniqueID("tk"), Action: "a",
	})
	if err != nil {
		t.Fatalf("seedGlobalLiftInstinct: Upsert: %v", err)
	}
	return id
}

// assertLiftSnapshotEqual compares a fetched snapshot against the values
// upserted, with a 1e-9 epsilon on the two float fields and a 2s tolerance
// on ComputedAt (backends may round/truncate sub-second precision).
func assertLiftSnapshotEqual(t *testing.T, got, want *persistence.InstinctLiftSnapshot) {
	t.Helper()
	const epsilon = 1e-9
	if got.Domain != want.Domain {
		t.Errorf("Domain = %q, want %q", got.Domain, want.Domain)
	}
	if math.Abs(got.Lift-want.Lift) > epsilon {
		t.Errorf("Lift = %v, want %v", got.Lift, want.Lift)
	}
	if got.TreatmentN != want.TreatmentN || got.TreatmentSucc != want.TreatmentSucc {
		t.Errorf("treatment = (%d,%d), want (%d,%d)", got.TreatmentN, got.TreatmentSucc, want.TreatmentN, want.TreatmentSucc)
	}
	if got.BaselineN != want.BaselineN || got.BaselineSucc != want.BaselineSucc {
		t.Errorf("baseline = (%d,%d), want (%d,%d)", got.BaselineN, got.BaselineSucc, want.BaselineN, want.BaselineSucc)
	}
	if math.Abs(got.StdError-want.StdError) > epsilon {
		t.Errorf("StdError = %v, want %v", got.StdError, want.StdError)
	}
	if got.Verdict != want.Verdict {
		t.Errorf("Verdict = %q, want %q", got.Verdict, want.Verdict)
	}
	if diff := got.ComputedAt.Sub(want.ComputedAt); diff < -2*time.Second || diff > 2*time.Second {
		t.Errorf("ComputedAt = %v, want ~%v (within 2s)", got.ComputedAt, want.ComputedAt)
	}
}

// RunInstinctLiftSuite exercises persistence.InstinctLiftRepository's
// snapshot upsert/get round-trip and the Recovery*/Budget*/Architect*
// domain-query methods end to end. The repo passed in must connect to
// an empty instinct_lift table; instincts provides the parent
// InstinctRepository used to seed the FK'd instincts row each case
// needs (and to write instinct_applications rows); outcomes and tasks
// seed the execution_step_outcomes / tasks audit rows the
// Recovery/Budget complement queries join against; proposals seeds
// the workflow_proposals rows the Architect* queries join against.
//
// Architect cases are Postgres-only: they probe proposals.Insert up
// front (see architectSupported) and t.Skip when it returns
// persistence.ErrNotFound, which is exactly what the SQLite
// WorkflowProposalRepository stub always returns (SQLite has no
// architect surface — see sqlite/workflow_proposal_repository.go).
// Probing the actual method under test is the cleanest backend
// detection available here: unlike the other Postgres-only contract
// suites in coverage_gap_suites.go (which are separate RunXSuite
// functions wired only into the Postgres test entrypoint), the
// Architect cases live inside this single shared entrypoint that both
// backends already call with the same signature, so a runtime gate
// is simpler than forking the call site.
func RunInstinctLiftSuite(t *testing.T, repo persistence.InstinctLiftRepository, instincts persistence.InstinctRepository, outcomes persistence.ExecutionStepOutcomeRepository, tasks persistence.TaskRepository, proposals persistence.WorkflowProposalRepository) {
	t.Helper()
	t.Run("UpsertThenGet", func(t *testing.T) { testLiftUpsertThenGet(t, repo, instincts) })
	t.Run("UpsertReplaces", func(t *testing.T) { testLiftUpsertReplaces(t, repo, instincts) })
	t.Run("GetEmptyInput", func(t *testing.T) { testLiftGetEmptyInput(t, repo) })
	t.Run("GetMissing", func(t *testing.T) { testLiftGetMissing(t, repo) })
	t.Run("RecoveryApplied", func(t *testing.T) { testLiftRecoveryApplied(t, repo, instincts) })
	t.Run("RecoveryComplement", func(t *testing.T) { testLiftRecoveryComplement(t, repo, instincts, outcomes) })
	t.Run("BudgetApplied", func(t *testing.T) { testLiftBudgetApplied(t, repo, instincts, tasks) })
	t.Run("BudgetComplement", func(t *testing.T) { testLiftBudgetComplement(t, repo, instincts, outcomes, tasks) })
	t.Run("Architect", func(t *testing.T) {
		if !architectSupported(t, proposals) {
			t.Skip("architect surface not supported on this backend (WorkflowProposalRepository.Insert returned persistence.ErrNotFound) — SQLite has no architect; see sqlite/workflow_proposal_repository.go")
		}
		t.Run("Applied", func(t *testing.T) { testLiftArchitectApplied(t, repo, instincts, proposals) })
		t.Run("Complement", func(t *testing.T) { testLiftArchitectComplement(t, repo, instincts, proposals) })
		t.Run("ComplementCrossProject", func(t *testing.T) { testLiftArchitectComplementCrossProject(t, repo, instincts, proposals) })
	})
}

// architectSupported probes whether the backend under test actually
// stores workflow proposals by attempting a throwaway Insert. The
// SQLite WorkflowProposalRepository stub's Insert unconditionally
// returns persistence.ErrNotFound (single-process SQLite deployments
// aren't the memetic-workflows architect's audience); the Postgres
// implementation stores the row. Probing the exact method the
// Architect* queries depend on is more honest than threading a
// separate "backend kind" enum through repotest just for this one
// case group.
func architectSupported(t *testing.T, proposals persistence.WorkflowProposalRepository) bool {
	t.Helper()
	probe := &persistence.WorkflowProposal{
		ID:             uniqueID("prop-probe"),
		WorkflowID:     uniqueID("wf-probe"),
		Kind:           persistence.WorkflowProposalKindUnspecified,
		ProposalYAML:   "steps: []",
		Motivation:     "architect-support probe (repotest.architectSupported)",
		EvidenceRunIDs: []string{uniqueID("run")},
		Confidence:     0.1,
		ArchitectModel: "repotest-probe",
		CreatedAt:      time.Now().UTC(),
	}
	err := proposals.Insert(context.Background(), probe)
	if err == nil {
		return true
	}
	if errors.Is(err, persistence.ErrNotFound) {
		return false
	}
	t.Fatalf("architectSupported: unexpected Insert error: %v", err)
	return false
}

// seedWorkflowProposal inserts one workflow_proposals fixture row via
// the given WorkflowProposalRepository and returns its ID. createdAt
// is passed straight through to WorkflowProposal.CreatedAt — Insert
// only defaults it to time.Now() when zero — so Architect* case
// fixtures can place proposals precisely relative to a fixed `since`
// the same way the Recovery/Budget fixtures place applications /
// step outcomes.
func seedWorkflowProposal(t *testing.T, proposals persistence.WorkflowProposalRepository, workflowID string, kind persistence.WorkflowProposalKind, instinctIDs []string, createdAt time.Time) string {
	t.Helper()
	id := uniqueID("prop")
	p := &persistence.WorkflowProposal{
		ID:             id,
		WorkflowID:     workflowID,
		Kind:           kind,
		ProposalYAML:   "steps: []",
		Motivation:     "lift-suite fixture",
		EvidenceRunIDs: []string{uniqueID("run")},
		InstinctIDs:    instinctIDs,
		Confidence:     0.9,
		ArchitectModel: "lift-suite-fixture",
		CreatedAt:      createdAt,
	}
	if err := proposals.Insert(context.Background(), p); err != nil {
		t.Fatalf("seedWorkflowProposal: Insert: %v", err)
	}
	return id
}

// decideWorkflowProposal transitions a pending proposal to
// approved/rejected via Decide. decided_at is stamped by the
// repository as NOW() (not controllable from the fixture side) —
// that's fine, since ArchitectAppliedOutcomes/ArchitectComplementOutcomes
// only check decided_at IS NOT NULL, never compare it to `since`.
func decideWorkflowProposal(t *testing.T, proposals persistence.WorkflowProposalRepository, id string, status persistence.WorkflowProposalStatus) {
	t.Helper()
	if err := proposals.Decide(context.Background(), id, status, "lift-suite", ""); err != nil {
		t.Fatalf("decideWorkflowProposal: Decide(%s): %v", id, err)
	}
}

// testLiftArchitectApplied covers ArchitectAppliedOutcomes: P1 (instinct
// X's ID in instinct_ids, decided approved → success), P2 (contains X,
// decided rejected → not success), P3 (contains X, still pending →
// excluded, decided_at IS NULL), P4 (does not contain X, decided →
// excluded regardless, wrong treatment set) → N=2, Successes=1.
func testLiftArchitectApplied(t *testing.T, repo persistence.InstinctLiftRepository, instincts persistence.InstinctRepository, proposals persistence.WorkflowProposalRepository) {
	t.Helper()
	ctx := context.Background()
	instX := seedLiftInstinct(t, instincts)
	wf1 := uniqueID("wf")
	const kind = persistence.WorkflowProposalKindAddStep
	since := time.Date(2026, 3, 5, 12, 0, 0, 0, time.UTC)

	p1 := seedWorkflowProposal(t, proposals, wf1, kind, []string{instX}, since.Add(1*time.Minute))
	decideWorkflowProposal(t, proposals, p1, persistence.WorkflowProposalStatusApproved)

	p2 := seedWorkflowProposal(t, proposals, wf1, kind, []string{instX}, since.Add(2*time.Minute))
	decideWorkflowProposal(t, proposals, p2, persistence.WorkflowProposalStatusRejected)

	// P3: contains X but never decided — excluded. Left pending
	// deliberately, so it must live on its own workflow: the partial
	// unique index only allows one pending proposal per workflow_id,
	// and P4 below needs to be inserted-then-decided on a workflow of
	// its own too.
	seedWorkflowProposal(t, proposals, wf1, kind, []string{instX}, since.Add(3*time.Minute))

	// P4: decided, but doesn't contain X — excluded from X's treatment
	// set regardless of workflow (ArchitectAppliedOutcomes doesn't
	// filter on workflow_id), so it doesn't need to share wf1.
	wf2 := uniqueID("wf")
	p4 := seedWorkflowProposal(t, proposals, wf2, kind, nil, since.Add(4*time.Minute))
	decideWorkflowProposal(t, proposals, p4, persistence.WorkflowProposalStatusApproved)

	got, err := repo.ArchitectAppliedOutcomes(ctx, instX, since)
	if err != nil {
		t.Fatalf("ArchitectAppliedOutcomes: %v", err)
	}
	assertLiftOutcomes(t, "ArchitectAppliedOutcomes", got, 2, 1)
}

// testLiftArchitectComplement covers ArchitectComplementOutcomes: P1
// (contains X, decided) and P2 (contains X, decided) establish that
// instinct X's treatment set touched workflow wf1 / kind K in the
// window; P4 (same workflow+kind, does NOT contain X, decided
// approved → success) and P6 (same workflow+kind, NULL instinct_ids,
// decided rejected → failure) are the complement; P5 (a different
// workflow, decided, no X) is excluded by the workflow-membership
// subquery → N=2, Successes=1.
func testLiftArchitectComplement(t *testing.T, repo persistence.InstinctLiftRepository, instincts persistence.InstinctRepository, proposals persistence.WorkflowProposalRepository) {
	t.Helper()
	ctx := context.Background()
	instX := seedLiftInstinct(t, instincts)
	wf1 := uniqueID("wf")
	wf2 := uniqueID("wf")
	const kind = persistence.WorkflowProposalKindAddStep
	since := time.Date(2026, 3, 6, 12, 0, 0, 0, time.UTC)

	// P1/P2: establish that X's treatment set touched (wf1, kind).
	p1 := seedWorkflowProposal(t, proposals, wf1, kind, []string{instX}, since.Add(1*time.Minute))
	decideWorkflowProposal(t, proposals, p1, persistence.WorkflowProposalStatusApproved)
	p2 := seedWorkflowProposal(t, proposals, wf1, kind, []string{instX}, since.Add(2*time.Minute))
	decideWorkflowProposal(t, proposals, p2, persistence.WorkflowProposalStatusRejected)

	// P4: same (workflow, kind), no X, decided approved → complement success.
	p4 := seedWorkflowProposal(t, proposals, wf1, kind, nil, since.Add(3*time.Minute))
	decideWorkflowProposal(t, proposals, p4, persistence.WorkflowProposalStatusApproved)

	// P5: a DIFFERENT workflow, no X, decided — excluded by the
	// workflow-membership subquery even though the kind matches.
	p5 := seedWorkflowProposal(t, proposals, wf2, kind, nil, since.Add(3*time.Minute))
	decideWorkflowProposal(t, proposals, p5, persistence.WorkflowProposalStatusApproved)

	// P6: same (workflow, kind), NULL instinct_ids, decided rejected →
	// complement failure. Exercises the COALESCE(instinct_ids,'{}')
	// NULL-safety of the NOT (... @> ...) predicate.
	p6 := seedWorkflowProposal(t, proposals, wf1, kind, nil, since.Add(4*time.Minute))
	decideWorkflowProposal(t, proposals, p6, persistence.WorkflowProposalStatusRejected)

	got, err := repo.ArchitectComplementOutcomes(ctx, instX, since)
	if err != nil {
		t.Fatalf("ArchitectComplementOutcomes: %v", err)
	}
	assertLiftOutcomes(t, "ArchitectComplementOutcomes", got, 2, 1)
}

// testLiftArchitectComplementCrossProject (review-20260719-4396 S4):
// a GLOBAL-scope instinct with treatment proposals on workflows wfA
// and wfB — standing in for workflows in two DIFFERENT projects,
// since workflow_proposals has no project_id column of its own; a
// workflow's project membership lives one join away, in the
// workflows table the architect targets. The complement must include
// decided no-X proposals from BOTH workflows: proving the
// cross-project reach is a query property (no project filter anywhere
// in ArchitectComplementOutcomes' SQL), not an artifact of a
// project-scoped instinct happening to touch only one project.
func testLiftArchitectComplementCrossProject(t *testing.T, repo persistence.InstinctLiftRepository, instincts persistence.InstinctRepository, proposals persistence.WorkflowProposalRepository) {
	t.Helper()
	ctx := context.Background()
	instX := seedGlobalLiftInstinct(t, instincts)
	wfA := uniqueID("wf") // workflow in "project A"
	wfB := uniqueID("wf") // workflow in "project B"
	const kind = persistence.WorkflowProposalKindChangeTimeout
	since := time.Date(2026, 3, 7, 12, 0, 0, 0, time.UTC)

	// Treatment: X-tagged, decided proposals on BOTH projects' workflows.
	pA1 := seedWorkflowProposal(t, proposals, wfA, kind, []string{instX}, since.Add(1*time.Minute))
	decideWorkflowProposal(t, proposals, pA1, persistence.WorkflowProposalStatusApproved)
	pB1 := seedWorkflowProposal(t, proposals, wfB, kind, []string{instX}, since.Add(1*time.Minute))
	decideWorkflowProposal(t, proposals, pB1, persistence.WorkflowProposalStatusApproved)

	// Complement candidates: decided, no-X proposals on each project's
	// workflow — one success, one failure, one from each project.
	pA2 := seedWorkflowProposal(t, proposals, wfA, kind, nil, since.Add(2*time.Minute))
	decideWorkflowProposal(t, proposals, pA2, persistence.WorkflowProposalStatusApproved)
	pB2 := seedWorkflowProposal(t, proposals, wfB, kind, nil, since.Add(2*time.Minute))
	decideWorkflowProposal(t, proposals, pB2, persistence.WorkflowProposalStatusRejected)

	got, err := repo.ArchitectComplementOutcomes(ctx, instX, since)
	if err != nil {
		t.Fatalf("ArchitectComplementOutcomes(cross-project): %v", err)
	}
	assertLiftOutcomes(t, "ArchitectComplementOutcomes(cross-project)", got, 2, 1)
}

// recordLiftApplication is a thin wrapper over InstinctRepository.
// RecordApplication for the lift-suite fixtures: every field lift
// queries key off (instinct_id, surface, result, applied_at,
// execution_id, step_id, task_id) is explicit at the call site so
// cases read as a table of inputs, not hidden defaults.
func recordLiftApplication(t *testing.T, instincts persistence.InstinctRepository, instinctID, taskID, surface, result, executionID, stepID string, appliedAt time.Time) {
	t.Helper()
	if err := instincts.RecordApplication(context.Background(), &persistence.InstinctApplication{
		InstinctID:  instinctID,
		TaskID:      taskID,
		Surface:     surface,
		Result:      result,
		AppliedAt:   appliedAt,
		ExecutionID: executionID,
		StepID:      stepID,
	}); err != nil {
		t.Fatalf("RecordApplication: %v", err)
	}
}

// liftStepOutcomeRole is the role every RunInstinctLiftSuite fixture
// step outcome carries. All Recovery/Budget complement queries below
// filter on role="dev", so it's a fixed fixture constant rather than a
// recordLiftStepOutcome parameter (which every call site would fill in
// identically).
const liftStepOutcomeRole = "dev"

// recordLiftStepOutcome is the execution_step_outcomes-side fixture
// helper, mirroring recordLiftApplication.
func recordLiftStepOutcome(t *testing.T, outcomes persistence.ExecutionStepOutcomeRepository, projectID, taskID, executionID, stepID, errorClass, outcome string, recordedAt time.Time) {
	t.Helper()
	if err := outcomes.Record(context.Background(), &persistence.ExecutionStepOutcome{
		ID:          uniqueID("oc"),
		ProjectID:   projectID,
		TaskID:      taskID,
		ExecutionID: executionID,
		StepID:      stepID,
		Role:        liftStepOutcomeRole,
		Outcome:     outcome,
		ErrorClass:  errorClass,
		RecordedAt:  recordedAt,
	}); err != nil {
		t.Fatalf("execution_step_outcomes Record: %v", err)
	}
}

// seedLiftTask creates a tasks row in the given status via
// TaskRepository.Create, returning its ID. Budget* queries INNER JOIN
// tasks, so every task_id they resolve must have a real parent row.
func seedLiftTask(t *testing.T, tasks persistence.TaskRepository, projectID string, status persistence.TaskStatus) string {
	t.Helper()
	task := &persistence.Task{
		ID:        uniqueID("task"),
		ProjectID: projectID,
		Priority:  50,
		Payload:   []byte(`{}`),
		Status:    status,
	}
	if err := tasks.Create(context.Background(), task); err != nil {
		t.Fatalf("seedLiftTask: Create: %v", err)
	}
	return task.ID
}

// assertLiftOutcomes is the shared (N, Successes) assertion for the
// domain-query tests below.
func assertLiftOutcomes(t *testing.T, label string, got persistence.LiftOutcomes, wantN, wantSuccesses int) {
	t.Helper()
	if got.N != wantN || got.Successes != wantSuccesses {
		t.Errorf("%s = (N=%d, Successes=%d), want (N=%d, Successes=%d)", label, got.N, got.Successes, wantN, wantSuccesses)
	}
}

// testLiftRecoveryApplied covers RecoveryAppliedOutcomes: 3 resolved
// lead_recovery applications for instinct A (2 succeeded, 1 failed)
// inside the window, plus 1 outside the window and 1 unresolved
// ('ignored') result inside the window that must both be excluded.
func testLiftRecoveryApplied(t *testing.T, repo persistence.InstinctLiftRepository, instincts persistence.InstinctRepository) {
	t.Helper()
	ctx := context.Background()
	instA := seedLiftInstinct(t, instincts)
	since := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	recordLiftApplication(t, instincts, instA, "", "lead_recovery", "succeeded", uniqueID("exec"), "s1", since.Add(1*time.Minute))
	recordLiftApplication(t, instincts, instA, "", "lead_recovery", "succeeded", uniqueID("exec"), "s2", since.Add(2*time.Minute))
	recordLiftApplication(t, instincts, instA, "", "lead_recovery", "failed", uniqueID("exec"), "s3", since.Add(3*time.Minute))
	// Outside the window — excluded even though resolved.
	recordLiftApplication(t, instincts, instA, "", "lead_recovery", "succeeded", uniqueID("exec"), "s4", since.Add(-1*time.Hour))
	// Unresolved (still pending operator/auto feedback) — excluded regardless of window.
	recordLiftApplication(t, instincts, instA, "", "lead_recovery", "ignored", uniqueID("exec"), "s5", since.Add(4*time.Minute))

	got, err := repo.RecoveryAppliedOutcomes(ctx, instA, since)
	if err != nil {
		t.Fatalf("RecoveryAppliedOutcomes: %v", err)
	}
	assertLiftOutcomes(t, "RecoveryAppliedOutcomes", got, 3, 2)
}

// testLiftRecoveryComplement covers RecoveryComplementOutcomes: four
// failed (project p1, role dev, error_class timeout) steps S1..S4 —
// S1 carries an application of instinct A on its (execution, step)
// and is excluded; S2 has a LATER 'ok' outcome (success); S3 has an
// EARLIER 'ok' outcome only (not success, since the failure isn't
// recovered by something that already happened); S4 has no 'ok' at
// all (failure). A fifth failed step S5 in a second project proves
// the "" (global-scope) projectID drops the project filter.
func testLiftRecoveryComplement(t *testing.T, repo persistence.InstinctLiftRepository, instincts persistence.InstinctRepository, outcomes persistence.ExecutionStepOutcomeRepository) {
	t.Helper()
	ctx := context.Background()
	instC := seedLiftInstinct(t, instincts)
	const role = "dev"
	const errorClass = "timeout"
	p1 := uniqueID("proj")
	since := time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)

	// S1: failed, but instinct C was already applied to this exact
	// (execution, step) — excluded by the NOT EXISTS clause.
	exec1, step1 := uniqueID("exec"), "s1"
	recordLiftStepOutcome(t, outcomes, p1, uniqueID("task"), exec1, step1, errorClass, "failed", since.Add(1*time.Minute))
	recordLiftApplication(t, instincts, instC, "", "lead_recovery", "ignored", exec1, step1, since.Add(1*time.Minute))

	// S2: failed, then a LATER ok on the same (execution, step) → success.
	exec2, step2 := uniqueID("exec"), "s2"
	recordLiftStepOutcome(t, outcomes, p1, uniqueID("task"), exec2, step2, errorClass, "failed", since.Add(1*time.Minute))
	recordLiftStepOutcome(t, outcomes, p1, uniqueID("task"), exec2, step2, "", "ok", since.Add(2*time.Minute))

	// S3: an EARLIER ok, then the failure — not a recovery.
	exec3, step3 := uniqueID("exec"), "s3"
	recordLiftStepOutcome(t, outcomes, p1, uniqueID("task"), exec3, step3, "", "ok", since.Add(1*time.Minute))
	recordLiftStepOutcome(t, outcomes, p1, uniqueID("task"), exec3, step3, errorClass, "failed", since.Add(2*time.Minute))

	// S4: failed, no ok ever → failure.
	exec4, step4 := uniqueID("exec"), "s4"
	recordLiftStepOutcome(t, outcomes, p1, uniqueID("task"), exec4, step4, errorClass, "failed", since.Add(1*time.Minute))

	got, err := repo.RecoveryComplementOutcomes(ctx, instC, p1, role, errorClass, since)
	if err != nil {
		t.Fatalf("RecoveryComplementOutcomes(project-scoped): %v", err)
	}
	assertLiftOutcomes(t, "RecoveryComplementOutcomes(project-scoped)", got, 3, 1)

	// S5: same failure shape as S4, but in a SECOND project. A
	// project-scoped call must not see it; the "" global-scope call must.
	p2 := uniqueID("proj")
	exec5, step5 := uniqueID("exec"), "s5"
	recordLiftStepOutcome(t, outcomes, p2, uniqueID("task"), exec5, step5, errorClass, "failed", since.Add(1*time.Minute))

	gotScoped, err := repo.RecoveryComplementOutcomes(ctx, instC, p1, role, errorClass, since)
	if err != nil {
		t.Fatalf("RecoveryComplementOutcomes(project-scoped, after S5): %v", err)
	}
	assertLiftOutcomes(t, "RecoveryComplementOutcomes(project-scoped, after S5)", gotScoped, 3, 1)

	gotGlobal, err := repo.RecoveryComplementOutcomes(ctx, instC, "", role, errorClass, since)
	if err != nil {
		t.Fatalf("RecoveryComplementOutcomes(global): %v", err)
	}
	assertLiftOutcomes(t, "RecoveryComplementOutcomes(global)", gotGlobal, 4, 1)
}

// testLiftBudgetApplied covers BudgetAppliedOutcomes: applications
// surface='tool_budget' for instinct B on tasks T1 (COMPLETED), T2
// (FAILED), T3 (RUNNING — excluded, not terminal).
func testLiftBudgetApplied(t *testing.T, repo persistence.InstinctLiftRepository, instincts persistence.InstinctRepository, tasks persistence.TaskRepository) {
	t.Helper()
	ctx := context.Background()
	instB := seedLiftInstinct(t, instincts)
	project := uniqueID("proj")
	since := time.Date(2026, 3, 3, 12, 0, 0, 0, time.UTC)

	t1 := seedLiftTask(t, tasks, project, persistence.TaskStatusCompleted)
	t2 := seedLiftTask(t, tasks, project, persistence.TaskStatusFailed)
	t3 := seedLiftTask(t, tasks, project, persistence.TaskStatusRunning)

	recordLiftApplication(t, instincts, instB, t1, "tool_budget", "accepted", "", "", since.Add(1*time.Minute))
	recordLiftApplication(t, instincts, instB, t2, "tool_budget", "accepted", "", "", since.Add(1*time.Minute))
	recordLiftApplication(t, instincts, instB, t3, "tool_budget", "accepted", "", "", since.Add(1*time.Minute))

	got, err := repo.BudgetAppliedOutcomes(ctx, instB, since)
	if err != nil {
		t.Fatalf("BudgetAppliedOutcomes: %v", err)
	}
	assertLiftOutcomes(t, "BudgetAppliedOutcomes", got, 2, 1)
}

// testLiftBudgetComplement covers BudgetComplementOutcomes: step
// outcomes for (p1, dev) on tasks T4 (COMPLETED, no application →
// counted, success), T1-equivalent (has a tool_budget application of
// this instinct → excluded), T5 (FAILED, no application → counted,
// failure).
func testLiftBudgetComplement(t *testing.T, repo persistence.InstinctLiftRepository, instincts persistence.InstinctRepository, outcomes persistence.ExecutionStepOutcomeRepository, tasks persistence.TaskRepository) {
	t.Helper()
	ctx := context.Background()
	instD := seedLiftInstinct(t, instincts)
	const role = "dev"
	p1 := uniqueID("proj")
	since := time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC)

	t4 := seedLiftTask(t, tasks, p1, persistence.TaskStatusCompleted)
	tWithApp := seedLiftTask(t, tasks, p1, persistence.TaskStatusCompleted)
	t5 := seedLiftTask(t, tasks, p1, persistence.TaskStatusFailed)

	recordLiftApplication(t, instincts, instD, tWithApp, "tool_budget", "accepted", "", "", since.Add(1*time.Minute))

	recordLiftStepOutcome(t, outcomes, p1, t4, uniqueID("exec"), "s1", "", "ok", since.Add(1*time.Minute))
	recordLiftStepOutcome(t, outcomes, p1, tWithApp, uniqueID("exec"), "s1", "", "ok", since.Add(1*time.Minute))
	recordLiftStepOutcome(t, outcomes, p1, t5, uniqueID("exec"), "s1", "", "ok", since.Add(1*time.Minute))

	got, err := repo.BudgetComplementOutcomes(ctx, instD, p1, role, since)
	if err != nil {
		t.Fatalf("BudgetComplementOutcomes: %v", err)
	}
	assertLiftOutcomes(t, "BudgetComplementOutcomes", got, 2, 1)
}

func testLiftUpsertThenGet(t *testing.T, repo persistence.InstinctLiftRepository, instincts persistence.InstinctRepository) {
	t.Helper()
	ctx := context.Background()
	id := seedLiftInstinct(t, instincts)
	snap := &persistence.InstinctLiftSnapshot{
		InstinctID:    id,
		Domain:        persistence.InstinctDomainRecovery,
		Lift:          0.375,
		TreatmentN:    20,
		TreatmentSucc: 15,
		BaselineN:     10,
		BaselineSucc:  4,
		StdError:      0.081,
		Verdict:       persistence.LiftVerdictHelping,
		ComputedAt:    time.Now().UTC(),
	}
	if err := repo.UpsertLiftSnapshot(ctx, snap); err != nil {
		t.Fatalf("UpsertLiftSnapshot: %v", err)
	}

	got, err := repo.GetLiftSnapshots(ctx, []string{id})
	if err != nil {
		t.Fatalf("GetLiftSnapshots: %v", err)
	}
	row, ok := got[id]
	if !ok {
		t.Fatalf("GetLiftSnapshots did not return instinct %s", id)
	}
	assertLiftSnapshotEqual(t, row, snap)
}

func testLiftUpsertReplaces(t *testing.T, repo persistence.InstinctLiftRepository, instincts persistence.InstinctRepository) {
	t.Helper()
	ctx := context.Background()
	id := seedLiftInstinct(t, instincts)
	first := &persistence.InstinctLiftSnapshot{
		InstinctID: id, Domain: persistence.InstinctDomainRecovery,
		Lift: 0.1, TreatmentN: 5, TreatmentSucc: 3, BaselineN: 5, BaselineSucc: 2,
		StdError: 0.2, Verdict: persistence.LiftVerdictUnknown,
		ComputedAt: time.Now().UTC().Add(-time.Hour),
	}
	if err := repo.UpsertLiftSnapshot(ctx, first); err != nil {
		t.Fatalf("UpsertLiftSnapshot(first): %v", err)
	}
	second := &persistence.InstinctLiftSnapshot{
		InstinctID: id, Domain: persistence.InstinctDomainRecovery,
		Lift: 0.5, TreatmentN: 30, TreatmentSucc: 25, BaselineN: 20, BaselineSucc: 8,
		StdError: 0.05, Verdict: persistence.LiftVerdictHelping,
		ComputedAt: time.Now().UTC(),
	}
	if err := repo.UpsertLiftSnapshot(ctx, second); err != nil {
		t.Fatalf("UpsertLiftSnapshot(second): %v", err)
	}

	got, err := repo.GetLiftSnapshots(ctx, []string{id})
	if err != nil {
		t.Fatalf("GetLiftSnapshots: %v", err)
	}
	row, ok := got[id]
	if !ok {
		t.Fatalf("GetLiftSnapshots did not return instinct %s", id)
	}
	// Get must return the SECOND upsert's values only (snapshot semantics,
	// not an event log) — assert against `second`, not `first`.
	assertLiftSnapshotEqual(t, row, second)
}

func testLiftGetEmptyInput(t *testing.T, repo persistence.InstinctLiftRepository) {
	t.Helper()
	got, err := repo.GetLiftSnapshots(context.Background(), nil)
	if err != nil {
		t.Fatalf("GetLiftSnapshots(nil): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("GetLiftSnapshots(nil) returned %d entries, want 0", len(got))
	}
}

func testLiftGetMissing(t *testing.T, repo persistence.InstinctLiftRepository) {
	t.Helper()
	ghost := uniqueID("ghost")
	got, err := repo.GetLiftSnapshots(context.Background(), []string{ghost})
	if err != nil {
		t.Fatalf("GetLiftSnapshots(missing): %v", err)
	}
	if _, ok := got[ghost]; ok {
		t.Errorf("missing id should be absent from map")
	}
	if len(got) != 0 {
		t.Errorf("GetLiftSnapshots(missing) returned %d entries, want 0", len(got))
	}
}
