// Package reminders runs the scheduled-reminders heartbeat. See
// https://docs.vornik.io
//
// The Runner polls dispatcher_reminders every TickInterval,
// leases rows whose fire_at <= now via FOR UPDATE SKIP LOCKED,
// and delivers each via the ConversationChannel registry that
// owns the operator's last-active channel (Telegram, email,
// webchat, etc.). Idempotent + HA-safe — multiple Runner
// instances in 2026.8.0 will share work via the SKIP LOCKED
// claim semantic.
package reminders

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/persistence"
)

// DefaultTickInterval is the cadence between automatic sweeps.
// 30s lines up with the LLD §7 table — short enough that a
// reminder set for "in 1 minute" fires within ~30s of its
// target, generous enough that the heartbeat query (one CTE
// touching a partial index) costs ~nothing on the DB.
const DefaultTickInterval = 30 * time.Second

// DefaultBatchSize caps the per-tick claim batch. Prevents one
// tick from locking the table on a backlog (e.g. after the
// daemon was down for an hour and 50,000 reminders are due).
const DefaultBatchSize = 100

// DefaultFiringGrace is how long a row may sit in 'firing' before
// sweepStuckFiring reclaims it (crash recovery, design §9).
// Overridable via VORNIK_REMINDERS_FIRING_GRACE (a Go duration
// string, e.g. "5m"); an unparseable or non-positive value falls
// back to this default rather than disabling the sweep.
const DefaultFiringGrace = 15 * time.Minute

// firingGraceEnvVar is the operator override for DefaultFiringGrace.
const firingGraceEnvVar = "VORNIK_REMINDERS_FIRING_GRACE"

// legacyFiringGraceEnvVar is the pre-rename name. It shipped to main +
// prod under the SWARMD_ prefix before the reminder knobs were migrated
// to the codebase-standard VORNIK_ scheme; read as a fallback so a
// deployment that set the old name keeps working. Remove once no
// deployment references it.
const legacyFiringGraceEnvVar = "SWARMD_REMINDERS_FIRING_GRACE"

// taskTypeEnvVar overrides the default task type a task-kind reminder
// spawns (Config.DefaultTaskType). Falls back to "research" when neither
// the Config field nor this env var is set.
const taskTypeEnvVar = "VORNIK_REMINDERS_TASK_TYPE"

// defaultReminderTaskType is the task type handed to the creator when
// neither Config.DefaultTaskType nor taskTypeEnvVar is set.
const defaultReminderTaskType = "research"

// sweepEveryNTicks gates sweepStuckFiring to a slower cadence than the
// main due-lease tick. Stuck-firing rows are a rare crash-recovery
// case, not something that needs sub-minute polling. At
// DefaultTickInterval (30s), 10 ticks ≈ 5 minutes — comfortably under
// DefaultFiringGrace (15m) so a row crossing the grace threshold gets
// swept within one cycle rather than waiting a full grace window.
const sweepEveryNTicks = 10

// ChannelResolver returns the conversation.Channel registered
// for a given channel name (e.g. "telegram"). Returns nil when
// the channel isn't wired on this deployment — Runner records
// the row as errored rather than crashing.
type ChannelResolver interface {
	ResolveChannel(name string) conversation.Channel
}

// ScheduledTaskParams is the narrow shape the runner hands the task
// creator at fire time. Keeps the reminders package decoupled from
// taskcreate.Params (the container adapter maps between them).
type ScheduledTaskParams struct {
	ProjectID      string
	Prompt         string
	TaskType       string
	IdempotencyKey string
	ReminderID     string
}

// TaskCreator creates the task a task-kind reminder fires. The
// container adapts *taskcreate.Creator to this (see
// container_reminders.go). Returns the new task id.
type TaskCreator interface {
	CreateScheduledTask(ctx context.Context, p ScheduledTaskParams) (string, error)
}

// Config wires the Runner. Repo + Resolver are required;
// everything else has sane defaults.
type Config struct {
	Repo         persistence.ReminderRepository
	Resolver     ChannelResolver
	AuditRepo    persistence.AdminAuditRepository // optional
	TickInterval time.Duration
	BatchSize    int
	Logger       zerolog.Logger
	Clock        func() time.Time // injectable for tests
	// LeaderGate gates the heartbeat in multi-instance
	// deployments so only one replica claims due rows. nil
	// (single-process default) runs every tick. See
	// https://docs.vornik.io §3.
	LeaderGate LeaderGate
	// Creator spawns the task for task-kind reminders. nil disables
	// the task-kind path (text-kind still works).
	Creator TaskCreator
	// DefaultTaskType is the TaskType handed to the creator (project's
	// default workflow handles it). Empty defaults to "research", or the
	// VORNIK_REMINDERS_TASK_TYPE override, resolved at New time.
	DefaultTaskType string
	// FiringGrace is how long a row may sit in 'firing' before
	// sweepStuckFiring reclaims it. Zero defaults to
	// DefaultFiringGrace (or the VORNIK_REMINDERS_FIRING_GRACE
	// override) at New time.
	FiringGrace time.Duration
}

