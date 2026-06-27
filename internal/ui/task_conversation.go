package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/scheduler"
)

// Phase 26+27 of the conversational task lifecycle (LLD §5.1).
//
// The task detail template (task_detail.html) renders three new
// panels when taskMessageRepo is wired: conversation thread,
// open-checkpoint card, scratchpad summary. Operator actions
// (compose, answer, amend, pause/resume/close) post to these
// handlers, which mirror the api package's logic exactly so the
// state machine can't disagree depending on which surface the
// operator used.

const uiInspectMaxBytes = 64 * 1024

// TaskConversationView is the UI-facing struct mounted on
// TaskDetailData. Empty when conversational lifecycle isn't wired
// or the task has no messages yet.
type TaskConversationView struct {
	Enabled        bool
	Messages       []*persistence.TaskMessage
	OpenCheckpoint *persistence.TaskMessage
	// CheckpointPayload is the parsed metadata of OpenCheckpoint
	// so the template can render options/draft/task_for_human
	// without re-parsing JSON inside the template.
	CheckpointPayload *CheckpointView
	Scratchpad        *persistence.TaskScratchpad
	// ScratchpadOpenQuestions is the list pulled out of the
	// scratchpad's JSON so the template can range over it.
	ScratchpadOpenQuestions []string
	// PhaseTracker shows the operator the lead's phase progress.
	PhaseTracker []PhaseEntry
	// Closeable / Pauseable / Resumeable drive the action buttons.
	Closeable  bool
	Pauseable  bool
	Resumeable bool
}

// CheckpointView mirrors the executor's CheckpointPayload but
// flattened for templates.
type CheckpointView struct {
	Kind                string
	Question            string
	Options             []CheckpointOptionView
	TaskForHuman        string
	Draft               string
	ExpectedBy          string
	DefaultIfNoResponse string
}

// CheckpointOptionView is one row in a decision checkpoint.
type CheckpointOptionView struct {
	ID    string
	Label string
}

// PhaseEntry is one row in the phase tracker sidebar.
type PhaseEntry struct {
	Name      string
	Status    string
	IsCurrent bool
}

// loadConversationView builds the TaskConversationView for the
// task detail handler. Best-effort — any read error surfaces as
// "no conversation yet" rather than failing the page.
func (s *Server) loadConversationView(ctx context.Context, task *persistence.Task) TaskConversationView {
	view := TaskConversationView{
		Enabled: s.taskMessageRepo != nil,
	}
	if !view.Enabled || task == nil {
		return view
	}
	msgs, err := s.taskMessageRepo.List(ctx, persistence.TaskMessageFilter{
		TaskID: task.ID,
		Limit:  200,
	})
	if err == nil {
		view.Messages = msgs
	}

	// Unified-timeline merge (2026-05-26): interleave task-scoped
	// steering hints as synthetic TaskMessage rows with
	// MessageKind="hint". The merge happens here rather than in
	// the template so the existing render loop (which already
	// switches on MessageKind for colour + label) doesn't have to
	// learn a second slice. Best-effort — a hint-repo error
	// degrades to "messages only", same as the legacy path.
	if s.hintRepo != nil {
		if hints, hErr := s.hintRepo.ListByTask(ctx, task.ID); hErr == nil && len(hints) > 0 {
			view.Messages = mergeHintsIntoMessages(view.Messages, hints)
		}
	}

	if task.OpenCheckpointID != nil {
		if cp, err := s.taskMessageRepo.GetOpenCheckpoint(ctx, task.ID); err == nil && cp != nil {
			view.OpenCheckpoint = cp
			if cp.Metadata != nil {
				if cv := parseCheckpointView(cp.Metadata); cv != nil {
					view.CheckpointPayload = cv
				}
			}
		}
	}

	if s.taskScratchpadRepo != nil {
		if sp, err := s.taskScratchpadRepo.Get(ctx, task.ID); err == nil && sp != nil {
			view.Scratchpad = sp
			view.ScratchpadOpenQuestions = parseScratchpadQuestions(sp.OpenQuestions)
			view.PhaseTracker = buildPhaseTracker(sp)
		}
	}

	view.Closeable = task.Status == persistence.TaskStatusCompleted ||
		task.Status == persistence.TaskStatusAwaitingInput ||
		task.Status == persistence.TaskStatusAwaitingExternal
	view.Pauseable = scheduler.IsActiveTaskStatus(task.Status) || scheduler.IsAwaitingInput(task.Status)
	view.Resumeable = task.Status == persistence.TaskStatusPaused
	return view
}

