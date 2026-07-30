package service

import (
	"context"

	"vornik.io/vornik/internal/chatorigin"
	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/dispatcher"
)

// turnStatusReporter routes the dispatcher's in-flight progress to the originating
// channel, when that channel can display it.
//
// OPERATOR REQUEST 2026-07-30: "on slack there is a mechanism to display statuses — for
// thinking, tool calling, etc — it can be rewritten and doesn't have to use any of the
// context." That mechanism is assistant.threads.setStatus, and it is deliberately NOT
// what this uses: it needs `assistant:write`, a manifest change and a reinstall, and it
// converts DMs into assistant threads. The Slack channel instead rewrites the progress
// placeholder it already owns, which needs no new scope.
//
// The dispatcher knows nothing about channels, so the resolution lives here: name →
// conversation.Channel → optional TurnStatusChannel. A channel that cannot display a
// status is simply skipped, which is why telegram and email need no change.
type turnStatusReporter struct {
	resolver chatorigin.ChannelResolver
}

// ReportTurnStatus implements dispatcher.TurnStatusReporter.
//
// Best-effort and non-blocking by contract: this is called from inside the tool loop, so
// a slow or failing status update must never add latency to the very turn it is
// reporting on.
func (r *turnStatusReporter) ReportTurnStatus(ctx context.Context, channelName, sessionID, status string) {
	if r == nil || r.resolver == nil || channelName == "" || sessionID == "" {
		return
	}
	ch := r.resolver.ResolveChannel(channelName)
	if ch == nil {
		return
	}
	display, ok := ch.(conversation.TurnStatusChannel)
	if !ok {
		// Telegram, email, web-chat: no transient-status surface. Not an error.
		return
	}
	display.SetTurnStatus(ctx, sessionID, status)
}

// Compile-time guard against signature drift between the two packages.
var _ dispatcher.TurnStatusReporter = (*turnStatusReporter)(nil)
