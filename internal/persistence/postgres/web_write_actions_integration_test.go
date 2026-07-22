//go:build integration

package postgres

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// TestIntegration_WebWriteActions drives the supervised web-write lifecycle
// against REAL Postgres (the dedicated throwaway integration DB, NEVER the live
// production database — see connectMigrated):
// pending → approved → submitting → submitted, and asserts the C3 double-submit
// guard — a SECOND CASToSubmitting after the state has already advanced returns
// false. sqlmock cannot validate the guarded-CAS WHERE semantics (it returns a
// canned RowsAffected regardless of the source-state guard), so this pins the
// single-winner invariant end-to-end.
func TestIntegration_WebWriteActions(t *testing.T) {
	ctx := context.Background()
	db := connectMigrated(t).DB
	repo := persistence.NewWebWriteRepo(db)

	mustExec(t, db, "DELETE FROM web_write_actions WHERE submission_id = $1", "s-int-1")
	t.Cleanup(func() {
		mustExec(t, db, "DELETE FROM web_write_actions WHERE submission_id = $1", "s-int-1")
	})

	a := &persistence.WebWriteAction{
		SubmissionID:     "s-int-1",
		ProjectID:        "proj-int",
		TargetURL:        "https://claims.airline.com/x",
		TargetHost:       "claims.airline.com",
		PayloadJSON:      []byte(`{"name":"Ada"}`),
		SelectorBindings: []byte(`{"name":"#name"}`),
		FieldTableJSON:   []byte(`[{"name":"name","value":"Ada"}]`),
		VolatileFields:   []byte(`["csrf"]`),
	}
	if err := repo.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := repo.Get(ctx, "s-int-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "pending" {
		t.Fatalf("fresh row status = %q, want pending", got.Status)
	}

	// pending → approved (stamps token hash + approver + decided_at).
	if err := repo.Approve(ctx, "s-int-1", "hash-abc", "operator@x"); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	got, _ = repo.Get(ctx, "s-int-1")
	if got.Status != "approved" || got.ApprovalTokenHash != "hash-abc" ||
		got.Approver != "operator@x" || !got.DecidedAt.Valid {
		t.Fatalf("post-approve row = %+v", got)
	}

	// approved → submitting: the single winner.
	ok, err := repo.CASToSubmitting(ctx, "s-int-1")
	if err != nil {
		t.Fatalf("CASToSubmitting (winner): %v", err)
	}
	if !ok {
		t.Fatal("first CASToSubmitting should win (true)")
	}
	got, _ = repo.Get(ctx, "s-int-1")
	if got.Status != "submitting" || !got.TokenConsumedAt.Valid {
		t.Fatalf("post-CAS row = %+v (want submitting + token_consumed_at set)", got)
	}

	// C3 double-submit guard: a SECOND CAS finds the row no longer 'approved'
	// and must LOSE.
	ok2, err := repo.CASToSubmitting(ctx, "s-int-1")
	if err != nil {
		t.Fatalf("CASToSubmitting (loser): %v", err)
	}
	if ok2 {
		t.Fatal("second CASToSubmitting must return false (already advanced past approved)")
	}

	// submitting → submitted (stamps submitted_at).
	if err := repo.Finalize(ctx, "s-int-1", "submitted"); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	got, _ = repo.Get(ctx, "s-int-1")
	if got.Status != "submitted" || !got.SubmittedAt.Valid {
		t.Fatalf("post-finalize row = %+v (want submitted + submitted_at set)", got)
	}

	// A Finalize from a terminal state is a no-op guarded CAS → ErrNoTransition.
	if err := repo.Finalize(ctx, "s-int-1", "failed"); err != persistence.ErrNoTransition {
		t.Fatalf("Finalize from terminal state = %v, want ErrNoTransition", err)
	}
}
