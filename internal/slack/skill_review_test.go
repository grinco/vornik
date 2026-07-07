package slack

import (
	"strings"
	"testing"
)

func TestParseSkillAction(t *testing.T) {
	if ap, id, ok := ParseSkillAction("skill_approve:sk-1"); !ok || !ap || id != "sk-1" {
		t.Fatalf("approve parse: ok=%v approve=%v id=%q", ok, ap, id)
	}
	if ap, id, ok := ParseSkillAction("skill_reject:sk-2"); !ok || ap || id != "sk-2" {
		t.Fatalf("reject parse: ok=%v approve=%v id=%q", ok, ap, id)
	}
	if _, _, ok := ParseSkillAction("project_select:foo"); ok {
		t.Fatalf("non-skill action must not parse as skill")
	}
}

func TestSlackEscape(t *testing.T) {
	got := slackEscape("evil <https://x|click> & <@U1>")
	if got != "evil &lt;https://x|click&gt; &amp; &lt;@U1&gt;" {
		t.Fatalf("slackEscape did not neutralize markup: %q", got)
	}
}

func TestBuildSkillReviewBlocks_EscapesUserContent(t *testing.T) {
	blocks := BuildSkillReviewBlocks([]SkillReviewDraft{
		{ID: "sk-x", Name: "n", Description: "<https://evil|click>"},
	})
	txt := blocks[1]["text"].(map[string]any)["text"].(string)
	if strings.Contains(txt, "<https://evil") {
		t.Fatalf("unescaped user content leaked into mrkdwn: %q", txt)
	}
}

func TestBuildSkillReviewBlocks(t *testing.T) {
	blocks := BuildSkillReviewBlocks([]SkillReviewDraft{
		{ID: "sk-1", Name: "trace-hang", Description: "when a model hangs"},
	})
	// header + section + actions = 3 blocks for one draft.
	if len(blocks) != 3 {
		t.Fatalf("expected 3 blocks (header+section+actions), got %d", len(blocks))
	}
	actions, _ := blocks[2]["elements"].([]map[string]any)
	if len(actions) != 2 {
		t.Fatalf("expected approve+reject buttons, got %d", len(actions))
	}
	if actions[0]["action_id"] != "skill_approve:sk-1" || actions[1]["action_id"] != "skill_reject:sk-1" {
		t.Fatalf("wrong action_ids: %v / %v", actions[0]["action_id"], actions[1]["action_id"])
	}
}
