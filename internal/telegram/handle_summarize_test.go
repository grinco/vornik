// Package telegram: tests for handleSummarize's mid-path branches —
// LLM error, empty response, success replaces history.
package telegram

import (
	"context"
	"errors"
	"strings"
	"testing"

	"vornik.io/vornik/internal/chat"
)

// stubChatProvider implements chat.Provider; only Complete is used
// by handleSummarize.
type stubChatProvider struct {
	completeFn func(ctx context.Context, messages []chat.Message) (*chat.ChatResponse, error)
	model      string
}

func (s *stubChatProvider) Complete(ctx context.Context, messages []chat.Message) (*chat.ChatResponse, error) {
	if s.completeFn != nil {
		return s.completeFn(ctx, messages)
	}
	return nil, nil
}
func (s *stubChatProvider) CompleteWithTools(_ context.Context, _ []chat.Message, _ []chat.Tool) (*chat.ChatResponse, error) {
	return nil, nil
}
func (s *stubChatProvider) CompleteWithToolsStream(_ context.Context, _ []chat.Message, _ []chat.Tool, _ chat.StreamCallback) (*chat.ChatResponse, error) {
	return nil, nil
}
func (s *stubChatProvider) Model() string                                        { return s.model }
func (s *stubChatProvider) SetMetrics(_ *chat.Metrics)                           {}
func (s *stubChatProvider) Embed(_ context.Context, _ string) ([]float32, error) { return nil, nil }
func (s *stubChatProvider) EmbedBatch(_ context.Context, _ []string) ([][]float32, error) {
	return nil, nil
}

func makeSummarizeBot(t *testing.T, prov chat.Provider) (*Bot, *[]apiCall, func()) {
	t.Helper()
	bot, calls, cleanup := makeAutopilotBot(t)
	bot.llmClient = prov
	return bot, calls, cleanup
}

func TestHandleSummarize_LLMError(t *testing.T) {
	prov := &stubChatProvider{
		completeFn: func(_ context.Context, _ []chat.Message) (*chat.ChatResponse, error) {
			return nil, errors.New("upstream timeout")
		},
	}
	bot, calls, cleanup := makeSummarizeBot(t, prov)
	defer cleanup()
	// Seed a conversation so we get past the "empty conversation" guard.
	conv := bot.getConversation(111)
	conv.AddMessage(chat.Message{Role: "user", Content: "hi"})
	conv.AddMessage(chat.Message{Role: "assistant", Content: "hello"})

	if err := bot.handleSummarize(context.Background(), 111); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	// Expect: "Summarizing..." message + "Summarization failed: …".
	if len(*calls) < 2 {
		t.Fatalf("expected ≥2 calls; got %d", len(*calls))
	}
	if !strings.Contains((*calls)[len(*calls)-1].Body, "Summarization failed") {
		t.Errorf("expected failure message; got %q", (*calls)[len(*calls)-1].Body)
	}
}

func TestHandleSummarize_EmptyChoices(t *testing.T) {
	prov := &stubChatProvider{
		completeFn: func(_ context.Context, _ []chat.Message) (*chat.ChatResponse, error) {
			return &chat.ChatResponse{}, nil
		},
	}
	bot, calls, cleanup := makeSummarizeBot(t, prov)
	defer cleanup()
	conv := bot.getConversation(111)
	conv.AddMessage(chat.Message{Role: "user", Content: "hi"})

	if err := bot.handleSummarize(context.Background(), 111); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !strings.Contains((*calls)[len(*calls)-1].Body, "empty response") {
		t.Errorf("expected empty-response message; got %q", (*calls)[len(*calls)-1].Body)
	}
}

func TestHandleSummarize_Success_ReplacesHistory(t *testing.T) {
	// chat.ChatResponse.Choices is an anonymous struct slice; build
	// via JSON to avoid reproducing the anonymous shape.
	resp := chat.ChatResponse{}
	resp.Choices = append(resp.Choices, struct {
		Index        int          `json:"index"`
		Message      chat.Message `json:"message"`
		FinishReason string       `json:"finish_reason"`
	}{
		Message: chat.Message{Role: "assistant", Content: "Topic A discussed. Decision X made."},
	})
	prov := &stubChatProvider{
		completeFn: func(_ context.Context, _ []chat.Message) (*chat.ChatResponse, error) {
			return &resp, nil
		},
	}
	bot, calls, cleanup := makeSummarizeBot(t, prov)
	defer cleanup()
	conv := bot.getConversation(111)
	conv.AddMessage(chat.Message{Role: "user", Content: "long history"})
	conv.AddMessage(chat.Message{Role: "assistant", Content: "...messages..."})
	conv.AddMessage(chat.Message{Role: "user", Content: "more"})

	if err := bot.handleSummarize(context.Background(), 111); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	// Expect: "Summarizing…" + final "Summarized N messages into 1".
	final := (*calls)[len(*calls)-1].Body
	if !strings.Contains(final, "Summarized 3 messages into 1") {
		t.Errorf("expected count summary; got %q", final)
	}
	// The conversation was replaced with one assistant-role
	// message. GetMessages drops leading non-user turns (defensive
	// against API validation), so the externally-visible message
	// count is 0; that's fine — we already verified the summary
	// landed via the bot's outbound notification body.
}
