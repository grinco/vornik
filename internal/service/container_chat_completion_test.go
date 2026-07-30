package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/persistence"
)

// captureChannel records what a notifier sent, and where.
type captureChannel struct {
	name string
	mu   sync.Mutex
	sent []conversation.ChannelMessage
	err  error
}

func (c *captureChannel) Name() string                                       { return c.name }
func (c *captureChannel) Start(context.Context, conversation.Receiver) error { return nil }
func (c *captureChannel) Stop() error                                        { return nil }
func (c *captureChannel) ListSessions(context.Context) ([]conversation.Session, error) {
	return nil, nil
}

func (c *captureChannel) ResolveSpeaker(_ context.Context, id string) (conversation.Speaker, error) {
	return conversation.Speaker{ID: id}, nil
}

func (c *captureChannel) Send(_ context.Context, msg conversation.ChannelMessage) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return "", c.err
	}
	c.sent = append(c.sent, msg)
	return "ts", nil
}

func (c *captureChannel) snapshot() []conversation.ChannelMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]conversation.ChannelMessage(nil), c.sent...)
}

// stubAudit resolves one chat_audit row by id.
type stubAudit struct {
	rows map[string]*persistence.ChatAuditEntry
	err  error
}

func (s stubAudit) GetByID(_ context.Context, id string) (*persistence.ChatAuditEntry, error) {
	if s.err != nil {
		return nil, s.err
	}
	row, ok := s.rows[id]
	if !ok {
		return nil, persistence.ErrNotFound
	}
	return row, nil
}

type stubResolveChannel struct {
	byName map[string]conversation.Channel
}

func (s stubResolveChannel) ResolveChannel(name string) conversation.Channel {
	return s.byName[name]
}

func turnTask(id, turnID string) *persistence.Task {
	t := &persistence.Task{ID: id, ProjectID: "p", Status: "COMPLETED"}
	if turnID != "" {
		t.ChatTurnID = &turnID
	}
	return t
}

func notifierFor(ch conversation.Channel, rows map[string]*persistence.ChatAuditEntry) *chatCompletionNotifier {
	return newChatCompletionNotifier(
		stubAudit{rows: rows},
		nil, // no task getter: these tasks carry their own ChatTurnID
		stubResolveChannel{byName: map[string]conversation.Channel{"slack": ch}},
		"https://vornik.example",
		true,
		map[string]bool{"slack": true},
		zerolog.Nop(),
	)
}

// CUSTOMER REPORT 2026-07-30 (remote installation): "vornik said on Slack it would
// schedule a job, then nothing happened; when asked the status of the job it didn't know
// anything about it."
//
// Cause: only telegram and email implement a completion notifier. A task created from
// Slack registered nothing, so no channel was ever told it finished — the promise was
// real, the report back was missing.
//
// Resolution is DB-backed (chatorigin: task.ChatTurnID → chat_audit_log → channel +
// session), so it works even when the daemon restarted between scheduling and
// completion — the case the operator said to assume.
func TestChatCompletionNotifier_NotifiesTheOriginatingSlackThread(t *testing.T) {
	ch := &captureChannel{name: "slack"}
	n := notifierFor(ch, map[string]*persistence.ChatAuditEntry{
		"turn-1": {ID: "turn-1", ChatID: "slack:T123/C_general#1785367141.211839", UserID: "slack:U_alice", ProjectID: "p"},
	})

	n.NotifyTaskCompleted(context.Background(), turnTask("task_42", "turn-1"), true, "wrote the report")

	sent := ch.snapshot()
	if len(sent) != 1 {
		t.Fatalf("messages sent = %d, want 1", len(sent))
	}
	if sent[0].SessionID != "T123/C_general#1785367141.211839" {
		t.Errorf("SessionID = %q, want the originating Slack thread", sent[0].SessionID)
	}
	// The person has to be able to act on it: which task, and where to look.
	for _, want := range []string{"task_42", "https://vornik.example"} {
		if !strings.Contains(sent[0].Text, want) {
			t.Errorf("notification does not mention %q: %s", want, sent[0].Text)
		}
	}
}

