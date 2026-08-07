package slack

import (
	"encoding/json"
	"strings"
	"testing"

	"vornik.io/vornik/internal/conversation"
)

// Until now Slack's Send dropped ChannelMessage.Buttons entirely — no render,
// no degradation, no log — which is why a decision checkpoint reached Slack
// with no way to answer it. Buttons must become a Block Kit actions block.
func TestBuildBlocks_RendersOneButtonPerOption(t *testing.T) {
	blocks := buildBlocks("Which blind type?", [][]conversation.MessageButton{
		{{Label: "Roller", CallbackData: "steer:c:task_1:0"}},
		{{Label: "Roman", CallbackData: "steer:c:task_1:1"}},
	})
	raw, err := json.Marshal(blocks)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(raw)

	for _, want := range []string{`"type":"section"`, `"type":"actions"`, `"type":"button"`,
		"Which blind type?", "Roller", "Roman", "steer:c:task_1:0", "steer:c:task_1:1"} {
		if !strings.Contains(got, want) {
			t.Errorf("blocks missing %q:\n%s", want, got)
		}
	}
}

// No buttons → no blocks at all, so an ordinary reply keeps posting as plain
// text exactly as before. Blocks are additive, never a rewrite of the normal path.
func TestBuildBlocks_NilWithoutButtons(t *testing.T) {
	if got := buildBlocks("just text", nil); got != nil {
		t.Errorf("buildBlocks with no buttons = %v, want nil", got)
	}
}

// Slack caps an action block at 25 elements and a button value at 2000 chars.
// Exceeding either makes chat.postMessage reject the WHOLE message — the
// operator would get silence again, the exact failure this fixes. Drop the
// offending button rather than lose the message; the text still carries the
// full option list.
func TestBuildBlocks_RespectsSlackLimits(t *testing.T) {
	rows := make([][]conversation.MessageButton, 0, 40)
	for i := 0; i < 40; i++ {
		rows = append(rows, []conversation.MessageButton{{Label: "opt", CallbackData: "steer:c:t:1"}})
	}
	blocks := buildBlocks("q", rows)
	raw, _ := json.Marshal(blocks)
	if n := strings.Count(string(raw), `"type":"button"`); n > slackMaxActionElements {
		t.Errorf("rendered %d buttons, want <= %d", n, slackMaxActionElements)
	}

	over := strings.Repeat("x", slackMaxButtonValue+1)
	blocks = buildBlocks("q", [][]conversation.MessageButton{
		{{Label: "ok", CallbackData: "short"}},
		{{Label: "too big", CallbackData: over}},
	})
	raw, _ = json.Marshal(blocks)
	if strings.Contains(string(raw), over) {
		t.Error("an over-long button value must be dropped, not sent")
	}
	if !strings.Contains(string(raw), "short") {
		t.Error("dropping one oversized button must not discard the valid ones")
	}
}

// Slack rejects a button with an empty text field, which would again cost the
// whole message.
func TestBuildBlocks_SkipsEmptyLabels(t *testing.T) {
	blocks := buildBlocks("q", [][]conversation.MessageButton{
		{{Label: "", CallbackData: "steer:c:t:0"}},
		{{Label: "Roman", CallbackData: "steer:c:t:1"}},
	})
	raw, _ := json.Marshal(blocks)
	if n := strings.Count(string(raw), `"type":"button"`); n != 1 {
		t.Errorf("rendered %d buttons, want 1 (the empty label is skipped):\n%s", n, raw)
	}
}
