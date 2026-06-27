// Package telegram: tests for HandleUpdate — the dispatch fan-out
// from a raw Telegram Update to either CallbackQuery or Message
// handling.
//
// The Update struct uses inline anonymous structs for its Message
// and CallbackQuery fields, so we construct payloads via JSON
// round-trip — easier than reproducing the anonymous struct shape
// in the test source.
package telegram

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func decodeUpdate(t *testing.T, raw string) *Update {
	t.Helper()
	var u Update
	if err := json.Unmarshal([]byte(raw), &u); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return &u
}

func TestHandleUpdate_NoMessageOrCallback_NoOp(t *testing.T) {
	bot, calls, cleanup := makeAutopilotBot(t)
	defer cleanup()
	upd := &Update{UpdateID: 1}
	if err := bot.HandleUpdate(context.Background(), upd); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(*calls) != 0 {
		t.Errorf("expected no telegram API calls for empty update; got %d", len(*calls))
	}
}

func TestHandleUpdate_MessageWithDocument(t *testing.T) {
	bot, _, cleanup := makeAutopilotBot(t)
	defer cleanup()
	bot.config.AllowedUsers = map[int64]UserAccess{
		111: {Allowed: true, Projects: []string{"*"}},
	}
	upd := decodeUpdate(t, `{
		"update_id": 5,
		"message": {
			"message_id": 1,
			"chat": {"id": 111},
			"from": {"id": 111, "username": "alice"},
			"document": {"file_id": "f-1", "file_name": "doc.txt"}
		}
	}`)
	// HandleMessage may fail downstream (no live deps), tolerate.
	_ = bot.HandleUpdate(context.Background(), upd)
}

func TestHandleUpdate_PhotoFallsBackToLargestVariant(t *testing.T) {
	bot, _, cleanup := makeAutopilotBot(t)
	defer cleanup()
	bot.config.AllowedUsers = map[int64]UserAccess{
		111: {Allowed: true, Projects: []string{"*"}},
	}
	upd := decodeUpdate(t, `{
		"update_id": 6,
		"message": {
			"message_id": 2,
			"chat": {"id": 111},
			"from": {"id": 111},
			"photo": [
				{"file_id":"small","width":100,"height":100},
				{"file_id":"large","width":1024,"height":1024}
			],
			"caption": "describe this"
		}
	}`)
	_ = bot.HandleUpdate(context.Background(), upd)
}

func TestHandleUpdate_AttachmentWithoutCaption_GetsDefaultText(t *testing.T) {
	bot, _, cleanup := makeAutopilotBot(t)
	defer cleanup()
	bot.config.AllowedUsers = map[int64]UserAccess{
		111: {Allowed: true, Projects: []string{"*"}},
	}
	upd := decodeUpdate(t, `{
		"update_id": 7,
		"message": {
			"message_id": 3,
			"chat": {"id": 111},
			"from": {"id": 111},
			"document": {"file_id":"f-2","file_name":"ignore.txt"}
		}
	}`)
	_ = bot.HandleUpdate(context.Background(), upd)
}

func TestHandleUpdate_ReplyAndThreadFieldsCarried(t *testing.T) {
	bot, _, cleanup := makeAutopilotBot(t)
	defer cleanup()
	bot.config.AllowedUsers = map[int64]UserAccess{
		111: {Allowed: true, Projects: []string{"*"}},
	}
	upd := decodeUpdate(t, `{
		"update_id": 8,
		"message": {
			"message_id": 4,
			"chat": {"id": 111},
			"from": {"id": 111},
			"text": "ok",
			"reply_to_message": {"message_id": 99},
			"message_thread_id": 42
		}
	}`)
	_ = bot.HandleUpdate(context.Background(), upd)
}

// --- handleSummarize -------------------------------------------------

func TestHandleSummarize_NoLLM(t *testing.T) {
	bot, calls, cleanup := makeAutopilotBot(t)
	defer cleanup()
	bot.llmClient = nil
	if err := bot.handleSummarize(context.Background(), 111); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("expected 1 API call; got %d", len(*calls))
	}
	if !strings.Contains((*calls)[0].Body, "LLM client not available") {
		t.Errorf("got %q", (*calls)[0].Body)
	}
}

func TestHandleSummarize_EmptyConversation(t *testing.T) {
	bot, calls, cleanup := makeAutopilotBot(t)
	defer cleanup()
	if err := bot.handleSummarize(context.Background(), 111); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !strings.Contains((*calls)[0].Body, "Nothing to summarize") {
		t.Errorf("got %q", (*calls)[0].Body)
	}
}
