package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/scheduler"
)

// Phase 24 of the conversational task lifecycle (LLD:
// https://docs.vornik.io §5.4).
//
// Endpoints — every operator-driven write goes through here:
//
//   POST /api/v1/projects/{p}/tasks/{id}/messages
//   GET  /api/v1/projects/{p}/tasks/{id}/messages?after=<id>&limit=<n>
//   POST /api/v1/projects/{p}/tasks/{id}/messages/{msg_id}/answer
//   POST /api/v1/projects/{p}/tasks/{id}/amend
//   POST /api/v1/projects/{p}/tasks/{id}/pause
//   POST /api/v1/projects/{p}/tasks/{id}/resume
//   POST /api/v1/projects/{p}/tasks/{id}/close
//
// All mutations:
//   1. Validate the requested transition via scheduler.ValidateTransition.
//   2. Write a task_message row BEFORE the state mutation — so even
//      a half-failed write leaves an audit breadcrumb.
//   3. Use TransitionConditional so concurrent answers / amends /
//      closures don't corrupt state. First write wins; second
//      gets 409.

const maxMessageContentBytes = 64 * 1024

// taskConversationDeps captures the shared dependency check —
// every handler in this file requires both the task message repo
// and the (existing) task repo. Centralised so each handler stays
// short.
func (s *Server) taskConversationReady() (string, bool) {
	if s.taskRepo == nil {
		return "task repository not configured", false
	}
	if s.taskMessageRepo == nil {
		return "conversational task lifecycle disabled (task_messages repo not wired)", false
	}
	return "", true
}

// taskFromRequest loads the task and verifies it lives in the URL's
// project. Returns the task on success, or writes the appropriate
// HTTP error and returns nil.
func (s *Server) taskFromRequest(w http.ResponseWriter, r *http.Request) (*persistence.Task, string, string) {
	projectID := extractProjectID(r)
	taskID := extractTaskID(r)
	if projectID == "" || taskID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "projectId and taskId are required")
		return nil, "", ""
	}
	task, err := s.taskRepo.Get(r.Context(), taskID)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
			respondError(w, http.StatusNotFound, "NOT_FOUND", "Task not found")
			return nil, "", ""
		}
		s.logger.Error().Err(err).Str("taskId", taskID).Msg("failed to get task")
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Failed to load task")
		return nil, "", ""
	}
	if task.ProjectID != projectID {
		respondError(w, http.StatusNotFound, "NOT_FOUND", "Task not found in project")
		return nil, "", ""
	}
	return task, projectID, taskID
}

// readJSONBody enforces a simple body cap + JSON shape on POST
// handlers. Returns false when the response was already written.
func (s *Server) readJSONBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, int64(maxMessageContentBytes)+4096)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

// ----------------------------------------------------------------
// GET messages
// ----------------------------------------------------------------

// ListTaskMessages handles GET /tasks/{id}/messages.
func (s *Server) ListTaskMessages(w http.ResponseWriter, r *http.Request) {
	if reason, ok := s.taskConversationReady(); !ok {
		respondError(w, http.StatusServiceUnavailable, "TASK_LIFECYCLE_DISABLED", reason)
		return
	}
	task, _, taskID := s.taskFromRequest(w, r)
	if task == nil {
		return
	}
	q := r.URL.Query()
	filter := persistence.TaskMessageFilter{TaskID: taskID}
	if v := q.Get("after"); v != "" {
		filter.After = &v
	}
	if v := q.Get("limit"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			filter.Limit = n
		}
	}
	if v := q.Get("kind"); v != "" {
		filter.MessageKinds = strings.Split(v, ",")
	}
	msgs, err := s.taskMessageRepo.List(r.Context(), filter)
	if err != nil {
		s.logger.Error().Err(err).Str("taskId", taskID).Msg("ListTaskMessages: List failed")
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to list messages")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"taskId":   taskID,
		"messages": msgs,
		"count":    len(msgs),
	})
}

