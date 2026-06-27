package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/stepoutcome"
)

// stubStepOutcomeRepo is an in-memory ExecutionStepOutcomeRepository for
// testing the outcome writer + attribution logic. It reproduces the
// "most recent pending row per (execution, step)" semantics of the
// Postgres implementation so tests assert the same shape as production.
type stubStepOutcomeRepo struct {
	mu          sync.Mutex
	rows        []*persistence.ExecutionStepOutcome
	recordErr   error // when non-nil, Record returns it (covers warn-outcome resilience)
	sweepErr    error // when non-nil, SweepPending returns it (covers sweep-resilience)
	finalizeErr error // when non-nil, FinalizePending returns a generic error (not ErrNotFound)
}

func newStubStepOutcomeRepo() *stubStepOutcomeRepo { return &stubStepOutcomeRepo{} }

func (s *stubStepOutcomeRepo) Record(_ context.Context, o *persistence.ExecutionStepOutcome) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.recordErr != nil {
		return s.recordErr
	}
	// Store a copy so later mutations don't bleed across assertions.
	cp := *o
	s.rows = append(s.rows, &cp)
	return nil
}

func (s *stubStepOutcomeRepo) Finalize(_ context.Context, id, outcome, errorClass, errorDetail string, attr *string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.rows {
		if r.ID == id {
			r.Outcome = outcome
			r.ErrorClass = errorClass
			r.ErrorDetail = errorDetail
			r.AttributedToStepID = attr
			return nil
		}
	}
	return persistence.ErrNotFound
}

func (s *stubStepOutcomeRepo) FinalizePending(_ context.Context, executionID, stepID, outcome, errorClass, errorDetail string, attr *string) (string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finalizeErr != nil {
		return "", "", s.finalizeErr
	}
	// Find the most recently recorded pending row for this (execution, step).
	for i := len(s.rows) - 1; i >= 0; i-- {
		r := s.rows[i]
		if r.ExecutionID == executionID && r.StepID == stepID && r.Outcome == string(stepoutcome.PendingValidation) {
			r.Outcome = outcome
			r.ErrorClass = errorClass
			r.ErrorDetail = errorDetail
			r.AttributedToStepID = attr
			return r.Role, r.Model, nil
		}
	}
	return "", "", persistence.ErrNotFound
}

func (s *stubStepOutcomeRepo) SweepPending(_ context.Context, executionID, fallback string) ([]persistence.SweepResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sweepErr != nil {
		return nil, s.sweepErr
	}
	var swept []persistence.SweepResult
	for _, r := range s.rows {
		if r.ExecutionID == executionID && r.Outcome == string(stepoutcome.PendingValidation) {
			r.Outcome = fallback
			swept = append(swept, persistence.SweepResult{StepID: r.StepID, Role: r.Role, Model: r.Model})
		}
	}
	return swept, nil
}

func (s *stubStepOutcomeRepo) List(_ context.Context, _ persistence.ExecutionStepOutcomeFilter) ([]*persistence.ExecutionStepOutcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*persistence.ExecutionStepOutcome, 0, len(s.rows))
	for _, r := range s.rows {
		cp := *r
		out = append(out, &cp)
	}
	return out, nil
}

// SupersedeAfter mirrors the postgres implementation closely enough for
// retry-from-step tests: it relabels rows whose recorded_at is strictly
// after `cutoff` and not already superseded. Used by the executor's
// RetryFromStep flow.
func (s *stubStepOutcomeRepo) SupersedeAfter(_ context.Context, executionID string, cutoff time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	for _, r := range s.rows {
		if r.ExecutionID != executionID {
			continue
		}
		if r.Outcome == "superseded" {
			continue
		}
		if r.RecordedAt.After(cutoff) {
			r.Outcome = "superseded"
			n++
		}
	}
	return n, nil
}

