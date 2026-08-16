// Package reminders runs the scheduled-reminders heartbeat. This file
// (completion_notifier.go) implements Task 6 of
// https://docs.vornik.io
// §4.2/§4.4 — delivering a task-kind reminder's outcome to the
// operator's channel once its spawned task completes.
package reminders

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/persistence"
)

// CompletionNotifier delivers a task-kind reminder's outcome to the
// operator's channel when the spawned task completes. It is registered
// into the executor's CompletionNotifier fan-out. Routing is resolved
// from the durable reminder row (ClaimDelivery), so delivery survives a
// daemon restart between spawn and completion. See design §4.2.
type CompletionNotifier struct {
	repo      persistence.ReminderRepository
	resolver  ChannelResolver
	auditRepo persistence.AdminAuditRepository
	logger    zerolog.Logger
	clock     func() time.Time

	artifactRepo   persistence.ArtifactRepository
	artifactReader ReminderArtifactReader
	fileResolver   ReminderFileSenderResolver
}

// ReminderArtifactReader is the backend-neutral read half of artifacts.Store.
type ReminderArtifactReader interface {
	Retrieve(ctx context.Context, artifactID string) ([]byte, error)
}

// ReminderFileSender is the same narrow stream contract used by dispatcher
// file tools, repeated here to avoid coupling the reminders package back to the
// dispatcher.
type ReminderFileSender interface {
	SendArtifactFile(ctx context.Context, fileName string, content io.Reader, caption string) error
}

// ReminderFileSenderResolver binds a durable reminder destination to its
// channel-specific file sender.
type ReminderFileSenderResolver interface {
	ResolveReminderFileSender(channel, channelRef string) ReminderFileSender
}

// CompletionOption adds optional scheduled-output delivery without changing
// the existing text-only constructor call sites.
type CompletionOption func(*CompletionNotifier)

// WithArtifactDelivery makes successful task-kind reminders forward every
// OUTPUT artifact to the reminder's original channel destination.
func WithArtifactDelivery(repo persistence.ArtifactRepository, reader ReminderArtifactReader, resolver ReminderFileSenderResolver) CompletionOption {
	return func(n *CompletionNotifier) {
		n.artifactRepo = repo
		n.artifactReader = reader
		n.fileResolver = resolver
	}
}

// NewCompletionNotifier constructs the notifier. auditRepo may be nil.
func NewCompletionNotifier(repo persistence.ReminderRepository, resolver ChannelResolver, auditRepo persistence.AdminAuditRepository, logger zerolog.Logger, clock func() time.Time, opts ...CompletionOption) *CompletionNotifier {
	if clock == nil {
		clock = time.Now
	}
	n := &CompletionNotifier{repo: repo, resolver: resolver, auditRepo: auditRepo, logger: logger, clock: clock}
	for _, opt := range opts {
		if opt != nil {
			opt(n)
		}
	}
	return n
}