// ----------------------------------------------------------------
// POST messages — generic compose
// ----------------------------------------------------------------

// PostTaskMessageRequest is the inbound shape for compose (kind=message)
// or directive. Answers go through the dedicated /answer route.
type PostTaskMessageRequest struct {
	Kind     string          `json:"kind"` // "message" | "directive" | "note"
	Content  string          `json:"content"`
	AuthorID string          `json:"authorId,omitempty"`
	Metadata json.RawMessage `json:"metadata,omitempty"`
}

// PostTaskMessage handles POST /tasks/{id}/messages.
//
// Allowed inbound kinds: "message", "directive". Other kinds
// (checkpoint, plan, phase_marker, note, closure_request, system)
// are written by the daemon, not the operator — they get rejected
// here. "answer" has its own route because it requires the
// checkpoint cross-reference.
func (s *Server) PostTaskMessage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "")
		return
	}
	if reason, ok := s.taskConversationReady(); !ok {
		respondError(w, http.StatusServiceUnavailable, "TASK_LIFECYCLE_DISABLED", reason)
		return
	}
	task, _, taskID := s.taskFromRequest(w, r)
	if task == nil {
		return
	}
	var req PostTaskMessageRequest
	if !s.readJSONBody(w, r, &req) {
		return
	}
	switch req.Kind {
	case persistence.TaskMessageKindMessage,
		persistence.TaskMessageKindDirective:
		// allowed
	case "":
		req.Kind = persistence.TaskMessageKindMessage
	default:
		respondError(w, http.StatusBadRequest, "INVALID_KIND",
			fmt.Sprintf("operator-write kind must be 'message' or 'directive'; got %q", req.Kind))
		return
	}
	if strings.TrimSpace(req.Content) == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "content required")
		return
	}
	if len(req.Content) > maxMessageContentBytes {
		req.Content = req.Content[:maxMessageContentBytes]
	}

	msg := &persistence.TaskMessage{
		TaskID:      taskID,
		AuthorKind:  persistence.TaskMessageAuthorOperator,
		MessageKind: req.Kind,
		Content:     req.Content,
		CreatedAt:   time.Now().UTC(),
	}
	if req.AuthorID != "" {
		msg.AuthorID = &req.AuthorID
	}
	if len(req.Metadata) > 0 {
		msg.Metadata = []byte(req.Metadata)
	}
	if err := s.taskMessageRepo.Insert(r.Context(), msg); err != nil {
		s.logger.Error().Err(err).Str("taskId", taskID).Msg("PostTaskMessage: insert failed")
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to write message")
		return
	}

	// Re-queue policy: any operator-typed input on an at-rest task
	// re-engages the lead, regardless of whether the kind is
	// "message" or "directive". Operators don't reliably
	// distinguish the two in their head ("Refresh the coverage
	// data" reads identical whether labelled message or directive);
	// making messages a no-op was a UX trap. Kind still
	// differentiates intent in the audit log and the lead's
	// prompt-precedence rule.
	//
	// Two re-queue strategies depending on origin (LLD §7.0):
	//
	//  - Waiting states (AWAITING_INPUT / AWAITING_EXTERNAL): the
	//    task didn't fail, it was at-rest. TransitionConditional
	//    clears the lease and flips status; attempt counter
	//    untouched.
	//
	//  - Terminal states (FAILED / CANCELLED / COMPLETED): the
	//    operator is course-correcting. RequeueTerminalTask resets
	//    attempt=1 (granting a fresh max_attempts budget),
	//    preserves max_attempts, and clears last_error /
	//    last_error_class atomically — so the next execution
	//    starts cleanly. CLOSED is intentionally excluded:
	//    archival is one-way; use /retry to revive a closed task.
	requeued := false
	switch task.Status {
	case persistence.TaskStatusAwaitingInput,
		persistence.TaskStatusAwaitingExternal:
		ok, terr := s.taskRepo.TransitionConditional(r.Context(), taskID,
			[]persistence.TaskStatus{task.Status},
			persistence.TaskStatusQueued,
			persistence.TransitionOpts{ClearLease: true})
		if terr != nil {
			s.logger.Error().Err(terr).Str("taskId", taskID).Msg("PostTaskMessage: re-queue failed")
			respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to re-queue task")
			return
		}
		requeued = ok
	case persistence.TaskStatusFailed,
		persistence.TaskStatusCancelled,
		persistence.TaskStatusCompleted:
		ok, terr := s.taskRepo.RequeueTerminalTask(r.Context(), taskID, 1, task.MaxAttempts)
		if terr != nil {
			s.logger.Error().Err(terr).Str("taskId", taskID).Msg("PostTaskMessage: terminal re-queue failed")
			respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to re-queue task")
			return
		}
		requeued = ok
	}
	if requeued && s.rescheduler != nil {
		s.rescheduler.Wake()
	}
	respondJSON(w, http.StatusCreated, map[string]any{
		"messageId": msg.ID,
		"requeued":  requeued,
	})
}

