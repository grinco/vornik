package configassist

import (
	"context"
	"fmt"
	"strings"

	"vornik.io/vornik/internal/persistence"
)

// ChatAdapter serves the chat entrypoint (design §6.3.3) for both channels.
//
// It exists so the two channel packages carry NO assistant vocabulary. Slack
// and Telegram each declare a one-method interface and hand it a project, an
// intent and a resolved account id; everything about classes, refusals,
// proposals and verdicts is rendered here. Two renderers for one engine is the
// shape that drifts, and the half that drifts is the one whose channel nobody
// is using that week.
type ChatAdapter struct{ engine *Engine }

// NewChatAdapter wraps an engine for chat use. A nil engine yields a nil
// adapter so the container can wire unconditionally and the channels can test
// the interface for nil, which is how they decide whether the entrypoint
// exists at all.
func NewChatAdapter(e *Engine) *ChatAdapter {
	if e == nil {
		return nil
	}
	return &ChatAdapter{engine: e}
}

// ProposeFromChat runs one request and renders the outcome as chat prose.
//
// It never returns "": every path says something. A chat that goes quiet is
// indistinguishable from a daemon that died, and the person is left holding a
// request whose fate they cannot determine.
//
// The ENTRYPOINT is fixed to chat here rather than taken as an argument. It
// decides the class ceiling (§6.3) and whether the result may auto-apply, and a
// caller that could choose it could choose its own authority.
func (a *ChatAdapter) ProposeFromChat(ctx context.Context, projectID, intent, accountID string) string {
	if a == nil || a.engine == nil {
		return "The configuration assistant isn't available on this deployment."
	}
	res, err := a.engine.Propose(ctx, Request{
		ProjectID:  projectID,
		Intent:     intent,
		Entrypoint: EntrypointChat,
		RequestID:  persistence.GenerateID("careq"),
		Actor: Actor{
			Kind: "human",
			// The RESOLVED account, never the chat handle. §5.4's whole point
			// is that the ledger can say which person asked, and a Slack user
			// id in this field would record the channel rather than the
			// person.
			//
			// AccountID is the STRUCTURED field attribution reads. The
			// adapter received the resolved id and rendered it into the three
			// display strings below without ever setting it, so every
			// chat-door proposal landed with an empty actor_account_id — the
			// one column that answers "which person asked?" (audit
			// 2026-09-15 CA-11). An id inside a principal string is not the
			// column.
			AccountID:    accountID,
			Principal:    "account:" + accountID,
			CredentialID: "account:" + accountID,
			SourceID:     "account:" + accountID,
		},
	})
	if err != nil {
		// A fault, not a refusal. Said differently on purpose: a refusal is
		// about the request and a fault is about this side, and telling
		// someone their request was rejected when the daemon broke sends them
		// to rewrite a request that was fine.
		return "The assistant failed before it could decide anything: " + err.Error() +
			"\n\nNothing was applied. This is a fault on this side, not a problem with what you asked."
	}
	return renderChatResult(res)
}

// renderChatResult turns a Result into the message a person reads.
//
// Separate from the call above so it is testable without an engine — the
// rendering is where the class ceiling's refusal either explains itself or
// does not, and that is worth asserting directly.
func renderChatResult(res *Result) string {
	switch {
	case res == nil:
		return "The assistant returned nothing at all, which is a fault on this side. Nothing was applied."
	case res.Refusal != nil:
		// The engine owns refusal text, including the by-hand remedy the class
		// ceiling names (§6.3). Rendered as-is rather than re-worded: a second
		// wording here would drift from the console's, and the console's is
		// the one the design specifies.
		return res.Refusal.Message
	case res.Proposal == nil:
		return "The assistant finished without filing anything and without refusing, which is a fault " +
			"on this side. Nothing was applied."
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Filed proposal %s", res.Proposal.ID)
	if res.Class != "" {
		fmt.Fprintf(&b, " (class %s)", res.Class)
	}
	b.WriteString(".")
	if s := strings.TrimSpace(res.Summary); s != "" {
		b.WriteString("\n\n" + s)
	}
	// Whether a MEASUREMENT stands behind it, because §4.1's honesty rules are
	// worth as much in chat as on the console: a proposal with nothing behind
	// it must say so rather than borrowing the authority of one that has.
	if !res.HasEvidence {
		b.WriteString("\n\nNo measurement stands behind this — it reflects what you asked for, not " +
			"something the deployment reported.")
	}
	// The chat entrypoint never auto-applies (§6.3, MayAutoApply). Saying so
	// closes the gap between "filed" and "done", which is exactly the gap a
	// person reading a chat reply will otherwise fill in themselves.
	b.WriteString("\n\nNothing has been applied. Review it in the control plane and apply it there.")
	return b.String()
}
