package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/slack"
	"vornik.io/vornik/internal/steering"
)

// SlackInteractionParser is the transport half of the Slack button path: it
// verifies the request signature and returns the typed, trusted payload.
// Satisfied by *slack.Channel.
//
// The split is deliberate (steering-notifications-design §v1.2/2).
// internal/slack is a pure transport package with no persistence; this package
// already holds the repositories. So verification lives there and authorization
// plus the answer live here, rather than dragging repos into the transport.
type SlackInteractionParser interface {
	ParseInteraction(r *http.Request, now time.Time) (slack.Interaction, error)
}

// steerActionPrefix is the namespace the steering notifier encodes into a
// button's value: `steer:c:<taskID>:<optionIndex>`. Shared with the Telegram
// callback format, which is why the index is positional.
const steerActionPrefix = "steer:c:"

// refusalText is returned BOTH for "no such waiting task" and for "you are not
// the operator this was addressed to".
//
// Identical wording is the point: a distinct "not authorized" would confirm the
// task exists and that somebody else owns it, turning a button anyone in the
// channel can see into an existence oracle. A bystander clicking is an expected
// event here, not an attack — the button is visible to everyone who can read
// the message.
const refusalText = "No waiting task matches that reference."

// HandleSlackInteraction serves POST /api/v1/slack/interactions — an operator
// tapping a steering button.
//
// Ack discipline: the empty 200 goes out immediately after verification and
// parsing, BEFORE authorization and before the answer is recorded. Slack allows
// three seconds and everything downstream can exceed it; the operator's
// confirmation rides response_url instead. This is the rule the event paths
// learned on 2026-07-30, when synchronous dispatch let Slack's timeout cancel
// the request context and kill an in-flight turn.
func (s *Server) HandleSlackInteraction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "POST required")
		return
	}
	if s.slackInteractions == nil {
		respondError(w, http.StatusNotFound, "NOT_FOUND", "slack interactions not configured")
		return
	}
	action, err := s.slackInteractions.ParseInteraction(r, time.Now())
	if err != nil {
		// Signature failure, replay, unknown workspace, or an interaction type
		// we do not serve. Never leak which.
		s.logger.Warn().Err(err).Msg("slack interaction: refused")
		respondError(w, http.StatusUnauthorized, "UNAUTHORIZED", "invalid interaction")
		return
	}

	slack.WriteInteractionAck(w)

	// Detached: the ack is already written, and the work must not be bound to a
	// request context Slack is about to close.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Minute)
	go func() {
		defer cancel()
		s.resolveSlackInteraction(ctx, action)
	}()
}

// resolveSlackInteraction authorizes the clicker and records the answer. Every
// outcome reaches the operator through response_url; nothing is written to the
// already-closed HTTP response.
func (s *Server) resolveSlackInteraction(ctx context.Context, action slack.Interaction) {
	taskID, optionIdx, ok := parseSteerAction(action.ActionValue)
	if !ok {
		s.postInteractionReply(ctx, action.ResponseURL, refusalText, false)
		return
	}
	if s.taskRepo == nil || s.taskMessageRepo == nil {
		s.postInteractionReply(ctx, action.ResponseURL, "Task control isn't available.", false)
		return
	}
	task, err := s.taskRepo.Get(ctx, taskID)
	if err != nil || task == nil {
		s.postInteractionReply(ctx, action.ResponseURL, refusalText, false)
		return
	}
	if !s.slackClickerMayAnswer(ctx, task, action.UserID) {
		// Same text as an unknown task — see refusalText.
		s.postInteractionReply(ctx, action.ResponseURL, refusalText, false)
		return
	}

	// Resolve the button's positional index to an option ID here, on the
	// adapter. steering.Answerer takes an ID precisely so no numbering
	// convention reaches shared code.
	optionID, ok := s.slackOptionIDAt(ctx, taskID, optionIdx)
	if !ok {
		s.postInteractionReply(ctx, action.ResponseURL, "This button is from an older prompt.", false)
		return
	}

	checkpointID := ""
	if task.OpenCheckpointID != nil {
		checkpointID = *task.OpenCheckpointID
	}
	res, err := steering.NewAnswerer(s.taskMessageRepo, s.taskRepo, s.rescheduler).
		Answer(ctx, steering.AnswerRequest{
			TaskID:       taskID,
			CheckpointID: checkpointID,
			OptionID:     optionID,
			AuthorID:     "slack:" + action.UserID,
			Source:       "slack_button",
		})
	switch {
	case errors.Is(err, steering.ErrCheckpointNotChatAnswerable):
		// Budget / taint-review checkpoints carry their own authorization,
		// enforced inside the primitive so every channel inherits it.
		s.postInteractionReply(ctx, action.ResponseURL,
			"This decision has to be made in the web UI.", false)
		return
	case errors.Is(err, steering.ErrNoOpenCheckpoint), errors.Is(err, steering.ErrUnknownOption):
		s.postInteractionReply(ctx, action.ResponseURL, "This decision was already handled.", false)
		return
	case err != nil:
		s.logger.Error().Err(err).Str("taskId", taskID).Msg("slack interaction: answer failed")
		s.postInteractionReply(ctx, action.ResponseURL, "Could not record your choice.", false)
		return
	}
	if res.AlreadyHandled {
		s.postInteractionReply(ctx, action.ResponseURL, "This decision was already handled.", false)
		return
	}
	// Replace the prompt in place: buttons gone, choice shown. Visible to the
	// channel, mirroring Telegram's markSteerRecorded, so the question visibly
	// stops looking unanswered.
	s.postInteractionReply(ctx, action.ResponseURL, "✓ Recorded: "+res.RecordedLabel, true)
}