// ----------------------------------------------------------------
// POST answer — reply to a checkpoint
// ----------------------------------------------------------------

// AnswerCheckpointRequest is the inbound shape for resolving an
// open checkpoint. Content carries the operator's reply text;
// metadata.choice (optional) carries a structured selection from
// a `decision` checkpoint.
type AnswerCheckpointRequest struct {
	Content  string          `json:"content"`
	AuthorID string          `json:"authorId,omitempty"`
	Choice   string          `json:"choice,omitempty"`   // for decision checkpoints
	Metadata json.RawMessage `json:"metadata,omitempty"` // additional structured payload
}

// AnswerCheckpoint handles POST /tasks/{id}/messages/{msg_id}/answer.
// Concurrent first-write-wins: a second operator answering the
// same checkpoint gets 409 with the existing answer attached.
func (s *Server) AnswerCheckpoint(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "")
		return
	}
	if reason, ok := s.taskConversationReady(); !ok {
		respondError(w, http.StatusServiceUnavailable, "TASK_LIFECYCLE_DISABLED", reason)
		return
	}
	task, _, taskID := s.taskFromRequest(w, r)
	if task == nil {
		return
	}
	checkpointID := extractCheckpointID(r)
	if checkpointID == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "checkpoint id required")
		return
	}

	var req AnswerCheckpointRequest
	if !s.readJSONBody(w, r, &req) {
		return
	}

	// Validate the requested transition before any write so an
	// answer to a checkpoint on a CLOSED task gets rejected up
	// front.
	if err := scheduler.ValidateTransition(
		task.Status, persistence.TaskStatusQueued,
		scheduler.TriggerOperatorAnswer,
	); err != nil {
		respondError(w, http.StatusConflict, "INVALID_STATE", err.Error())
		return
	}

	// Confirm the checkpoint is the open one for this task. The
	// task.OpenCheckpointID is denormalised from task_messages and
	// is the authoritative pointer; mismatch → 409.
	if task.OpenCheckpointID == nil || *task.OpenCheckpointID != checkpointID {
		respondError(w, http.StatusConflict, "INVALID_STATE",
			"checkpoint is no longer open (resolved or superseded)")
		return
	}

	// Build the answer message metadata. Carry the choice (if any)
	// and the operator-supplied metadata side-by-side.
	metaMap := map[string]any{}
	if req.Choice != "" {
		metaMap["choice"] = req.Choice
	}
	if len(req.Metadata) > 0 {
		var raw map[string]any
		if err := json.Unmarshal(req.Metadata, &raw); err == nil {
			for k, v := range raw {
				metaMap[k] = v
			}
		}
	}
	metaBytes, _ := json.Marshal(metaMap)

	if strings.TrimSpace(req.Content) == "" && req.Choice == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "content or choice required")
		return
	}

	// Choice-only answers must still carry readable Content: the chat
	// log renders Content, and the lead agent's conversation view
	// reads Content — an empty answer is invisible to both
	// (regression: task …a691c512ebd1c4fd, the lead re-asked an
	// already-answered checkpoint). Resolve the option label from the
	// open checkpoint's metadata; fall back to the raw choice id.
	content := req.Content
	if strings.TrimSpace(content) == "" && req.Choice != "" {
		content = req.Choice
		if cp, err := s.taskMessageRepo.GetOpenCheckpoint(r.Context(), taskID); err == nil && cp != nil {
			if label := persistence.CheckpointOptionLabel(cp.Metadata, req.Choice); label != "" {
				content = label
			}
		}
	}

	answer := &persistence.TaskMessage{
		TaskID:      taskID,
		ParentID:    &checkpointID,
		AuthorKind:  persistence.TaskMessageAuthorOperator,
		MessageKind: persistence.TaskMessageKindAnswer,
		Content:     content,
		Metadata:    metaBytes,
		CreatedAt:   time.Now().UTC(),
	}
	if req.AuthorID != "" {
		answer.AuthorID = &req.AuthorID
	}
	if err := s.taskMessageRepo.Insert(r.Context(), answer); err != nil {
		s.logger.Error().Err(err).Str("taskId", taskID).Msg("AnswerCheckpoint: insert failed")
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to write answer")
		return
	}

	// Mark the checkpoint resolved (idempotent on retry).
	if err := s.taskMessageRepo.MarkCheckpointResolved(r.Context(), taskID, checkpointID); err != nil {
		s.logger.Error().Err(err).Str("taskId", taskID).Msg("AnswerCheckpoint: resolve failed")
		// We've already written the answer; surface the resolve
		// error but report the message id so the caller knows
		// what to expect.
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "answer recorded but checkpoint resolution failed")
		return
	}

	// Atomic AWAITING_INPUT → QUEUED. If the task drifted
	// concurrently (another operator answered first; daemon picked
	// it back up), report 409 with the live status.
	ok, err := s.taskRepo.TransitionConditional(r.Context(), taskID,
		[]persistence.TaskStatus{persistence.TaskStatusAwaitingInput},
		persistence.TaskStatusQueued,
		persistence.TransitionOpts{ClearLease: true},
	)
	if err != nil {
		s.logger.Error().Err(err).Str("taskId", taskID).Msg("AnswerCheckpoint: transition failed")
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to re-queue task")
		return
	}
	if !ok {
		// Drift — another operator already answered; respond 409.
		fresh, gerr := s.taskRepo.Get(r.Context(), taskID)
		if gerr != nil {
			respondError(w, http.StatusConflict, "INVALID_STATE", "task no longer in AWAITING_INPUT")
			return
		}
		respondError(w, http.StatusConflict, "INVALID_STATE",
			fmt.Sprintf("task is now %s; checkpoint already resolved", fresh.Status))
		return
	}

	if s.rescheduler != nil {
		s.rescheduler.Wake()
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"messageId": answer.ID,
		"requeued":  true,
	})
}