// LeaderGate is the narrow contract the heartbeat consults
// before each sweep. Defined locally so the reminders package
// doesn't pull internal/leaderelection;
// *leaderelection.Elector satisfies structurally.
type LeaderGate interface {
	IsLeader() bool
}

// Runner is the heartbeat goroutine. Construct via New; drive
// via Run (long-lived) or Kick (out-of-band tick).
type Runner struct {
	cfg Config

	// kickCh forces a sweep mid-interval. A buffered channel of
	// size 1 collapses bursts into one upcoming sweep.
	kickCh chan struct{}

	// inflight serialises sweeps against each other — overlapping
	// runs against the same row would race on the firing→fired
	// transition.
	mu       sync.Mutex
	inflight bool

	// tickCount drives sweepEveryNTicks — the stuck-firing sweep's
	// slower cadence. Only touched from within tickOnce, which the
	// inflight guard already serialises, so no separate lock needed.
	tickCount uint64
}

// New constructs a Runner with defaults applied. Nil Repo /
// Resolver still produce a Runner; Run logs and skips ticks so
// callers can construct unconditionally.
func New(cfg Config) *Runner {
	if cfg.TickInterval <= 0 {
		cfg.TickInterval = DefaultTickInterval
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = DefaultBatchSize
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	if cfg.DefaultTaskType == "" {
		cfg.DefaultTaskType = defaultReminderTaskType
		if v := strings.TrimSpace(os.Getenv(taskTypeEnvVar)); v != "" {
			cfg.DefaultTaskType = v
			// The value is taken verbatim (there's no task-type registry
			// to validate against here) — a typo makes every task-kind
			// fire fail at task creation, so surface the override loudly.
			cfg.Logger.Info().Str("task_type", v).Str("env", taskTypeEnvVar).
				Msg("reminders: task-kind task type overridden via env; ensure it is a valid task type")
		}
	}
	if cfg.FiringGrace <= 0 {
		cfg.FiringGrace = DefaultFiringGrace
		if v := firingGraceOverride(); v != "" {
			if d, err := time.ParseDuration(v); err == nil && d > 0 {
				cfg.FiringGrace = d
			}
		}
	}
	return &Runner{cfg: cfg, kickCh: make(chan struct{}, 1)}
}

// firingGraceOverride reads the firing-grace override, preferring the
// VORNIK_ name and falling back to the legacy SWARMD_ name (which shipped
// to prod before the reminder knobs were migrated to the VORNIK_ scheme).
func firingGraceOverride() string {
	if v := strings.TrimSpace(os.Getenv(firingGraceEnvVar)); v != "" {
		return v
	}
	return strings.TrimSpace(os.Getenv(legacyFiringGraceEnvVar))
}

// Run blocks on a ticker until ctx is cancelled. Each tick (and
// each Kick) calls tickOnce.
func (r *Runner) Run(ctx context.Context) {
	r.cfg.Logger.Info().Dur("interval", r.cfg.TickInterval).Msg("reminders: heartbeat started")
	ticker := time.NewTicker(r.cfg.TickInterval)
	defer ticker.Stop()
	r.tickOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			r.cfg.Logger.Info().Msg("reminders: heartbeat stopped")
			return
		case <-ticker.C:
			r.tickOnce(ctx)
		case <-r.kickCh:
			r.tickOnce(ctx)
		}
	}
}

// SetLeaderGate attaches the leader gate after construction.
// Used by the service container so initReminders can stay
// dependency-light; the elector is wired alongside the other
// per-worker electors at Start time. Safe to call before Run.
func (r *Runner) SetLeaderGate(g LeaderGate) {
	if r == nil {
		return
	}
	r.cfg.LeaderGate = g
}