// A failure must say so. Reporting only successes is how "nothing happened" looks from
// the outside when a job died.
func TestChatCompletionNotifier_ReportsFailureAndTheReason(t *testing.T) {
	ch := &captureChannel{name: "slack"}
	n := notifierFor(ch, map[string]*persistence.ChatAuditEntry{
		"turn-1": {ID: "turn-1", ChatID: "slack:T123/C_general#main", ProjectID: "p"},
	})

	task := turnTask("task_43", "turn-1")
	task.Status = "FAILED"
	boom := "container exited 137"
	task.LastError = &boom

	n.NotifyTaskCompleted(context.Background(), task, false, "")

	sent := ch.snapshot()
	if len(sent) != 1 {
		t.Fatalf("messages sent = %d, want 1", len(sent))
	}
	if !strings.Contains(sent[0].Text, "container exited 137") {
		t.Errorf("failure notification omits the error: %s", sent[0].Text)
	}
}

// The executor calls NotifyTaskCompleted TWICE for a COMPLETED task (lead_handoff plus
// the terminal transition — see telegram/forum.go's note). Without dedup every finished
// job would be announced twice.
func TestChatCompletionNotifier_AnnouncesOnce(t *testing.T) {
	ch := &captureChannel{name: "slack"}
	n := notifierFor(ch, map[string]*persistence.ChatAuditEntry{
		"turn-1": {ID: "turn-1", ChatID: "slack:T123/C_general#main", ProjectID: "p"},
	})

	task := turnTask("task_44", "turn-1")
	n.NotifyTaskCompleted(context.Background(), task, true, "")
	n.NotifyTaskCompleted(context.Background(), task, true, "")

	if got := len(ch.snapshot()); got != 1 {
		t.Fatalf("messages sent = %d, want 1 — the executor fires this twice", got)
	}
}

// Telegram and email already announce completion through their own auto-resume path.
// This notifier must stay off their channels or every task would be reported twice, so
// responsibility is an explicit allowlist rather than "whatever resolves".
func TestChatCompletionNotifier_LeavesChannelsWithTheirOwnNotifierAlone(t *testing.T) {
	tg := &captureChannel{name: "telegram"}
	n := newChatCompletionNotifier(
		stubAudit{rows: map[string]*persistence.ChatAuditEntry{
			"turn-1": {ID: "turn-1", ChatID: "telegram:4242", ProjectID: "p"},
		}},
		nil,
		stubResolveChannel{byName: map[string]conversation.Channel{"telegram": tg}},
		"", true, map[string]bool{"slack": true}, zerolog.Nop(),
	)

	n.NotifyTaskCompleted(context.Background(), turnTask("task_45", "turn-1"), true, "")

	if got := len(tg.snapshot()); got != 0 {
		t.Fatalf("telegram messages = %d, want 0 — the bot's own notifier owns that channel", got)
	}
}

// A task with no chat origin (API, autonomy, A2A) has nobody to tell. That is not an
// error and must not log as one.
func TestChatCompletionNotifier_IgnoresTasksWithNoChatOrigin(t *testing.T) {
	ch := &captureChannel{name: "slack"}
	n := notifierFor(ch, nil)

	n.NotifyTaskCompleted(context.Background(), turnTask("task_46", ""), true, "")

	if got := len(ch.snapshot()); got != 0 {
		t.Fatalf("messages sent = %d, want 0", got)
	}
}

