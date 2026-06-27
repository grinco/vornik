package chat

import (
	"testing"
)

func TestNewConversation(t *testing.T) {
	tests := []struct {
		name       string
		id         string
		maxHistory int
		wantMax    int
	}{
		{
			name:       "basic conversation",
			id:         "conv-1",
			maxHistory: 50,
			wantMax:    50,
		},
		{
			name:       "zero max history uses default",
			id:         "conv-2",
			maxHistory: 0,
			wantMax:    100, // default
		},
		{
			name:       "negative max history uses default",
			id:         "conv-3",
			maxHistory: -10,
			wantMax:    100, // default
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conv := NewConversation(tt.id, tt.maxHistory)

			if conv.ID() != tt.id {
				t.Errorf("ID() = %q, want %q", conv.ID(), tt.id)
			}
			if conv.maxHistory != tt.wantMax {
				t.Errorf("maxHistory = %d, want %d", conv.maxHistory, tt.wantMax)
			}
			if conv.Len() != 0 {
				t.Errorf("new conversation should have 0 messages, got %d", conv.Len())
			}
		})
	}
}

func TestConversation_AddMessage(t *testing.T) {
	conv := NewConversation("test", 100)

	msg1 := Message{Role: "user", Content: "Hello"}
	msg2 := Message{Role: "assistant", Content: "Hi there!"}

	conv.AddMessage(msg1)
	if conv.Len() != 1 {
		t.Errorf("Len() = %d, want 1", conv.Len())
	}

	conv.AddMessage(msg2)
	if conv.Len() != 2 {
		t.Errorf("Len() = %d, want 2", conv.Len())
	}

	messages := conv.GetMessages()
	if len(messages) != 2 {
		t.Fatalf("len(messages) = %d, want 2", len(messages))
	}
	if messages[0].Role != msg1.Role || messages[0].Content != msg1.Content {
		t.Errorf("messages[0] = %v, want %v", messages[0], msg1)
	}
	if messages[1].Role != msg2.Role || messages[1].Content != msg2.Content {
		t.Errorf("messages[1] = %v, want %v", messages[1], msg2)
	}
}

func TestConversation_MaxHistory(t *testing.T) {
	maxHistory := 5
	conv := NewConversation("test", maxHistory)

	// Add more messages than maxHistory
	for i := 0; i < 10; i++ {
		conv.AddMessage(Message{
			Role:    "user",
			Content: string(rune('a' + i)),
		})
	}

	if conv.Len() != maxHistory {
		t.Errorf("Len() = %d, want %d", conv.Len(), maxHistory)
	}

	messages := conv.GetMessages()

	// Should have the last 5 messages (f, g, h, i, j)
	expected := []string{"f", "g", "h", "i", "j"}
	for i, msg := range messages {
		if msg.Content != expected[i] {
			t.Errorf("messages[%d].Content = %q, want %q", i, msg.Content, expected[i])
		}
	}
}

func TestConversation_GetMessages(t *testing.T) {
	conv := NewConversation("test", 100)

	// GetMessages on empty conversation should return empty slice
	messages := conv.GetMessages()
	if len(messages) != 0 {
		t.Errorf("GetMessages() on empty conversation returned %d messages", len(messages))
	}

	// Add a message
	conv.AddMessage(Message{Role: "user", Content: "test"})

	// GetMessages should return a copy (modifying it shouldn't affect the conversation)
	messages = conv.GetMessages()
	messages[0].Content = "modified"

	originalMessages := conv.GetMessages()
	if originalMessages[0].Content == "modified" {
		t.Error("GetMessages should return a copy, not a reference")
	}
}

func TestConversation_Clear(t *testing.T) {
	conv := NewConversation("test", 100)

	conv.AddMessage(Message{Role: "user", Content: "Hello"})
	conv.AddMessage(Message{Role: "assistant", Content: "Hi"})

	if conv.Len() != 2 {
		t.Errorf("Len() = %d, want 2 before clear", conv.Len())
	}

	conv.Clear()

	if conv.Len() != 0 {
		t.Errorf("Len() = %d, want 0 after clear", conv.Len())
	}

	messages := conv.GetMessages()
	if len(messages) != 0 {
		t.Errorf("GetMessages() returned %d messages after clear", len(messages))
	}
}

func TestConversationUndoRemovesLastTurn(t *testing.T) {
	conv := NewConversation("test", 100)
	conv.AddMessage(Message{Role: "user", Content: "first"})
	conv.AddMessage(Message{Role: "assistant", Content: "answer"})
	conv.AddMessage(Message{Role: "tool", Content: "tool output"})

	removed := conv.Undo()
	if removed != 3 {
		t.Fatalf("Undo() removed %d messages, want 3", removed)
	}
	if conv.Len() != 0 {
		t.Fatalf("Len() = %d, want 0", conv.Len())
	}
	if got := conv.Undo(); got != 0 {
		t.Fatalf("Undo() on empty conversation = %d, want 0", got)
	}
}

