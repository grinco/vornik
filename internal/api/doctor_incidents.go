package api

import (
	"context"
	"fmt"
	"strings"
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
		//
		// Neither is a SQLite deployment: the incident ledger has only a
		// Postgres repository, so this is the check the portability rule's
		// escape hatch was written for (doctor design, Extension 2026-09-04
		// E2). What it must NOT do is paste the driver's error in as its
		// verdict — "no such column: state" tells an operator nothing and
		// reads like a defect in the daemon. SKIPPED, with the reason in
		// operator language; the raw error stays in the check's Items for
		// anyone diagnosing.
		msg := "breach ledger not available on this deployment — the Art 33 " +
			"incident store is Postgres-only, and a deployment predating " +
			"migration 143 has no table on any driver"
		if !isMissingRelation(err) {
			msg = fmt.Sprintf("breach ledger not queryable (%v)", err)
		}
		return DoctorCheck{Name: name, Status: "SKIPPED", Message: msg,
			Items: []string{fmt.Sprintf("underlying error: %v", err)}}
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

// isMissingRelation reports whether err is a database saying the table or
// column simply is not there — the expected shape on a pre-migration
// deployment and on a driver this store was never built for. Distinguished
// from a real failure so the latter keeps its detail in the verdict.
func isMissingRelation(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, sig := range []string{
		"no such table",    // sqlite
		"no such column",   // sqlite
		"does not exist",   // postgres: relation/column does not exist
		"undefined_table",  // postgres sqlstate name
		"undefined_column", //
	} {
		if strings.Contains(msg, sig) {
			return true
		}
	}
	return false
}