// mergeHintsIntoMessages interleaves task-scoped steering hints
// into the conversation thread as synthetic TaskMessage rows with
// MessageKind="hint" (TaskMessageKindHint). Returned slice is
// sorted by CreatedAt ascending — the order the existing template
// already expects. Each hint becomes one synthetic message whose:
//
//   - AuthorKind: "operator" (mirrors the live page's compose box)
//   - MessageKind: "hint" — drives the dedicated colour + 💡 badge
//     in task_detail's render switch
//   - Content: the hint's content, prefixed with an applied/pending
//     marker so the operator can see the state at a glance
//   - Metadata: JSON {hint_id, scope, applied_at, step_id} so
//     future template logic can drill into the original hint
//
// The synthetic rows are NOT persisted to task_messages — they're
// materialised at render time only. Hint state changes (applied →
// consumed) are reflected on the next page render.
func mergeHintsIntoMessages(msgs []*persistence.TaskMessage, hints []*persistence.ExecutionHint) []*persistence.TaskMessage {
	if len(hints) == 0 {
		return msgs
	}
	merged := make([]*persistence.TaskMessage, 0, len(msgs)+len(hints))
	merged = append(merged, msgs...)
	for _, h := range hints {
		if h == nil {
			continue
		}
		applied := "pending"
		if h.AppliedAt != nil {
			applied = "applied " + h.AppliedAt.Format("Jan 02 15:04")
		}
		// Compact two-line content: state badge first, then the
		// hint body. The template's renderMarkdown handles the
		// rest without needing a new template branch.
		content := "_steering hint (" + applied + ")_\n\n" + h.Content
		meta, _ := json.Marshal(map[string]any{
			"hint_id":    h.ID,
			"applied_at": h.AppliedAt,
			"step_id":    h.StepID,
			"scope":      "task",
		})
		merged = append(merged, &persistence.TaskMessage{
			ID:          "hintmsg_" + h.ID,
			TaskID:      h.TaskID,
			AuthorKind:  persistence.TaskMessageAuthorOperator,
			MessageKind: persistence.TaskMessageKindHint,
			Content:     content,
			Metadata:    meta,
			CreatedAt:   h.CreatedAt,
		})
	}
	sort.SliceStable(merged, func(i, j int) bool {
		return merged[i].CreatedAt.Before(merged[j].CreatedAt)
	})
	return merged
}

func parseCheckpointView(meta []byte) *CheckpointView {
	var raw struct {
		Kind                string `json:"kind"`
		Question            string `json:"question"`
		TaskForHuman        string `json:"task_for_human"`
		Draft               string `json:"draft"`
		ExpectedBy          string `json:"expected_by"`
		DefaultIfNoResponse string `json:"default_if_no_response"`
		Options             []struct {
			ID    string `json:"id"`
			Label string `json:"label"`
		} `json:"options"`
	}
	if err := json.Unmarshal(meta, &raw); err != nil {
		return nil
	}
	out := &CheckpointView{
		Kind:                raw.Kind,
		Question:            raw.Question,
		TaskForHuman:        raw.TaskForHuman,
		Draft:               raw.Draft,
		ExpectedBy:          raw.ExpectedBy,
		DefaultIfNoResponse: raw.DefaultIfNoResponse,
	}
	for _, o := range raw.Options {
		out.Options = append(out.Options, CheckpointOptionView{ID: o.ID, Label: o.Label})
	}
	return out
}

func parseScratchpadQuestions(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	var out []string
	if err := json.Unmarshal(b, &out); err != nil {
		return nil
	}
	return out
}

func buildPhaseTracker(sp *persistence.TaskScratchpad) []PhaseEntry {
	if sp == nil || len(sp.PhaseHistory) == 0 {
		return nil
	}
	var raw []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(sp.PhaseHistory, &raw); err != nil {
		return nil
	}
	out := make([]PhaseEntry, 0, len(raw))
	for _, p := range raw {
		out = append(out, PhaseEntry{
			Name:      p.Name,
			Status:    p.Status,
			IsCurrent: sp.CurrentPhase != nil && *sp.CurrentPhase == p.Name,
		})
	}
	return out
}