// SetCreator attaches the task creator after construction, enabling the
// task-kind reminder path. Mirrors SetLeaderGate: the service container
// builds the Runner (initReminders) before the shared task-creation core
// exists (initHTTPServer, later in the boot sequence), so the container
// wires this in once the creator is available — see
// internal/service/subsystem_reminders.go. Safe to call before Run; nil
// creator leaves the task-kind path disabled (text-kind reminders are
// unaffected).
func (r *Runner) SetCreator(creator TaskCreator) {
	if r == nil {
		return
	}
	r.cfg.Creator = creator
}

// Kick forces an out-of-band sweep. Used after a fresh insert
// when the reminder is due immediately ("remind me in 30
// seconds"). Idempotent — overlapping calls collapse.
func (r *Runner) Kick() {
	select {
	case r.kickCh <- struct{}{}:
	default:
	}
}

func (r *Runner) tickOnce(ctx context.Context) {
	if r.cfg.Repo == nil {
		return
	}
	if r.cfg.LeaderGate != nil && !r.cfg.LeaderGate.IsLeader() {
		return
	}
	r.mu.Lock()
	if r.inflight {
		r.mu.Unlock()
		return
	}
	r.inflight = true
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.inflight = false
		r.mu.Unlock()
	}()

	// Stuck-firing reclaim sweep runs on a slower cadence than the
	// main due-lease pass below — gated here, ahead of LeaseDue, so it
	// still fires on ticks with no due work (an idle heartbeat is
	// exactly when a crash-orphaned row would otherwise sit unnoticed).
	r.tickCount++
	if r.tickCount%sweepEveryNTicks == 0 {
		r.sweepStuckFiring(ctx)
	}

	due, err := r.cfg.Repo.LeaseDue(ctx, r.cfg.Clock(), r.cfg.BatchSize)
	if err != nil {
		r.cfg.Logger.Error().Err(err).Msg("reminders: lease_due failed")
		return
	}
	if len(due) == 0 {
		return
	}
	r.cfg.Logger.Info().Int("count", len(due)).Msg("reminders: delivering due batch")
	for _, rem := range due {
		if rem == nil {
			continue
		}
		if rem.IsTaskKind() {
			r.deliverTask(ctx, rem)
			continue
		}
		r.deliver(ctx, rem)
	}
}

// sweepStuckFiring reclaims rows stuck in 'firing' past cfg.FiringGrace
// — the crash-recovery pass design §9 names: (a) a crash between
// LeaseDue and MarkTaskSpawned (task-kind spawn interrupted), (b) a
// crash between Send/ClaimDelivery and FinalizeDelivery (delivery
// interrupted), and (c) the pre-existing text-kind case where a failed
// Send leaves the row firing forever. Recurring rows re-arm via the
// same cron/bound logic as finalize (§4.1/§4.2's re-arm path);
// one-shot rows get MarkErrored — which, by design, does not itself
// change status off 'firing' (see MarkErrored's doc comment), so a
// permanently-broken one-shot row is re-flagged (error_count bumped,
// metric incremented) on every subsequent sweep cycle until an
// operator cancels it. That matches the existing accepted behavior for
// a failed text-kind send; this sweep just makes it periodic /
// observable instead of silent. ReclaimStuckFiring never returns
// 'awaiting_task' rows, so a genuinely long-running task is never
// touched here.
func (r *Runner) sweepStuckFiring(ctx context.Context) {
	cutoff := r.cfg.Clock().Add(-r.cfg.FiringGrace)
	stuck, err := r.cfg.Repo.ReclaimStuckFiring(ctx, cutoff, r.cfg.BatchSize)
	if err != nil {
		r.cfg.Logger.Error().Err(err).Msg("reminders: reclaim_stuck_firing failed")
		return
	}
	if len(stuck) == 0 {
		return
	}
	r.cfg.Logger.Warn().Int("count", len(stuck)).Msg("reminders: reclaiming stuck-firing rows")
	for _, rem := range stuck {
		if rem == nil {
			continue
		}
		metricFiringReclaimed.Inc()
		if !rem.IsRecurring() {
			if err := r.cfg.Repo.MarkErrored(ctx, rem.ID, "reclaimed: stuck in firing after crash"); err != nil {
				r.cfg.Logger.Warn().Err(err).Str("reminder_id", rem.ID).
					Msg("reminders: stuck-firing reclaim mark_errored failed")
			}
			continue
		}
		next, nerr := NextFireAt(rem.CronExpr, r.cfg.Clock())
		if nerr != nil {
			_ = r.cfg.Repo.MarkErrored(ctx, rem.ID, "reclaimed: re-arm cron invalid: "+nerr.Error())
			r.cfg.Logger.Warn().Err(nerr).Str("reminder_id", rem.ID).Str("cron_expr", rem.CronExpr).
				Msg("reminders: stuck-firing reclaim cron parse failed; marked errored")
			continue
		}
		if rem.RecurrenceUntil != nil && next.After(*rem.RecurrenceUntil) {
			// Past the operator-named bound — terminate cleanly,
			// same as finalize's terminal-when-bound-hit branch.
			if err := r.cfg.Repo.MarkFired(ctx, rem.ID); err != nil {
				r.cfg.Logger.Warn().Err(err).Str("reminder_id", rem.ID).
					Msg("reminders: stuck-firing reclaim mark_fired (bound past) failed")
			}
			continue
		}
		if err := r.cfg.Repo.Reschedule(ctx, rem.ID, next); err != nil {
			r.cfg.Logger.Warn().Err(err).Str("reminder_id", rem.ID).
				Msg("reminders: stuck-firing reclaim reschedule failed")
		}
	}
}