// CountByRoleModelOutcome is the in-memory equivalent of the postgres
// aggregate query: groups stored rows by (role, model) and counts
// matches against the supplied outcome literal within the window.
// Used by effective-cost-alert tests that want to drive the loop
// without a database.
func (s *stubStepOutcomeRepo) CountByRoleModelOutcome(_ context.Context, outcome string, since, until time.Time, _ string) ([]persistence.RoleModelOutcomeCount, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	type key struct{ role, model string }
	tally := make(map[key]int64)
	for _, r := range s.rows {
		if r.Outcome != outcome || r.Role == "" || r.Model == "" {
			continue
		}
		if !since.IsZero() && r.RecordedAt.Before(since) {
			continue
		}
		if !until.IsZero() && !r.RecordedAt.Before(until) {
			continue
		}
		tally[key{r.Role, r.Model}]++
	}
	out := make([]persistence.RoleModelOutcomeCount, 0, len(tally))
	for k, n := range tally {
		out = append(out, persistence.RoleModelOutcomeCount{Role: k.role, Model: k.model, Count: n})
	}
	return out, nil
}

// Compile-time assertion that the stub satisfies the production
// repository interface — adding a method to the interface should
// fail this file's compile, not produce a runtime nil-method panic
// in tests that pass the stub as the interface type.
var _ persistence.ExecutionStepOutcomeRepository = (*stubStepOutcomeRepo)(nil)

func TestRecordStepOutcome(t *testing.T) {
	task := &persistence.Task{ID: "t1", ProjectID: "p1"}
	exec := &persistence.Execution{ID: "e1"}

	t.Run("writes pending_validation with no finalized_at", func(t *testing.T) {
		repo := newStubStepOutcomeRepo()
		e := &Executor{outcomeRepo: repo, logger: zerolog.Nop()}
		e.recordStepOutcome(context.Background(), task, exec, "step_0", "coder", "qwen-coder",
			string(stepoutcome.PendingValidation), "", "", nil, nil)

		require.Len(t, repo.rows, 1)
		row := repo.rows[0]
		assert.Equal(t, "p1", row.ProjectID)
		assert.Equal(t, "t1", row.TaskID)
		assert.Equal(t, "e1", row.ExecutionID)
		assert.Equal(t, "step_0", row.StepID)
		assert.Equal(t, string(stepoutcome.PendingValidation), row.Outcome)
		assert.Nil(t, row.FinalizedAt, "pending_validation row must not be finalized at write time")
	})

	t.Run("writes terminal outcome with finalized_at set", func(t *testing.T) {
		repo := newStubStepOutcomeRepo()
		e := &Executor{outcomeRepo: repo, logger: zerolog.Nop()}
		e.recordStepOutcome(context.Background(), task, exec, "step_0", "coder", "qwen-coder",
			string(stepoutcome.Failed), stepoutcome.ClassContainerNonZeroExit, "boom", nil, nil)

		require.Len(t, repo.rows, 1)
		row := repo.rows[0]
		assert.Equal(t, string(stepoutcome.Failed), row.Outcome)
		assert.Equal(t, stepoutcome.ClassContainerNonZeroExit, row.ErrorClass)
		assert.Equal(t, "boom", row.ErrorDetail)
		assert.NotNil(t, row.FinalizedAt)
	})

	t.Run("no-op when outcome repo is nil", func(t *testing.T) {
		e := &Executor{outcomeRepo: nil, logger: zerolog.Nop()}
		require.NotPanics(t, func() {
			e.recordStepOutcome(context.Background(), task, exec, "step_0", "coder", "m",
				string(stepoutcome.PendingValidation), "", "", nil, nil)
		})
	})

	t.Run("truncates very long error detail", func(t *testing.T) {
		repo := newStubStepOutcomeRepo()
		e := &Executor{outcomeRepo: repo, logger: zerolog.Nop()}
		huge := make([]byte, 5000)
		for i := range huge {
			huge[i] = 'x'
		}
		e.recordStepOutcome(context.Background(), task, exec, "step_0", "coder", "m",
			string(stepoutcome.Failed), "oops", string(huge), nil, nil)

		require.Len(t, repo.rows, 1)
		// truncateStr caps content at 2000 chars then appends "..."; anything
		// well under the 5000-char raw input is acceptable here.
		assert.Less(t, len(repo.rows[0].ErrorDetail), len(huge))
		assert.LessOrEqual(t, len(repo.rows[0].ErrorDetail), 2100)
	})
}