// ----------------------------------------------------------------
// POST amend — operator changes the brief / scope
// ----------------------------------------------------------------

// AmendBriefRequest carries the new brief text. The original brief
// stays in the task payload; this writes a `system` message
// summarising the change and re-queues the task.
type AmendBriefRequest struct {
	NewBrief string `json:"newBrief"`
	Reason   string `json:"reason,omitempty"`
	AuthorID string `json:"authorId,omitempty"`
}

// AmendBrief handles POST /tasks/{id}/amend.
func (s *Server) AmendBrief(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "")
		return
	}
	if reason, ok := s.taskConversationReady(); !ok {
		respondError(w, http.StatusServiceUnavailable, "TASK_LIFECYCLE_DISABLED", reason)
		return
	}
	task, _, taskID := s.taskFromRequest(w, r)
	if task == nil {
		return
	}
	var req AmendBriefRequest
	if !s.readJSONBody(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.NewBrief) == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "newBrief required")
		return
	}

	if scheduler.IsTerminalTaskStatus(task.Status) {
		respondError(w, http.StatusConflict, "INVALID_STATE",
			fmt.Sprintf("cannot amend a %s task", task.Status))
		return
	}

	// Build a system message that records the amendment shape so
	// the lead's next execution sees what changed.
	body := req.NewBrief
	if req.Reason != "" {
		body = req.Reason + "\n\n---\n\n" + body
	}
	meta, _ := json.Marshal(map[string]any{
		"kind":   "brief_amendment",
		"reason": req.Reason,
	})
	msg := &persistence.TaskMessage{
		TaskID:      taskID,
		AuthorKind:  persistence.TaskMessageAuthorOperator,
		MessageKind: persistence.TaskMessageKindDirective,
		Content:     body,
		Metadata:    meta,
		CreatedAt:   time.Now().UTC(),
	}
	if req.AuthorID != "" {
		msg.AuthorID = &req.AuthorID
	}
	if err := s.taskMessageRepo.Insert(r.Context(), msg); err != nil {
		s.logger.Error().Err(err).Str("taskId", taskID).Msg("AmendBrief: insert failed")
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to write amendment")
		return
	}

	// Best-effort re-queue per directive policy. RUNNING/LEASED
	// don't transition (LLD §7 trigger 2: queue for next round);
	// other non-terminal states transition to QUEUED.
	requeued := false
	if !scheduler.IsTerminalTaskStatus(task.Status) &&
		task.Status != persistence.TaskStatusRunning &&
		task.Status != persistence.TaskStatusLeased {
		now := time.Now().UTC()
		ok, terr := s.taskRepo.TransitionConditional(r.Context(), taskID,
			[]persistence.TaskStatus{
				persistence.TaskStatusPending,
				persistence.TaskStatusAwaitingInput,
				persistence.TaskStatusAwaitingExternal,
				persistence.TaskStatusCompleted,
				persistence.TaskStatusPaused,
				persistence.TaskStatusQueued,
			},
			persistence.TaskStatusQueued,
			persistence.TransitionOpts{
				BriefAmendedAt: &now,
				ClearLease:     true,
			})
		if terr != nil {
			s.logger.Error().Err(terr).Str("taskId", taskID).Msg("AmendBrief: transition failed")
		}
		requeued = ok
	}
	if requeued && s.rescheduler != nil {
		s.rescheduler.Wake()
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"messageId": msg.ID,
		"requeued":  requeued,
	})
}