// deliver sends one reminder via its channel. On success the
// repo flips status=fired + writes an audit row; on failure the
// row stays in 'firing' with last_error stamped (v1 has no
// retry policy — operator can re-create if needed).
func (r *Runner) deliver(ctx context.Context, rem *persistence.Reminder) {
	if r.cfg.Resolver == nil {
		_ = r.cfg.Repo.MarkErrored(ctx, rem.ID, "no channel resolver wired")
		r.cfg.Logger.Warn().Str("reminder_id", rem.ID).Msg("reminders: no resolver — row marked errored")
		return
	}
	ch := r.cfg.Resolver.ResolveChannel(rem.Channel)
	if ch == nil {
		_ = r.cfg.Repo.MarkErrored(ctx, rem.ID, "channel "+rem.Channel+" not configured")
		r.cfg.Logger.Warn().
			Str("reminder_id", rem.ID).
			Str("channel", rem.Channel).
			Msg("reminders: channel not configured")
		return
	}
	msg := conversation.ChannelMessage{
		SessionID: rem.ChannelRef,
		Text:      reminderBodyWithMarker(rem),
		Timestamp: r.cfg.Clock(),
	}
	if _, err := ch.Send(ctx, msg); err != nil {
		_ = r.cfg.Repo.MarkErrored(ctx, rem.ID, err.Error())
		r.cfg.Logger.Warn().
			Err(err).
			Str("reminder_id", rem.ID).
			Str("channel", rem.Channel).
			Msg("reminders: send failed")
		return
	}
	// Recurring rows re-arm; one-shot rows go terminal. The
	// 'terminal-when-bound-hit' branch lets a bounded recurring
	// reminder ("every Monday until June 1") collapse cleanly
	// once the bound is past.
	finalizeErr := r.finalize(ctx, rem)
	if finalizeErr != nil {
		// Either MarkFired or Reschedule returning ErrNotFound
		// means someone cancelled between lease and finalize —
		// log loud, the operator at least got the message.
		r.cfg.Logger.Warn().
			Err(finalizeErr).
			Str("reminder_id", rem.ID).
			Bool("recurring", rem.IsRecurring()).
			Msg("reminders: finalize failed after successful send")
	}
	r.audit(ctx, rem)
}

