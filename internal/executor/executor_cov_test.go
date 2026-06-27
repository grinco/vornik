package executor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"vornik.io/vornik/internal/persistence"
)

// executorCov_steeringSink records NotifySteeringRequired calls so the
// notifySteering nil-safe wrapper test can assert the call reached the sink.
type executorCov_steeringSink struct {
	calls  int
	states []string
}

func (s *executorCov_steeringSink) NotifySteeringRequired(_ context.Context, _ *persistence.Task, state string) {
	s.calls++
	s.states = append(s.states, state)
}

// executorCov_reservRepo is a minimal BudgetReservationRepository whose
// SettleByTask records the call and can be made to error. The embedded
// interface satisfies the unused methods (they panic if ever called,
// which they aren't on the settle path).
type executorCov_reservRepo struct {
	persistence.BudgetReservationRepository
	settleCalls int
	settleErr   error
}

func (r *executorCov_reservRepo) SettleByTask(_ context.Context, _ string, _ time.Time) (int64, error) {
	r.settleCalls++
	return 0, r.settleErr
}

// TestExecutorCov_OptionSettersWireFields drives every previously-uncovered
// With* option through NewWithOptions and asserts the field landed. These are
// simple plumbing setters; calling each closes the 0% coverage gap and pins
// the wiring contract.
func TestExecutorCov_OptionSettersWireFields(t *testing.T) {
	rt := NewMockRuntime()
	er := NewMockExecRepo()
	ar := NewMockArtifactRepo()
	tr := NewMockTaskRepo()

	reserv := &executorCov_reservRepo{}
	steer := &executorCov_steeringSink{}

	e := NewWithOptions(rt, er, ar, tr, nil,
		WithBudgetReservationRepository(reserv),
		// All of these accept interface types; nil is a valid "disabled"
		// argument and still exercises the setter body.
		WithInstinctPlaybooks(nil, true),
		WithAPIKeyMinter(nil),
		WithSteeringNotifier(steer),
		WithLivePublisher(nil),
		WithRecoveryEventRepository(nil),
		WithHintRepository(nil),
		WithCrossProjectCallRepository(nil),
		WithProjectSpawnRepository(nil),
		WithProjectTemplateCatalog(nil),
		WithConfigsDir("/tmp/cov-configs"),
		WithRegistryReloader(nil),
		WithAdminAuditRepository(nil),
		WithSchemaRegistry(nil),
		WithJudgeRunner(nil),
	)

	assert.Same(t, reserv, e.reservRepo)
	assert.True(t, e.instinctPlaybooks)
	assert.Same(t, steer, e.steering)
	assert.Equal(t, "/tmp/cov-configs", e.configsDir)
	// WithJudgeRunner(nil) must store a genuine nil (typed-nil trap guard).
	assert.Nil(t, e.judgeRunner)
}

// TestExecutorCov_NotifySteering exercises both branches of the nil-safe
// wrapper: a wired sink receives the call, and the guarded no-op paths
// (nil receiver, nil sink, nil task) don't panic or dispatch.
func TestExecutorCov_NotifySteering(t *testing.T) {
	ctx := context.Background()
	sink := &executorCov_steeringSink{}
	e, _, _, _, _ := setup()
	e.steering = sink

	task := &persistence.Task{ID: "t1"}
	e.notifySteering(ctx, task, "AWAITING_INPUT")
	assert.Equal(t, 1, sink.calls)
	assert.Equal(t, []string{"AWAITING_INPUT"}, sink.states)

	// nil task → no dispatch.
	e.notifySteering(ctx, nil, "X")
	assert.Equal(t, 1, sink.calls)

	// nil sink → no dispatch, no panic.
	e.steering = nil
	assert.NotPanics(t, func() { e.notifySteering(ctx, task, "X") })

	// nil receiver → no panic.
	var eNil *Executor
	assert.NotPanics(t, func() { eNil.notifySteering(ctx, task, "X") })
}

// TestExecutorCov_SettleBudgetReservation covers the wired-success,
// wired-error, and disabled/no-op branches.
func TestExecutorCov_SettleBudgetReservation(t *testing.T) {
	ctx := context.Background()

	// Success path.
	reserv := &executorCov_reservRepo{}
	e, _, _, _, _ := setup()
	e.reservRepo = reserv
	e.settleBudgetReservation(ctx, "task-1")
	assert.Equal(t, 1, reserv.settleCalls)

	// Error path — logged, still returns (no panic).
	reservErr := &executorCov_reservRepo{settleErr: errors.New("ledger down")}
	e.reservRepo = reservErr
	assert.NotPanics(t, func() { e.settleBudgetReservation(ctx, "task-2") })
	assert.Equal(t, 1, reservErr.settleCalls)

	// No-op branches: nil repo, empty taskID, nil receiver.
	e.reservRepo = nil
	assert.NotPanics(t, func() { e.settleBudgetReservation(ctx, "task-3") })

	e.reservRepo = reserv
	e.settleBudgetReservation(ctx, "")
	assert.Equal(t, 1, reserv.settleCalls) // unchanged — empty ID short-circuits.

	var eNil *Executor
	assert.NotPanics(t, func() { eNil.settleBudgetReservation(ctx, "x") })
}

