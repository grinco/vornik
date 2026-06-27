//go:build smoke_codex

package chat

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// TestCodexSubscriptionSmoke is a manual smoke test guarded by the
// smoke_codex build tag so it doesn't run in CI. Set the env and run:
//
//	SMOKE_CODEX_MODEL=gpt-5.4-mini \
//	  go test -tags smoke_codex -v ./internal/chat/ -run TestCodexSubscriptionSmoke -count=1
//
// Requires ~/.codex/auth.json from a `codex login` session.
func TestCodexSubscriptionSmoke(t *testing.T) {
	model := os.Getenv("SMOKE_CODEX_MODEL")
	if model == "" {
		model = "gpt-5.4-mini"
	}
	c := NewCodexSubscriptionClient(model)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	resp, err := c.Complete(ctx, []Message{
		{Role: "system", Content: "You reply with one lowercase word only."},
		{Role: "user", Content: "reply with ready"},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(resp.Choices) == 0 {
		t.Fatalf("no choices in response: %+v", resp)
	}
	got := resp.Choices[0].Message.Content
	fmt.Printf("model=%s reply=%q prompt_tokens=%d completion_tokens=%d\n",
		resp.Model, got, resp.Usage.PromptTokens, resp.Usage.CompletionTokens)
	if got == "" {
		t.Fatalf("empty reply")
	}
}
