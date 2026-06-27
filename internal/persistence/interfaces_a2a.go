// Package persistence — A2A push-notification config domain.
//
// When an A2A caller submits a task with a webhook url
// (pushNotificationConfig), we persist it so the daemon can POST
// task-state updates (input-required / completed / failed / canceled) to
// that url even when the caller isn't holding an open SSE stream. See
// https://docs.vornik.io
package persistence

import (
	"context"
	"time"
)

// A2APushConfig is one A2A caller's webhook target for a task. Token is the
// optional bearer the caller wants echoed back on the push (so the client can
// authenticate that the webhook genuinely came from this daemon).
type A2APushConfig struct {
	TaskID    string
	URL       string
	Token     string
	CreatedAt time.Time
}

// A2APushConfigRepository persists per-task webhook push configs.
type A2APushConfigRepository interface {
	// Set upserts the config for a task (last-write-wins on task_id).
	Set(ctx context.Context, cfg A2APushConfig) error
	// Get returns the config for a task, or ErrNotFound when none is set.
	Get(ctx context.Context, taskID string) (*A2APushConfig, error)
}