// TestExecutorCov_AutoApplyEligible covers autoApplyConfig.eligible across
// disabled, below-confidence, all-classes, and class-filtered branches.
// minCleanSupport is 0 here (off), so support/contradict are ignored —
// the clean-support gate has its own test below.
func TestExecutorCov_AutoApplyEligible(t *testing.T) {
	// Disabled → never eligible.
	off := autoApplyConfig{enabled: false}
	assert.False(t, off.eligible(0.99, 99, 0, "timeout"))

	// Enabled, below confidence floor → not eligible.
	c := autoApplyConfig{enabled: true, minConfidence: 0.85}
	assert.False(t, c.eligible(0.5, 99, 0, "timeout"))

	// Enabled, meets floor, no class filter, no clean-support gate → eligible.
	assert.True(t, c.eligible(0.9, 99, 0, "anything"))

	// Class filter present: only listed classes eligible.
	cf := autoApplyConfig{
		enabled:        true,
		minConfidence:  0.85,
		allowedClasses: map[string]struct{}{"timeout": {}},
	}
	assert.True(t, cf.eligible(0.9, 99, 0, "timeout"))
	assert.False(t, cf.eligible(0.9, 99, 0, "parse_error"))
}

// TestExecutorCov_AutoApplyCleanSupportGate covers the clean-support gate:
// when minCleanSupport > 0, a remediation must have >= minCleanSupport
// corroborations AND zero contradictions, on top of the confidence floor.
// This is what lets a lowered confidence bar stay safe (instinct-auto-apply
// supply design, 2026-06-23).
func TestExecutorCov_AutoApplyCleanSupportGate(t *testing.T) {
	c := autoApplyConfig{enabled: true, minConfidence: 0.70, minCleanSupport: 10}

	// Enough clean support, zero contradictions, meets floor → eligible.
	assert.True(t, c.eligible(0.71, 12, 0, "context_timeout"),
		"12 clean supports, 0 contradictions, conf>=0.70 must be eligible")

	// Below the support threshold → not eligible even though conf clears.
	assert.False(t, c.eligible(0.71, 9, 0, "context_timeout"),
		"9 supports < min_clean_support 10 must be rejected")

	// At/above support but ANY contradiction → not eligible (the
	// decayed-but-now-mixed case: confidence may still clear 0.70, but a
	// regression disqualifies it).
	assert.False(t, c.eligible(0.71, 12, 1, "context_timeout"),
		"a single contradiction must disqualify regardless of support/confidence")

	// Below the confidence floor still rejected even with clean support.
	assert.False(t, c.eligible(0.5, 50, 0, "context_timeout"))

	// minCleanSupport == 0 (off) ignores support/contradict entirely —
	// backward-compatible with pre-supply behaviour.
	legacy := autoApplyConfig{enabled: true, minConfidence: 0.85, minCleanSupport: 0}
	assert.True(t, legacy.eligible(0.9, 0, 5, "anything"),
		"min_clean_support 0 must ignore support/contradict (legacy behaviour)")
}

// TestExecutorCov_WithInstinctAutoApply pins the default-confidence backfill
// and the allowed-class set construction (skipping empty strings).
func TestExecutorCov_WithInstinctAutoApply(t *testing.T) {
	e, _, _, _, _ := setup()

	// enabled with minConfidence<=0 → defaults to 0.85; empty class strings
	// are dropped from the set; clean-support threshold carried through.
	WithInstinctAutoApply(true, 0, 10, []string{"timeout", "", "parse_error"})(e)
	assert.True(t, e.instinctAutoApply.enabled)
	assert.Equal(t, 0.85, e.instinctAutoApply.minConfidence)
	assert.Equal(t, 10, e.instinctAutoApply.minCleanSupport)
	assert.Len(t, e.instinctAutoApply.allowedClasses, 2)
	_, hasEmpty := e.instinctAutoApply.allowedClasses[""]
	assert.False(t, hasEmpty)

	// disabled with no classes → empty set, supplied confidence preserved,
	// clean-support gate off.
	e2, _, _, _, _ := setup()
	WithInstinctAutoApply(false, 0.7, 0, nil)(e2)
	assert.False(t, e2.instinctAutoApply.enabled)
	assert.Equal(t, 0.7, e2.instinctAutoApply.minConfidence)
	assert.Equal(t, 0, e2.instinctAutoApply.minCleanSupport)
	assert.Nil(t, e2.instinctAutoApply.allowedClasses)

	// negative clean-support is clamped to 0 (off).
	e3, _, _, _, _ := setup()
	WithInstinctAutoApply(true, 0.7, -5, nil)(e3)
	assert.Equal(t, 0, e3.instinctAutoApply.minCleanSupport)
}
