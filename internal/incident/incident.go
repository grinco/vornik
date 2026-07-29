// Package incident implements the GDPR Article 33/34 personal-data-breach
// ledger: the path from "we became aware" to "the supervisory authority was
// notified", and the documentation obligation that applies whether or not
// anyone was notified.
//
// WHY A LEDGER RATHER THAN A PROCEDURE DOC. Art 33(5) obliges the controller to
// DOCUMENT every personal-data breach — the facts, the effects, and the remedial
// action — including breaches that were correctly not notified. A written
// procedure satisfies nothing on its own; a supervisory authority asks for the
// record. So the record is the feature, and it is satisfied by construction: an
// incident cannot progress without the facts being written down.
//
// WHAT THIS DELIBERATELY DOES NOT DO. It does not decide for you whether a
// breach is notifiable. The design accepted a smaller first slice precisely
// because a wrong automated "no notification needed" is far worse than a prompt
// to a human: it produces a documented, confident, unlawful decision. What is
// automated is the clock, the prompts, and the refusal to let a decision go
// unrecorded.
//
// see LLD § https://docs.vornik.io §4.10
package incident

import (
	"fmt"
	"strings"
	"time"
)

// NotifyDeadline is the Art 33(1) outer limit: notification to the supervisory
// authority without undue delay and, where feasible, not later than 72 hours
// after BECOMING AWARE of the breach.
const NotifyDeadline = 72 * time.Hour

// WarnBefore is how long before the deadline the operator is warned. 24 hours,
// because a notification needs drafting, facts gathering, and a decision — a
// warning at hour 71 is a notification of failure.
const WarnBefore = 24 * time.Hour

// State is where an incident has got to.
type State string

const (
	// StateDetected — recorded, clock running, not yet assessed.
	StateDetected State = "detected"
	// StateAssessed — a human has judged risk to rights and freedoms, and that
	// judgement is recorded with its reasoning.
	StateAssessed State = "assessed"
	// StateNotified — the supervisory authority has been notified.
	StateNotified State = "notified"
	// StateNotNotifiable — assessed as unlikely to result in a risk, so Art 33
	// notification does not apply. This is an OUTCOME with a recorded ground,
	// not an absence of one: Art 33(1) makes notification the default and
	// non-notification the exception.
	StateNotNotifiable State = "not_notifiable"
	// StateClosed — remedial action complete and recorded.
	StateClosed State = "closed"
)

// Incident is one personal-data breach and its Art 33(5) record.
type Incident struct {
	ID    string
	State State

	// OccurredAt is when the breach happened, where known. Often unknown, and
	// deliberately NOT what the clock runs from.
	OccurredAt time.Time
	// BecameAwareAt starts the Art 33(1) 72-hour clock. Kept separate from
	// OccurredAt because conflating them is the classic error in both
	// directions: running the clock from occurrence invents a missed deadline
	// for a breach discovered late, and running it from occurrence when
	// discovery was immediate hides one.
	BecameAwareAt time.Time

	// The Art 33(5) record. Required before an incident may be assessed —
	// a risk judgement with no recorded facts is unauditable.
	Facts    string // what happened, what data, how many subjects
	Effects  string // likely consequences for the data subjects
	Remedial string // measures taken or proposed, including mitigation

	// AuthorityRisk is the Art 33 threshold: is a risk to rights and freedoms
	// likely? Notification is owed unless this is answered "no" WITH a reason.
	AuthorityRisk       string // "", "yes", "no"
	AuthorityRiskReason string
	NotifiedAuthorityAt time.Time
	AuthorityReference  string // the authority's case reference, once known

	// SubjectRisk is the Art 34 threshold, which is HIGHER than Art 33's: is a
	// HIGH risk to rights and freedoms likely? A breach can be notifiable to the
	// authority and not to the subjects, and treating the two as one question is
	// how subjects get either over-alarmed or under-informed.
	SubjectRisk        string // "", "yes", "no"
	SubjectRiskReason  string
	NotifiedSubjectsAt time.Time
	SubjectExemption   string // Art 34(3)(a)/(b)/(c) ground, where relied on

	AssessedBy string
	ClosedAt   time.Time
}

// Deadline is the Art 33(1) 72-hour limit from awareness.
func (i Incident) Deadline() time.Time { return i.BecameAwareAt.Add(NotifyDeadline) }

// live reports whether the incident still needs action on the Art 33 clock.
func (i Incident) live() bool {
	switch i.State {
	case StateNotified, StateNotNotifiable, StateClosed:
		return false
	default:
		return true
	}
}

// Overdue reports whether the 72-hour window has passed without a decision.
//
// "Without a decision" rather than "without a notification": deciding the breach
// is not notifiable, with a recorded ground, discharges Art 33 as fully as
// notifying does.
func (i Incident) Overdue(now time.Time) bool {
	return i.live() && now.After(i.Deadline())
}

// NeedsAttention reports whether the operator should be warned now.
func (i Incident) NeedsAttention(now time.Time) bool {
	return i.live() && now.After(i.Deadline().Add(-WarnBefore))
}

// RecordFacts writes the Art 33(5) documentation.
//
// All three fields are required. Art 33(5) names facts, effects, and remedial
// action specifically, and a record missing one of them does not satisfy the
// obligation however well-intentioned the rest is.
func (i *Incident) RecordFacts(facts, effects, remedial string) error {
	for name, v := range map[string]string{"facts": facts, "effects": effects, "remedial": remedial} {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("incident: %s is required — Art 33(5) obliges documenting the facts, "+
				"the effects, and the remedial action, and a record missing one does not satisfy it", name)
		}
	}
	i.Facts, i.Effects, i.Remedial = facts, effects, remedial
	return nil
}

