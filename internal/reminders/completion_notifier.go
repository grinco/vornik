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
	"sort"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/executor"
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
	childLister    ReminderChildTaskLister
}

// ReminderChildTaskLister walks one level of the task tree. Satisfied by
// persistence.TaskRepository, which already carries GetChildren.
type ReminderChildTaskLister interface {
	GetChildren(ctx context.Context, parentTaskID string) ([]*persistence.Task, error)
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

// WithArtifactDelivery makes successful task-kind reminders forward the
// deliverable OUTPUT artifacts to the reminder's original channel destination.
//
// The child lister is REQUIRED rather than optional, and that is the fix for
// the defect this option shipped with: the reminder's own task is frequently a
// router that DELEGATES, so the thing the operator asked for lives on a
// descendant. Passing the lister at the wiring point makes it impossible to
// enable artifact delivery and quietly keep looking at one task.
func WithArtifactDelivery(repo persistence.ArtifactRepository, reader ReminderArtifactReader, resolver ReminderFileSenderResolver, children ReminderChildTaskLister) CompletionOption {
	return func(n *CompletionNotifier) {
		n.artifactRepo = repo
		n.artifactReader = reader
		n.fileResolver = resolver
		n.childLister = children
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

// deliverOutputArtifacts forwards the operator-facing OUTPUT artifacts produced
// anywhere in the fired task's tree.
//
// ACROSS THE TREE, not just the reminder's own task. The design assumed "one
// fire spawns exactly one task", which is true of the FIRE and false of the
// work: the spawned task routinely runs a router workflow that delegates, and
// the deliverable is written by the child. Filtering artifacts to the
// reminder's task alone delivered the router's own scaffolding and never the
// briefing the operator asked for — measured 2026-08-22, where the parent held
// two route-response transcripts (886 B) and the child held the 3.9 KB morning
// briefing.
//
// TRANSCRIPTS ARE EXCLUDED via executor.IsTranscriptArtifact, the same
// classifier memory-ingest and the companion result() inliner use. Its
// docstring already says presentation and ingest must not drift on what counts
// as a transcript, and this is a presentation surface. Without it, widening to
// the tree would have made the problem worse — today's digest would have
// shipped nine files instead of two.
//
// Input and intermediate artifacts stay private to the task; only OUTPUT is
// ever forwarded.
func (n *CompletionNotifier) deliverOutputArtifacts(ctx context.Context, rem *persistence.Reminder, task *persistence.Task) error {
	if n == nil || n.artifactRepo == nil || n.artifactReader == nil || n.fileResolver == nil || rem == nil || task == nil {
		return nil
	}
	sender := n.fileResolver.ResolveReminderFileSender(rem.Channel, rem.ChannelRef)
	if sender == nil {
		// Not an error — a channel may have no attachment surface — but it is
		// not nothing either. The operator gets the text body and never learns
		// the report existed, so say so where an operator can find it.
		n.logger.Info().Str("reminder_id", rem.ID).Str("channel", rem.Channel).
			Msg("reminders: channel has no file sender; scheduled report not attached")
		return nil
	}

	for _, artifact := range n.collectTreeOutputs(ctx, task) {
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

// maxReminderTreeTasks bounds the descendant walk. A reminder that fans out
// past this has a delivery problem no attachment list will fix, and the cap
// keeps one bad schedule from walking an unbounded tree on every fire.
const maxReminderTreeTasks = 64

// collectTreeOutputs gathers deliverable OUTPUT artifacts from the task and its
// descendants, oldest first, de-duplicated.
//
// Best-effort by design: a failure to walk the tree or to list one task's
// artifacts must not strand the completion notice, which is the useful part.
// Anything skipped is logged rather than silently dropped.
func (n *CompletionNotifier) collectTreeOutputs(ctx context.Context, root *persistence.Task) []*persistence.Artifact {
	var out []*persistence.Artifact
	seenArtifact := map[string]bool{}
	seenTask := map[string]bool{}

	queue := []*persistence.Task{root}
	for len(queue) > 0 && len(seenTask) < maxReminderTreeTasks {
		cur := queue[0]
		queue = queue[1:]
		if cur == nil || seenTask[cur.ID] {
			continue // cycle guard: parentage should be acyclic, data heals imperfectly
		}
		seenTask[cur.ID] = true

		taskID := cur.ID
		artifacts, err := n.artifactRepo.List(ctx, persistence.ArtifactFilter{TaskID: &taskID, PageSize: 100})
		if err != nil {
			n.logger.Warn().Err(err).Str("task_id", taskID).
				Msg("reminders: listing output artifacts failed; continuing with the rest of the tree")
		}
		for _, a := range artifacts {
			if a == nil || a.ArtifactClass != persistence.ArtifactClassOutput {
				continue
			}
			if executor.IsTranscriptArtifact(a.Name) {
				continue // per-step agent transcript, not a deliverable
			}
			if seenArtifact[a.ID] {
				continue
			}
			seenArtifact[a.ID] = true
			out = append(out, a)
		}

		if n.childLister == nil {
			continue
		}
		children, err := n.childLister.GetChildren(ctx, taskID)
		if err != nil {
			n.logger.Warn().Err(err).Str("task_id", taskID).
				Msg("reminders: listing child tasks failed; deliverables below this task are not attached")
			continue
		}
		queue = append(queue, children...)
	}

	// Oldest first, then by name: delivery order should reflect how the work
	// was produced and must not vary between two identical runs.
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].Name < out[j].Name
	})
	return out
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
