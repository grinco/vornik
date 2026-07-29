package incident

import (
	"strings"
	"testing"
	"time"
)

func detected(aware time.Time) Incident {
	return Incident{ID: "inc-1", State: StateDetected, BecameAwareAt: aware}
}

func withFacts(t *testing.T, aware time.Time) Incident {
	t.Helper()
	i := detected(aware)
	if err := i.RecordFacts("a misdirected email exposed 3 addresses",
		"recipients could see each other's addresses", "recalled; recipients asked to delete"); err != nil {
		t.Fatal(err)
	}
	return i
}

// The Art 33(1) clock runs from AWARENESS, not occurrence. Conflating them
// errs in both directions: a breach discovered late gets a deadline that already
// expired, and one discovered immediately gets a deadline that has not started.
func TestDeadlineRunsFromAwarenessNotOccurrence(t *testing.T) {
	occurred := time.Now().Add(-30 * 24 * time.Hour)
	aware := time.Now().Add(-2 * time.Hour)
	i := detected(aware)
	i.OccurredAt = occurred

	if got := i.Deadline(); !got.Equal(aware.Add(NotifyDeadline)) {
		t.Errorf("deadline = %v, want 72h after awareness", got)
	}
	if i.Overdue(time.Now()) {
		t.Error("a breach discovered two hours ago is not overdue, however old the breach is")
	}
}

// All three Art 33(5) elements are named in the article, so a record missing one
// does not satisfy it however good the rest is.
func TestRecordFacts_RequiresAllThreeElements(t *testing.T) {
	i := detected(time.Now())
	for name, call := range map[string]func() error{
		"no facts":    func() error { return i.RecordFacts("", "e", "r") },
		"no effects":  func() error { return i.RecordFacts("f", "  ", "r") },
		"no remedial": func() error { return i.RecordFacts("f", "e", "") },
	} {
		if err := call(); err == nil {
			t.Errorf("%s: expected rejection", name)
		} else if !strings.Contains(err.Error(), "33(5)") {
			t.Errorf("%s: error should cite the article: %v", name, err)
		}
	}
}

// A risk judgement with no recorded facts behind it is unauditable.
func TestAssess_RequiresFactsFirst(t *testing.T) {
	i := detected(time.Now())
	err := i.Assess("vadim", "yes", "addresses exposed", "no", "low sensitivity")
	if err == nil {
		t.Fatal("assessing without facts must be refused")
	}
	if !strings.Contains(err.Error(), "unauditable") {
		t.Errorf("error should explain why: %v", err)
	}
}

// Art 33 (risk) and Art 34 (HIGH risk) are DIFFERENT thresholds. A breach can be
// notifiable to the authority and not to the subjects, and collapsing them into
// one question either over-alarms people or leaves them uninformed.
func TestAssess_TwoSeparateThresholds(t *testing.T) {
	i := withFacts(t, time.Now())
	if err := i.Assess("vadim", "yes", "risk to rights is likely",
		"no", "no high risk: addresses only, no special-category data"); err != nil {
		t.Fatalf("Assess: %v", err)
	}
	if !i.MustNotifyAuthority() {
		t.Error("authority notification should be owed")
	}
	if i.MustNotifySubjects() {
		t.Error("subject communication should NOT be owed at the lower risk finding")
	}
}

// Every answer needs a reason, including "yes": Art 33(5) requires the
// assessment documented, not merely its conclusion.
func TestAssess_EveryAnswerNeedsAReason(t *testing.T) {
	i := withFacts(t, time.Now())
	if err := i.Assess("vadim", "yes", "", "no", "reason"); err == nil {
		t.Error("a yes with no reason must be refused")
	}
	if err := i.Assess("vadim", "no", "reason", "no", "  "); err == nil {
		t.Error("a no with no reason must be refused")
	}
	if err := i.Assess("", "no", "r", "no", "r"); err == nil {
		t.Error("an unattributed assessment must be refused")
	}
	if err := i.Assess("vadim", "maybe", "r", "no", "r"); err == nil {
		t.Error("a non-binary risk answer must be refused")
	}
}

// Non-notification is an OUTCOME with a ground, not an absence of one — Art 33(1)
// makes notification the default. It must rest on the recorded assessment rather
// than bypass it.
func TestMarkNotNotifiable_MustRestOnTheAssessment(t *testing.T) {
	i := withFacts(t, time.Now())
	if err := i.MarkNotNotifiable(); err == nil {
		t.Error("cannot conclude not-notifiable before assessing")
	}
	_ = i.Assess("vadim", "yes", "risk likely", "no", "not high")
	err := i.MarkNotNotifiable()
	if err == nil {
		t.Fatal("cannot mark not-notifiable when the assessment says a risk IS likely")
	}
	if !strings.Contains(err.Error(), "change the assessment") {
		t.Errorf("error should point at the right fix: %v", err)
	}

	// With an assessment that says no, it is the correct terminal state.
	j := withFacts(t, time.Now())
	_ = j.Assess("vadim", "no", "data was already public", "no", "no high risk")
	if err := j.MarkNotNotifiable(); err != nil {
		t.Fatalf("MarkNotNotifiable: %v", err)
	}
	if j.State != StateNotNotifiable {
		t.Errorf("state = %q", j.State)
	}
	// Discharged, so the clock stops — deciding correctly discharges Art 33 as
	// fully as notifying does.
	if j.Overdue(time.Now().Add(100 * time.Hour)) {
		t.Error("a recorded not-notifiable decision discharges Art 33; the clock must stop")
	}
}