// ----------------------------------------------------------------
// Pause / resume / close
// ----------------------------------------------------------------

// PauseTaskRequest carries optional reason + author.
type PauseTaskRequest struct {
	Reason   string `json:"reason,omitempty"`
	AuthorID string `json:"authorId,omitempty"`
}

// PauseTask handles POST /tasks/{id}/pause.
func (s *Server) PauseTask(w http.ResponseWriter, r *http.Request) {
	s.simpleStatusFlip(w, r,
		"pause",
		[]persistence.TaskStatus{
			persistence.TaskStatusPending,
			persistence.TaskStatusQueued,
			persistence.TaskStatusLeased,
			persistence.TaskStatusRunning,
			persistence.TaskStatusWaitingForChildren,
			persistence.TaskStatusAwaitingInput,
			persistence.TaskStatusAwaitingExternal,
		},
		persistence.TaskStatusPaused,
		scheduler.TriggerOperatorPause,
	)
}

// ResumeTask handles POST /tasks/{id}/resume.
func (s *Server) ResumeTask(w http.ResponseWriter, r *http.Request) {
	s.simpleStatusFlip(w, r,
		"resume",
		[]persistence.TaskStatus{persistence.TaskStatusPaused},
		persistence.TaskStatusQueued,
		scheduler.TriggerOperatorResume,
	)
}

