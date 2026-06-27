// Learned overlay — Consumer A of the continuous-learning instinct
// layer (slice 3). It augments the static, rule-based corpus above with
// the worker-mined recovery-domain instincts so the failed-task UI and
// the lead's recovery context can show "similar failures here resolved
// by …" alongside the shipped suggestions.
//
// Invariants (LLD "What does NOT change"):
//   - Advisory-only: the overlay returns evidence the operator/lead
//     reads; it NEVER auto-pivots recovery. Behaviour change still goes
//     through the existing operator approval / architect-review gates.
//   - Opt-in: the caller is responsible for the
//     instinct.consumers.failure_playbooks gate. This package stays a
//     pure read overlay; with the gate off the caller never calls it and
//     the static Lookup/All behaviour is byte-for-byte unchanged.
//   - Read-only: it only queries the instinct repository; it never
//     mutates the audit spine.
package playbook

import (
	"context"
	"encoding/json"
	"sort"

	"vornik.io/vornik/internal/persistence"
)

// LearnedRemediation is one worker-mined recovery instinct rendered for
// a surface. It is intentionally a flat, presentation-friendly shape so
// the UI template and the executor prompt builder can render it without
// re-parsing the trigger JSON or knowing the instinct schema.
type LearnedRemediation struct {
	// InstinctID is the source instinct row, carried so the caller can
	// record an InstinctApplication when it surfaces this remediation.
	InstinctID string `json:"instinct_id"`
	// Action is the learned action string ("retrying the step resolved
	// the … failure", "switching off model … resolved the … failure").
	Action string `json:"action"`
	// ErrorClass is the failure class the instinct keys on (from its
	// trigger), echoed so consumers can group without re-parsing.
	ErrorClass string `json:"error_class,omitempty"`
	// Role is the role the instinct keys on, when present.
	Role string `json:"role,omitempty"`
	// Confidence is the materialised Wilson-lower-bound confidence in
	// [0,1]. Surfaces let the operator weigh stronger signals first.
	Confidence float64 `json:"confidence"`
	// SupportCount / ContradictCount are the raw evidence tallies, shown
	// as a "N resolved / M regressed" badge.
	SupportCount    int `json:"support_count"`
	ContradictCount int `json:"contradict_count"`
	// AutoApplied (v2) marks a remediation the auto-apply consumer promoted
	// from advisory to a prompt-level DIRECTIVE — it cleared the confidence
	// floor + error-class allowlist. Set by the executor (not the lister),
	// so the recovery prompt renders it as "apply this" rather than "weigh
	// this", and its application row is recorded 'auto_applied'. Default
	// false = advisory, the unchanged behaviour.
	AutoApplied bool `json:"auto_applied,omitempty"`
}

// learnedTrigger is the minimal view of the instinct trigger_json the
// overlay needs to match on class/role. It mirrors the relevant subset
// of instinct.Trigger without importing that package (which would create
// an import cycle: instinct already imports nothing from playbook, and
// keeping playbook free of instinct keeps the dependency arrow one-way).
type learnedTrigger struct {
	Role       string `json:"role"`
	ErrorClass string `json:"error_class"`
}

// LearnedRemediationLister is the read-only slice of
// persistence.InstinctRepository the overlay needs. Declared here so
// callers can pass the repo directly (it satisfies this interface) and
// tests can inject a tiny fake without standing up a DB.
type LearnedRemediationLister interface {
	List(ctx context.Context, filter persistence.InstinctFilter) ([]*persistence.Instinct, error)
}

// LearnedRemediations returns the top-confidence active/promoted
// recovery-domain instincts that match the given failure class (and,
// when a role is supplied, that role), highest confidence first.
//
// class is the stepoutcome ErrorClass the instinct trigger keys on (the
// same string recorded on execution_step_outcomes and mirrored into
// trigger_json.error_class). An empty class returns no matches — a
// recovery instinct without a class to anchor on isn't actionable here.
//
// project scopes the query; an empty project returns no matches (the
// overlay is project-local — global promotion is a separate slice).
//
// role, when non-empty, filters to instincts whose trigger role matches;
// empty role matches any role (the broader "this class was resolved
// here" view). limit caps the result; <=0 defaults to 3 so the surface
// stays compact.
//
// repo nil yields (nil, nil) so an un-wired daemon degrades to the
// static corpus with no error. The function is read-only.
func LearnedRemediations(ctx context.Context, repo LearnedRemediationLister, class, project, role string, limit int) ([]LearnedRemediation, error) {
	if repo == nil || class == "" || project == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 3
	}

	domain := persistence.InstinctDomainRecovery
	pid := project
	// We pull active AND promoted rows; both are "trusted enough to
	// surface" per the confidence model. List sorts by confidence desc,
	// so we query each status and merge rather than relying on a single
	// status filter (the filter takes one status).
	var rows []*persistence.Instinct
	for _, status := range []string{persistence.InstinctStatusActive, persistence.InstinctStatusPromoted} {
		st := status
		got, err := repo.List(ctx, persistence.InstinctFilter{
			ProjectID: &pid,
			Domain:    &domain,
			Status:    &st,
			// Pull a generous page so class/role filtering in memory has
			// enough candidates; the table is small and the worker keeps
			// it bounded.
			PageSize: 200,
		})
		if err != nil {
			return nil, err
		}
		rows = append(rows, got...)
	}

	out := make([]LearnedRemediation, 0, len(rows))
	for _, in := range rows {
		if in == nil {
			continue
		}
		tr := learnedTrigger{}
		if len(in.Trigger) > 0 {
			// A malformed trigger just means we can't match it — skip
			// rather than error, so one bad row never blanks the overlay.
			_ = json.Unmarshal(in.Trigger, &tr)
		}
		if tr.ErrorClass != class {
			continue
		}
		if role != "" && tr.Role != "" && tr.Role != role {
			continue
		}
		out = append(out, LearnedRemediation{
			InstinctID:      in.ID,
			Action:          in.Action,
			ErrorClass:      tr.ErrorClass,
			Role:            tr.Role,
			Confidence:      in.Confidence,
			SupportCount:    in.SupportCount,
			ContradictCount: in.ContradictCount,
		})
	}

	// Highest confidence first; stable tiebreak on instinct ID so the
	// order is deterministic across calls (the per-status merge above can
	// otherwise interleave equal-confidence rows non-deterministically).
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Confidence != out[j].Confidence {
			return out[i].Confidence > out[j].Confidence
		}
		return out[i].InstinctID < out[j].InstinctID
	})

	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
