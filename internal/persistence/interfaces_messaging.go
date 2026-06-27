// Package persistence — inbound-channel + notification interfaces.
//
// TaskMessage (conversational lifecycle), TaskWatcher (notification subscriptions), Webhook ingress audit, TelegramThread (Forum-Topic mapping).
// Split from interfaces.go on 2026-05-28 to keep each domain in
// its own file. Same package; no API change — pure file-org.
package persistence

import (
	"context"
)

// TaskMessageRepository persists conversational task lifecycle
// messages. See task-lifecycle-conversational-design.md §4.1.
type TaskMessageRepository interface {
	// Insert writes a new message. Returns the assigned ID populated
	// on the input struct.
	Insert(ctx context.Context, msg *TaskMessage) error

	// List returns messages for a task in chronological order.
	List(ctx context.Context, filter TaskMessageFilter) ([]*TaskMessage, error)

	// GetOpenCheckpoint returns the unresolved checkpoint message
	// for a task, if any. Returns nil + nil error when no
	// checkpoint is open.
	GetOpenCheckpoint(ctx context.Context, taskID string) (*TaskMessage, error)

	// MarkCheckpointResolved flips a checkpoint's metadata.resolved
	// flag and clears the task's open_checkpoint_id pointer in one
	// transaction. Idempotent.
	MarkCheckpointResolved(ctx context.Context, taskID, checkpointID string) error
}

// TaskWatcherRepository manages notification subscriptions for task completion.
type TaskWatcherRepository interface {
	// Watch registers a chat to be notified when a task completes.
	Watch(ctx context.Context, taskID string, chatID int64) error

	// GetWatchers returns all chat IDs watching a task.
	GetWatchers(ctx context.Context, taskID string) ([]int64, error)

	// RemoveWatchers deletes all watchers for a task (after notification).
	RemoveWatchers(ctx context.Context, taskID string) error
}

// WebhookEventRepository persists accepted/rejected webhook ingress events.
type WebhookEventRepository interface {
	// Record inserts one webhook ingress audit event.
	Record(ctx context.Context, event *WebhookEvent) error

	// List returns webhook events matching the filter, newest first.
	List(ctx context.Context, filter WebhookEventFilter) ([]*WebhookEvent, error)
}

// TelegramThreadRepository maps Telegram Forum Topics to tasks so
// inbound replies in a topic route to the correct task_messages
// thread and outbound lifecycle events fan out to the matching
// topic. See https://docs.vornik.io
type TelegramThreadRepository interface {
	// GetByTask returns the forum thread for a task, or ErrNotFound
	// when none has been created yet.
	GetByTask(ctx context.Context, taskID string) (*TelegramTaskThread, error)

	// GetByThread returns the thread by (chat_id, thread_id). Used
	// for inbound message routing. Returns ErrNotFound when the
	// pair is unknown.
	GetByThread(ctx context.Context, chatID, threadID int64) (*TelegramTaskThread, error)

	// Insert persists a new mapping. On a (chat_id, thread_id)
	// unique-conflict, returns ErrDuplicateKey so the caller can
	// re-resolve via GetByTask.
	Insert(ctx context.Context, t *TelegramTaskThread) error

	// MarkClosed stamps closed_at=now() for the task's thread.
	// Idempotent — calling on an already-closed row is a no-op.
	MarkClosed(ctx context.Context, taskID string) error
}
