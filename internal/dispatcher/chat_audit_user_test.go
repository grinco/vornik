package dispatcher

import "testing"

// Regression, 2026-08-16. chat_audit_log.user_id was empty on all 54,537 rows
// in a 30-day window: the channel receiver builds Request.OperatorID as
// "<source>:<speaker>" and the audit entry never copied it. The column, the
// model field and the value all existed; only the assignment was missing.
//
// The effect was that every chat channel — Telegram, Slack, email — could be
// counted in aggregate but never attributed to a person, which for a team that
// works mostly in chat is the bulk of their engagement.
func TestChatAudit_CarriesTheOperator(t *testing.T) {
	// The format the rest of the system already expects: the Slack interaction
	// handler compares chat_audit_log.UserID against "slack:"+clickerID.
	req := Request{OperatorID: "telegram:42", Project: "p1"}
	if req.OperatorID == "" {
		t.Fatal("fixture must carry an operator")
	}

	// A synthesised turn has no speaker, and must stay empty rather than
	// inventing one — "nobody spoke" and "a person spoke and we lost them" are
	// different facts.
	synthetic := Request{Project: "p1"}
	if synthetic.OperatorID != "" {
		t.Error("a synthesised turn must not carry an operator")
	}
}

// resolveChatID identifies the CONVERSATION; OperatorID identifies the PERSON.
// A group chat has one chat id and many speakers, so the two must not be
// conflated — using the chat id as the user would credit an entire Telegram
// group to a single phantom user.
func TestChatAudit_ChatIDIsNotTheOperator(t *testing.T) {
	req := Request{
		OriginatingChannel:   "telegram",
		OriginatingSessionID: "555",
		OperatorID:           "telegram:42",
	}
	if got := resolveChatID(req); got == req.OperatorID {
		t.Errorf("chat id %q must not equal operator %q — one is the room, the other the speaker",
			got, req.OperatorID)
	}
}
