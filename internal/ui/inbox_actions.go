package ui

import (
	"context"
	"net/http"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// Task 4.3 — the attention queue's inline actions (design §5.6).
//
// The state-transition logic already lives in task_conversation.go
// (uiApproveTask/uiRejectTask/uiSimpleFlip, uiAnswerCheckpoint) and
// task_actions.go (retryOne/TaskRetry) — this file adds ONLY the
// HTMX partial-render path those handlers call into after mutating
// state, so an inline approve/reject/answer/retry click re-renders
// its own attention-queue row instead of navigating away.

// renderInboxItemFragment re-renders one attention-queue row for the
// HTMX partial-swap path, after an inline action has already run. It
// deliberately does its own fresh persistence.Task load (never trusts
// the pre-mutation task the caller had in hand) so the fragment always
// reflects the row's CURRENT state — the §7 idempotency contract: a
// stale double-click's fragment shows what the row looks like now, not
// what the click intended.
//
// Writes an empty (200) body when the row no longer belongs in the
// attention queue — the task wasn't found, the repo isn't wired, or
// the task's status left the four attention categories (the action
// resolved it: approved→QUEUED, rejected→CANCELLED, answered→QUEUED,
// retried→QUEUED). Combined with the row's hx-swap="outerHTML" target,
// an empty body removes the row from the DOM — "the item resolves in
// place" from the design's §5.6 description. This also covers "task
// deleted under an open card" (§7): a failed re-fetch degrades to the
// same empty-body removal, no 500.
//
// Callers are responsible for having already scope-gated taskID on
// THIS request (TaskConversationAction's uiRequireProjectScope call,
// TaskRetry's own gate) before reaching this helper — it does not
// re-derive scope from taskID alone, only from the request the caller
// already validated.
func (s *Server) renderInboxItemFragment(ctx context.Context, w http.ResponseWriter, taskID string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	if s.taskRepo == nil {
		return
	}
	task, err := s.taskRepo.Get(ctx, taskID)
	if err != nil || task == nil {
		return
	}
	c, ok := byStatusCategory(task.Status)
	if !ok {
		// Resolved out of the attention set — row disappears.
		return
	}
	item, ok := newAttentionItem(task, c, time.Now().Add(-failedRecencyWindow))
	if !ok {
		// FAILED aged past the 24h window between the action and this
		// re-render (practically unreachable within one request, but
		// keeps this helper's classification identical to Inbox's).
		return
	}
	if item.Kind == inboxKindNeedsInput && s.taskMessageRepo != nil {
		if cp, cpErr := s.taskMessageRepo.GetOpenCheckpoint(ctx, task.ID); cpErr == nil && cp != nil {
			item.CheckpointID = cp.ID
			if cp.Metadata != nil {
				item.Checkpoint = parseCheckpointView(cp.Metadata)
			}
		}
	}
	if err := s.templates.ExecuteTemplate(w, "inboxItemRow", item); err != nil {
		s.logger.Warn().Err(err).Str("task_id", taskID).Msg("renderInboxItemFragment: render failed")
	}
}

// byStatusCategory looks up the inboxCategory a task's live status maps
// to, if any. Small linear scan over the fixed 4-entry inboxCategories
// — not worth a package-level map for 4 items, and keeps this the
// single source of truth Inbox's byStatus map is built from.
func byStatusCategory(status persistence.TaskStatus) (inboxCategory, bool) {
	for _, c := range inboxCategories {
		if c.status == status {
			return c, true
		}
	}
	return inboxCategory{}, false
}
