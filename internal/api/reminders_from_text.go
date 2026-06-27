package api

// Natural-language reminder creation endpoint.
//
//   POST /api/v1/reminders/from-text
//
// Takes free-form text + operator context, runs it through the
// reminders parser (LLM-backed), and commits a one-shot OR
// recurring dispatcher_reminders row. Returns the parsed shape +
// the created reminder row. Recurring rows carry a 5-field POSIX
// cron expression that the heartbeat re-arms after every fire;
// see migration 67 + internal/reminders/cron.go.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/reminders"
)

// FromTextRequest is the inbound shape for POST
// /api/v1/reminders/from-text. Mirrors the CLI flags so the
// REST layer + CLI layer share serialisation.
type FromTextRequest struct {
	// Text is the free-form natural-language input. Required.
	Text string `json:"text"`

	// OperatorID identifies the operator the reminder belongs
	// to. Same shape as the legacy set_reminder tool — "channel:
	// id" (e.g. "telegram:42", "webchat:abc123"). Required.
	OperatorID string `json:"operator_id"`

	// Channel + ChannelRef describe where the reminder is
	// delivered when it fires. Channel is one of "telegram",
	// "slack", "email", "github", "webchat" — same values the
	// receivers register under. ChannelRef is the
	// channel-specific routing target (telegram chat_id as
	// string, slack channel/thread id, email message-id, etc.).
	// Both required.
	Channel    string `json:"channel"`
	ChannelRef string `json:"channel_ref"`

	// ProjectID scopes the reminder to a project. Optional —
	// global operator reminders leave it empty.
	ProjectID string `json:"project_id,omitempty"`

	// OperatorTimezone is the IANA TZ the LLM uses to resolve
	// relative phrasings ("tomorrow at 9"). Empty defaults to
	// UTC; production callers pass the operator's TZ from the
	// (future) operator-profile surface.
	OperatorTimezone string `json:"operator_timezone,omitempty"`

	// DryRun skips the dispatcher_reminders INSERT — the
	// response carries only the parsed intent. The CLI uses
	// this for the y/N confirmation prompt before commit.
	DryRun bool `json:"dry_run,omitempty"`
}

// FromTextResponse is the outbound shape. The parsed intent is
// always populated on success; Reminder is set on a real
// commit (DryRun=false) and absent on dry-run.
type FromTextResponse struct {
	// Intent carries the parser's structured output. Human-
	// readable fire_at + confidence + reasoning drive the
	// confirmation UX.
	Intent FromTextIntentJSON `json:"intent"`

	// Reminder is the created row, present only on a real
	// commit. Same shape ListReminders / ShowReminder use.
	Reminder *ReminderEntryJSON `json:"reminder,omitempty"`
}

// FromTextIntentJSON mirrors reminders.ReminderIntent for the
// wire. Times as RFC3339 strings to match the rest of the
// reminders surface. Kind/CronExpr/RecurrenceUntil are zero-
// valued for one-shot intents, populated for recurring.
type FromTextIntentJSON struct {
	Kind            string  `json:"kind"`
	FireAt          string  `json:"fire_at"`
	CronExpr        string  `json:"cron_expr,omitempty"`
	RecurrenceUntil string  `json:"recurrence_until,omitempty"`
	Content         string  `json:"content"`
	Confidence      float64 `json:"confidence"`
	Reasoning       string  `json:"reasoning,omitempty"`
}

const maxReminderFromTextBodyBytes = 8 * 1024

