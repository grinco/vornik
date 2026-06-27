package projectwizard

import (
	"context"
	"errors"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// activeDraftCount counts the operator's uncommitted, un-cancelled
// sessions — the same predicate the projects-page drafts banner uses.
func activeDraftCount(t *testing.T, store *fakeSessionStore, operatorID string) int {
	t.Helper()
	rows, err := store.ListByOperator(context.Background(), operatorID, 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	n := 0
	for _, r := range rows {
		if r != nil && r.CommittedProjectID == nil && r.CancelledAt == nil {
			n++
		}
	}
	return n
}

// TestConverse_NewSessionCancelledOnFailedFirstTurn is the
// counter-climb regression: a brand-new session whose first turn fails
// (e.g. the assistant model hits its token limit) must not linger as an
// active draft. Otherwise every retry — the error path returns no
// session_id, so the client can't reuse the session — orphans another
// draft and the banner counter climbs (1, 2, 3, …).
func TestConverse_NewSessionCancelledOnFailedFirstTurn(t *testing.T) {
	w, store, _ := newWizardForTest(chatReply{err: errors.New("token limit exceeded")})

	_, err := w.Converse(context.Background(), "", "op_1", "build me a competitor tracker")
	if err == nil {
		t.Fatal("expected a converse error when the LLM call fails")
	}
	if got := activeDraftCount(t, store, "op_1"); got != 0 {
		t.Errorf("a failed first turn must leave 0 active drafts (it cancels its own session), got %d", got)
	}

	// Retry also fails → still must not accumulate drafts.
	w2, store2, _ := newWizardForTest(
		chatReply{err: errors.New("token limit exceeded")},
		chatReply{err: errors.New("token limit exceeded")},
	)
	_, _ = w2.Converse(context.Background(), "", "op_1", "try once")
	_, _ = w2.Converse(context.Background(), "", "op_1", "try again")
	if got := activeDraftCount(t, store2, "op_1"); got != 0 {
		t.Errorf("repeated failed first turns must not pile up drafts, got %d", got)
	}
}

// TestConverse_ResumedSessionPreservedOnFailedTurn is the other half:
// a RESUMED session (real prior turns) whose next turn fails must be
// left intact — its work is not discarded just because one turn errored.
func TestConverse_ResumedSessionPreservedOnFailedTurn(t *testing.T) {
	w, store, _ := newWizardForTest(chatReply{err: errors.New("transient boom")})
	existing := &persistence.ProjectWizardSession{
		ID:         persistence.GenerateID("pw"),
		OperatorID: "op_1",
		Transcript: []byte(`[{"role":"user","content":"prior","created_at":"2026-05-31T00:00:00Z"}]`),
	}
	if err := store.Insert(context.Background(), existing); err != nil {
		t.Fatalf("seed: %v", err)
	}

	_, err := w.Converse(context.Background(), existing.ID, "op_1", "continue please")
	if err == nil {
		t.Fatal("expected a converse error")
	}
	if got := activeDraftCount(t, store, "op_1"); got != 1 {
		t.Errorf("a resumed session must survive a failed turn, got %d active drafts", got)
	}
}

// TestConverse_NewSessionKeptOnSuccess guards the happy path: a
// successful first turn leaves the session active (committable later),
// not cancelled by the cleanup defer.
func TestConverse_NewSessionKeptOnSuccess(t *testing.T) {
	w, store, _ := newWizardForTest(chatReply{content: envelopeProposeDraft})
	if _, err := w.Converse(context.Background(), "", "op_1", "a news feed"); err != nil {
		t.Fatalf("converse: %v", err)
	}
	if got := activeDraftCount(t, store, "op_1"); got != 1 {
		t.Errorf("a successful first turn must keep the session active, got %d", got)
	}
}