func TestNotifyAuthority_OnlyAfterAssessmentAndOnlyIfOwed(t *testing.T) {
	i := withFacts(t, time.Now())
	if err := i.NotifyAuthority(time.Now(), "ref"); err == nil {
		t.Error("notifying before assessing must be refused")
	}
	_ = i.Assess("vadim", "no", "no risk", "no", "no high risk")
	if err := i.NotifyAuthority(time.Now(), "ref"); err == nil {
		t.Error("notifying against a not-notifiable assessment must be refused")
	}

	j := withFacts(t, time.Now())
	_ = j.Assess("vadim", "yes", "risk likely", "no", "not high")
	if err := j.NotifyAuthority(time.Now(), "UOOU-2026-1"); err != nil {
		t.Fatalf("NotifyAuthority: %v", err)
	}
	if j.State != StateNotified || j.AuthorityReference != "UOOU-2026-1" {
		t.Errorf("unexpected: %+v", j)
	}
}

// Communicating with subjects and relying on an Art 34(3) exemption are
// alternatives: neither recorded leaves the obligation open while looking
// handled, and both recorded is incoherent.
func TestNotifySubjects_ExactlyOneOfCommunicationOrExemption(t *testing.T) {
	i := withFacts(t, time.Now())
	_ = i.Assess("vadim", "yes", "risk", "yes", "special-category data exposed")

	if err := i.NotifySubjects(time.Time{}, ""); err == nil {
		t.Error("recording neither must be refused")
	}
	if err := i.NotifySubjects(time.Now(), "34(3)(a) encrypted"); err == nil {
		t.Error("recording both must be refused")
	}
	if err := i.NotifySubjects(time.Now(), ""); err != nil {
		t.Fatalf("a plain communication should be accepted: %v", err)
	}

	// The exemption path alone is equally valid.
	j := withFacts(t, time.Now())
	_ = j.Assess("v", "yes", "r", "yes", "high risk")
	if err := j.NotifySubjects(time.Time{}, "Art 34(3)(a): data was encrypted and keys unaffected"); err != nil {
		t.Fatalf("exemption path: %v", err)
	}
}

// A high-risk incident must not be closed with Art 34 left open.
func TestClose_RefusesWhileArt34IsOpen(t *testing.T) {
	i := withFacts(t, time.Now())
	_ = i.Assess("vadim", "yes", "risk", "yes", "high risk to subjects")
	_ = i.NotifyAuthority(time.Now(), "ref")

	err := i.Close(time.Now())
	if err == nil {
		t.Fatal("closing with subjects un-notified must be refused")
	}
	if !strings.Contains(err.Error(), "Art 34 is still open") {
		t.Errorf("error should name the open obligation: %v", err)
	}

	_ = i.NotifySubjects(time.Now(), "")
	if err := i.Close(time.Now()); err != nil {
		t.Fatalf("Close after both notifications: %v", err)
	}
	if i.State != StateClosed {
		t.Errorf("state = %q", i.State)
	}
}

// Closing from Detected would file away a breach nobody decided about.
func TestClose_RefusesBeforeArt33IsDischarged(t *testing.T) {
	i := withFacts(t, time.Now())
	if err := i.Close(time.Now()); err == nil {
		t.Fatal("closing an undecided incident must be refused")
	}
}

// The warning arrives with a day left, not at hour 71 — a notification needs
// drafting and a decision, so a warning that leaves no time is just an alarm.
func TestDeadlineWarningLeavesTimeToAct(t *testing.T) {
	i := detected(time.Now().Add(-50 * time.Hour))
	if i.Overdue(time.Now()) {
		t.Error("50 hours in is not overdue")
	}
	if !i.NeedsAttention(time.Now()) {
		t.Error("50 hours into a 72-hour clock must already be flagged")
	}

	fresh := detected(time.Now().Add(-2 * time.Hour))
	if fresh.NeedsAttention(time.Now()) {
		t.Error("two hours in should not yet be flagged")
	}

	late := detected(time.Now().Add(-80 * time.Hour))
	if !late.Overdue(time.Now()) {
		t.Error("80 hours in is overdue")
	}
}