// deliverTask handles a task-kind fire: create the task, then atomically
// re-arm + record it. The row is already 'firing' (leased), so no other
// tick can touch it. A crash before MarkTaskSpawned leaves it 'firing'
// (recoverable by the Phase-C sweep) — the fire is never silently lost.
// See design §4.1.
func (r *Runner) deliverTask(ctx context.Context, rem *persistence.Reminder) {
	if r.cfg.Creator == nil {
		_ = r.cfg.Repo.MarkErrored(ctx, rem.ID, "task creator not wired")
		r.cfg.Logger.Warn().Str("reminder_id", rem.ID).Msg("reminders: no task creator wired — row marked errored")
		return
	}
	// Idempotency: rem.ID + fire slot. A re-leased slot returns the same task.
	idem := rem.ID + ":" + strconv.FormatInt(rem.FireAt.Unix(), 10)
	taskID, err := r.cfg.Creator.CreateScheduledTask(ctx, ScheduledTaskParams{
		ProjectID:      rem.ProjectID,
		Prompt:         rem.Content,
		TaskType:       r.cfg.DefaultTaskType,
		IdempotencyKey: idem,
		ReminderID:     rem.ID,
	})
	if err != nil {
		_ = r.cfg.Repo.MarkErrored(ctx, rem.ID, "task creation failed: "+err.Error())
		r.cfg.Logger.Warn().Err(err).Str("reminder_id", rem.ID).Msg("reminders: task-kind spawn failed")
		return
	}
	var nextFireAt *time.Time
	if rem.IsRecurring() {
		next, nerr := NextFireAt(rem.CronExpr, r.cfg.Clock())
		if nerr != nil {
			_ = r.cfg.Repo.MarkErrored(ctx, rem.ID, "re-arm cron invalid: "+nerr.Error())
			r.cfg.Logger.Warn().Err(nerr).Str("reminder_id", rem.ID).Str("cron_expr", rem.CronExpr).
				Msg("reminders: task-kind re-arm cron parse failed; task created but row marked errored")
			return
		}
		if rem.RecurrenceUntil == nil || !next.After(*rem.RecurrenceUntil) {
			nextFireAt = &next
		}
		// If bounded and past the bound, leave nextFireAt nil so the row
		// terminalizes at delivery (FinalizeDelivery terminal=true).
	}
	if err := r.cfg.Repo.MarkTaskSpawned(ctx, rem.ID, taskID, nextFireAt); err != nil {
		r.cfg.Logger.Warn().Err(err).Str("reminder_id", rem.ID).Msg("reminders: mark_task_spawned failed after task create")
		return
	}
	metricTaskSpawned.WithLabelValues(rem.ProjectID).Inc()
	r.auditTask(ctx, rem, taskID, "reminder.task_spawned")
}

// auditTask writes the spawn/deliver audit row (mirrors audit()).
func (r *Runner) auditTask(ctx context.Context, rem *persistence.Reminder, taskID, action string) {
	if r.cfg.AuditRepo == nil {
		return
	}
	_ = r.cfg.AuditRepo.Insert(ctx, &persistence.AdminAuditEntry{
		Principal: rem.OperatorID,
		Source:    "reminder-heartbeat",
		Action:    action,
		Target:    rem.ID,
		After:     `{"task_id":"` + taskID + `","project_id":"` + rem.ProjectID + `"}`,
	})
}

// finalize transitions a row that just delivered to its next
// terminal state. One-shot rows go to 'fired'. Recurring rows
// reschedule to the next cron slot, OR — when the next slot
// exceeds RecurrenceUntil — go terminal so the bounded loop
// finally collapses.
func (r *Runner) finalize(ctx context.Context, rem *persistence.Reminder) error {
	if !rem.IsRecurring() {
		return r.cfg.Repo.MarkFired(ctx, rem.ID)
	}
	next, err := NextFireAt(rem.CronExpr, r.cfg.Clock())
	if err != nil {
		// A corrupt or drifted cron expression at delivery
		// time can't re-arm; mark errored so an operator
		// notices on the next list query rather than the
		// heartbeat looping forever.
		r.cfg.Logger.Error().
			Err(err).
			Str("reminder_id", rem.ID).
			Str("cron_expr", rem.CronExpr).
			Msg("reminders: cron parse failed at re-arm; marking errored")
		return r.cfg.Repo.MarkErrored(ctx, rem.ID, "re-arm cron invalid: "+err.Error())
	}
	if rem.RecurrenceUntil != nil && next.After(*rem.RecurrenceUntil) {
		// Past the operator-named bound — terminate cleanly so
		// the heartbeat stops touching this row.
		return r.cfg.Repo.MarkFired(ctx, rem.ID)
	}
	return r.cfg.Repo.Reschedule(ctx, rem.ID, next)
}

// reminderBodyWithMarker prefixes the operator-supplied content
// with a small "⏰ Reminder" header so the recipient sees this
// is the daemon's scheduled outbound, not a fresh inbound from
// another operator. Cheap UX win — Telegram/email clients
// flatten everything otherwise.
func reminderBodyWithMarker(rem *persistence.Reminder) string {
	return "⏰ Reminder: " + rem.Content
}

func (r *Runner) audit(ctx context.Context, rem *persistence.Reminder) {
	if r.cfg.AuditRepo == nil {
		return
	}
	_ = r.cfg.AuditRepo.Insert(ctx, &persistence.AdminAuditEntry{
		Principal: rem.OperatorID,
		Source:    "reminder-heartbeat",
		Action:    "reminder.fired",
		Target:    rem.ID,
		After:     `{"channel":"` + rem.Channel + `","content_length":` + itoa(len(rem.Content)) + `}`,
	})
}

// itoa avoids the strconv import for a one-liner audit JSON.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
