package telegram

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

func TestIsSkillApprover(t *testing.T) {
	b := &Bot{config: BotConfig{AllowedUsers: map[int64]UserAccess{
		559741208: {Allowed: true},
		111:       {Allowed: false},
	}}}
	if !b.isSkillApprover(559741208) {
		t.Error("allowed operator must be an approver")
	}
	if b.isSkillApprover(111) {
		t.Error("disallowed user must not approve")
	}
	if b.isSkillApprover(999) {
		t.Error("unknown user must not approve")
	}
}

func TestBuildSkillReviewDigest(t *testing.T) {
	drafts := []*persistence.Skill{
		{ID: "skill-1", Name: "trace-hang", Description: "when a model hangs"},
		{ID: "skill-2", Name: "restart-flow", Description: "safe restart"},
	}
	text, markup := buildSkillReviewDigest(drafts)
	if !strings.Contains(text, "2 skill(s)") || !strings.Contains(text, "trace-hang") || !strings.Contains(text, "restart-flow") {
		t.Fatalf("digest text missing content:\n%s", text)
	}
	// Two buttons (approve+reject) per draft = 4 buttons across the grid.
	count := 0
	for _, row := range markup.InlineKeyboard {
		count += len(row)
	}
	if count != 4 {
		t.Fatalf("expected 4 buttons (approve+reject x2), got %d", count)
	}
}