// NotifyTaskCompleted implements executor.CompletionNotifier structurally.
// It claims delivery atomically (at-most-once), sends the outcome, then
// finalizes.
func (n *CompletionNotifier) NotifyTaskCompleted(ctx context.Context, task *persistence.Task, success bool, message string) {
	if n == nil || n.repo == nil || task == nil {
		return
	}
	rem, ok, err := n.repo.ClaimDelivery(ctx, task.ID)
	if err != nil {
		n.logger.Warn().Err(err).Str("task_id", task.ID).Msg("reminders: claim_delivery failed")
		return
	}
	if !ok {
		return // not ours, already delivered, or a duplicate/HA callback
	}
	if n.resolver == nil {
		_ = n.repo.MarkErrored(ctx, rem.ID, "no channel resolver wired")
		return
	}
	ch := n.resolver.ResolveChannel(rem.Channel)
	if ch == nil {
		_ = n.repo.MarkErrored(ctx, rem.ID, "channel "+rem.Channel+" not configured")
		return
	}
	body := scheduledUpdateBody(rem, task, success, message)
	if success {
		if err := n.deliverOutputArtifacts(ctx, rem, task); err != nil {
			// The completion itself is still useful and must not get stranded in
			// `firing` merely because a file upload failed. Tell the operator
			// plainly so they can ask send_artifact to retry.
			body += "\n\n⚠️ The report file could not be attached: " + firstLine(err.Error())
			n.logger.Warn().Err(err).Str("reminder_id", rem.ID).Str("task_id", task.ID).
				Msg("reminders: output artifact delivery failed")
		}
	}
	if _, err := ch.Send(ctx, conversation.ChannelMessage{
		SessionID: rem.ChannelRef, Text: body, Timestamp: n.clock(),
	}); err != nil {
		metricTaskDeliverErrors.Inc()
		_ = n.repo.MarkErrored(ctx, rem.ID, err.Error())
		n.logger.Warn().Err(err).Str("reminder_id", rem.ID).Msg("reminders: task-kind send failed")
		return
	}
	terminal := !rem.IsRecurring()
	if rem.IsRecurring() && rem.RecurrenceUntil != nil {
		// Bounded recurring reminder: mirror runner.finalize's bound check
		// (§2.3/§4.2) so the last-in-bound fire terminalizes here too — a
		// past-fire_at row that stayed non-terminal would otherwise re-fire
		// forever (LeaseDue has no upper-bound awareness of its own).
		next, nerr := NextFireAt(rem.CronExpr, n.clock())
		if nerr != nil || next.After(*rem.RecurrenceUntil) {
			terminal = true
		}
	}
	if ferr := n.repo.FinalizeDelivery(ctx, rem.ID, task.ID, terminal); ferr != nil {
		n.logger.Warn().Err(ferr).Str("reminder_id", rem.ID).Msg("reminders: finalize_delivery failed after send")
	}
	metricTaskDelivered.WithLabelValues(boolLabel(success)).Inc()
	// Skip metric: a recurring row whose armed fire_at is already in the
	// past means the task outran a slot.
	if !terminal && rem.FireAt.Before(n.clock()) {
		metricTaskSkipped.Inc()
	}
	if n.auditRepo != nil {
		_ = n.auditRepo.Insert(ctx, &persistence.AdminAuditEntry{
			Principal: rem.OperatorID, Source: "reminder-heartbeat",
			Action: "reminder.task_delivered", Target: rem.ID,
			After: `{"task_id":"` + task.ID + `","success":` + boolLabel(success) + `}`,
		})
	}
}

// deliverOutputArtifacts forwards only operator-facing OUTPUT artifacts. Input
// and intermediate files can contain raw uploads or scratch data and must never
// leak merely because they share the completed task ID.
func (n *CompletionNotifier) deliverOutputArtifacts(ctx context.Context, rem *persistence.Reminder, task *persistence.Task) error {
	if n == nil || n.artifactRepo == nil || n.artifactReader == nil || n.fileResolver == nil || rem == nil || task == nil {
		return nil
	}
	sender := n.fileResolver.ResolveReminderFileSender(rem.Channel, rem.ChannelRef)
	if sender == nil {
		return nil // channel has no attachment surface; preserve text-only behavior
	}
	taskID := task.ID
	artifacts, err := n.artifactRepo.List(ctx, persistence.ArtifactFilter{TaskID: &taskID, PageSize: 100})
	if err != nil {
		return fmt.Errorf("list output artifacts: %w", err)
	}
	for _, artifact := range artifacts {
		if artifact == nil || artifact.ArtifactClass != persistence.ArtifactClassOutput {
			continue
		}
		data, err := n.artifactReader.Retrieve(ctx, artifact.ID)
		if err != nil {
			return fmt.Errorf("read %s: %w", artifact.Name, err)
		}
		if err := sender.SendArtifactFile(ctx, artifact.Name, bytes.NewReader(data), "Scheduled report: "+artifact.Name); err != nil {
			return fmt.Errorf("send %s: %w", artifact.Name, err)
		}
	}
	return nil
}

// scheduledUpdateBody renders the delivery message. See design §4.4.
func scheduledUpdateBody(rem *persistence.Reminder, task *persistence.Task, success bool, message string) string {
	if !success {
		return "⚠️ Your scheduled update \"" + label(rem) + "\" couldn't complete: " + firstLine(message)
	}
	return "📊 Scheduled update — " + label(rem) + "\n\n" + deliverable(task, message)
}

func deliverable(task *persistence.Task, message string) string {
	if task != nil && len(task.ResultEnvelope) > 0 {
		return string(task.ResultEnvelope)
	}
	return message
}

// label is the first line of content, capped at 60 runes. Rune-safe —
// operator content includes non-ASCII (Czech, emoji); a byte-index slice
// (s[:57]) can split a multi-byte rune mid-sequence and emit invalid UTF-8.
func label(rem *persistence.Reminder) string {
	s := firstLine(rem.Content)
	r := []rune(s)
	if len(r) > 60 {
		return string(r[:57]) + "..."
	}
	return s
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

func boolLabel(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