func TestFinalizePendingOutcome(t *testing.T) {
	task := &persistence.Task{ID: "t1", ProjectID: "p1"}
	exec := &persistence.Execution{ID: "e1"}

	t.Run("flips pending to the requested outcome", func(t *testing.T) {
		repo := newStubStepOutcomeRepo()
		e := &Executor{outcomeRepo: repo, logger: zerolog.Nop()}
		e.recordStepOutcome(context.Background(), task, exec, "step_lead", "lead", "lead-model",
			string(stepoutcome.PendingValidation), "", "", nil, nil)

		attr := "plan_step"
		e.finalizePendingOutcome(context.Background(), exec.ID, "step_lead",
			string(stepoutcome.ParseError), stepoutcome.ClassParseInvalidJSON,
			"invalid JSON from lead agent: unexpected token", &attr)

		require.Len(t, repo.rows, 1)
		row := repo.rows[0]
		assert.Equal(t, string(stepoutcome.ParseError), row.Outcome)
		assert.Equal(t, stepoutcome.ClassParseInvalidJSON, row.ErrorClass)
		require.NotNil(t, row.AttributedToStepID)
		assert.Equal(t, "plan_step", *row.AttributedToStepID)
	})

	t.Run("silent when no pending row exists (pre-feature execution)", func(t *testing.T) {
		repo := newStubStepOutcomeRepo()
		e := &Executor{outcomeRepo: repo, logger: zerolog.Nop()}
		require.NotPanics(t, func() {
			e.finalizePendingOutcome(context.Background(), "nonexistent-exec", "nowhere-step",
				string(stepoutcome.ParseError), "parse", "nope", nil)
		})
	})
}

func TestSweepPendingOutcomes_NilRepoIsNoop(t *testing.T) {
	e := &Executor{logger: zerolog.Nop()} // no outcomeRepo
	// Should not panic.
	e.sweepPendingOutcomes(context.Background(), "any-exec", string(stepoutcome.OK))
}

func TestSweepPendingOutcomes_ErrorIsLoggedAndReturns(t *testing.T) {
	repo := newStubStepOutcomeRepo()
	repo.sweepErr = errors.New("db down")
	e := &Executor{outcomeRepo: repo, logger: zerolog.Nop()}
	// Best-effort: error doesn't propagate.
	assert.NotPanics(t, func() {
		e.sweepPendingOutcomes(context.Background(), "exec1", string(stepoutcome.OK))
	})
}

// TestFinalizePendingOutcome_NilRepoIsNoop pins the nil-repo branch.
func TestFinalizePendingOutcome_NilRepoIsNoop(t *testing.T) {
	e := &Executor{logger: zerolog.Nop()} // no outcomeRepo
	assert.NotPanics(t, func() {
		e.finalizePendingOutcome(context.Background(), "exec", "step", "ok", "", "", nil)
	})
}

func TestFinalizePendingOutcome_GenericErrorIsLogged(t *testing.T) {
	repo := newStubStepOutcomeRepo()
	repo.finalizeErr = errors.New("db down")
	e := &Executor{outcomeRepo: repo, logger: zerolog.Nop()}
	assert.NotPanics(t, func() {
		e.finalizePendingOutcome(context.Background(), "exec", "step", "ok", "", "", nil)
	})
}

func TestSweepPendingOutcomes(t *testing.T) {
	task := &persistence.Task{ID: "t1", ProjectID: "p1"}
	exec := &persistence.Execution{ID: "e1"}

	repo := newStubStepOutcomeRepo()
	e := &Executor{outcomeRepo: repo, logger: zerolog.Nop()}

	// Two pending, one already-terminal row.
	e.recordStepOutcome(context.Background(), task, exec, "s1", "lead", "m",
		string(stepoutcome.PendingValidation), "", "", nil, nil)
	e.recordStepOutcome(context.Background(), task, exec, "s2", "coder", "m",
		string(stepoutcome.PendingValidation), "", "", nil, nil)
	e.recordStepOutcome(context.Background(), task, exec, "s3", "coder", "m",
		string(stepoutcome.Failed), "oops", "boom", nil, nil)

	e.sweepPendingOutcomes(context.Background(), exec.ID, string(stepoutcome.OK))

	outcomes := map[string]string{}
	for _, r := range repo.rows {
		outcomes[r.StepID] = r.Outcome
	}
	assert.Equal(t, string(stepoutcome.OK), outcomes["s1"])
	assert.Equal(t, string(stepoutcome.OK), outcomes["s2"])
	assert.Equal(t, string(stepoutcome.Failed), outcomes["s3"], "terminal rows are not touched by sweep")
}

