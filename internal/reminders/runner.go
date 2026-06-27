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

// ChannelResolver returns the conversation.Channel registered
// for a given channel name (e.g. "telegram"). Returns nil when
// the channel isn't wired on this deployment — Runner records
// the row as errored rather than crashing.
type ChannelResolver interface {
	ResolveChannel(name string) conversation.Channel
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
	return &Runner{cfg: cfg, kickCh: make(chan struct{}, 1)}
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
		r.deliver(ctx, rem)
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
