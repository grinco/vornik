package email

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/persistence"
)

// followupStore is the per-channel pending-followup map. Keyed by
// taskID so NotifyTaskCompleted can look up the originating
// session in O(1). Mirrors the Telegram bot's pendingFollowups
// pattern — kept separate per channel so cross-channel leaks
// (a Telegram task firing on an email channel, etc.) are
// impossible by construction.
type followupStore struct {
	mu      sync.Mutex
	entries map[string]followupEntry
}

type followupEntry struct {
	SessionID string
	ProjectID string
}

func newFollowupStore() *followupStore {
	return &followupStore{entries: map[string]followupEntry{}}
}

func (s *followupStore) record(taskID, sessionID, projectID string) {
	if s == nil || taskID == "" || sessionID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries == nil {
		s.entries = map[string]followupEntry{}
	}
	s.entries[taskID] = followupEntry{SessionID: sessionID, ProjectID: projectID}
}

func (s *followupStore) claim(taskID string) (followupEntry, bool) {
	if s == nil {
		return followupEntry{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[taskID]
	if ok {
		delete(s.entries, taskID)
	}
	return e, ok
}

// RegisterFollowup records that an email session is waiting on
// the supplied task. Called by the dispatcher's create_task tool
// when await_completion=true and the inbound originated from this
// channel. Idempotent; second call wins (the dispatcher may
// re-issue create_task on a tool-loop retry).
//
// Sessions are identified by the email thread root Message-ID —
// see threadSessionID in channel.go. The dispatcher's Request
// carries the originating sessionID through OriginatingSessionID
// so the registration knows which thread to thread the resume on.
func (c *Channel) RegisterFollowup(sessionID, taskID, projectID string) {
	if c == nil {
		return
	}
	c.followups.record(taskID, sessionID, projectID)
	c.logger.Info().
		Str("task_id", taskID).
		Str("session_id", sessionID).
		Str("project", projectID).
		Msg("email: follow-up registered — thread will auto-resume on task completion")
}

// NotifyTaskCompleted implements executor.CompletionNotifier. The
// service container wires this so the executor's per-task
// "task done" event fans out to every channel that might be
// waiting. Each channel filters by its own pending map; tasks not
// registered here are no-ops, so wiring multiple notifiers can't
// cross-fire.
func (c *Channel) NotifyTaskCompleted(ctx context.Context, task *persistence.Task, success bool, message string) {
	if c == nil || task == nil {
		return
	}
	entry, ok := c.followups.claim(task.ID)
	if !ok {
		return
	}
	c.triggerFollowup(ctx, task, success, message, entry)
}

// triggerFollowup composes a synthetic user turn carrying the
// task's outcome and feeds it to the dispatcher receiver. Same
// shape as the Telegram bot's triggerFollowup, simplified for the
// email channel:
//   - no per-task notification (the inbound thread IS the
//     notification surface)
//   - no artifact list — the LLM's reply can call read_artifact
//     when it wants to attach files; pushing every artifact into
//     the prompt would bloat the context window for routine
//     "task done" cycles
//   - no findActiveDescendant leaf walk — the parent-wait fix in
//     workflow.go (commit 770df1d) makes that workaround
//     redundant; by the time we land here the parent's TERMINAL
//     status truly reflects the whole tree's outcome
func (c *Channel) triggerFollowup(ctx context.Context, task *persistence.Task, success bool, message string, entry followupEntry) {
	c.recvMu.RLock()
	recv := c.recv
	c.recvMu.RUnlock()
	if recv == nil {
		c.logger.Warn().
			Str("task_id", task.ID).
			Str("session_id", entry.SessionID).
			Msg("email: follow-up fired but Receiver not bound — dropping resume")
		return
	}

	// Status text is derived from `success` rather than task.Status —
	// see telegram/bot.go:triggerFollowup for the 2026-05-21 incident
	// that motivated this: the executor's in-memory *Task is stale
	// (still "LEASED") when NotifyTaskCompleted fires because
	// taskRepo.UpdateStatus only writes to the DB row. Reading from
	// the success bool the executor already passed keeps the
	// synthetic turn coherent.
	var sb strings.Builder
	if success {
		fmt.Fprintf(&sb, "[Task %s completed successfully.]\n", task.ID)
	} else {
		fmt.Fprintf(&sb, "[Task %s did NOT complete successfully. Status: %s.]\n", task.ID, task.Status)
		if task.LastError != nil && *task.LastError != "" {
			errMsg := *task.LastError
			if len(errMsg) > 1500 {
				errMsg = errMsg[:1500] + "…"
			}
			fmt.Fprintf(&sb, "Error: %s\n", errMsg)
		}
	}
	if message != "" {
		trimmed := message
		if len(trimmed) > 800 {
			trimmed = trimmed[:800] + "…"
		}
		fmt.Fprintf(&sb, "Last status: %s\n", trimmed)
	}

	c.logger.Info().
		Str("task_id", task.ID).
		Str("session_id", entry.SessionID).
		Bool("success", success).
		Msg("email: auto-resume firing — task complete, threading on session")

	msg := conversation.ChannelMessage{
		Source:    channelName,
		SessionID: entry.SessionID,
		Text:      sb.String(),
		// SpeakerID intentionally empty — this is a synthetic
		// turn from the system, not a human. The dispatcher's
		// system-prompt path already accepts a non-attributed
		// user message; ResolveSpeaker is bypassed because the
		// session is already known on this channel.
	}
	if err := recv.Receive(ctx, msg); err != nil {
		c.logger.Warn().
			Err(err).
			Str("task_id", task.ID).
			Str("session_id", entry.SessionID).
			Msg("email: follow-up Receive returned error — resume lost")
	}
}
