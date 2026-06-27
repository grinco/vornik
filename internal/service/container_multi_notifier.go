package service

import (
	"context"

	"vornik.io/vornik/internal/executor"
	"vornik.io/vornik/internal/persistence"
)

// multiCompletionNotifier fans out task-completion events to every
// wired channel notifier (Telegram bot, email channels, future
// Slack/GitHub). Each downstream notifier independently filters by
// its own pending-followup map — a task registered on Telegram
// fires only the bot's resume; one registered on an email channel
// fires only that channel's resume. Cross-channel leaks are
// impossible because each notifier is keyed on its own state.
//
// Used when more than one channel needs to receive completion
// events. Single-channel deployments (Telegram-only) still use
// the bot directly via SetCompletionNotifier; this multiplexer
// only kicks in when emails are also wired.
type multiCompletionNotifier struct {
	notifiers []executor.CompletionNotifier
}

// newMultiCompletionNotifier builds a notifier that broadcasts
// across all supplied sinks. Nil entries are skipped at build
// time so callers can pass optional notifiers unconditionally.
// Returns nil when no live notifiers remain so the caller can
// branch on "no wiring at all" if needed.
func newMultiCompletionNotifier(notifiers ...executor.CompletionNotifier) executor.CompletionNotifier {
	live := make([]executor.CompletionNotifier, 0, len(notifiers))
	for _, n := range notifiers {
		if n == nil {
			continue
		}
		live = append(live, n)
	}
	if len(live) == 0 {
		return nil
	}
	if len(live) == 1 {
		return live[0]
	}
	return &multiCompletionNotifier{notifiers: live}
}

// NotifyTaskCompleted broadcasts to every notifier. Each one is
// invoked sequentially — the dispatcher resume + email send are
// the heaviest operations and serialising them avoids a thundering
// herd against the chat client. None of the notifiers are
// expected to be slow (each one's resume call returns as soon as
// it has queued the synthetic user turn).
func (m *multiCompletionNotifier) NotifyTaskCompleted(ctx context.Context, task *persistence.Task, success bool, message string) {
	if m == nil {
		return
	}
	for _, n := range m.notifiers {
		n.NotifyTaskCompleted(ctx, task, success, message)
	}
}