func TestClassifyLeadParseError(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		wantOut   string
		wantClass string
	}{
		{
			name:      "invalid JSON",
			err:       errors.New("invalid JSON from lead agent: unexpected end of input"),
			wantOut:   string(stepoutcome.ParseError),
			wantClass: stepoutcome.ClassParseInvalidJSON,
		},
		{
			name:      "refused",
			err:       errors.New("lead agent refused to plan: missing prerequisite X"),
			wantOut:   string(stepoutcome.Refused),
			wantClass: stepoutcome.ClassParsePlanRefused,
		},
		{
			name:      "no steps",
			err:       errors.New("lead agent plan contains no steps"),
			wantOut:   string(stepoutcome.SchemaViolation),
			wantClass: stepoutcome.ClassParsePlanNoSteps,
		},
		{
			name:      "empty result",
			err:       errors.New("empty result from lead agent"),
			wantOut:   string(stepoutcome.SchemaViolation),
			wantClass: stepoutcome.ClassParsePlanNoSteps,
		},
		{
			name:      "wrapped invalid JSON (mirrors fmt.Errorf %w)",
			err:       fmt.Errorf("outer: %w", errors.New("invalid JSON from lead agent: x")),
			wantOut:   string(stepoutcome.ParseError),
			wantClass: stepoutcome.ClassParseInvalidJSON,
		},
		{
			name:      "unknown → defaults to parse_error",
			err:       errors.New("some new shape of error not yet catalogued"),
			wantOut:   string(stepoutcome.ParseError),
			wantClass: stepoutcome.ClassParseInvalidJSON,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, class := classifyLeadParseError(tc.err)
			assert.Equal(t, tc.wantOut, out)
			assert.Equal(t, tc.wantClass, class)
		})
	}
}

func TestDetectDegenerateLoop(t *testing.T) {
	cases := []struct {
		name       string
		entries    []auditEntryForDetection
		expectLoop bool
	}{
		{
			name:       "empty",
			entries:    nil,
			expectLoop: false,
		},
		{
			name: "two identical — below threshold",
			entries: []auditEntryForDetection{
				{Tool: "file_read", Input: "foo.go"},
				{Tool: "file_read", Input: "foo.go"},
			},
			expectLoop: false,
		},
		{
			name: "three consecutive identical trips detector",
			entries: []auditEntryForDetection{
				{Tool: "file_read", Input: "foo.go"},
				{Tool: "file_read", Input: "foo.go"},
				{Tool: "file_read", Input: "foo.go"},
			},
			expectLoop: true,
		},
		{
			name: "non-consecutive repeats are not a loop",
			entries: []auditEntryForDetection{
				{Tool: "file_read", Input: "foo.go"},
				{Tool: "file_write", Input: "bar.go"},
				{Tool: "file_read", Input: "foo.go"},
				{Tool: "file_write", Input: "bar.go"},
				{Tool: "file_read", Input: "foo.go"},
			},
			expectLoop: false,
		},
		{
			name: "different inputs with same tool are not a loop",
			entries: []auditEntryForDetection{
				{Tool: "file_read", Input: "foo.go"},
				{Tool: "file_read", Input: "bar.go"},
				{Tool: "file_read", Input: "baz.go"},
			},
			expectLoop: false,
		},
		{
			name: "loop later in the sequence still detected",
			entries: []auditEntryForDetection{
				{Tool: "grep", Input: "x"},
				{Tool: "file_read", Input: "a.go"},
				{Tool: "file_read", Input: "b.go"},
				{Tool: "file_read", Input: "b.go"},
				{Tool: "file_read", Input: "b.go"},
			},
			expectLoop: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			detail := detectDegenerateLoop(tc.entries)
			if tc.expectLoop {
				assert.NotEmpty(t, detail, "expected loop detection, got none")
			} else {
				assert.Empty(t, detail, "unexpected loop detection: %s", detail)
			}
		})
	}
}