// ----------------------------------------------------------------
// Action handlers — POST endpoints under /ui/tasks/<id>/...
// ----------------------------------------------------------------

// TaskConversationAction multiplexes the per-action POST routes.
// Path shape on the wire: /ui/tasks/{id}/{action}, where action is
// one of: message | directive | answer | amend | pause | resume | close.
//
// By the time this handler runs the /ui prefix has already been
// stripped by uiSubtreeHandler, so r.URL.Path is /tasks/{id}/{action}.
//
// Each action mutates state via the same atomic primitives the
// API handlers use (TaskMessageRepository + TaskRepository.
// TransitionConditional). After the mutation, the operator is
// redirected back to the task detail page with a notice query
// param so the toast banner explains what happened.
func (s *Server) TaskConversationAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.taskMessageRepo == nil || s.taskRepo == nil {
		http.Error(w, "task lifecycle not configured", http.StatusServiceUnavailable)
		return
	}

	// Path is /tasks/{id}/{action} after the /ui prefix is stripped
	// upstream; tolerate both shapes so the handler works regardless
	// of whether someone wires it directly under /ui or behind the
	// subtree handler.
	trimmed := strings.TrimPrefix(r.URL.Path, "/ui")
	trimmed = strings.TrimPrefix(trimmed, "/tasks/")
	parts := strings.Split(trimmed, "/")
	if len(parts) < 2 {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	taskID := parts[0]
	action := parts[1]

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	task, err := s.taskRepo.Get(ctx, taskID)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	notice := ""
	switch action {
	case "message", "directive":
		notice = s.uiPostMessage(ctx, task, r, persistence.TaskMessageKindMessage, action == "directive")
	case "answer":
		notice = s.uiAnswerCheckpoint(ctx, task, r)
	case "amend":
		notice = s.uiAmendBrief(ctx, task, r)
	case "pause":
		notice = s.uiPauseTask(ctx, task)
	case "resume":
		notice = s.uiResumeTask(ctx, task)
	case "close":
		notice = s.uiCloseTask(ctx, task, r)
	case "approve":
		notice = s.uiApproveTask(ctx, task)
	case "reject":
		notice = s.uiRejectTask(ctx, task)
	default:
		http.Error(w, "unknown action: "+action, http.StatusBadRequest)
		return
	}

	target := fmt.Sprintf("/ui/tasks/%s", taskID)
	if notice != "" {
		target += "?notice=" + notice
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func (s *Server) uiPostMessage(ctx context.Context, task *persistence.Task, r *http.Request, defaultKind string, isDirective bool) string {
	content := strings.TrimSpace(r.FormValue("content"))
	if content == "" {
		return "empty-message"
	}
	if len(content) > uiInspectMaxBytes {
		content = content[:uiInspectMaxBytes]
	}
	kind := defaultKind
	if isDirective {
		kind = persistence.TaskMessageKindDirective
	}
	authorID := strings.TrimSpace(r.FormValue("author"))
	msg := &persistence.TaskMessage{
		TaskID:      task.ID,
		AuthorKind:  persistence.TaskMessageAuthorOperator,
		MessageKind: kind,
		Content:     content,
		CreatedAt:   time.Now().UTC(),
	}
	if authorID != "" {
		msg.AuthorID = &authorID
	}
	if err := s.taskMessageRepo.Insert(ctx, msg); err != nil {
		s.logger.Error().Err(err).Str("task_id", task.ID).Msg("uiPostMessage: insert failed")
		return "message-failed"
	}
	// Phase 80 — push the new message to any SSE subscribers so
	// the conversation pane re-renders without polling. Best-effort;
	// a missing/full bus drops silently.
	if s.sseBus != nil {
		s.sseBus.Publish(task.ID, SSEEvent{Kind: "message", Data: "new-message"})
	}
	// Re-queue policy mirrors the API handler in
	// task_conversation_handlers.go — keep these two surfaces in
	// lockstep so the state machine can't disagree depending on
	// which client the operator used.
	//
	// Two strategies depending on origin (LLD §7.0):
	//
	//  - Waiting states (AWAITING_INPUT / AWAITING_EXTERNAL): the
	//    task didn't fail, it was at-rest. TransitionConditional
	//    clears the lease; attempt counter untouched.
	//
	//  - Terminal states (FAILED / CANCELLED / COMPLETED): the
	//    operator is course-correcting a bad outcome.
	//    RequeueTerminalTask resets attempt=1 (granting a fresh
	//    max_attempts budget), preserves max_attempts, and
	//    clears last_error / last_error_class atomically. CLOSED
	//    is intentionally excluded — archival is one-way; use
	//    /retry to revive a closed task.
	requeued := false
	switch task.Status {
	case persistence.TaskStatusAwaitingInput,
		persistence.TaskStatusAwaitingExternal:
		ok, _ := s.taskRepo.TransitionConditional(ctx, task.ID,
			[]persistence.TaskStatus{task.Status},
			persistence.TaskStatusQueued,
			persistence.TransitionOpts{ClearLease: true})
		requeued = ok
	case persistence.TaskStatusFailed,
		persistence.TaskStatusCancelled,
		persistence.TaskStatusCompleted:
		ok, _ := s.taskRepo.RequeueTerminalTask(ctx, task.ID, 1, task.MaxAttempts)
		requeued = ok
	}
	if requeued {
		if s.rescheduler != nil {
			s.rescheduler.Wake()
		}
		if s.sseBus != nil {
			s.sseBus.Publish(task.ID, SSEEvent{Kind: "status", Data: "QUEUED"})
		}
	}
	if isDirective {
		if requeued {
			return "directive-requeued"
		}
		return "directive-recorded"
	}
	if requeued {
		return "message-requeued"
	}
	return "message-sent"
}

func (s *Server) uiAnswerCheckpoint(ctx context.Context, task *persistence.Task, r *http.Request) string {
	checkpointID := strings.TrimSpace(r.FormValue("checkpoint_id"))
	content := strings.TrimSpace(r.FormValue("content"))
	choice := strings.TrimSpace(r.FormValue("choice"))
	authorID := strings.TrimSpace(r.FormValue("author"))
	if checkpointID == "" {
		return "missing-checkpoint"
	}
	if content == "" && choice == "" {
		return "empty-answer"
	}
	if err := scheduler.ValidateTransition(task.Status, persistence.TaskStatusQueued, scheduler.TriggerOperatorAnswer); err != nil {
		return "checkpoint-stale"
	}
	if task.OpenCheckpointID == nil || *task.OpenCheckpointID != checkpointID {
		return "checkpoint-resolved"
	}
	// Choice-only answers must still carry readable Content: the chat
	// log renders Content, and the lead agent's conversation view reads
	// Content — an empty answer is invisible to both (regression: task
	// …a691c512ebd1c4fd, the lead re-asked an already-answered
	// checkpoint). Resolve the option label from the open checkpoint's
	// metadata; fall back to the raw choice id when it doesn't resolve.
	if content == "" && choice != "" {
		content = choice
		if cp, err := s.taskMessageRepo.GetOpenCheckpoint(ctx, task.ID); err == nil && cp != nil {
			if label := persistence.CheckpointOptionLabel(cp.Metadata, choice); label != "" {
				content = label
			}
		}
	}
	meta, _ := json.Marshal(map[string]any{"choice": choice})
	answer := &persistence.TaskMessage{
		TaskID:      task.ID,
		ParentID:    &checkpointID,
		AuthorKind:  persistence.TaskMessageAuthorOperator,
		MessageKind: persistence.TaskMessageKindAnswer,
		Content:     content,
		Metadata:    meta,
		CreatedAt:   time.Now().UTC(),
	}
	if authorID != "" {
		answer.AuthorID = &authorID
	}
	if err := s.taskMessageRepo.Insert(ctx, answer); err != nil {
		return "answer-failed"
	}
	if err := s.taskMessageRepo.MarkCheckpointResolved(ctx, task.ID, checkpointID); err != nil {
		s.logger.Error().Err(err).Str("task_id", task.ID).Msg("uiAnswerCheckpoint: resolve failed")
	}
	ok, _ := s.taskRepo.TransitionConditional(ctx, task.ID,
		[]persistence.TaskStatus{persistence.TaskStatusAwaitingInput},
		persistence.TaskStatusQueued,
		persistence.TransitionOpts{ClearLease: true})
	if !ok {
		return "checkpoint-stale"
	}
	if s.rescheduler != nil {
		s.rescheduler.Wake()
	}
	if s.sseBus != nil {
		s.sseBus.Publish(task.ID, SSEEvent{Kind: "status", Data: "QUEUED"})
		s.sseBus.Publish(task.ID, SSEEvent{Kind: "message", Data: "answer-sent"})
	}
	return "answer-sent"
}

func (s *Server) uiAmendBrief(ctx context.Context, task *persistence.Task, r *http.Request) string {
	newBrief := strings.TrimSpace(r.FormValue("new_brief"))
	reason := strings.TrimSpace(r.FormValue("reason"))
	authorID := strings.TrimSpace(r.FormValue("author"))
	if newBrief == "" {
		return "empty-amend"
	}
	if scheduler.IsTerminalTaskStatus(task.Status) {
		return "task-terminal"
	}
	body := newBrief
	if reason != "" {
		body = reason + "\n\n---\n\n" + body
	}
	meta, _ := json.Marshal(map[string]any{"kind": "brief_amendment", "reason": reason})
	msg := &persistence.TaskMessage{
		TaskID:      task.ID,
		AuthorKind:  persistence.TaskMessageAuthorOperator,
		MessageKind: persistence.TaskMessageKindDirective,
		Content:     body,
		Metadata:    meta,
		CreatedAt:   time.Now().UTC(),
	}
	if authorID != "" {
		msg.AuthorID = &authorID
	}
	if err := s.taskMessageRepo.Insert(ctx, msg); err != nil {
		return "amend-failed"
	}
	requeued := false
	if !scheduler.IsTerminalTaskStatus(task.Status) &&
		task.Status != persistence.TaskStatusRunning &&
		task.Status != persistence.TaskStatusLeased {
		now := time.Now().UTC()
		ok, _ := s.taskRepo.TransitionConditional(ctx, task.ID,
			[]persistence.TaskStatus{
				persistence.TaskStatusPending,
				persistence.TaskStatusAwaitingInput,
				persistence.TaskStatusAwaitingExternal,
				persistence.TaskStatusCompleted,
				persistence.TaskStatusPaused,
				persistence.TaskStatusQueued,
			},
			persistence.TaskStatusQueued,
			persistence.TransitionOpts{BriefAmendedAt: &now, ClearLease: true})
		requeued = ok
	}
	if requeued && s.rescheduler != nil {
		s.rescheduler.Wake()
	}
	return "brief-amended"
}

// uiPauseTask is the pause-specific handler. RUNNING tasks must
// route through executor.Pause so the in-flight goroutine gets
// SIGTERM + ctx-cancel; otherwise the goroutine runs to
// completion (success or failure) and overwrites task.Status with
// FAILED/COMPLETED 24s+ later, masking the operator's intent.
//
// Live evidence: T-…1c44 (2026-05-23) — UI pause flipped
// task.Status to PAUSED via uiSimpleFlip but executor's goroutine
// kept running, hit the merge step, and handleFailure overwrote
// PAUSED with FAILED.
//
// For non-RUNNING states (PENDING / QUEUED / LEASED /
// WAITING_FOR_CHILDREN / AWAITING_*), there is no executor
// goroutine to cancel; the bare TransitionConditional path is
// correct.
func (s *Server) uiPauseTask(ctx context.Context, task *persistence.Task) string {
	from := []persistence.TaskStatus{
		persistence.TaskStatusPending,
		persistence.TaskStatusQueued,
		persistence.TaskStatusLeased,
		persistence.TaskStatusRunning,
		persistence.TaskStatusWaitingForChildren,
		persistence.TaskStatusAwaitingInput,
		persistence.TaskStatusAwaitingExternal,
	}

	// RUNNING tasks: cancel the goroutine first. executor.Pause
	// stops the container, flips execution_status + task.Status
	// to PAUSED atomically, cancels ctx, and BLOCKS until the
	// activeExecutions entry is cleared. A successful Pause makes
	// the bare TransitionConditional below redundant — and skipping
	// it avoids the rare race where the goroutine's defer cleared
	// the status before we got here.
	if s.executor != nil && task.Status == persistence.TaskStatusRunning {
		if err := s.executor.Pause(task.ID); err == nil {
			// executor.Pause already flipped task.Status to PAUSED.
			meta, _ := json.Marshal(map[string]any{"kind": "paused", "from": string(task.Status)})
			_ = s.taskMessageRepo.Insert(ctx, &persistence.TaskMessage{
				TaskID:      task.ID,
				AuthorKind:  persistence.TaskMessageAuthorOperator,
				MessageKind: persistence.TaskMessageKindSystem,
				Content:     "task paused by operator",
				Metadata:    meta,
				CreatedAt:   time.Now().UTC(),
			})
			if s.sseBus != nil {
				s.sseBus.Publish(task.ID, SSEEvent{Kind: "status", Data: string(persistence.TaskStatusPaused)})
			}
			return "paused"
		} else if !strings.Contains(err.Error(), "no active execution") {
			// Real failure — surface it. "no active execution" is
			// a benign race (goroutine finished between caller's
			// load and here); fall through to the bare DB flip.
			s.logger.Warn().Err(err).Str("task_id", task.ID).Msg("uiPauseTask: executor.Pause failed; falling through to bare flip")
		}
	}

	// Non-RUNNING (or benign race) — bare conditional transition.
	return s.uiSimpleFlip(ctx, task, from, persistence.TaskStatusPaused, "paused", false)
}

// uiResumeTask is the resume-specific handler. Tasks paused by
// executor.Pause have an existing PAUSED execution that must be
// continued in place; flipping to QUEUED instead creates a NEW
// execution via the scheduler's dispatch path while the PAUSED
// row sits forever (operator-observed 2026-05-26 — the paused
// execution stayed parked and a new one started running on
// resume).
//
// Resolution: try executor.Resume first. That path loads the
// existing PAUSED execution row, flips it to RUNNING, and
// re-invokes runExecution on the SAME row so the checkpoint /
// step pointer / lineage data are preserved. Fall back to the
// bare flip-to-QUEUED only when there's no resumable execution
// (e.g. task paused before any execution started, or the
// execution was orphaned).
func (s *Server) uiResumeTask(ctx context.Context, task *persistence.Task) string {
	if s.executor != nil && task.Status == persistence.TaskStatusPaused {
		if err := s.executor.ResumeTask(task.ID); err == nil {
			meta, _ := json.Marshal(map[string]any{"kind": "resumed", "from": string(task.Status)})
			_ = s.taskMessageRepo.Insert(ctx, &persistence.TaskMessage{
				TaskID:      task.ID,
				AuthorKind:  persistence.TaskMessageAuthorOperator,
				MessageKind: persistence.TaskMessageKindSystem,
				Content:     "task resumed by operator",
				Metadata:    meta,
				CreatedAt:   time.Now().UTC(),
			})
			if s.sseBus != nil {
				s.sseBus.Publish(task.ID, SSEEvent{Kind: "status", Data: string(persistence.TaskStatusRunning)})
			}
			return "resumed"
		} else {
			// Common benign cases:
			//   - "no paused execution exists"      — task paused before any execution started
			//   - "execution is not paused"         — race with a daemon-startup recover
			//   - "task is already being executed"  — already resumed
			// All three are handled by falling through to the bare
			// flip-to-QUEUED, which lets the scheduler dispatch a
			// fresh execution. Real failures (DB blip, missing repo)
			// are also tolerated here — operator can re-click.
			s.logger.Info().Err(err).Str("task_id", task.ID).
				Msg("uiResumeTask: executor.Resume not applicable; falling through to flip-to-QUEUED")
		}
	}
	return s.uiSimpleFlip(ctx, task,
		[]persistence.TaskStatus{persistence.TaskStatusPaused},
		persistence.TaskStatusQueued, "resumed", true)
}

// uiApproveTask releases an autonomy task parked in AWAITING_APPROVAL
// into the queue. The scheduler then leases and runs it normally. See
// https://docs.vornik.io
func (s *Server) uiApproveTask(ctx context.Context, task *persistence.Task) string {
	return s.uiSimpleFlip(ctx, task,
		[]persistence.TaskStatus{persistence.TaskStatusAwaitingApproval},
		persistence.TaskStatusQueued, "approved", true)
}

// uiRejectTask declines an approval-gated task — it never runs.
func (s *Server) uiRejectTask(ctx context.Context, task *persistence.Task) string {
	return s.uiSimpleFlip(ctx, task,
		[]persistence.TaskStatus{persistence.TaskStatusAwaitingApproval},
		persistence.TaskStatusCancelled, "rejected", false)
}

func (s *Server) uiSimpleFlip(ctx context.Context, task *persistence.Task,
	from []persistence.TaskStatus, to persistence.TaskStatus, verb string, wakeOnSuccess bool,
) string {
	meta, _ := json.Marshal(map[string]any{"kind": verb, "from": string(task.Status)})
	msg := &persistence.TaskMessage{
		TaskID:      task.ID,
		AuthorKind:  persistence.TaskMessageAuthorOperator,
		MessageKind: persistence.TaskMessageKindSystem,
		Content:     "task " + verb + " by operator",
		Metadata:    meta,
		CreatedAt:   time.Now().UTC(),
	}
	if err := s.taskMessageRepo.Insert(ctx, msg); err != nil {
		return verb + "-failed"
	}
	opts := persistence.TransitionOpts{}
	if to == persistence.TaskStatusQueued {
		opts.ClearLease = true
	}
	ok, _ := s.taskRepo.TransitionConditional(ctx, task.ID, from, to, opts)
	if !ok {
		return verb + "-stale"
	}
	if wakeOnSuccess && s.rescheduler != nil {
		s.rescheduler.Wake()
	}
	if s.sseBus != nil {
		s.sseBus.Publish(task.ID, SSEEvent{Kind: "status", Data: string(to)})
	}
	return verb
}

func (s *Server) uiCloseTask(ctx context.Context, task *persistence.Task, r *http.Request) string {
	reason := strings.TrimSpace(r.FormValue("reason"))
	authorID := strings.TrimSpace(r.FormValue("author"))
	if err := scheduler.ValidateTransition(task.Status, persistence.TaskStatusClosed, scheduler.TriggerOperatorClose); err != nil {
		return "close-not-eligible"
	}
	closer := authorID
	if closer == "" {
		closer = "operator"
	}
	body := reason
	if body == "" {
		body = "closed by operator"
	}
	meta, _ := json.Marshal(map[string]any{"kind": "task_closed", "closedBy": closer, "reason": reason})
	msg := &persistence.TaskMessage{
		TaskID:      task.ID,
		AuthorKind:  persistence.TaskMessageAuthorSystem,
		MessageKind: persistence.TaskMessageKindSystem,
		Content:     body,
		Metadata:    meta,
		CreatedAt:   time.Now().UTC(),
	}
	if err := s.taskMessageRepo.Insert(ctx, msg); err != nil {
		return "close-failed"
	}
	ok, _ := s.taskRepo.TransitionConditional(ctx, task.ID,
		[]persistence.TaskStatus{
			persistence.TaskStatusCompleted,
			persistence.TaskStatusAwaitingInput,
			persistence.TaskStatusAwaitingExternal,
			persistence.TaskStatusFailed,
		},
		persistence.TaskStatusClosed,
		persistence.TransitionOpts{ClosedBy: &closer, SetClosedAtNow: true, ClearLease: true})
	if !ok {
		return "close-stale"
	}
	// Drive the parent-unblock sweep when this task is a child of a
	// parent that's been waiting on it. Without this hook the parent
	// sits in WAITING_FOR_CHILDREN forever (the executor's auto-sweep
	// only fires on executor-set terminal statuses, not operator
	// closure). Best-effort: if the executor isn't wired (rare —
	// minimal-container builds, tests) the parent is unblocked the
	// next time the executor sweeps for any other reason.
	if s.executor != nil && task.ParentTaskID != nil && *task.ParentTaskID != "" {
		s.executor.NotifyChildTerminal(ctx, task.ID)
	}
	if s.sseBus != nil {
		s.sseBus.Publish(task.ID, SSEEvent{Kind: "status", Data: "CLOSED"})
	}
	return "task-closed"
}
