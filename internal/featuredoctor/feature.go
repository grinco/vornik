// Package featuredoctor declares vornik's opt-in features and diagnoses
// or guides enabling them. It is the single source of truth for "what
// features exist, how to turn them on, and how to know they work."
// Checks reuse the DoctorCheck idea (inspect live state -> met/unmet +
// remediation) and run server-side because they touch DB/model/config.
package featuredoctor

import (
	"context"

	"vornik.io/vornik/internal/version"
)

// ApplyMechanism is how a gate change takes effect.
type ApplyMechanism int

const (
	ReloadHot       ApplyMechanism = iota // `vornikctl config reload`, no restart
	RestartRequired                       // needs a daemon restart (idle-window gated)
)

// Status is the diagnosed state of a feature.
type Status string

// Gates-off resolves to StatusReady or StatusBlocked (which subsume the
// headline "disabled" notion); StatusDisabled is reserved for future use
// (e.g. an explicit admin-disable that supersedes readiness).
// StatusUnknown is produced when config cannot be read (nil ConfigReader).
const (
	StatusUnknown  Status = "unknown"  // config unavailable; gates/prereqs not evaluated
	StatusDisabled Status = "disabled" // reserved — not currently produced
	StatusReady    Status = "ready"    // gates off, no unfixable prereq unmet -> safe to enable
	StatusBlocked  Status = "blocked"  // gates off, an unfixable prereq unmet
	StatusOK       Status = "ok"       // gates on, prereqs met, verify passes
	StatusDegraded Status = "degraded" // gates on but a prereq unmet or verify fails
)

// Gate is a config key that participates in turning a feature on.
type Gate struct {
	Key      string // dotted path into config.go, e.g. "instinct.consumers.application_feedback"
	EnableTo any    // value that turns it on
}

// PrereqResult is the outcome of one prerequisite or verification check.
type PrereqResult struct {
	OK          bool
	Detail      string
	Fixable     bool   // true => the doctor can resolve it (a config gate); false => operator must act
	Remediation string // shown when !OK
}

// Prereq is a named precondition for safely enabling a feature.
type Prereq struct {
	Name  string
	Check func(ctx context.Context, deps Deps) PrereqResult
}

// VerifyFunc answers "is the enabled feature actually working?".
type VerifyFunc func(ctx context.Context, deps Deps) PrereqResult

// Feature is one declared opt-in capability.
type Feature struct {
	ID      string
	Title   string
	Summary string
	LLDRef  string // https://docs.vornik.io<doc>.md — drift-lint anchors here
	DocRef  string // docs/public/<path>.md — customer-docs coverage anchor; "" => not customer-facing (must be allowlisted)
	Gates   []Gate
	Prereqs []Prereq
	Verify  VerifyFunc
	Apply   ApplyMechanism
	Edition string // version.EditionCommunity | version.EditionEnterprise
}

// IsEnterprise reports whether the feature is Enterprise-only.
func (f Feature) IsEnterprise() bool { return f.Edition == version.EditionEnterprise }
