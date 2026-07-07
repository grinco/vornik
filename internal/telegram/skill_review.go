package telegram

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/skills"
)

// Knowledge-skill approval surface over Telegram (LLD 2026-07-07-
// knowledge-skill-learning-loop-design). Proposed skills are DRAFTS
// until an allowed operator taps Approve/Reject on a batched review
// digest. Buttons ride the standard "skill" callback namespace; the
// decision itself flows through the channel-neutral skills.ApplyDecision
// so Telegram, Slack, and the Web UI can't diverge.

const skillReviewDigestCap = 8 // max drafts surfaced per digest message

// notifiedSkillDrafts tracks draft IDs already surfaced so the periodic
// digest notifies once per new draft rather than re-pinging every tick
// (the operator's anti-spam requirement).
type notifiedSkillDrafts struct {
	mu   sync.Mutex
	seen map[string]bool
}

func (n *notifiedSkillDrafts) freshOnly(ids []string) []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.seen == nil {
		n.seen = make(map[string]bool)
	}
	var fresh []string
	for _, id := range ids {
		if !n.seen[id] {
			fresh = append(fresh, id)
			n.seen[id] = true
		}
	}
	return fresh
}

// isSkillApprover reports whether a Telegram user may approve/reject
// skills. Any allowed operator qualifies — the daemon treats an allowed
// operator as SkillAdmin for skill moderation so non-technical staff
// just tap a button (no key handling).
func (b *Bot) isSkillApprover(userID int64) bool {
	ua, ok := b.config.AllowedUsers[userID]
	return ok && ua.Allowed
}

// handleSkillCallback applies an approve/reject button tap. action is
// "approve" or "reject"; payload is the skill id.
func (b *Bot) handleSkillCallback(ctx context.Context, callbackID string, userID int64, action, payload string) error {
	if b.skillRepo == nil {
		return b.answerCallbackQuery(ctx, callbackID, "Skill review isn't available on this daemon.", true)
	}
	if !b.isSkillApprover(userID) {
		return b.answerCallbackQuery(ctx, callbackID, "You're not authorized to approve skills.", true)
	}
	var d skills.Decision
	switch action {
	case "approve":
		d = skills.Approve
	case "reject":
		d = skills.Reject
	default:
		return b.answerCallbackQuery(ctx, callbackID, "Unrecognised skill action.", true)
	}
	outcome, err := skills.ApplyDecision(ctx, b.skillRepo, payload, d)
	if err != nil {
		b.logger.Warn().Err(err).Str("skill_id", payload).Str("action", action).Msg("skill review: decision failed")
		return b.answerCallbackQuery(ctx, callbackID, "That skill could not be found.", true)
	}
	return b.answerCallbackQuery(ctx, callbackID, "Skill "+outcome+".", false)
}

// buildSkillReviewDigest renders a batched review message + keyboard for
// the given drafts (already capped by the caller). Buttons whose
// callback data would exceed Telegram's limit are silently dropped from
// the keyboard (the skill still shows in the text).
func buildSkillReviewDigest(drafts []*persistence.Skill) (string, InlineKeyboardMarkup) {
	var text strings.Builder
	fmt.Fprintf(&text, "🧠 %d skill(s) awaiting your review:\n", len(drafts))
	var buttons []Button
	for i, s := range drafts {
		fmt.Fprintf(&text, "\n%d. *%s* — %s", i+1, s.Name, s.Description)
		if s.IsGlobal {
			// Blast-radius label: an approved global skill fires in every
			// project, so the approver must see the scope before deciding.
			text.WriteString("\n   ⚠ GLOBAL — affects ALL projects once approved")
		}
		if approve, err := EncodeCallback("skill", "approve", s.ID); err == nil {
			buttons = append(buttons, Button{Text: fmt.Sprintf("✅ %d", i+1), Data: approve})
		}
		if reject, err := EncodeCallback("skill", "reject", s.ID); err == nil {
			buttons = append(buttons, Button{Text: fmt.Sprintf("❌ %d", i+1), Data: reject})
		}
	}
	return text.String(), KeyboardGrid(2, buttons...)
}

// sendSkillReviewDigest surfaces fresh draft skills to every allowed
// operator as one batched message. No-op when nothing is new. Called on
// a periodic (leader-gated) tick.
func (b *Bot) sendSkillReviewDigest(ctx context.Context) {
	if b.skillRepo == nil {
		return
	}
	drafts, err := b.skillRepo.ListDrafts(ctx, skillReviewDigestCap)
	if err != nil || len(drafts) == 0 {
		return
	}
	ids := make([]string, 0, len(drafts))
	for _, s := range drafts {
		ids = append(ids, s.ID)
	}
	if len(b.skillDigestSeen.freshOnly(ids)) == 0 {
		return // nothing new since last digest
	}
	text, markup := buildSkillReviewDigest(drafts)
	for chatID, ua := range b.config.AllowedUsers {
		if !ua.Allowed {
			continue
		}
		if err := b.sendMessageWithMarkup(ctx, chatID, text, &markup); err != nil {
			b.logger.Warn().Err(err).Int64("chat_id", chatID).Msg("skill review: digest send failed")
		}
	}
}