func TestConversationPinnedMessagesSurviveClearAndAreCopied(t *testing.T) {
	conv := NewConversation("test", 100)
	conv.Pin(Message{Role: "system", Content: "project context"})
	conv.AddMessage(Message{Role: "user", Content: "hello"})

	msgs := conv.GetMessages()
	if len(msgs) != 2 || msgs[0].Role != "system" || msgs[1].Role != "user" {
		t.Fatalf("GetMessages() = %#v", msgs)
	}

	conv.Clear()
	msgs = conv.GetMessages()
	if len(msgs) != 1 || msgs[0].Content != "project context" {
		t.Fatalf("pinned message was not retained after Clear: %#v", msgs)
	}

	pinned := conv.PinnedMessages()
	pinned[0].Content = "mutated"
	if got := conv.PinnedMessages()[0].Content; got != "project context" {
		t.Fatalf("PinnedMessages() leaked backing slice, got %q", got)
	}
}

func TestConversation_LastMessage(t *testing.T) {
	t.Run("returns last message", func(t *testing.T) {
		conv := NewConversation("test", 100)
		conv.AddMessage(Message{Role: "user", Content: "first"})
		conv.AddMessage(Message{Role: "assistant", Content: "last"})

		msg, err := conv.LastMessage()
		if err != nil {
			t.Fatalf("LastMessage() error: %v", err)
		}
		if msg.Content != "last" {
			t.Errorf("LastMessage().Content = %q, want %q", msg.Content, "last")
		}
	})

	t.Run("error on empty conversation", func(t *testing.T) {
		conv := NewConversation("test", 100)

		_, err := conv.LastMessage()
		if err == nil {
			t.Error("LastMessage() should return error on empty conversation")
		}
	})
}

func TestConversation_ID(t *testing.T) {
	conv := NewConversation("my-conversation-id", 100)

	if conv.ID() != "my-conversation-id" {
		t.Errorf("ID() = %q, want %q", conv.ID(), "my-conversation-id")
	}
}

func TestConversation_ConcurrentAccess(t *testing.T) {
	conv := NewConversation("test", 100)
	done := make(chan bool)

	// Writer goroutine
	go func() {
		for i := 0; i < 100; i++ {
			conv.AddMessage(Message{Role: "user", Content: string(rune(i))})
		}
		done <- true
	}()

	// Reader goroutine
	go func() {
		for i := 0; i < 100; i++ {
			_ = conv.GetMessages()
			_ = conv.Len()
		}
		done <- true
	}()

	// Wait for both goroutines
	<-done
	<-done
}

// TestConversation_TokenBudgetTrim verifies that AddMessage drops older turns
// when the estimated token count exceeds the configured budget. This is the
// mechanism that prevents a runaway conversation from overflowing the
// upstream gateway's context window.
func TestConversation_TokenBudgetTrim(t *testing.T) {
	conv := NewConversation("t", 1000) // high msg cap so only tokens trim
	conv.SetMaxTokens(50)              // very small budget: ~200 chars

	// Turn 1: ~80 chars, safely under budget.
	conv.AddMessage(Message{Role: "user", Content: shortFiller(80)})
	conv.AddMessage(Message{Role: "assistant", Content: shortFiller(80)})

	if conv.Len() != 2 {
		t.Fatalf("after turn 1: len=%d, want 2", conv.Len())
	}

	// Turn 2: adding ~240 chars pushes total >50 tokens — turn 1 should drop.
	conv.AddMessage(Message{Role: "user", Content: shortFiller(240)})

	msgs := conv.GetMessages()
	if len(msgs) != 1 {
		t.Fatalf("expected one message after trim (current turn only), got %d", len(msgs))
	}
	if msgs[0].Role != "user" {
		t.Errorf("remaining message should be 'user', got %q", msgs[0].Role)
	}
	if len(msgs[0].Content) != 240 {
		t.Errorf("remaining message should be the current (large) user turn, got len=%d", len(msgs[0].Content))
	}
}

// TestConversation_DoesNotDropOnlyTurn confirms that a single oversized turn
// is kept intact even when it alone exceeds the token budget. Dropping it
// would erase the user's current question; instead we rely on the dispatcher
// retry+prune as the last line of defense against upstream rejection.
func TestConversation_DoesNotDropOnlyTurn(t *testing.T) {
	conv := NewConversation("t", 1000)
	conv.SetMaxTokens(10) // ~40 chars

	conv.AddMessage(Message{Role: "user", Content: shortFiller(500)})

	if conv.Len() != 1 {
		t.Errorf("expected the only (oversized) turn to be preserved, got Len=%d", conv.Len())
	}
}

// TestConversation_TokenBudgetDisabled confirms that maxTokens=0 disables
// token-aware trimming and only the message-count cap applies.
func TestConversation_TokenBudgetDisabled(t *testing.T) {
	conv := NewConversation("t", 100)
	// maxTokens not set — token trim should be inert regardless of content size.

	for i := 0; i < 10; i++ {
		conv.AddMessage(Message{Role: "user", Content: shortFiller(10000)})
	}

	if conv.Len() != 10 {
		t.Errorf("with maxTokens=0, no token-based trimming expected; len=%d", conv.Len())
	}
}

// shortFiller returns a string of the requested length composed of printable
// ASCII. Content differs per length to catch accidental dedup.
func shortFiller(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'a' + byte(i%26)
	}
	return string(b)
}