// ApproveTask handles POST /tasks/{id}/approve — the autonomy
// manual-approval gate. A task parked in AWAITING_APPROVAL (created
// under a project with requireApproval) is released into the queue;
// the scheduler then leases and runs it normally. Only AWAITING_APPROVAL
// is approvable — a stuck PENDING task is not (the state-machine
// validator returns the 409). simpleStatusFlip writes the operator
// audit message, runs the atomic transition (ClearLease since the
// destination is QUEUED), and wakes the scheduler. See
// https://docs.vornik.io
func (s *Server) ApproveTask(w http.ResponseWriter, r *http.Request) {
	s.simpleStatusFlip(w, r,
		"approve",
		[]persistence.TaskStatus{persistence.TaskStatusAwaitingApproval},
		persistence.TaskStatusQueued,
		scheduler.TriggerOperatorApprove,
	)
}

// RejectTask handles POST /tasks/{id}/reject — the operator declined an
// approval-gated task, so it never runs. AWAITING_APPROVAL → CANCELLED.
func (s *Server) RejectTask(w http.ResponseWriter, r *http.Request) {
	s.simpleStatusFlip(w, r,
		"reject",
		[]persistence.TaskStatus{persistence.TaskStatusAwaitingApproval},
		persistence.TaskStatusCancelled,
		scheduler.TriggerOperatorReject,
	)
}

// CloseTaskRequest carries the operator's closure reason.
type CloseTaskRequest struct {
	Reason   string `json:"reason,omitempty"`
	AuthorID string `json:"authorId,omitempty"`
}

// CloseTask handles POST /tasks/{id}/close.
func (s *Server) CloseTask(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "")
		return
	}
	if reason, ok := s.taskConversationReady(); !ok {
		respondError(w, http.StatusServiceUnavailable, "TASK_LIFECYCLE_DISABLED", reason)
		return
	}
	task, _, taskID := s.taskFromRequest(w, r)
	if task == nil {
		return
	}
	var req CloseTaskRequest
	limitJSONBody(w, r)
	_ = json.NewDecoder(r.Body).Decode(&req) // body optional; still size-capped

	if err := scheduler.ValidateTransition(
		task.Status, persistence.TaskStatusClosed,
		scheduler.TriggerOperatorClose,
	); err != nil {
		respondError(w, http.StatusConflict, "INVALID_STATE", err.Error())
		return
	}

	// System message recording the closure (operator-attributed).
	closer := req.AuthorID
	if closer == "" {
		closer = "operator"
	}
	body := req.Reason
	if body == "" {
		body = "closed by operator"
	}
	meta, _ := json.Marshal(map[string]any{
		"kind":     "task_closed",
		"closedBy": closer,
		"reason":   req.Reason,
	})
	msg := &persistence.TaskMessage{
		TaskID:      taskID,
		AuthorKind:  persistence.TaskMessageAuthorSystem,
		MessageKind: persistence.TaskMessageKindSystem,
		Content:     body,
		Metadata:    meta,
		CreatedAt:   time.Now().UTC(),
	}
	if err := s.taskMessageRepo.Insert(r.Context(), msg); err != nil {
		s.logger.Error().Err(err).Str("taskId", taskID).Msg("CloseTask: insert failed")
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to write closure message")
		return
	}

	ok, err := s.taskRepo.TransitionConditional(r.Context(), taskID,
		[]persistence.TaskStatus{
			persistence.TaskStatusCompleted,
			persistence.TaskStatusAwaitingInput,
			persistence.TaskStatusAwaitingExternal,
		},
		persistence.TaskStatusClosed,
		persistence.TransitionOpts{
			ClosedBy:       &closer,
			SetClosedAtNow: true,
			ClearLease:     true,
		},
	)
	if err != nil {
		s.logger.Error().Err(err).Str("taskId", taskID).Msg("CloseTask: transition failed")
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to close task")
		return
	}
	if !ok {
		respondError(w, http.StatusConflict, "INVALID_STATE", "task drifted out of close-eligible state")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"taskId":   taskID,
		"status":   string(persistence.TaskStatusClosed),
		"closedBy": closer,
	})
}