func TestClassifyGateEvalError(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		wantOut   string
		wantClass string
	}{
		{
			name:      "parse error blames producer",
			err:       errors.New("failed to parse gate input as JSON: unexpected token — agent response was: not-json"),
			wantOut:   string(stepoutcome.ParseError),
			wantClass: stepoutcome.ClassGateInvalidJSON,
		},
		{
			name:      "no matching condition is downstream_rejected",
			err:       errors.New("no gate condition matched (expected one of: [review.approved == true | review.approved == false], got: {\"other\":1})"),
			wantOut:   string(stepoutcome.DownstreamRejected),
			wantClass: stepoutcome.ClassGateEvalFailed,
		},
		{
			name:      "evaluator bug blames the gate itself",
			err:       errors.New("unknown operator ~= in gate condition"),
			wantOut:   string(stepoutcome.GateFailed),
			wantClass: stepoutcome.ClassGateEvalFailed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, class := classifyGateEvalError(tc.err)
			assert.Equal(t, tc.wantOut, out)
			assert.Equal(t, tc.wantClass, class)
		})
	}
}

// TestLeadParseFailureAttribution reproduces the reference incident:
// the lead container exits 0 + returns unparseable JSON; the failure
// should be attributed to the lead step (parse_error), not to the
// plan step as a whole.
//
// We exercise the attribution logic directly rather than running a
// full workflow — the goal is to verify that when the parse failure
// classifier + finalizePendingOutcome are fed the real error shape,
// the resulting row carries the expected outcome and class.
func TestLeadParseFailureAttribution(t *testing.T) {
	repo := newStubStepOutcomeRepo()
	e := &Executor{outcomeRepo: repo, logger: zerolog.Nop()}

	task := &persistence.Task{ID: "task_20260420071127_1f31ae44a2f6dd6d", ProjectID: "p1"}
	exec := &persistence.Execution{ID: "e42"}
	leadStepID := "plan_lead_coordinator"

	// Simulate: executeAgentStep's defer wrote a pending_validation row
	// for the lead step because the container exited cleanly.
	e.recordStepOutcome(context.Background(), task, exec, leadStepID, "lead", "qwen-coder",
		string(stepoutcome.PendingValidation), "", "", nil, nil)

	// Simulate: parsePlanSteps failed with the real "invalid JSON" error.
	// runLeadPlanning wraps it as "could not parse plan from lead output: %w".
	parseErr := fmt.Errorf("could not parse plan from lead output: %w",
		errors.New("invalid JSON from lead agent: unexpected token x at position 0"))
	out, class := classifyLeadParseError(parseErr)
	attrStepID := "plan"
	e.finalizePendingOutcome(context.Background(), exec.ID, leadStepID,
		out, class, parseErr.Error(), &attrStepID)

	// The lead step's row is now terminal with the right labels, not
	// pending and not "ok".
	require.Len(t, repo.rows, 1)
	row := repo.rows[0]
	assert.Equal(t, leadStepID, row.StepID)
	assert.Equal(t, "lead", row.Role)
	assert.Equal(t, "qwen-coder", row.Model)
	assert.Equal(t, string(stepoutcome.ParseError), row.Outcome)
	assert.Equal(t, stepoutcome.ClassParseInvalidJSON, row.ErrorClass)
	require.NotNil(t, row.AttributedToStepID)
	assert.Equal(t, "plan", *row.AttributedToStepID)
	// Error detail preserves the wrapped root-cause chain for debugging.
	assert.Contains(t, row.ErrorDetail, "invalid JSON from lead agent")
}