// Assess records the human risk judgement against both thresholds.
//
// Two separate questions, deliberately:
//
//   - authorityRisk answers Art 33: is a risk to rights and freedoms likely?
//     Notification is owed unless the answer is "no" with a reason, because
//     Art 33(1) makes notification the default and silence the exception.
//   - subjectRisk answers Art 34, whose threshold is HIGH risk. A breach can be
//     notifiable to the authority and not to the subjects; collapsing the two
//     either over-alarms people or leaves them uninformed.
//
// Both answers require a reason even when they are "yes", because Art 33(5)
// needs the assessment recorded, not just its conclusion.
func (i *Incident) Assess(by, authorityRisk, authorityReason, subjectRisk, subjectReason string) error {
	if i.State != StateDetected {
		return fmt.Errorf("incident: cannot assess an incident in state %q", i.State)
	}
	if strings.TrimSpace(i.Facts) == "" {
		return fmt.Errorf("incident: record the Art 33(5) facts before assessing risk — " +
			"a risk judgement with no recorded facts behind it is unauditable")
	}
	if strings.TrimSpace(by) == "" {
		return fmt.Errorf("incident: the assessor must be named — an unattributable risk decision " +
			"cannot be defended later")
	}
	if err := validRisk("authority", authorityRisk, authorityReason); err != nil {
		return err
	}
	if err := validRisk("subject", subjectRisk, subjectReason); err != nil {
		return err
	}
	i.AssessedBy = by
	i.AuthorityRisk, i.AuthorityRiskReason = authorityRisk, authorityReason
	i.SubjectRisk, i.SubjectRiskReason = subjectRisk, subjectReason
	i.State = StateAssessed
	return nil
}

func validRisk(which, answer, reason string) error {
	switch answer {
	case "yes", "no":
	default:
		return fmt.Errorf("incident: %s risk must be answered yes or no (got %q)", which, answer)
	}
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("incident: the %s risk answer needs a reason — Art 33(5) requires the "+
			"assessment to be documented, not just its conclusion", which)
	}
	return nil
}

// MustNotifyAuthority reports whether Art 33 notification is owed.
func (i Incident) MustNotifyAuthority() bool { return i.AuthorityRisk == "yes" }

// MustNotifySubjects reports whether Art 34 communication is owed.
func (i Incident) MustNotifySubjects() bool { return i.SubjectRisk == "yes" }

// NotifyAuthority records notification of the supervisory authority.
func (i *Incident) NotifyAuthority(at time.Time, reference string) error {
	if i.State != StateAssessed {
		return fmt.Errorf("incident: assess the risk before notifying (state %q)", i.State)
	}
	if !i.MustNotifyAuthority() {
		return fmt.Errorf("incident: this incident was assessed as not notifiable; " +
			"re-assess if that judgement has changed rather than notifying against the record")
	}
	i.NotifiedAuthorityAt, i.AuthorityReference = at, reference
	i.State = StateNotified
	return nil
}

// MarkNotNotifiable records the Art 33(1) exception.
//
// Reachable only from Assessed, and only when the assessment actually said no.
// The exception has to rest on the recorded judgement, not on a later shortcut
// past it.
func (i *Incident) MarkNotNotifiable() error {
	if i.State != StateAssessed {
		return fmt.Errorf("incident: assess the risk before concluding it is not notifiable (state %q)", i.State)
	}
	if i.MustNotifyAuthority() {
		return fmt.Errorf("incident: the assessment says a risk IS likely, so Art 33 notification is owed; " +
			"change the assessment if it was wrong rather than overriding its conclusion here")
	}
	i.State = StateNotNotifiable
	return nil
}

// NotifySubjects records Art 34 communication to the data subjects, or the
// Art 34(3) ground relied on instead.
//
// An exemption is an alternative to communicating, so exactly one of the two
// must be supplied: recording neither leaves the obligation open while looking
// handled, and recording both is incoherent.
func (i *Incident) NotifySubjects(at time.Time, exemption string) error {
	if !i.MustNotifySubjects() {
		return fmt.Errorf("incident: subjects were assessed as not at high risk; " +
			"re-assess rather than recording a communication the record does not support")
	}
	hasTime, hasExemption := !at.IsZero(), strings.TrimSpace(exemption) != ""
	if hasTime == hasExemption {
		return fmt.Errorf("incident: supply EITHER the time subjects were communicated with " +
			"OR the Art 34(3) ground relied on instead — recording neither leaves the obligation " +
			"open while appearing handled, and recording both is incoherent")
	}
	i.NotifiedSubjectsAt, i.SubjectExemption = at, exemption
	return nil
}

// Close records that remedial action is complete.
//
// Only from a state where Art 33 has been discharged — notified, or assessed as
// not notifiable. Closing from Detected would file away an undecided breach.
func (i *Incident) Close(at time.Time) error {
	switch i.State {
	case StateNotified, StateNotNotifiable:
	default:
		return fmt.Errorf("incident: cannot close from state %q — Art 33 must be discharged first, "+
			"either by notifying or by recording why notification was not owed", i.State)
	}
	if strings.TrimSpace(i.Remedial) == "" {
		return fmt.Errorf("incident: remedial action must be recorded before closing (Art 33(5))")
	}
	// An Art 34 obligation left open must not be closed over.
	if i.MustNotifySubjects() && i.NotifiedSubjectsAt.IsZero() && strings.TrimSpace(i.SubjectExemption) == "" {
		return fmt.Errorf("incident: subjects were assessed as at HIGH risk but neither a communication " +
			"nor an Art 34(3) exemption is recorded — Art 34 is still open")
	}
	i.State, i.ClosedAt = StateClosed, at
	return nil
}