// CreateReminderFromText handles POST /api/v1/reminders/from-text.
//
// 503 REMINDERS_DISABLED when either the reminder repo or the
// chat provider isn't wired (single-process / no-LLM
// deployments fall back to the existing manual surface).
// 400 BAD_REQUEST when required fields are missing.
// 422 PARSE_FAILED when the parser can't extract a reminder
// with sufficient confidence (covers garbage input, invalid
// cron expressions, recurrence bounds that precede the first
// fire).
// 200 + body on success.
func (s *Server) CreateReminderFromText(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "POST only")
		return
	}
	if s.reminderRepo == nil {
		respondError(w, http.StatusServiceUnavailable, "REMINDERS_DISABLED",
			"reminders repository not wired on this deployment")
		return
	}
	if s.chatProvider == nil {
		respondError(w, http.StatusServiceUnavailable, "PARSER_DISABLED",
			"natural-language parser requires a chat provider; configure one or use POST /api/v1/reminders for manual creation")
		return
	}

	var req FromTextRequest
	if err := decodeJSONBody(w, r, maxReminderFromTextBodyBytes, &req); err != nil {
		respondError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid JSON body: "+err.Error())
		return
	}
	req.Text = strings.TrimSpace(req.Text)
	req.OperatorID = strings.TrimSpace(req.OperatorID)
	req.Channel = strings.TrimSpace(req.Channel)
	req.ChannelRef = strings.TrimSpace(req.ChannelRef)
	req.ProjectID = strings.TrimSpace(req.ProjectID)
	req.OperatorTimezone = strings.TrimSpace(req.OperatorTimezone)
	if req.Text == "" || req.OperatorID == "" || req.Channel == "" || req.ChannelRef == "" {
		respondError(w, http.StatusBadRequest, "BAD_REQUEST",
			"text, operator_id, channel, and channel_ref are required")
		return
	}
	if req.ProjectID != "" && !requestAllowsProject(r, req.ProjectID) {
		respondError(w, http.StatusForbidden, "FORBIDDEN", "Access denied to project")
		return
	}

	parser := reminders.NewLLMParser(s.chatProvider)
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	intent, err := parser.Parse(ctx, reminders.ParserInput{
		Text:             req.Text,
		OperatorTimezone: req.OperatorTimezone,
	})
	switch {
	case errors.Is(err, reminders.ErrUnparseable):
		respondError(w, http.StatusUnprocessableEntity, "PARSE_FAILED",
			"could not parse a reminder from the input: "+err.Error())
		return
	case errors.Is(err, reminders.ErrFireAtInPast):
		respondError(w, http.StatusUnprocessableEntity, "FIRE_AT_IN_PAST",
			fmt.Sprintf("parsed fire_at is in the past (%s); rephrase or use a future time", intent.FireAt.Format(time.RFC3339)))
		return
	case errors.Is(err, reminders.ErrLLMUnavailable):
		respondError(w, http.StatusBadGateway, "PARSER_UPSTREAM_ERROR",
			"parser LLM upstream error: "+err.Error())
		return
	case err != nil:
		respondError(w, http.StatusInternalServerError, "INTERNAL", "parser failed: "+err.Error())
		return
	}

	intentJSON := FromTextIntentJSON{
		Kind:       string(intent.Kind),
		FireAt:     intent.FireAt.UTC().Format(time.RFC3339),
		CronExpr:   intent.CronExpr,
		Content:    intent.Content,
		Confidence: intent.Confidence,
		Reasoning:  intent.Reasoning,
	}
	if intent.RecurrenceUntil != nil {
		intentJSON.RecurrenceUntil = intent.RecurrenceUntil.UTC().Format(time.RFC3339)
	}
	resp := FromTextResponse{Intent: intentJSON}

	if req.DryRun {
		respondJSON(w, http.StatusOK, resp)
		return
	}

	rem := &persistence.Reminder{
		OperatorID:      req.OperatorID,
		Channel:         req.Channel,
		ChannelRef:      req.ChannelRef,
		ProjectID:       req.ProjectID,
		FireAt:          intent.FireAt,
		Content:         intent.Content,
		Status:          persistence.ReminderStatusPending,
		CreatedVia:      "api_nl",
		CronExpr:        intent.CronExpr,
		RecurrenceUntil: intent.RecurrenceUntil,
	}
	if err := s.reminderRepo.Insert(ctx, rem); err != nil {
		respondError(w, http.StatusInternalServerError, "INTERNAL", "insert failed: "+err.Error())
		return
	}
	entry := reminderToJSON(rem)
	resp.Reminder = &entry
	respondJSON(w, http.StatusCreated, resp)
}
