package configassist

import (
	"context"
	"strings"
	"testing"

	"vornik.io/vornik/internal/chat"
)

// Regression: audit 2026-09-15 CA-11 — "The Assistant Does Not Reliably
// Record the Known Account".
//
// The chat adapter is HANDED the resolved account id — the whole point of the
// identity work behind the chat door — and then dropped it into display
// strings without ever setting Actor.AccountID. The proposal ledger's
// actor_account_id came out empty, so the one column that answers "which
// person asked for this?" was blank on exactly the door where a person asked.
//
// A human id embedded inside a principal string is not the structured field
// attribution reads.
func TestChatAdapter_RecordsTheResolvedAccount(t *testing.T) {
	f := newFixture(t)
	f.scriptEdit("projects/assistant.yaml", "cadence: 1h", "cadence: 2h")
	f.scriptJudge("pass", "cadence: 2h")
	f.cfg.ChatEntrypoint = true

	adapter := NewChatAdapter(f.engine)
	out := adapter.ProposeFromChat(context.Background(), "assistant", "slow the feed down", "user_alice")
	if strings.Contains(strings.ToLower(out), "isn't available") {
		t.Fatalf("adapter not wired: %s", out)
	}
	p := f.store.last()
	if p == nil {
		t.Fatalf("no proposal filed: %s", out)
	}
	if p.ActorAccountID != "user_alice" {
		t.Fatalf("actor_account_id = %q, want the resolved account id: the ledger cannot say who asked", p.ActorAccountID)
	}
	if p.ActorKind != "human" {
		t.Fatalf("actor_kind = %q, want human", p.ActorKind)
	}
}

// The system (healing) door has no person behind it, and must NOT invent one:
// an automatic repair attributed to a human reads as somebody's decision.
func TestHealingRequest_RecordsNoAccount(t *testing.T) {
	f := newFixture(t)
	f.assistant.script = []*chat.ChatResponse{
		toolResp("glm-5.2:cloud", `PLAN: ["workflows/w1.md"]`,
			call("c1", "file_edit", map[string]any{"path": "workflows/w1.md", "old_string": "timeout: \"10m\"", "new_string": "timeout: \"15m\""})),
		textResp("glm-5.2:cloud", "Adjusted the workflow."),
	}
	f.scriptJudge("pass", "timeout 15m")
	res, err := f.engine.ProposeForHealing(context.Background(), HealingRequest{ProjectID: "assistant", WorkflowID: "w1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Proposal != nil && res.Proposal.ActorAccountID != "" {
		t.Fatalf("a system repair must not name a person: %q", res.Proposal.ActorAccountID)
	}
}
