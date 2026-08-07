package slack

import "vornik.io/vornik/internal/conversation"

// Slack's Block Kit limits. Exceeding either makes chat.postMessage reject the
// ENTIRE message, which would put the operator back where this feature started
// — a checkpoint they never see. So both are enforced by dropping the offending
// button rather than by risking the send: the message text carries the full
// option list regardless, so a dropped button degrades the UX without losing
// the question.
const (
	slackMaxActionElements = 25   // elements in one actions block
	slackMaxButtonValue    = 2000 // characters in a button's value
)

// blockElement is one Block Kit element. Only the button shape is modelled;
// the rest of Block Kit is deliberately absent, as elsewhere in this package.
type blockElement struct {
	Type     string         `json:"type"`
	Text     *blockText     `json:"text,omitempty"`
	Value    string         `json:"value,omitempty"`
	ActionID string         `json:"action_id,omitempty"`
	Elements []blockElement `json:"elements,omitempty"`
}

type blockText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// buildBlocks renders a steering prompt as Block Kit: a section carrying the
// text, then one actions block of buttons.
//
// Buttons are how a Slack operator answers a decision checkpoint. Before this,
// Slack's Send ignored ChannelMessage.Buttons entirely — the field only
// internal/telegram read — so the options existed nowhere the operator could
// reach them (https://docs.vornik.io §v1.2).
//
// Returns nil when there are no buttons, so ordinary replies keep posting as
// plain text on exactly the path they always did.
func buildBlocks(text string, rows [][]conversation.MessageButton) []blockElement {
	elements := make([]blockElement, 0, slackMaxActionElements)
	for _, row := range rows {
		for _, b := range row {
			if len(elements) >= slackMaxActionElements {
				break
			}
			// Slack rejects an empty button text or an over-long value, and it
			// rejects the whole message with it — skip rather than gamble.
			if b.Label == "" || len(b.CallbackData) > slackMaxButtonValue {
				continue
			}
			elements = append(elements, blockElement{
				Type:     "button",
				Text:     &blockText{Type: "plain_text", Text: b.Label},
				Value:    b.CallbackData,
				ActionID: b.CallbackData,
			})
		}
	}
	if len(elements) == 0 {
		return nil
	}
	return []blockElement{
		{Type: "section", Text: &blockText{Type: "mrkdwn", Text: text}},
		{Type: "actions", Elements: elements},
	}
}
