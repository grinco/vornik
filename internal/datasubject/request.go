package datasubject

import (
	"fmt"
	"strings"
	"time"
)

// RequestKind is the right being exercised.
type RequestKind string

// The rights a subject may exercise. Portability and access differ in scope
// (Art 20 covers only data the subject provided under consent or contract),
// which is why they are separate kinds rather than one "export".
const (
	RequestAccess        RequestKind = "access"        // Art 15
	RequestRectification RequestKind = "rectification" // Art 16
	RequestErasure       RequestKind = "erasure"       // Art 17
	RequestRestriction   RequestKind = "restriction"   // Art 18
	RequestPortability   RequestKind = "portability"   // Art 20
	RequestObjection     RequestKind = "objection"     // Art 21
)

// RequestState is where a request has got to.
type RequestState string

const (
	// StateOpen — recorded, clock running, identity NOT yet established.
	StateOpen RequestState = "open"
	// StateVerified — the requester's identity is established (Art 12(6)).
	// This is the only state from which data may be produced or erased.
	StateVerified RequestState = "verified"
	// StateActioned — the right has been exercised; the report exists.
	StateActioned RequestState = "actioned"
	// StateRefused — refused with a reason, which is itself a response the
	// subject may challenge. A refusal is an outcome, not an absence of one.
	StateRefused RequestState = "refused"
	// StateClosed — terminal.
	StateClosed RequestState = "closed"
)

// Art 12(3) response deadlines.
const (
	// ResponseDeadline is the one-month baseline.
	ResponseDeadline = 30 * 24 * time.Hour
	// ExtendedDeadline is the maximum with an Art 12(3) extension (three
	// months total), available for complex or numerous requests — and only
	// where the subject was informed of the extension within the first month.
	ExtendedDeadline = 90 * 24 * time.Hour
	// WarnBefore is how long before the deadline an operator should be warned.
	// Nine days: the warning has to leave time to actually DO the work, not
	// merely to notice the miss.
	WarnBefore = 9 * 24 * time.Hour
)

// Request is one rights request and its audit trail.
type Request struct {
	ID        string
	SubjectID string
	Kind      RequestKind
	State     RequestState
	OpenedAt  time.Time

	// VerifiedBy / VerifiedHow record WHO established identity and HOW.
	// Both are required to leave StateOpen: an unattributable verification is
	// indistinguishable from none, and handing an export to an unverified
	// requester is itself a personal-data breach.
	VerifiedBy  string
	VerifiedHow string
	VerifiedAt  time.Time

	// Extended records an Art 12(3) extension. The extension is only lawful if
	// the subject was told about it within the first month, so the reason is
	// recorded for the accountability trail.
	Extended       bool
	ExtendedReason string

	// ReportHash pins the artefact that was produced, so "what did we actually
	// send them" is answerable later (Art 5(2)).
	ReportHash string

	// RefusedReason is required in StateRefused. A refusal with no stated
	// ground is the shape of obstruction, and Art 12(4) requires the subject be
	// told why.
	RefusedReason string

	// ErasureGround is the Art 17(1) limb an erasure request is made under.
	// Captured at INTAKE, because it decides whether a row that also concerns
	// another person may survive redacted or must be deleted outright
	// (design §5.3). Required for RequestErasure and meaningless otherwise —
	// PlanErasure refuses rather than inferring it, since inferring the ground
	// would mean inferring whether a third party's data is destroyed.
	ErasureGround ErasureGround
}

// Deadline is when the Art 12(3) response is due.
func (r Request) Deadline() time.Time {
	if r.Extended {
		return r.OpenedAt.Add(ExtendedDeadline)
	}
	return r.OpenedAt.Add(ResponseDeadline)
}

// Overdue reports whether the response deadline has passed unmet.
func (r Request) Overdue(now time.Time) bool {
	if r.State == StateActioned || r.State == StateRefused || r.State == StateClosed {
		return false
	}
	return now.After(r.Deadline())
}

// NeedsAttention reports whether the operator should be warned. Deliberately
// fires BEFORE the deadline rather than on it: a warning that arrives the day a
// legal deadline expires is a notification of failure, not a prompt to act.
func (r Request) NeedsAttention(now time.Time) bool {
	if r.State == StateActioned || r.State == StateRefused || r.State == StateClosed {
		return false
	}
	return now.After(r.Deadline().Add(-WarnBefore))
}

// MayProduceData reports whether this request has cleared the identity gate.
//
// The gate is the whole point: producing an Art 15 export for an unverified
// requester discloses one person's data to another, so the request mechanism is
// itself a plausible attack on the deployment. Callers MUST consult this before
// assembling an export or performing an erasure.
func (r Request) MayProduceData() bool { return r.State == StateVerified }

// Verify moves a request to StateVerified.
//
// Requires both who verified and how, because an unattributable verification is
// indistinguishable from none — and the accountability question after an
// incident is "who let this through, on what evidence".
func (r *Request) Verify(by, how string, now time.Time) error {
	if r.State != StateOpen {
		return fmt.Errorf("datasubject: cannot verify a request in state %q", r.State)
	}
	if strings.TrimSpace(by) == "" || strings.TrimSpace(how) == "" {
		return fmt.Errorf("datasubject: verification requires both who verified and how " +
			"(an unattributable verification is indistinguishable from none)")
	}
	r.State, r.VerifiedBy, r.VerifiedHow, r.VerifiedAt = StateVerified, by, how, now
	return nil
}

// Action records that the right was exercised, pinning the report produced.
func (r *Request) Action(reportHash string, now time.Time) error {
	if r.State != StateVerified {
		return fmt.Errorf("datasubject: cannot action a request in state %q — "+
			"identity must be established first (Art 12(6))", r.State)
	}
	if strings.TrimSpace(reportHash) == "" {
		return fmt.Errorf("datasubject: actioning a request requires the report hash, " +
			"so what was sent remains answerable (Art 5(2))")
	}
	r.State, r.ReportHash = StateActioned, reportHash
	_ = now
	return nil
}

// Refuse records a refusal and its ground.
//
// Available from Open as well as Verified: the commonest lawful refusal is
// "we cannot identify you from this" under Art 12(6), which by definition
// happens before verification.
func (r *Request) Refuse(reason string) error {
	switch r.State {
	case StateOpen, StateVerified:
	default:
		return fmt.Errorf("datasubject: cannot refuse a request in state %q", r.State)
	}
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("datasubject: a refusal requires a stated ground — " +
			"Art 12(4) obliges telling the subject why, and an unexplained refusal is the shape of obstruction")
	}
	r.State, r.RefusedReason = StateRefused, reason
	return nil
}

// Extend applies the Art 12(3) extension.
//
// Only from a live state, and only with a reason: the extension is lawful only
// for complex or numerous requests AND only where the subject was informed
// within the first month, so an unreasoned extension cannot be justified later.
func (r *Request) Extend(reason string, now time.Time) error {
	switch r.State {
	case StateOpen, StateVerified:
	default:
		return fmt.Errorf("datasubject: cannot extend a request in state %q", r.State)
	}
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("datasubject: an Art 12(3) extension requires a reason (complexity or number of requests)")
	}
	if now.After(r.OpenedAt.Add(ResponseDeadline)) {
		return fmt.Errorf("datasubject: the Art 12(3) extension must be claimed WITHIN the first month " +
			"and the subject informed then; it cannot be applied retroactively to a deadline already missed")
	}
	r.Extended, r.ExtendedReason = true, reason
	return nil
}
