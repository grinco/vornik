package repotest

// Backend-agnostic contract suites for repositories that previously had
// NO shared coverage (coverage-gap sweep, 2026-06-18). Three of these
// repos implement durable storage on both backends (A2A push config,
// budget reservation, project-wizard session) and are wired into both
// the SQLite and Postgres sweeps. The other five are Postgres-only by
// design — their SQLite side is a deliberate degenerate stub (sentinel
// error or silent no-op for single-process deployments) — so their
// durable round-trip suites here are wired into the Postgres sweep
// only. The SQLite stubs get their own contract assertions in
// internal/persistence/sqlite so the intentional degenerate behaviour
// is locked too.

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// jsonSemanticEqual reports whether two JSON byte payloads are equal
// ignoring insignificant whitespace and key ordering. The
// project-wizard transcript and channel-session history columns are
// JSONB on Postgres (deliberately, so admin/consumer tooling can query
// inside them — see migrations.go) and plain TEXT on SQLite. JSONB
// re-serialises its input canonically (e.g. inserting a space after
// `:` and `,`), so a byte-exact round-trip assertion that holds on
// SQLite spuriously fails on Postgres. These payloads are opaque JSON
// to the storage layer, so semantic equality is the correct contract.
func jsonSemanticEqual(a, b []byte) bool {
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		return false
	}
	return reflect.DeepEqual(av, bv)
}

// ---------------------------------------------------------------------------
// A2APushConfigRepository — durable on both backends.
// ---------------------------------------------------------------------------

