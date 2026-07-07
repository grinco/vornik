package slack

import "strings"

// Slack knowledge-skill review primitives (LLD 2026-07-07-knowledge-
// skill-learning-loop-design, approval surface).
//
// These build the interactive review message (Block Kit) and parse the
// block_actions callback, reusing the SAME channel-neutral decision the
// Telegram + Web-UI + MCP surfaces use (internal/skills.ApplyDecision).
//
// WIRING STILL REQUIRED before Slack approvals are live (deliberately
// NOT auto-wired — an ungated Slack approve would let anyone in a
// channel activate skills into swarm roles):
//   1. Operator sets the Slack app's Interactivity Request URL to the
//      daemon's /api/v1/slack/webhook (Slack posts block_actions there
//      as an x-www-form-urlencoded `payload=` field).
//   2. A Slack approver-allowlist (which Slack user IDs map to
//      SkillAdmin) — Slack today carries channel allowlists, not an
//      operator→approver map. The inbound handler must gate on that
//      before calling ApplyDecision.

// SkillReviewDraft is the minimal projection the block builder needs.
type SkillReviewDraft struct {
	ID          string
	Name        string
	Description string
}

// BuildSkillReviewBlocks renders a Block Kit message: a header + one
// section-with-buttons per draft. action_id encodes the decision + id
// (skill_approve:<id> / skill_reject:<id>), parsed by ParseSkillAction.
func BuildSkillReviewBlocks(drafts []SkillReviewDraft) []map[string]any {
	blocks := []map[string]any{
		{
			"type": "section",
			"text": map[string]any{"type": "mrkdwn", "text": "*Knowledge skills awaiting review*"},
		},
	}
	for _, d := range drafts {
		blocks = append(blocks,
			map[string]any{
				"type": "section",
				"text": map[string]any{"type": "mrkdwn", "text": "*" + d.Name + "* — " + d.Description},
			},
			map[string]any{
				"type": "actions",
				"elements": []map[string]any{
					{
						"type":      "button",
						"text":      map[string]any{"type": "plain_text", "text": "✅ Approve"},
						"style":     "primary",
						"action_id": "skill_approve:" + d.ID,
						"value":     d.ID,
					},
					{
						"type":      "button",
						"text":      map[string]any{"type": "plain_text", "text": "❌ Reject"},
						"style":     "danger",
						"action_id": "skill_reject:" + d.ID,
						"value":     d.ID,
					},
				},
			},
		)
	}
	return blocks
}

// ParseSkillAction decodes a block_actions action_id into (approve,
// skillID). ok is false when the action_id isn't a skill action.
func ParseSkillAction(actionID string) (approve bool, skillID string, ok bool) {
	switch {
	case strings.HasPrefix(actionID, "skill_approve:"):
		return true, strings.TrimPrefix(actionID, "skill_approve:"), true
	case strings.HasPrefix(actionID, "skill_reject:"):
		return false, strings.TrimPrefix(actionID, "skill_reject:"), true
	default:
		return false, "", false
	}
}