// simpleStatusFlip is the shared handler body for pause/resume —
// validate transition, write a system message, do an atomic
// conditional UPDATE, hint the scheduler. Close has its own
// handler because the metadata + closed_at/closed_by fields are
// load-bearing.
func (s *Server) simpleStatusFlip(
	w http.ResponseWriter, r *http.Request,
	verb string,
	from []persistence.TaskStatus,
	to persistence.TaskStatus,
	trigger scheduler.TransitionTrigger,
) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "")
		return
	}
	if reason, ok := s.taskConversationReady(); !ok {
		respondError(w, http.StatusServiceUnavailable, "TASK_LIFECYCLE_DISABLED", reason)
		return
	}
	task, _, taskID := s.taskFromRequest(w, r)
	if task == nil {
		return
	}
	if err := scheduler.ValidateTransition(task.Status, to, trigger); err != nil {
		respondError(w, http.StatusConflict, "INVALID_STATE", err.Error())
		return
	}
	// System message records the operator action.
	meta, _ := json.Marshal(map[string]any{"kind": verb, "from": string(task.Status)})
	msg := &persistence.TaskMessage{
		TaskID:      taskID,
		AuthorKind:  persistence.TaskMessageAuthorOperator,
		MessageKind: persistence.TaskMessageKindSystem,
		Content:     "task " + verb + "d by operator",
		Metadata:    meta,
		CreatedAt:   time.Now().UTC(),
	}
	if err := s.taskMessageRepo.Insert(r.Context(), msg); err != nil {
		s.logger.Error().Err(err).Str("taskId", taskID).Msg(verb + ": insert failed")
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to write "+verb+" message")
		return
	}
	// Pause path: if the task is currently running with an active
	// executor goroutine, executor.Pause() does the right thing —
	// stops the container, stamps the pause reason in the state
	// snapshot, flips DB status, and BLOCKS until the goroutine's
	// activeExecutions entry is cleared. Without this call the API
	// just flipped the DB row, leaving the goroutine alive; the
	// next Resume would find activeExecutions[taskID] still
	// populated and the scheduler dispatch would terminal-fail
	// with "task is already being executed". Live evidence:
	// exec_8bec1d…5e89 (2026-05-10) — pause/resume cycles failed
	// 100% of the time before this fix. If the task is in a
	// non-running state (PENDING / QUEUED / WAITING_FOR_CHILDREN /
	// AWAITING_*), executor.Pause returns "no active execution"
	// and we fall through to the bare DB transition below — which
	// is what those states need anyway.
	if verb == "pause" && s.executor != nil && task.Status == persistence.TaskStatusRunning {
		if _, err := s.executor.Pause(taskID); err != nil {
			// "no active execution" is a benign race (goroutine
			// finished between our load and here) — fall through
			// to the DB transition. Anything else is a real
			// failure: surface it.
			if !strings.Contains(err.Error(), "no active execution") {
				s.logger.Error().Err(err).Str("taskId", taskID).Msg("pause: executor.Pause failed")
				respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to pause executor: "+err.Error())
				return
			}
		} else {
			// executor.Pause already flipped status to PAUSED.
			// Skip the redundant DB transition.
			respondJSON(w, http.StatusOK, map[string]any{
				"taskId": taskID,
				"status": string(persistence.TaskStatusPaused),
			})
			return
		}
	}

	opts := persistence.TransitionOpts{}
	if to == persistence.TaskStatusQueued {
		opts.ClearLease = true
	}
	ok, err := s.taskRepo.TransitionConditional(r.Context(), taskID, from, to, opts)
	if err != nil {
		s.logger.Error().Err(err).Str("taskId", taskID).Msg(verb + ": transition failed")
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to "+verb+" task")
		return
	}
	if !ok {
		respondError(w, http.StatusConflict, "INVALID_STATE", "task drifted out of "+verb+"-eligible state")
		return
	}
	// Autonomy manual-approval resolution counter (vornik_autonomy_approvals_total).
	if s.apiMetrics != nil && s.apiMetrics.ApprovalsTotal != nil {
		switch verb {
		case "approve":
			s.apiMetrics.ApprovalsTotal.WithLabelValues(task.ProjectID, "approved").Inc()
		case "reject":
			s.apiMetrics.ApprovalsTotal.WithLabelValues(task.ProjectID, "rejected").Inc()
		}
	}
	if to == persistence.TaskStatusQueued && s.rescheduler != nil {
		s.rescheduler.Wake()
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"taskId": taskID,
		"status": string(to),
	})
}

