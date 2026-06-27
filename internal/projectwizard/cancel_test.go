package projectwizard

import (
	"context"
	"errors"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// pinSession seeds an uncommitted session for the given operator and
// returns its ID. Mirrors pinReadySession but without the
// ready_to_commit/proposal machinery — cancel doesn't need either.
func pinSession(t *testing.T, store *fakeSessionStore, operatorID string) string {
	t.Helper()
	session := &persistence.ProjectWizardSession{
		ID:         persistence.GenerateID("pw"),
		OperatorID: operatorID,
	}
	if err := store.Insert(context.Background(), session); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	return session.ID
}

func TestCancel_OwnedUncommitted_Succeeds(t *testing.T) {
	w, store, _ := newWizardForTest()
	sessionID := pinSession(t, store, "op_1")

	if err := w.Cancel(context.Background(), sessionID, "op_1"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	stored, _ := store.Get(context.Background(), sessionID)
	if stored == nil || stored.CancelledAt == nil {
		t.Fatalf("session not stamped cancelled: %+v", stored)
	}
}

func TestCancel_Idempotent(t *testing.T) {
	w, store, _ := newWizardForTest()
	sessionID := pinSession(t, store, "op_1")

	if err := w.Cancel(context.Background(), sessionID, "op_1"); err != nil {
		t.Fatalf("first Cancel: %v", err)
	}
	// Second cancel on the same session is a no-op success.
	if err := w.Cancel(context.Background(), sessionID, "op_1"); err != nil {
		t.Fatalf("second Cancel should be idempotent, got: %v", err)
	}
}

func TestCancel_CommittedRejected(t *testing.T) {
	w, store, _ := newWizardForTest()
	sessionID := pinReadySession(t, store, "op_1", minimalValidProposal())
	// Stamp the session committed via the store.
	if err := store.CommitTo(context.Background(), sessionID, "test-project"); err != nil {
		t.Fatalf("seed commit: %v", err)
	}

	err := w.Cancel(context.Background(), sessionID, "op_1")
	if !errors.Is(err, ErrSessionCommitted) {
		t.Fatalf("expected ErrSessionCommitted, got: %v", err)
	}
}

func TestCancel_DifferentOperator_NotFound(t *testing.T) {
	w, store, _ := newWizardForTest()
	sessionID := pinSession(t, store, "op_1")

	err := w.Cancel(context.Background(), sessionID, "op_2")
	if !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for cross-operator cancel, got: %v", err)
	}
	// Slot must remain occupied for op_1 (not actually cancelled).
	stored, _ := store.Get(context.Background(), sessionID)
	if stored.CancelledAt != nil {
		t.Fatal("cross-operator cancel must not stamp the session")
	}
}

func TestCancel_Missing_NotFound(t *testing.T) {
	w, _, _ := newWizardForTest()
	err := w.Cancel(context.Background(), "pw_does_not_exist", "op_1")
	if !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got: %v", err)
	}
}

// TestConverse_CapExcludesCancelled verifies the per-operator active
// session cap counts only uncommitted, un-cancelled rows: an operator
// holding MaxActiveSessions cancelled sessions can still start a new
// one.
func TestConverse_CapExcludesCancelled(t *testing.T) {
	w, store, _ := newWizardForTest(chatReply{content: envelopeAskQuestion})
	w.MaxActiveSessions = 3

	// Seed 3 cancelled sessions for op_1 — at the cap if they counted.
	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		s := &persistence.ProjectWizardSession{
			ID:          persistence.GenerateID("pw"),
			OperatorID:  "op_1",
			CancelledAt: &now,
		}
		if err := store.Insert(context.Background(), s); err != nil {
			t.Fatalf("seed cancelled session: %v", err)
		}
	}

	// A fresh session must still be allowed.
	res, err := w.Converse(context.Background(), "", "op_1", "start a new project")
	if err != nil {
		t.Fatalf("Converse should succeed when existing sessions are cancelled, got: %v", err)
	}
	if res == nil || res.SessionID == "" {
		t.Fatal("expected a fresh session ID")
	}
}

// TestConverse_CapCountsActive is the negative control: 3 live
// uncommitted sessions DO hit the cap.
func TestConverse_CapCountsActive(t *testing.T) {
	w, store, _ := newWizardForTest(chatReply{content: envelopeAskQuestion})
	w.MaxActiveSessions = 3

	for i := 0; i < 3; i++ {
		pinSession(t, store, "op_1")
	}

	_, err := w.Converse(context.Background(), "", "op_1", "start a new project")
	if !errors.Is(err, ErrTooManySessions) {
		t.Fatalf("expected ErrTooManySessions, got: %v", err)
	}
}

// TestConverse_CancelledSessionNotConversable verifies a cancelled
// session can't be resumed.
func TestConverse_CancelledSessionNotConversable(t *testing.T) {
	w, store, _ := newWizardForTest(chatReply{content: envelopeAskQuestion})
	sessionID := pinSession(t, store, "op_1")
	if err := w.Cancel(context.Background(), sessionID, "op_1"); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	_, err := w.Converse(context.Background(), sessionID, "op_1", "keep going")
	if !errors.Is(err, ErrSessionCancelled) {
		t.Fatalf("expected ErrSessionCancelled, got: %v", err)
	}
}
