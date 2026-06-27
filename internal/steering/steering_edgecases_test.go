package steering

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/persistence"
)

// --- operator-alert edge cases --------------------------------------------

func TestOperatorAlert_NilReceiverSafe(t *testing.T) {
	var n *OperatorAlertNotifier
	// Must not panic on a nil receiver (mirrors the chat Notifier's contract).
	n.NotifySteeringRequired(context.Background(), autonomyTask(), string(persistence.TaskStatusAwaitingApproval))
}

func TestOperatorAlert_NilTask(t *testing.T) {
	ch := &fakeChannel{name: "telegram"}
	n, tg := newOperatorAlert(t, ch, OperatorAlertConfig{Channel: "telegram", Session: "555"}, true)
	n.NotifySteeringRequired(context.Background(), nil, string(persistence.TaskStatusAwaitingApproval))
	if len(tg.sent) != 0 {
		t.Fatalf("nil task must be a no-op")
	}
}

func TestOperatorAlert_ChannelNotWired(t *testing.T) {
	// Recipient configured for "telegram" but the resolver has no such channel.
	n, _ := newOperatorAlert(t, nil, OperatorAlertConfig{Channel: "telegram", Session: "555"}, true)
	n.NotifySteeringRequired(context.Background(), autonomyTask(), string(persistence.TaskStatusAwaitingApproval))
	// No panic, nothing to assert beyond not crashing — the channel is absent.
}

func TestOperatorAlert_SendErrorIsNotMarkedSent(t *testing.T) {
	ch := &fakeChannel{name: "telegram", sendErr: errors.New("telegram down")}
	n, tg := newOperatorAlert(t, ch, OperatorAlertConfig{Channel: "telegram", Session: "555"}, true)

	task := autonomyTask()
	n.NotifySteeringRequired(context.Background(), task, string(persistence.TaskStatusAwaitingApproval))
	n.NotifySteeringRequired(context.Background(), task, string(persistence.TaskStatusAwaitingApproval))

	// A failed send must NOT record dedup, so the next attempt tries again.
	if len(tg.sent) != 2 {
		t.Fatalf("failed send must not suppress a retry: got %d attempts, want 2", len(tg.sent))
	}
}

func TestOperatorAlert_AwaitingInputWording(t *testing.T) {
	ch := &fakeChannel{name: "telegram"}
	n, tg := newOperatorAlert(t, ch, OperatorAlertConfig{Channel: "telegram", Session: "555"}, true)
	n.NotifySteeringRequired(context.Background(), autonomyTask(), string(persistence.TaskStatusAwaitingInput))
	if len(tg.sent) != 1 || !strings.Contains(tg.sent[0].Text, "input") {
		t.Fatalf("AWAITING_INPUT alert should mention input; got %+v", tg.sent)
	}
}

func TestOperatorAlert_DedupMapPrunes(t *testing.T) {
	ch := &fakeChannel{name: "telegram"}
	n, tg := newOperatorAlert(t, ch, OperatorAlertConfig{Channel: "telegram", Session: "555"}, true)
	// Exceed the 4096 dedup-map cap with distinct tasks to exercise the prune
	// branch; the notifier must keep sending and not grow unbounded.
	const n2 = 4100
	for i := 0; i < n2; i++ {
		task := autonomyTask()
		task.ID = fmt.Sprintf("task_auto_%d", i)
		n.NotifySteeringRequired(context.Background(), task, string(persistence.TaskStatusAwaitingApproval))
	}
	if len(tg.sent) != n2 {
		t.Fatalf("every distinct task should alert once: got %d, want %d", len(tg.sent), n2)
	}
}

// --- chat Notifier edge cases ---------------------------------------------

func TestNotify_SendErrorIsNotMarkedSent(t *testing.T) {
	row := &persistence.ChatAuditEntry{ID: "chat_1", ChatID: "555", UserID: "telegram:555", ProjectID: "p1"}
	ch := &fakeChannel{name: "telegram", sendErr: errors.New("boom")}
	n, tg := newNotifier(t, row, ch, true)

	task := &persistence.Task{ID: "task_a", ProjectID: "p1", ChatTurnID: turnID("chat_1")}
	n.NotifySteeringRequired(context.Background(), task, string(persistence.TaskStatusAwaitingInput))
	n.NotifySteeringRequired(context.Background(), task, string(persistence.TaskStatusAwaitingInput))
	if len(tg.sent) != 2 {
		t.Fatalf("failed send must not suppress a retry: got %d, want 2", len(tg.sent))
	}
}

func TestEmailAddrFromUserID(t *testing.T) {
	cases := map[string]string{
		"email:ops@example.com": "ops@example.com",
		"plainaddr@example.com": "plainaddr@example.com", // no channel prefix
		"":                      "",
	}
	for in, want := range cases {
		if got := emailAddrFromUserID(in); got != want {
			t.Errorf("emailAddrFromUserID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsAllDigits(t *testing.T) {
	cases := map[string]bool{
		"12345": true,
		"":      false,
		"12a45": false,
		"-12":   false,
		"007":   true,
	}
	for in, want := range cases {
		if got := isAllDigits(in); got != want {
			t.Errorf("isAllDigits(%q) = %v, want %v", in, got, want)
		}
	}
}

// compile-time assurance the fakes satisfy the channel interface used above.
var _ conversation.Channel = (*fakeChannel)(nil)
var _ = zerolog.Nop