func TestMissingDeclaredOutputs(t *testing.T) {
	e := &Executor{}
	worktree := t.TempDir()

	// Create one artifact on disk. The other two are claimed but not
	// written — the helper should return both missing paths.
	existing := "artifacts/out/research.md"
	if err := os.MkdirAll(worktree+"/artifacts/out", 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(worktree+"/"+existing, []byte("hello"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	result := []byte(`{
		"status":"COMPLETED",
		"outputArtifacts":[
			{"name":"research","path":"/app/workspace/artifacts/out/research.md"},
			{"name":"scan","path":"/app/workspace/artifacts/out/scan-linkedin-jobs-cz-2026-04-20.md"},
			{"name":"rel","path":"artifacts/out/not-here.md"}
		]
	}`)

	missing := e.missingDeclaredOutputs(result, worktree)
	assert.ElementsMatch(t,
		[]string{
			"/app/workspace/artifacts/out/scan-linkedin-jobs-cz-2026-04-20.md",
			"artifacts/out/not-here.md",
		},
		missing,
		"exactly the declared-but-missing paths should be returned",
	)

	// Zero declared artifacts is a legitimate step outcome.
	assert.Nil(t, e.missingDeclaredOutputs([]byte(`{"status":"COMPLETED"}`), worktree))
	// Empty / non-JSON result: fall through to normal classification.
	assert.Nil(t, e.missingDeclaredOutputs(nil, worktree))
	assert.Nil(t, e.missingDeclaredOutputs([]byte("not-json"), worktree))
}

func TestMissingDeclaredOutputsRejectsEscapingPath(t *testing.T) {
	e := &Executor{}
	worktree := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.md")
	require.NoError(t, os.WriteFile(secret, []byte("secret"), 0o644))

	result := []byte(fmt.Sprintf(`{
		"status":"COMPLETED",
		"outputArtifacts":[{"name":"secret","path":%q}]
	}`, secret))

	missing := e.missingDeclaredOutputs(result, worktree)
	assert.Equal(t, []string{secret}, missing, "absolute host paths outside the workspace must not satisfy declared output checks")
}

// TestBudgetStamp_agentVsNonAgent — migration-106 regression guard
// (instinct ↔ tool-budget seam, Slice 1).
//
// An agent step finalised via recordStepOutcomeWithSignalsAndBudget must
// carry all three budget-stamp columns; a non-agent step written via the
// narrower recordStepOutcomeWithSignals must leave them NULL/zero.
func TestBudgetStamp_agentVsNonAgent(t *testing.T) {
	task := &persistence.Task{ID: "t-budget", ProjectID: "p-budget"}
	exec := &persistence.Execution{ID: "e-budget"}

	t.Run("agent_step_writes_budget_triple", func(t *testing.T) {
		repo := newStubStepOutcomeRepo()
		e := &Executor{outcomeRepo: repo, logger: zerolog.Nop()}

		eff := 120
		used := 43
		stamp := agentBudgetStamp{
			ComplexityTier:      "complex",
			EffectiveToolBudget: &eff,
			ToolCallsUsed:       &used,
		}
		e.recordStepOutcomeWithSignalsAndBudget(
			context.Background(), task, exec,
			"agent-step-1", "coder", "qwen-coder",
			string(stepoutcome.PendingValidation), "", "", nil, nil, nil,
			stamp,
		)

		require.Len(t, repo.rows, 1)
		row := repo.rows[0]
		assert.Equal(t, "complex", row.ComplexityTier, "ComplexityTier must be stamped on agent step")
		require.NotNil(t, row.EffectiveToolBudget, "EffectiveToolBudget must be non-nil on agent step")
		assert.Equal(t, 120, *row.EffectiveToolBudget, "EffectiveToolBudget value mismatch")
		require.NotNil(t, row.ToolCallsUsed, "ToolCallsUsed must be non-nil on agent step")
		assert.Equal(t, 43, *row.ToolCallsUsed, "ToolCallsUsed value mismatch")
	})

	t.Run("non_agent_step_leaves_budget_triple_null", func(t *testing.T) {
		repo := newStubStepOutcomeRepo()
		e := &Executor{outcomeRepo: repo, logger: zerolog.Nop()}

		// system/gate/approval steps use the narrow recorder (no budget stamp).
		e.recordStepOutcome(
			context.Background(), task, exec,
			"gate-step-1", "gate", "",
			string(stepoutcome.PendingValidation), "", "", nil, nil,
		)

		require.Len(t, repo.rows, 1)
		row := repo.rows[0]
		assert.Empty(t, row.ComplexityTier, "ComplexityTier must be empty (NULL) on non-agent step")
		assert.Nil(t, row.EffectiveToolBudget, "EffectiveToolBudget must be nil (NULL) on non-agent step")
		assert.Nil(t, row.ToolCallsUsed, "ToolCallsUsed must be nil (NULL) on non-agent step")
	})
}
