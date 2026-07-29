package api

import (
	"context"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence/postgres"
)

// checkBreachDeadlines surfaces undischarged Art 33 breach notifications.
//
// The whole failure mode this guards is a clock nobody is watching: GDPR
// Art 33(1) gives 72 hours from BECOMING AWARE of a personal-data breach to
// notify the supervisory authority, and a missed deadline is not recoverable —
// you cannot un-miss it, you can only explain it. So the check warns while there
// is still a day left, and escalates to ERROR once the window has closed.
//
// "Undischarged" rather than "un-notified": recording that a breach is not
// notifiable, with its ground, discharges Art 33 as fully as notifying does.
// An incident sitting in 'detected' is the dangerous state, because nobody has
// decided anything about it yet.
//
// see LLD § https://docs.vornik.io §4.10
func (h *DoctorHandlers) checkBreachDeadlines(ctx context.Context) DoctorCheck {
	name := "breach_deadlines"

	if h.db == nil {
		return DoctorCheck{Name: name, Status: "SKIPPED", Message: "no database handle"}
	}
	repo := postgres.NewIncidentRepository(h.db)
	overdue, soon, err := repo.CountLiveByDeadline(ctx, time.Now())
	if err != nil {
		// A deployment predating migration 143 has no table; that is not a
		// finding, and reporting it as one would train operators to ignore this
		// check.
		return DoctorCheck{Name: name, Status: "SKIPPED",
			Message: fmt.Sprintf("breach ledger not queryable (%v)", err)}
	}

	switch {
	case overdue > 0:
		return DoctorCheck{Name: name, Status: "ERROR",
			Message: fmt.Sprintf("%d personal-data breach(es) past the Art 33(1) 72-hour notification "+
				"deadline with no recorded decision. This cannot be un-missed — notify the supervisory "+
				"authority now, or record why notification is not owed, and document the delay: Art 33(1) "+
				"requires reasons for a late notification. See: vornikctl incident list", overdue)}
	case soon > 0:
		return DoctorCheck{Name: name, Status: "WARNING",
			Message: fmt.Sprintf("%d personal-data breach(es) approaching the Art 33(1) 72-hour deadline. "+
				"Assess and decide now — a notification needs facts gathered and a risk judgement made, "+
				"neither of which compresses well. See: vornikctl incident list", soon)}
	default:
		return DoctorCheck{Name: name, Status: "OK",
			Message: "no personal-data breaches within the Art 33 notification window"}
	}
}
