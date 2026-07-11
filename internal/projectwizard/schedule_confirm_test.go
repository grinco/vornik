package projectwizard

import (
	"context"
	"errors"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// TestConfirmSchedule_OwnedUncommitted_Succeeds — the schedule chip's
// confirm POST (design §5.4) stamps ScheduleConfirmedAt/Cron on the
// session so a later turn's scheduleConfirmed check (composer_engine.go)
// can see the operator explicitly approved this cadence.
func TestConfirmSchedule_OwnedUncommitted_Succeeds(t *testing.T) {
	w, store, _ := newWizardForTest()
	sessionID := pinSession(t, store, "op_1")

	if err := w.ConfirmSchedule(context.Background(), sessionID, "op_1", "24h"); err != nil {
		t.Fatalf("ConfirmSchedule: %v", err)
	}

	stored, _ := store.Get(context.Background(), sessionID)
	if stored == nil || stored.ScheduleConfirmedAt == nil {
		t.Fatalf("session not stamped with ScheduleConfirmedAt: %+v", stored)
	}
	if stored.ScheduleConfirmedCron != "24h" {
		t.Errorf("ScheduleConfirmedCron = %q, want 24h", stored.ScheduleConfirmedCron)
	}
}

// TestConfirmSchedule_ReConfirmOverwritesPriorCron — a later turn that
// changes the schedule re-confirms with a new cron value; the stored
// value must reflect the LATEST confirmation, not the first one
// (otherwise schedulesEquivalent would wrongly treat a changed
// cadence as already-confirmed).
func TestConfirmSchedule_ReConfirmOverwritesPriorCron(t *testing.T) {
	w, store, _ := newWizardForTest()
	sessionID := pinSession(t, store, "op_1")

	if err := w.ConfirmSchedule(context.Background(), sessionID, "op_1", "24h"); err != nil {
		t.Fatalf("first ConfirmSchedule: %v", err)
	}
	if err := w.ConfirmSchedule(context.Background(), sessionID, "op_1", "1h"); err != nil {
		t.Fatalf("second ConfirmSchedule: %v", err)
	}

	stored, _ := store.Get(context.Background(), sessionID)
	if stored.ScheduleConfirmedCron != "1h" {
		t.Errorf("ScheduleConfirmedCron = %q, want 1h (latest confirmation)", stored.ScheduleConfirmedCron)
	}
}

func TestConfirmSchedule_EmptyCron_Rejected(t *testing.T) {
	w, store, _ := newWizardForTest()
	sessionID := pinSession(t, store, "op_1")

	err := w.ConfirmSchedule(context.Background(), sessionID, "op_1", "  ")
	if err == nil {
		t.Fatal("expected an error for empty cron")
	}
}

func TestConfirmSchedule_CommittedRejected(t *testing.T) {
	w, store, _ := newWizardForTest()
	sessionID := pinReadySession(t, store, "op_1", minimalValidProposal())
	if err := store.CommitTo(context.Background(), sessionID, "test-project"); err != nil {
		t.Fatalf("seed commit: %v", err)
	}

	err := w.ConfirmSchedule(context.Background(), sessionID, "op_1", "24h")
	if !errors.Is(err, ErrSessionCommitted) {
		t.Fatalf("expected ErrSessionCommitted, got: %v", err)
	}
}

func TestConfirmSchedule_CancelledRejected(t *testing.T) {
	w, store, _ := newWizardForTest()
	sessionID := pinSession(t, store, "op_1")
	if err := w.Cancel(context.Background(), sessionID, "op_1"); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	err := w.ConfirmSchedule(context.Background(), sessionID, "op_1", "24h")
	if !errors.Is(err, ErrSessionCancelled) {
		t.Fatalf("expected ErrSessionCancelled, got: %v", err)
	}
}

func TestConfirmSchedule_DifferentOperator_NotFound(t *testing.T) {
	w, store, _ := newWizardForTest()
	sessionID := pinSession(t, store, "op_1")

	err := w.ConfirmSchedule(context.Background(), sessionID, "op_2", "24h")
	if !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for cross-operator confirm, got: %v", err)
	}
	stored, _ := store.Get(context.Background(), sessionID)
	if stored.ScheduleConfirmedAt != nil {
		t.Fatal("cross-operator confirm must not stamp the session")
	}
}

func TestConfirmSchedule_Missing_NotFound(t *testing.T) {
	w, _, _ := newWizardForTest()
	err := w.ConfirmSchedule(context.Background(), "pw_does_not_exist", "op_1", "24h")
	if !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got: %v", err)
	}
}

func TestConfirmSchedule_NotFullyWired(t *testing.T) {
	var w Wizard // zero-value: no Sessions store wired
	if err := w.ConfirmSchedule(context.Background(), "pw_1", "op_1", "24h"); err == nil {
		t.Fatal("expected an error when the wizard has no session store wired")
	}
}

func TestConfirmSchedule_EmptySessionID_Rejected(t *testing.T) {
	w, _, _ := newWizardForTest()
	if err := w.ConfirmSchedule(context.Background(), "  ", "op_1", "24h"); err == nil {
		t.Fatal("expected an error for an empty session id")
	}
}

func TestConfirmSchedule_EmptyOperatorID_Rejected(t *testing.T) {
	w, store, _ := newWizardForTest()
	sessionID := pinSession(t, store, "op_1")
	if err := w.ConfirmSchedule(context.Background(), sessionID, "  ", "24h"); err == nil {
		t.Fatal("expected an error for an empty operator id")
	}
}

// TestConfirmSchedule_SessionLoadError_Propagates covers the
// store.Get failure path (distinct from Get returning nil/"not
// found") — a genuine infrastructure error must surface, not be
// swallowed as a not-found.
func TestConfirmSchedule_SessionLoadError_Propagates(t *testing.T) {
	w, store, _ := newWizardForTest()
	sessionID := pinSession(t, store, "op_1")
	store.errOn = "Get"

	err := w.ConfirmSchedule(context.Background(), sessionID, "op_1", "24h")
	if err == nil || errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("expected a load-error (not ErrNotFound), got: %v", err)
	}
}

// TestConfirmSchedule_SessionUpdateError_Propagates covers the
// store.Update failure path — a persist failure must surface as an
// error rather than being silently swallowed.
func TestConfirmSchedule_SessionUpdateError_Propagates(t *testing.T) {
	w, store, _ := newWizardForTest()
	sessionID := pinSession(t, store, "op_1")
	store.errOn = "Update"

	if err := w.ConfirmSchedule(context.Background(), sessionID, "op_1", "24h"); err == nil {
		t.Fatal("expected an error when the session store's Update fails")
	}
}