// slackClickerMayAnswer applies §3a: the clicker must be the operator this
// checkpoint was addressed to.
//
// The Slack sender allowlist is NOT sufficient — it authorizes use of the bot,
// not steering somebody else's task. A button is visible to everyone who can
// read the channel, so without this any allow-listed member could resolve
// another operator's decision and the lead would proceed on it silently.
func (s *Server) slackClickerMayAnswer(ctx context.Context, task *persistence.Task, clickerID string) bool {
	if clickerID == "" {
		return false
	}
	if task.ChatTurnID == nil || *task.ChatTurnID == "" || s.chatAuditRepo == nil {
		// Ownerless (autonomy) task: no originating operator to match. Refuse
		// here rather than guess — the operator-alert recipient path is a
		// follow-up (design §3a rule 2) and admitting anyone would be worse.
		return false
	}
	row, err := s.chatAuditRepo.GetByID(ctx, *task.ChatTurnID)
	if err != nil || row == nil {
		return false
	}
	// chat_audit_log.UserID is channel-prefixed ("slack:U123").
	return strings.EqualFold(strings.TrimSpace(row.UserID), "slack:"+clickerID)
}

// slackOptionIDAt maps the button's positional index to the option's stable id.
func (s *Server) slackOptionIDAt(ctx context.Context, taskID string, idx int) (string, bool) {
	cp, err := s.taskMessageRepo.GetOpenCheckpoint(ctx, taskID)
	if err != nil || cp == nil || len(cp.Metadata) == 0 {
		return "", false
	}
	var meta struct {
		Options []struct {
			ID string `json:"id"`
		} `json:"options"`
	}
	if err := json.Unmarshal(cp.Metadata, &meta); err != nil {
		return "", false
	}
	if idx < 0 || idx >= len(meta.Options) {
		return "", false
	}
	return meta.Options[idx].ID, true
}

// parseSteerAction splits `steer:c:<taskID>:<index>`. Task ids contain no
// colon, so the index is after the LAST one — same rule the Telegram callback
// decoder applies.
func parseSteerAction(value string) (taskID string, idx int, ok bool) {
	rest, found := strings.CutPrefix(value, steerActionPrefix)
	if !found {
		return "", 0, false
	}
	i := strings.LastIndex(rest, ":")
	if i <= 0 || i+1 >= len(rest) {
		return "", 0, false
	}
	n := 0
	for _, r := range rest[i+1:] {
		if r < '0' || r > '9' {
			return "", 0, false
		}
		n = n*10 + int(r-'0')
	}
	return rest[:i], n, true
}

// postInteractionReply sends the outcome to Slack's response_url. replace=true
// swaps the original message (dropping the buttons); false posts an ephemeral
// note visible only to the clicker, which is what refusals use so a bystander's
// tap never disturbs a prompt others still need.
//
// response_url expires after ~30 minutes; a failure here is logged and dropped,
// leaving the prompt looking unanswered — the pre-existing staleness, not a new
// failure mode.
func (s *Server) postInteractionReply(ctx context.Context, responseURL, text string, replace bool) {
	if strings.TrimSpace(responseURL) == "" {
		return
	}
	payload := map[string]any{"text": text}
	if replace {
		payload["replace_original"] = true
	} else {
		payload["response_type"] = "ephemeral"
		payload["replace_original"] = false
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, responseURL, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.logger.Warn().Err(err).Msg("slack interaction: response_url post failed")
		return
	}
	_ = resp.Body.Close()
}
