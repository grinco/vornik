// Package skills holds channel-neutral knowledge-skill business logic
// shared by every approval surface (companion MCP, Telegram, Slack, Web
// UI) so they can't diverge (LLD 2026-07-07-knowledge-skill-learning-
// loop-design). Per-surface authorization is the caller's job; this
// package only applies an already-authorized decision.
package skills

import (
	"context"

	"vornik.io/vornik/internal/persistence"
)

// Decision is an approve/reject verdict from an authorized approver.
type Decision int

const (
	// Approve promotes a draft to active.
	Approve Decision = iota
	// Reject retires a skill.
	Reject
)

// ApplyDecision applies an already-authorized approve/reject to a skill
// and returns the resulting maturity. Idempotent: acting on a skill
// that already holds the target state is a no-op that reports the
// current maturity. Rejecting an active/trusted skill credits a
// "corrected" maturity signal; rejecting a draft does not.
//
// The CALLER must have verified the actor is authorized to moderate
// this skill (an allowed operator / SkillAdmin) before calling.
func ApplyDecision(ctx context.Context, repo persistence.SkillRepository, skillID string, d Decision) (string, error) {
	s, err := repo.GetByID(ctx, skillID)
	if err != nil {
		return "", err
	}
	switch d {
	case Approve:
		if s.Maturity == persistence.SkillMaturityActive || s.Maturity == persistence.SkillMaturityTrusted {
			return s.Maturity, nil // idempotent
		}
		if err := repo.SetMaturity(ctx, skillID, persistence.SkillMaturityActive); err != nil {
			return "", err
		}
		return persistence.SkillMaturityActive, nil
	default: // Reject
		if s.Maturity == persistence.SkillMaturityRetired {
			return persistence.SkillMaturityRetired, nil // idempotent
		}
		if s.Maturity == persistence.SkillMaturityActive || s.Maturity == persistence.SkillMaturityTrusted {
			_ = repo.RecordFeedback(ctx, skillID, persistence.SkillSignalCorrected)
		}
		if err := repo.SetMaturity(ctx, skillID, persistence.SkillMaturityRetired); err != nil {
			return "", err
		}
		return persistence.SkillMaturityRetired, nil
	}
}