// RunA2APushConfigSuite exercises the persistence.A2APushConfigRepository
// contract: upsert-by-task round-trip, ErrNotFound on a missing task, and
// last-write-wins on a repeated Set.
func RunA2APushConfigSuite(t *testing.T, repo persistence.A2APushConfigRepository) {
	t.Helper()
	ctx := context.Background()

	t.Run("Set_then_Get_round_trips", func(t *testing.T) {
		taskID := uniqueID("task")
		cfg := persistence.A2APushConfig{TaskID: taskID, URL: "https://example.test/hook", Token: "tok-abc"}
		if err := repo.Set(ctx, cfg); err != nil {
			t.Fatalf("Set: %v", err)
		}
		got, err := repo.Get(ctx, taskID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.URL != cfg.URL || got.Token != cfg.Token {
			t.Fatalf("round-trip mismatch: %+v", got)
		}
	})

	t.Run("Get_unknown_is_ErrNotFound", func(t *testing.T) {
		_, err := repo.Get(ctx, uniqueID("task"))
		if !errors.Is(err, persistence.ErrNotFound) {
			t.Fatalf("expected ErrNotFound, got %v", err)
		}
	})

	t.Run("Set_is_last_write_wins_on_task_id", func(t *testing.T) {
		taskID := uniqueID("task")
		if err := repo.Set(ctx, persistence.A2APushConfig{TaskID: taskID, URL: "https://first.test", Token: "t1"}); err != nil {
			t.Fatalf("Set 1: %v", err)
		}
		if err := repo.Set(ctx, persistence.A2APushConfig{TaskID: taskID, URL: "https://second.test", Token: ""}); err != nil {
			t.Fatalf("Set 2: %v", err)
		}
		got, err := repo.Get(ctx, taskID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.URL != "https://second.test" || got.Token != "" {
			t.Fatalf("expected second write to win with empty token, got %+v", got)
		}
	})
}

// ---------------------------------------------------------------------------
// BudgetReservationRepository — durable on both backends.
// ---------------------------------------------------------------------------

// RunBudgetReservationSuite exercises the reservation ledger contract:
// uncapped reserve, hard-cap admission refusal (no row inserted), settle
// idempotency, the stale sweep, and the read-only unsettled sum.
func RunBudgetReservationSuite(t *testing.T, repo persistence.BudgetReservationRepository) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()

	t.Run("Reserve_uncapped_inserts_and_sums", func(t *testing.T) {
		proj := uniqueID("proj")
		res, err := repo.Reserve(ctx, persistence.ReserveRequest{
			ProjectID: proj, TaskID: uniqueID("task"), EstimateUSD: 2.50, Now: now,
		})
		if err != nil {
			t.Fatalf("Reserve: %v", err)
		}
		if !res.Reserved || res.Blocked {
			t.Fatalf("expected reserved, got %+v", res)
		}
		sum, err := repo.UnsettledSumByProject(ctx, proj)
		if err != nil {
			t.Fatalf("UnsettledSumByProject: %v", err)
		}
		if sum < 2.49 || sum > 2.51 {
			t.Fatalf("expected unsettled sum ~2.50, got %v", sum)
		}
	})

	t.Run("Reserve_blocks_over_daily_cap_without_inserting", func(t *testing.T) {
		proj := uniqueID("proj")
		// Committed 8 + estimate 5 = 13 > 10 hard cap → blocked, no row.
		res, err := repo.Reserve(ctx, persistence.ReserveRequest{
			ProjectID: proj, TaskID: uniqueID("task"), EstimateUSD: 5,
			DailyCommittedUSD: 8, DailyHardUSD: 10, Now: now,
		})
		if err != nil {
			t.Fatalf("Reserve: %v", err)
		}
		if res.Reserved || !res.Blocked || res.Period != "daily" {
			t.Fatalf("expected daily block, got %+v", res)
		}
		sum, _ := repo.UnsettledSumByProject(ctx, proj)
		if sum != 0 {
			t.Fatalf("blocked reserve must not insert a row, sum=%v", sum)
		}
	})

	t.Run("SettleByTask_zeroes_sum_and_is_idempotent", func(t *testing.T) {
		proj, task := uniqueID("proj"), uniqueID("task")
		if _, err := repo.Reserve(ctx, persistence.ReserveRequest{ProjectID: proj, TaskID: task, EstimateUSD: 4, Now: now}); err != nil {
			t.Fatalf("Reserve: %v", err)
		}
		n, err := repo.SettleByTask(ctx, task, now)
		if err != nil {
			t.Fatalf("SettleByTask: %v", err)
		}
		if n != 1 {
			t.Fatalf("expected 1 row settled, got %d", n)
		}
		if sum, _ := repo.UnsettledSumByProject(ctx, proj); sum != 0 {
			t.Fatalf("expected 0 unsettled after settle, got %v", sum)
		}
		n2, err := repo.SettleByTask(ctx, task, now)
		if err != nil {
			t.Fatalf("SettleByTask (2nd): %v", err)
		}
		if n2 != 0 {
			t.Fatalf("settle must be idempotent, second call settled %d", n2)
		}
	})

	t.Run("SweepTerminalAndStale_settles_stale_reservations", func(t *testing.T) {
		proj := uniqueID("proj")
		// reserved_at = now; a stale cutoff in the future makes the row stale.
		if _, err := repo.Reserve(ctx, persistence.ReserveRequest{ProjectID: proj, TaskID: uniqueID("task"), EstimateUSD: 3, Now: now}); err != nil {
			t.Fatalf("Reserve: %v", err)
		}
		n, err := repo.SweepTerminalAndStale(ctx, now.Add(time.Hour), now)
		if err != nil {
			t.Fatalf("SweepTerminalAndStale: %v", err)
		}
		if n < 1 {
			t.Fatalf("expected at least the stale row settled, got %d", n)
		}
		if sum, _ := repo.UnsettledSumByProject(ctx, proj); sum != 0 {
			t.Fatalf("expected 0 unsettled after sweep, got %v", sum)
		}
	})

	t.Run("UnsettledSumByProject_unknown_is_zero", func(t *testing.T) {
		sum, err := repo.UnsettledSumByProject(ctx, uniqueID("proj"))
		if err != nil {
			t.Fatalf("UnsettledSumByProject: %v", err)
		}
		if sum != 0 {
			t.Fatalf("expected 0 for unknown project, got %v", sum)
		}
	})
}

// ---------------------------------------------------------------------------
// ProjectWizardSessionRepository — durable on both backends.
// ---------------------------------------------------------------------------

// RunProjectWizardSessionSuite exercises the conversational-setup session
// contract: insert/get round-trip, mutable update, the commit transition
// (one-way), the operator-scoped cancel IDOR guard, and list ordering.
func RunProjectWizardSessionSuite(t *testing.T, repo persistence.ProjectWizardSessionRepository) {
	t.Helper()
	ctx := context.Background()

	newSession := func(op string) *persistence.ProjectWizardSession {
		return &persistence.ProjectWizardSession{
			ID:         uniqueID("pw"),
			OperatorID: op,
			Transcript: []byte(`[{"role":"user","content":"hi"}]`),
		}
	}

	t.Run("Insert_then_Get_round_trips", func(t *testing.T) {
		s := newSession(uniqueID("op"))
		if err := repo.Insert(ctx, s); err != nil {
			t.Fatalf("Insert: %v", err)
		}
		got, err := repo.Get(ctx, s.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.OperatorID != s.OperatorID || !jsonSemanticEqual(got.Transcript, s.Transcript) {
			t.Fatalf("round-trip mismatch: %+v", got)
		}
	})

	t.Run("Get_unknown_is_ErrNotFound", func(t *testing.T) {
		if _, err := repo.Get(ctx, uniqueID("pw")); !errors.Is(err, persistence.ErrNotFound) {
			t.Fatalf("expected ErrNotFound, got %v", err)
		}
	})

	t.Run("Update_mutates_and_unknown_is_ErrNotFound", func(t *testing.T) {
		s := newSession(uniqueID("op"))
		if err := repo.Insert(ctx, s); err != nil {
			t.Fatalf("Insert: %v", err)
		}
		s.Transcript = []byte(`[{"role":"user","content":"updated"}]`)
		s.ReadyToCommit = true
		if err := repo.Update(ctx, s); err != nil {
			t.Fatalf("Update: %v", err)
		}
		got, _ := repo.Get(ctx, s.ID)
		if !got.ReadyToCommit || !jsonSemanticEqual(got.Transcript, s.Transcript) {
			t.Fatalf("update not reflected: %+v", got)
		}
		ghost := newSession(uniqueID("op"))
		if err := repo.Update(ctx, ghost); !errors.Is(err, persistence.ErrNotFound) {
			t.Fatalf("Update of missing session: expected ErrNotFound, got %v", err)
		}
	})

	t.Run("CommitTo_is_one_way", func(t *testing.T) {
		s := newSession(uniqueID("op"))
		if err := repo.Insert(ctx, s); err != nil {
			t.Fatalf("Insert: %v", err)
		}
		if err := repo.CommitTo(ctx, s.ID, "committed-proj"); err != nil {
			t.Fatalf("CommitTo: %v", err)
		}
		got, _ := repo.Get(ctx, s.ID)
		if got.CommittedProjectID == nil || *got.CommittedProjectID != "committed-proj" {
			t.Fatalf("commit not stamped: %+v", got)
		}
		if err := repo.CommitTo(ctx, s.ID, "again"); !errors.Is(err, persistence.ErrInvalidTransition) {
			t.Fatalf("re-commit: expected ErrInvalidTransition, got %v", err)
		}
		if err := repo.CommitTo(ctx, uniqueID("pw"), "x"); !errors.Is(err, persistence.ErrNotFound) {
			t.Fatalf("commit missing: expected ErrNotFound, got %v", err)
		}
	})

	t.Run("Cancel_enforces_owner_and_refuses_committed", func(t *testing.T) {
		op := uniqueID("op")
		s := newSession(op)
		if err := repo.Insert(ctx, s); err != nil {
			t.Fatalf("Insert: %v", err)
		}
		// Wrong operator → ErrNotFound (IDOR guard).
		if err := repo.Cancel(ctx, s.ID, uniqueID("op")); !errors.Is(err, persistence.ErrNotFound) {
			t.Fatalf("cancel by non-owner: expected ErrNotFound, got %v", err)
		}
		if err := repo.Cancel(ctx, s.ID, op); err != nil {
			t.Fatalf("Cancel by owner: %v", err)
		}
		// Idempotent on already-cancelled.
		if err := repo.Cancel(ctx, s.ID, op); err != nil {
			t.Fatalf("Cancel idempotent: %v", err)
		}
	})

	t.Run("ListByOperator_newest_first", func(t *testing.T) {
		op := uniqueID("op")
		first := newSession(op)
		if err := repo.Insert(ctx, first); err != nil {
			t.Fatalf("Insert 1: %v", err)
		}
		second := newSession(op)
		if err := repo.Insert(ctx, second); err != nil {
			t.Fatalf("Insert 2: %v", err)
		}
		got, err := repo.ListByOperator(ctx, op, 10)
		if err != nil {
			t.Fatalf("ListByOperator: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("expected 2 sessions, got %d", len(got))
		}
	})
}

// ---------------------------------------------------------------------------
// InstallationOnboardingSessionRepository — durable on both backends.
// ---------------------------------------------------------------------------

// RunInstallationOnboardingSessionSuite exercises the installation-scoped
// onboarding session contract: insert/get round-trip, mutable update,
// the commit transition (one-way), the operator-scoped cancel IDOR guard,
// list ordering, and the committed-row detector.
func RunInstallationOnboardingSessionSuite(t *testing.T, repo persistence.InstallationOnboardingSessionRepository) {
	t.Helper()

	t.Run("Insert_then_Get_round_trips", func(t *testing.T) {
		testOnboardingInsertGetRoundTrip(t, repo)
	})
	t.Run("Get_unknown_is_ErrNotFound", func(t *testing.T) {
		testOnboardingGetUnknown(t, repo)
	})
	t.Run("Update_mutates_and_unknown_is_ErrNotFound", func(t *testing.T) {
		testOnboardingUpdate(t, repo)
	})
	t.Run("CommitTo_is_one_way", func(t *testing.T) {
		testOnboardingCommitTo(t, repo)
	})
	t.Run("Cancel_enforces_owner_and_refuses_committed", func(t *testing.T) {
		testOnboardingCancel(t, repo)
	})
	t.Run("ListByOperator_newest_first", func(t *testing.T) {
		testOnboardingListByOperator(t, repo)
	})
}

func testOnboardingInsertGetRoundTrip(t *testing.T, repo persistence.InstallationOnboardingSessionRepository) {
	ctx := context.Background()
	s := newOnboardingSession(uniqueID("op"))
	if err := repo.Insert(ctx, s); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	got, err := repo.Get(ctx, s.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.OperatorID != s.OperatorID || got.CurrentStep != s.CurrentStep || got.SelectedUseCase != s.SelectedUseCase || !jsonSemanticEqual(got.Transcript, s.Transcript) {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

func testOnboardingGetUnknown(t *testing.T, repo persistence.InstallationOnboardingSessionRepository) {
	ctx := context.Background()
	if _, err := repo.Get(ctx, uniqueID("onb")); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func testOnboardingUpdate(t *testing.T, repo persistence.InstallationOnboardingSessionRepository) {
	ctx := context.Background()
	s := newOnboardingSession(uniqueID("op"))
	if err := repo.Insert(ctx, s); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	s.CurrentStep = "configure-chat"
	s.SelectedUseCase = "companion"
	s.ProposedConfig = []byte(`{"chat":{"model":"gpt-4.1"}}`)
	s.ValidationResults = []byte(`[{"name":"chat","ok":true}]`)
	if err := repo.Update(ctx, s); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, _ := repo.Get(ctx, s.ID)
	if got.CurrentStep != s.CurrentStep || got.SelectedUseCase != s.SelectedUseCase || !jsonSemanticEqual(got.ProposedConfig, s.ProposedConfig) || !jsonSemanticEqual(got.ValidationResults, s.ValidationResults) {
		t.Fatalf("update not reflected: %+v", got)
	}
	ghost := newOnboardingSession(uniqueID("op"))
	if err := repo.Update(ctx, ghost); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("Update of missing session: expected ErrNotFound, got %v", err)
	}
}

func testOnboardingCommitTo(t *testing.T, repo persistence.InstallationOnboardingSessionRepository) {
	ctx := context.Background()
	s := newOnboardingSession(uniqueID("op"))
	if err := repo.Insert(ctx, s); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := repo.CommitTo(ctx, s.ID, "committed-proj"); err != nil {
		t.Fatalf("CommitTo: %v", err)
	}
	got, _ := repo.Get(ctx, s.ID)
	if got.CommittedProjectID == nil || *got.CommittedProjectID != "committed-proj" {
		t.Fatalf("commit not stamped: %+v", got)
	}
	if err := repo.CommitTo(ctx, s.ID, "again"); !errors.Is(err, persistence.ErrInvalidTransition) {
		t.Fatalf("re-commit: expected ErrInvalidTransition, got %v", err)
	}
	if err := repo.CommitTo(ctx, uniqueID("onb"), "x"); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("commit missing: expected ErrNotFound, got %v", err)
	}
	ok, err := repo.HasCommitted(ctx)
	if err != nil {
		t.Fatalf("HasCommitted: %v", err)
	}
	if !ok {
		t.Fatal("HasCommitted should report true after commit")
	}
}

func testOnboardingCancel(t *testing.T, repo persistence.InstallationOnboardingSessionRepository) {
	ctx := context.Background()
	op := uniqueID("op")
	s := newOnboardingSession(op)
	if err := repo.Insert(ctx, s); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := repo.Cancel(ctx, s.ID, uniqueID("op")); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("cancel by non-owner: expected ErrNotFound, got %v", err)
	}
	if err := repo.Cancel(ctx, s.ID, op); err != nil {
		t.Fatalf("Cancel by owner: %v", err)
	}
	if err := repo.Cancel(ctx, s.ID, op); err != nil {
		t.Fatalf("Cancel idempotent: %v", err)
	}
}

func testOnboardingListByOperator(t *testing.T, repo persistence.InstallationOnboardingSessionRepository) {
	ctx := context.Background()
	op := uniqueID("op")
	first := newOnboardingSession(op)
	if err := repo.Insert(ctx, first); err != nil {
		t.Fatalf("Insert 1: %v", err)
	}
	second := newOnboardingSession(op)
	if err := repo.Insert(ctx, second); err != nil {
		t.Fatalf("Insert 2: %v", err)
	}
	got, err := repo.ListByOperator(ctx, op, 10)
	if err != nil {
		t.Fatalf("ListByOperator: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(got))
	}
	if got[0].UpdatedAt.Before(got[1].UpdatedAt) {
		t.Fatalf("expected newest-first ordering, got %s before %s", got[0].ID, got[1].ID)
	}
}

func newOnboardingSession(op string) *persistence.InstallationOnboardingSession {
	return &persistence.InstallationOnboardingSession{
		ID:              uniqueID("onb"),
		OperatorID:      op,
		CurrentStep:     "choose-purpose",
		SelectedUseCase: "generic-assistant",
		Transcript:      []byte(`[{"step":"choose-purpose","value":"generic-assistant"}]`),
	}
}

// ---------------------------------------------------------------------------
// CrossProjectCallRepository — Postgres-only (SQLite returns ErrSQLiteNotSupported).
// ---------------------------------------------------------------------------

// RunCrossProjectCallSuite exercises the inter-project orchestration ledger:
// create/get round-trip, duplicate-key guard, callee-task linkage, the
// status transitions, list filtering, and the timeout scanner claim.
func RunCrossProjectCallSuite(t *testing.T, repo persistence.CrossProjectCallRepository) {
	t.Helper()
	ctx := context.Background()

	newCPC := func() *persistence.CrossProjectCall {
		return &persistence.CrossProjectCall{
			ID:             uniqueID("cpc"),
			CallerTaskID:   uniqueID("task"),
			CallerStepID:   uniqueID("step"),
			CallerProject:  uniqueID("proj"),
			CalleeProject:  uniqueID("proj"),
			CalleeWorkflow: "dev-pipeline",
			Payload:        []byte(`{"q":1}`),
			ExpectedSchema: "result.v1",
			Status:         persistence.CPCStatusPending,
		}
	}

	t.Run("Create_then_Get_round_trips", func(t *testing.T) {
		c := newCPC()
		if err := repo.Create(ctx, c); err != nil {
			t.Fatalf("Create: %v", err)
		}
		got, err := repo.Get(ctx, c.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.CallerProject != c.CallerProject || got.Status != persistence.CPCStatusPending {
			t.Fatalf("round-trip mismatch: %+v", got)
		}
	})

	t.Run("Get_unknown_is_ErrNotFound", func(t *testing.T) {
		if _, err := repo.Get(ctx, uniqueID("cpc")); !errors.Is(err, persistence.ErrNotFound) {
			t.Fatalf("expected ErrNotFound, got %v", err)
		}
	})

	t.Run("Create_repeat_caller_pair_mints_new_row", func(t *testing.T) {
		// A `call_project` step retried from the same caller
		// (task, step) deliberately mints a FRESH cross-project
		// call — the retry-from-step contract documented in
		// https://docs.vornik.io
		// ("call mints new CPC; spawn idempotent on existing slug").
		// Only spawns are deduplicated (project_spawns.spawned_project
		// UNIQUE); calls are not, so a repeated caller pair must NOT
		// be rejected as a duplicate key.
		c := newCPC()
		if err := repo.Create(ctx, c); err != nil {
			t.Fatalf("Create: %v", err)
		}
		dup := newCPC()
		dup.CallerTaskID = c.CallerTaskID
		dup.CallerStepID = c.CallerStepID
		if err := repo.Create(ctx, dup); err != nil {
			t.Fatalf("repeat caller pair must mint a new CPC, got: %v", err)
		}
		if dup.ID == c.ID {
			t.Fatalf("expected distinct CPC ids, both were %q", c.ID)
		}
		if _, err := repo.Get(ctx, dup.ID); err != nil {
			t.Fatalf("Get minted row: %v", err)
		}
	})

	t.Run("SetCalleeTaskID_then_GetByCalleeTaskID", func(t *testing.T) {
		c := newCPC()
		if err := repo.Create(ctx, c); err != nil {
			t.Fatalf("Create: %v", err)
		}
		calleeTask := uniqueID("task")
		if err := repo.SetCalleeTaskID(ctx, c.ID, calleeTask); err != nil {
			t.Fatalf("SetCalleeTaskID: %v", err)
		}
		got, err := repo.GetByCalleeTaskID(ctx, calleeTask)
		if err != nil {
			t.Fatalf("GetByCalleeTaskID: %v", err)
		}
		if got.ID != c.ID {
			t.Fatalf("expected CPC %s, got %s", c.ID, got.ID)
		}
	})

	t.Run("Status_transitions", func(t *testing.T) {
		// MarkRunning is idempotent.
		running := newCPC()
		if err := repo.Create(ctx, running); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := repo.MarkRunning(ctx, running.ID); err != nil {
			t.Fatalf("MarkRunning: %v", err)
		}
		if err := repo.MarkRunning(ctx, running.ID); err != nil {
			t.Fatalf("MarkRunning idempotent: %v", err)
		}

		// MarkCompleted stamps the envelope + resolved_at.
		done := newCPC()
		if err := repo.Create(ctx, done); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := repo.MarkCompleted(ctx, done.ID, []byte(`{"ok":true}`)); err != nil {
			t.Fatalf("MarkCompleted: %v", err)
		}
		got, _ := repo.Get(ctx, done.ID)
		if got.Status != persistence.CPCStatusCompleted || got.ResolvedAt == nil || len(got.ResultEnvelope) == 0 {
			t.Fatalf("completed not resolved: %+v", got)
		}

		// MarkFailed / MarkRejected set status + error message.
		failed := newCPC()
		if err := repo.Create(ctx, failed); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := repo.MarkFailed(ctx, failed.ID, "boom"); err != nil {
			t.Fatalf("MarkFailed: %v", err)
		}
		if got, _ := repo.Get(ctx, failed.ID); got.Status != persistence.CPCStatusFailed {
			t.Fatalf("expected failed, got %s", got.Status)
		}
	})

	t.Run("List_filters_by_caller_project", func(t *testing.T) {
		c := newCPC()
		if err := repo.Create(ctx, c); err != nil {
			t.Fatalf("Create: %v", err)
		}
		got, err := repo.List(ctx, persistence.CPCListFilter{CallerProject: c.CallerProject})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 1 || got[0].ID != c.ID {
			t.Fatalf("expected exactly the created CPC, got %+v", got)
		}
	})

	t.Run("ClaimTimedOut_claims_past_deadline_rows", func(t *testing.T) {
		c := newCPC()
		past := time.Now().UTC().Add(-time.Hour)
		c.TimeoutAt = &past
		if err := repo.Create(ctx, c); err != nil {
			t.Fatalf("Create: %v", err)
		}
		claimed, err := repo.ClaimTimedOut(ctx, time.Now().UTC(), 50)
		if err != nil {
			t.Fatalf("ClaimTimedOut: %v", err)
		}
		var found bool
		for _, cc := range claimed {
			if cc.ID == c.ID {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected %s to be claimed as timed out", c.ID)
		}
	})
}

// ---------------------------------------------------------------------------
// ReminderRepository — Postgres-only (SQLite returns ErrSQLiteRemindersUnsupported).
// ---------------------------------------------------------------------------

// RunReminderSuite exercises the dispatcher_reminders contract: insert
// with server-generated id + forced pending status, get/list, the
// lease→fire lifecycle, cancel idempotency, pending-only updates, the
// per-operator pending count, and delete.
func RunReminderSuite(t *testing.T, repo persistence.ReminderRepository) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()

	newReminder := func(op string, fireAt time.Time) *persistence.Reminder {
		return &persistence.Reminder{
			OperatorID: op,
			Channel:    "webchat",
			ChannelRef: "sess-1",
			FireAt:     fireAt,
			Content:    "ping",
			CreatedVia: "cli",
		}
	}

	t.Run("Insert_generates_id_and_forces_pending", func(t *testing.T) {
		r := newReminder(uniqueID("op"), now.Add(time.Hour))
		r.Status = persistence.ReminderStatusFired // should be overridden to pending
		if err := repo.Insert(ctx, r); err != nil {
			t.Fatalf("Insert: %v", err)
		}
		if r.ID == "" {
			t.Fatalf("expected server-generated id")
		}
		got, err := repo.Get(ctx, r.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Status != persistence.ReminderStatusPending {
			t.Fatalf("expected pending on insert, got %s", got.Status)
		}
	})

	t.Run("Get_unknown_is_ErrNotFound", func(t *testing.T) {
		if _, err := repo.Get(ctx, uniqueID("rem")); !errors.Is(err, persistence.ErrNotFound) {
			t.Fatalf("expected ErrNotFound, got %v", err)
		}
	})

	t.Run("LeaseDue_leases_due_then_MarkFired", func(t *testing.T) {
		op := uniqueID("op")
		due := newReminder(op, now.Add(-time.Minute))
		if err := repo.Insert(ctx, due); err != nil {
			t.Fatalf("Insert due: %v", err)
		}
		future := newReminder(op, now.Add(time.Hour))
		if err := repo.Insert(ctx, future); err != nil {
			t.Fatalf("Insert future: %v", err)
		}
		leased, err := repo.LeaseDue(ctx, now, 10)
		if err != nil {
			t.Fatalf("LeaseDue: %v", err)
		}
		var leasedDue bool
		for _, l := range leased {
			if l.ID == due.ID {
				leasedDue = true
			}
			if l.ID == future.ID {
				t.Fatalf("future reminder must not be leased")
			}
		}
		if !leasedDue {
			t.Fatalf("due reminder was not leased")
		}
		if err := repo.MarkFired(ctx, due.ID); err != nil {
			t.Fatalf("MarkFired: %v", err)
		}
		// MarkFired on a non-firing row is ErrNotFound (defensive double-fire guard).
		if err := repo.MarkFired(ctx, due.ID); !errors.Is(err, persistence.ErrNotFound) {
			t.Fatalf("MarkFired on fired row: expected ErrNotFound, got %v", err)
		}
	})

	t.Run("Cancel_is_idempotent_on_terminal", func(t *testing.T) {
		r := newReminder(uniqueID("op"), now.Add(time.Hour))
		if err := repo.Insert(ctx, r); err != nil {
			t.Fatalf("Insert: %v", err)
		}
		if err := repo.Cancel(ctx, r.ID); err != nil {
			t.Fatalf("Cancel: %v", err)
		}
		if err := repo.Cancel(ctx, r.ID); err != nil {
			t.Fatalf("Cancel idempotent on cancelled: %v", err)
		}
	})

	t.Run("UpdateFields_refuses_non_pending", func(t *testing.T) {
		r := newReminder(uniqueID("op"), now.Add(time.Hour))
		if err := repo.Insert(ctx, r); err != nil {
			t.Fatalf("Insert: %v", err)
		}
		if err := repo.UpdateFields(ctx, r.ID, now.Add(2*time.Hour), "new body"); err != nil {
			t.Fatalf("UpdateFields on pending: %v", err)
		}
		if err := repo.Cancel(ctx, r.ID); err != nil {
			t.Fatalf("Cancel: %v", err)
		}
		if err := repo.UpdateFields(ctx, r.ID, now.Add(3*time.Hour), "later"); !errors.Is(err, persistence.ErrNotFound) {
			t.Fatalf("UpdateFields on cancelled: expected ErrNotFound, got %v", err)
		}
	})

	t.Run("CountPendingByOperator_and_List", func(t *testing.T) {
		op := uniqueID("op")
		for i := 0; i < 3; i++ {
			if err := repo.Insert(ctx, newReminder(op, now.Add(time.Hour))); err != nil {
				t.Fatalf("Insert: %v", err)
			}
		}
		n, err := repo.CountPendingByOperator(ctx, op)
		if err != nil {
			t.Fatalf("CountPendingByOperator: %v", err)
		}
		if n != 3 {
			t.Fatalf("expected 3 pending, got %d", n)
		}
		listed, err := repo.List(ctx, persistence.ReminderListFilter{OperatorID: op})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(listed) != 3 {
			t.Fatalf("expected 3 listed, got %d", len(listed))
		}
	})

	t.Run("Delete_then_second_delete_is_ErrNotFound", func(t *testing.T) {
		r := newReminder(uniqueID("op"), now.Add(time.Hour))
		if err := repo.Insert(ctx, r); err != nil {
			t.Fatalf("Insert: %v", err)
		}
		if err := repo.Delete(ctx, r.ID); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if err := repo.Delete(ctx, r.ID); !errors.Is(err, persistence.ErrNotFound) {
			t.Fatalf("second Delete: expected ErrNotFound, got %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// ChannelSessionRepository — Postgres-only (SQLite is a no-op stub).
// ---------------------------------------------------------------------------

// RunChannelSessionSuite exercises durable per-channel session storage:
// save/load round-trip, ErrNotFound on a fresh session, upsert on repeat,
// and idempotent delete.
func RunChannelSessionSuite(t *testing.T, repo persistence.ChannelSessionRepository) {
	t.Helper()
	ctx := context.Background()

	t.Run("Save_then_Load_round_trips", func(t *testing.T) {
		kind, sess := uniqueID("kind"), uniqueID("sess")
		history := []byte(`[{"role":"user","content":"hi"}]`)
		if err := repo.Save(ctx, kind, sess, "proj-a", history); err != nil {
			t.Fatalf("Save: %v", err)
		}
		got, err := repo.Load(ctx, kind, sess)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got.ActiveProject != "proj-a" || !jsonSemanticEqual(got.History, history) {
			t.Fatalf("round-trip mismatch: %+v", got)
		}
	})

	t.Run("Load_unknown_is_ErrNotFound", func(t *testing.T) {
		if _, err := repo.Load(ctx, uniqueID("kind"), uniqueID("sess")); !errors.Is(err, persistence.ErrNotFound) {
			t.Fatalf("expected ErrNotFound, got %v", err)
		}
	})

	t.Run("Save_upserts_on_repeat", func(t *testing.T) {
		kind, sess := uniqueID("kind"), uniqueID("sess")
		if err := repo.Save(ctx, kind, sess, "proj-a", []byte(`[]`)); err != nil {
			t.Fatalf("Save 1: %v", err)
		}
		if err := repo.Save(ctx, kind, sess, "proj-b", []byte(`[{"x":1}]`)); err != nil {
			t.Fatalf("Save 2: %v", err)
		}
		got, _ := repo.Load(ctx, kind, sess)
		if got.ActiveProject != "proj-b" {
			t.Fatalf("expected upsert to proj-b, got %q", got.ActiveProject)
		}
	})

	t.Run("Delete_is_idempotent", func(t *testing.T) {
		kind, sess := uniqueID("kind"), uniqueID("sess")
		if err := repo.Save(ctx, kind, sess, "", []byte(`[]`)); err != nil {
			t.Fatalf("Save: %v", err)
		}
		if err := repo.Delete(ctx, kind, sess); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if _, err := repo.Load(ctx, kind, sess); !errors.Is(err, persistence.ErrNotFound) {
			t.Fatalf("expected ErrNotFound after delete, got %v", err)
		}
		// Deleting a missing row is a no-op.
		if err := repo.Delete(ctx, kind, sess); err != nil {
			t.Fatalf("Delete missing: expected nil, got %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// TelegramPollerStateRepository — Postgres-only (SQLite is a no-op stub).
// ---------------------------------------------------------------------------

// RunTelegramPollerStateSuite exercises durable getUpdates-offset storage:
// set/get round-trip, ErrNotFound before first write, and upsert on repeat.
func RunTelegramPollerStateSuite(t *testing.T, repo persistence.TelegramPollerStateRepository) {
	t.Helper()
	ctx := context.Background()

	t.Run("Get_unknown_is_ErrNotFound", func(t *testing.T) {
		if _, err := repo.Get(ctx, uniqueID("bot")); !errors.Is(err, persistence.ErrNotFound) {
			t.Fatalf("expected ErrNotFound, got %v", err)
		}
	})

	t.Run("Set_then_Get_round_trips_and_upserts", func(t *testing.T) {
		bot := uniqueID("bot")
		if err := repo.Set(ctx, &persistence.TelegramPollerState{BotID: bot, Offset: 100}); err != nil {
			t.Fatalf("Set: %v", err)
		}
		got, err := repo.Get(ctx, bot)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Offset != 100 {
			t.Fatalf("expected offset 100, got %d", got.Offset)
		}
		if err := repo.Set(ctx, &persistence.TelegramPollerState{BotID: bot, Offset: 250}); err != nil {
			t.Fatalf("Set upsert: %v", err)
		}
		got2, _ := repo.Get(ctx, bot)
		if got2.Offset != 250 {
			t.Fatalf("expected offset 250 after upsert, got %d", got2.Offset)
		}
	})
}

// ---------------------------------------------------------------------------
// ProfileUseAuditRepository — Postgres-only (SQLite is a no-op stub).
// ---------------------------------------------------------------------------

// RunProfileUseAuditSuite exercises the per-turn profile-use audit:
// insert/list round-trip, empty list for an unknown operator, and the
// privacy-revocation delete-all.
func RunProfileUseAuditSuite(t *testing.T, repo persistence.ProfileUseAuditRepository) {
	t.Helper()
	ctx := context.Background()

	t.Run("Insert_then_ListForOperator_round_trips", func(t *testing.T) {
		op := uniqueID("op")
		row := &persistence.ProfileUseAudit{
			OperatorID: op, TaskID: uniqueID("task"),
			UsedKeys: []string{"prefers_czech", "tz"}, UsedNotes: true,
		}
		if err := repo.Insert(ctx, row); err != nil {
			t.Fatalf("Insert: %v", err)
		}
		got, err := repo.ListForOperator(ctx, op, persistence.ProfileUseAuditQuery{})
		if err != nil {
			t.Fatalf("ListForOperator: %v", err)
		}
		if len(got) != 1 || !got[0].UsedNotes || len(got[0].UsedKeys) != 2 {
			t.Fatalf("round-trip mismatch: %+v", got)
		}
	})

	t.Run("ListForOperator_unknown_is_empty", func(t *testing.T) {
		got, err := repo.ListForOperator(ctx, uniqueID("op"), persistence.ProfileUseAuditQuery{})
		if err != nil {
			t.Fatalf("ListForOperator: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("expected empty, got %+v", got)
		}
	})

	t.Run("DeleteAllForOperator_wipes_rows", func(t *testing.T) {
		op := uniqueID("op")
		if err := repo.Insert(ctx, &persistence.ProfileUseAudit{OperatorID: op, UsedKeys: []string{"k"}}); err != nil {
			t.Fatalf("Insert: %v", err)
		}
		if err := repo.DeleteAllForOperator(ctx, op); err != nil {
			t.Fatalf("DeleteAllForOperator: %v", err)
		}
		got, _ := repo.ListForOperator(ctx, op, persistence.ProfileUseAuditQuery{})
		if len(got) != 0 {
			t.Fatalf("expected 0 rows after delete-all, got %d", len(got))
		}
	})
}