// SummarizeThreadRequest is the lead's call to compress a span of
// older messages into a single `note` task_message. The lead
// generates the summary text itself; this endpoint just persists
// it and records which message_ids it covered so the prompt
// builder can filter them out of the lead's working window on
// future executions.
type SummarizeThreadRequest struct {
	MessageIDs []string `json:"messageIds"`
	Summary    string   `json:"summary"`
	AuthorID   string   `json:"authorId,omitempty"` // defaults to "lead"
}

// SummarizeThread handles POST /tasks/{id}/summarize.
//
// Phase 32 of the conversational task lifecycle. The lead calls
// this when its conversation window is getting long and it wants
// to compress older messages into a single summary that travels
// with the task as a `note`. Subsequent prompt-building filters
// out the original messages in favour of the summary.
//
// Authority: this endpoint is called by an agent inside a
// container, not by an operator. The author_kind is fixed to
// "lead" (the only non-operator producer of summaries).
func (s *Server) SummarizeThread(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "")
		return
	}
	if reason, ok := s.taskConversationReady(); !ok {
		respondError(w, http.StatusServiceUnavailable, "TASK_LIFECYCLE_DISABLED", reason)
		return
	}
	task, _, taskID := s.taskFromRequest(w, r)
	if task == nil {
		return
	}
	var req SummarizeThreadRequest
	if !s.readJSONBody(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Summary) == "" {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "summary required")
		return
	}
	if len(req.MessageIDs) == 0 {
		respondError(w, http.StatusBadRequest, "VALIDATION_ERROR", "messageIds required (at least one)")
		return
	}
	if len(req.Summary) > maxMessageContentBytes {
		req.Summary = req.Summary[:maxMessageContentBytes]
	}

	authorID := req.AuthorID
	if authorID == "" {
		authorID = "lead"
	}
	meta, _ := json.Marshal(map[string]any{
		"kind":                     "thread_summary",
		"summarized_message_ids":   req.MessageIDs,
		"summarized_message_count": len(req.MessageIDs),
	})

	msg := &persistence.TaskMessage{
		TaskID:      taskID,
		AuthorKind:  persistence.TaskMessageAuthorLead,
		AuthorID:    &authorID,
		MessageKind: persistence.TaskMessageKindNote,
		Content:     req.Summary,
		Metadata:    meta,
		CreatedAt:   time.Now().UTC(),
	}
	if err := s.taskMessageRepo.Insert(r.Context(), msg); err != nil {
		s.logger.Error().Err(err).Str("taskId", taskID).Msg("SummarizeThread: insert failed")
		respondError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to write summary")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"messageId":       msg.ID,
		"summarizedCount": len(req.MessageIDs),
	})
}

// extractCheckpointID pulls /messages/{id}/answer's id out of the URL.
func extractCheckpointID(r *http.Request) string {
	// Path shape: /api/v1/projects/{p}/tasks/{t}/messages/{id}/answer
	parts := strings.Split(r.URL.Path, "/")
	for i, p := range parts {
		if p == "messages" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}