// Every failure mode degrades to silence rather than blocking the executor's terminal
// transition: a task must reach COMPLETED even when nobody can be told about it.
func TestChatCompletionNotifier_NeverBlocksOnFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		make func() *chatCompletionNotifier
	}{
		{"audit lookup fails", func() *chatCompletionNotifier {
			return newChatCompletionNotifier(stubAudit{err: errors.New("db down")}, nil,
				stubResolveChannel{byName: map[string]conversation.Channel{"slack": &captureChannel{name: "slack"}}},
				"", true, map[string]bool{"slack": true}, zerolog.Nop())
		}},
		{"channel not wired", func() *chatCompletionNotifier {
			return newChatCompletionNotifier(
				stubAudit{rows: map[string]*persistence.ChatAuditEntry{
					"turn-1": {ID: "turn-1", ChatID: "slack:T1/C1#main"},
				}}, nil, stubResolveChannel{byName: nil}, "", true,
				map[string]bool{"slack": true}, zerolog.Nop())
		}},
		{"send errors", func() *chatCompletionNotifier {
			return notifierFor(&captureChannel{name: "slack", err: errors.New("slack 500")},
				map[string]*persistence.ChatAuditEntry{"turn-1": {ID: "turn-1", ChatID: "slack:T1/C1#main"}})
		}},
		{"disabled by config", func() *chatCompletionNotifier {
			return newChatCompletionNotifier(
				stubAudit{rows: map[string]*persistence.ChatAuditEntry{
					"turn-1": {ID: "turn-1", ChatID: "slack:T1/C1#main"},
				}}, nil,
				stubResolveChannel{byName: map[string]conversation.Channel{"slack": &captureChannel{name: "slack"}}},
				"", false, map[string]bool{"slack": true}, zerolog.Nop())
		}},
	} {
		t.Run(tc.name, func(_ *testing.T) {
			// The assertion is that this returns at all rather than blocking or panicking.
			tc.make().NotifyTaskCompleted(context.Background(), turnTask("task_47", "turn-1"), true, "")
		})
	}
}

// A nil notifier is the "not wired" case and every call site passes it unconditionally.
func TestChatCompletionNotifier_NilIsSafe(_ *testing.T) {
	var n *chatCompletionNotifier
	n.NotifyTaskCompleted(context.Background(), turnTask("task_48", "turn-1"), true, "")
}

// OPERATOR REQUEST 2026-07-30: use rich text — a hyperlink on words rather than a raw
// URL, plus emoji and emphasis. A task URL is long and a channel full of them is
// unreadable.
func TestChatCompletionNotifier_UsesRichTextNotARawURL(t *testing.T) {
	ch := &captureChannel{name: "slack"}
	n := notifierFor(ch, map[string]*persistence.ChatAuditEntry{
		"turn-1": {ID: "turn-1", ChatID: "slack:T123/C_general#main", ProjectID: "p"},
	})

	n.NotifyTaskCompleted(context.Background(), turnTask("task_42", "turn-1"), true, "")

	sent := ch.snapshot()
	if len(sent) != 1 {
		t.Fatalf("messages = %d, want 1", len(sent))
	}
	text := sent[0].Text

	// The URL must be inside Slack's link syntax, anchored on words.
	if !strings.Contains(text, "|open the task>") {
		t.Errorf("the task URL is not a hyperlink:\n%s", text)
	}
	// A bare URL sitting on its own is the thing being fixed.
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "http") {
			t.Errorf("a raw URL is still pasted on its own line: %q", trimmed)
		}
	}
	if !strings.Contains(text, ":white_check_mark:") {
		t.Error("success is not signalled with an emoji")
	}
	if !strings.Contains(text, "*Finished*") {
		t.Error("the outcome is not emphasised")
	}
}

// A channel with different link syntax must get its own, not Slack's. This is the whole
// reason the helper takes a channel name.
func TestChatCompletionNotifier_LinkSyntaxFollowsTheChannel(t *testing.T) {
	tg := &captureChannel{name: "telegram"}
	n := newChatCompletionNotifier(
		stubAudit{rows: map[string]*persistence.ChatAuditEntry{
			"turn-1": {ID: "turn-1", ChatID: "telegram:4242", ProjectID: "p"},
		}},
		nil,
		stubResolveChannel{byName: map[string]conversation.Channel{"telegram": tg}},
		"https://vornik.example", true,
		// Telegram is normally excluded (it has its own notifier); allow it here purely
		// to assert the rendering follows the channel rather than being hardcoded.
		map[string]bool{"telegram": true}, zerolog.Nop(),
	)

	n.NotifyTaskCompleted(context.Background(), turnTask("task_43", "turn-1"), true, "")

	sent := tg.snapshot()
	if len(sent) != 1 {
		t.Fatalf("messages = %d, want 1", len(sent))
	}
	if !strings.Contains(sent[0].Text, "[open the task](https://vornik.example") {
		t.Errorf("telegram did not get Markdown link syntax:\n%s", sent[0].Text)
	}
}
