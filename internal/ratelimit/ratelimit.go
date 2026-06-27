// Package ratelimit provides a per-project sliding-window counter used to
// cap task creation rate. Shared between the autonomy loop, the dispatcher
// create_task tool, and the API POST /tasks handler so a project can't
// burst past its cap by routing through a different entry point.
//
// Two backends are available behind the ProjectLimiter interface:
//   - Limiter (in-process, default) — counter resets on daemon restart.
//     Fine for single-daemon deployments; caps are defensive (keep a
//     misbehaving loop from DoS-ing the executor) so a transient
//     restart-window over-allowance is acceptable.
//   - PostgresProjectLimiter (sub-item 5 of rate-limit hardening) —
//     counters persist in the ratelimit_counters table so two daemons
//     sharing the same project enforce the same combined cap. Opt-in
//     via config RateLimit.Backend: "postgres".
//
// Durable cost accounting (i.e. budget.Check) still lives in the
// TaskLLMUsage table — these limiters are about request RATE, not
// money.
package ratelimit

import (
	"context"
	"sync"
	"time"

	"vornik.io/vornik/internal/registry"
)

// ProjectLimiter is the dependency injection seam between the
// in-process Limiter and the postgres-backed implementation.
// Callers consume this interface so the swap is transparent.
// Existing callers of *Limiter keep working unchanged — the
// concrete type satisfies this interface.
type ProjectLimiter interface {
	// Check inspects whether one more task for p would exceed
	// the configured caps. Read-only; does not consume a slot.
	// Implementations MUST be safe under concurrent calls.
	Check(p *registry.Project, now time.Time) Decision
	// Record marks that a task has been accepted, counting
	// toward future Check calls. Idempotent only by absence of
	// transactional guarantee — callers invoke Record once per
	// accepted task, never speculatively.
	Record(projectID string, now time.Time)
}

// ProjectLimiterCtx extends ProjectLimiter with a context-aware
// variant; the postgres backend needs ctx for cancellation
// propagation through the DB driver. In-process Limiter ignores
// ctx (it has no IO to cancel). New call sites that have a
// request context should prefer this surface; legacy call sites
// (autonomy loop, dispatcher) can keep using ProjectLimiter.
type ProjectLimiterCtx interface {
	ProjectLimiter
	CheckCtx(ctx context.Context, p *registry.Project, now time.Time) Decision
	RecordCtx(ctx context.Context, projectID string, now time.Time)
}

// Limiter tracks per-project task-creation timestamps in two rolling
// windows (minute + hour) and reports whether new work would exceed the
// caps. Safe for concurrent use.
type Limiter struct {
	mu  sync.Mutex
	log map[string][]time.Time // projectID → timestamps, newest last
}

// New returns a fresh Limiter.
func New() *Limiter {
	return &Limiter{log: make(map[string][]time.Time)}
}

// Decision is the outcome of Check.
type Decision struct {
	// Blocked is true when adding this task would exceed a configured limit.
	Blocked bool
	// Reason is operator-facing text; safe to surface in API errors and
	// chat responses. Empty when not blocked.
	Reason string
	// MinuteCount / HourCount are the current per-window counts at check time,
	// useful for logging and panel display.
	MinuteCount int
	HourCount   int
	// RetryAfter is the time until the binding window frees a slot.
	// Set only by the key-scoped CheckKey path (the trading surface)
	// when Blocked; the task-creation Check path leaves it zero (its
	// callers don't emit Retry-After). Zero otherwise.
	RetryAfter time.Duration
}

// Check inspects the project's rate limits without consuming a slot. Returns
// Blocked=true when adding one more task would exceed a configured window.
// Caps of zero disable that dimension.
func (l *Limiter) Check(p *registry.Project, now time.Time) Decision {
	if l == nil || p == nil {
		return Decision{}
	}
	if p.RateLimit.TasksPerMinute == 0 && p.RateLimit.TasksPerHour == 0 {
		return Decision{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	ts := l.log[p.ID]

	minuteCutoff := now.Add(-1 * time.Minute)
	hourCutoff := now.Add(-1 * time.Hour)
	ts = pruneOlderThan(ts, hourCutoff)
	l.log[p.ID] = ts // persist the pruned slice so memory doesn't balloon

	var minuteCount, hourCount int
	for _, t := range ts {
		if t.After(hourCutoff) {
			hourCount++
		}
		if t.After(minuteCutoff) {
			minuteCount++
		}
	}

	d := Decision{MinuteCount: minuteCount, HourCount: hourCount}
	if p.RateLimit.TasksPerMinute > 0 && minuteCount >= p.RateLimit.TasksPerMinute {
		d.Blocked = true
		d.Reason = "per-minute task rate limit reached"
		return d
	}
	if p.RateLimit.TasksPerHour > 0 && hourCount >= p.RateLimit.TasksPerHour {
		d.Blocked = true
		d.Reason = "per-hour task rate limit reached"
		return d
	}
	return d
}

// Record marks that a task was created for the project, counting toward
// future Check calls. Callers should invoke Record only after the task is
// actually accepted (past idempotency + validation), never before Check.
func (l *Limiter) Record(projectID string, now time.Time) {
	if l == nil || projectID == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.log[projectID] = append(l.log[projectID], now)
}

// CheckCtx is the context-aware variant of Check. The in-process
// limiter does no IO so ctx is unused; the signature exists so
// callers that hold a request context can call the same method
// regardless of which backend is wired.
func (l *Limiter) CheckCtx(_ context.Context, p *registry.Project, now time.Time) Decision {
	return l.Check(p, now)
}

// RecordCtx is the context-aware variant of Record. Same
// rationale as CheckCtx: signature compatibility with the
// postgres backend; the in-process limiter ignores ctx.
func (l *Limiter) RecordCtx(_ context.Context, projectID string, now time.Time) {
	l.Record(projectID, now)
}

// Compile-time check: in-process Limiter satisfies both surfaces.
var (
	_ ProjectLimiter    = (*Limiter)(nil)
	_ ProjectLimiterCtx = (*Limiter)(nil)
)

// ProjectSnapshot is the read-only view of one project's task-creation
// activity over the trailing minute and hour windows. Returned by
// Limiter.Snapshot for the
// /api/v1/projects/{id}/ratelimit-status surface.
type ProjectSnapshot struct {
	ProjectID   string
	MinuteCount int
	HourCount   int
}

// Snapshot returns the per-project task counts in the trailing
// minute and hour windows. Pure read — does NOT prune the
// underlying log (that would race with Check on the same project).
// Zero counts for unseen projects are not in the returned slice;
// callers infer "no activity" from absence. Caller may mutate the
// returned slice freely.
func (l *Limiter) Snapshot(now time.Time) []ProjectSnapshot {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	minuteCutoff := now.Add(-1 * time.Minute)
	hourCutoff := now.Add(-1 * time.Hour)
	out := make([]ProjectSnapshot, 0, len(l.log))
	for pid, ts := range l.log {
		var s ProjectSnapshot
		s.ProjectID = pid
		for _, t := range ts {
			if t.After(hourCutoff) {
				s.HourCount++
			}
			if t.After(minuteCutoff) {
				s.MinuteCount++
			}
		}
		// Skip projects whose log is entirely outside the hour
		// window — they appear inactive to the operator panel.
		if s.HourCount == 0 && s.MinuteCount == 0 {
			continue
		}
		out = append(out, s)
	}
	return out
}

// SnapshotFor returns the trailing minute and hour task counts for
// a single project. The boolean return discriminates "unseen
// project / zero counts" (false) from "seen and zero" (true with
// zero counts) — useful when the caller wants to render an explicit
// "0 tasks this hour" badge vs hide the panel entirely.
func (l *Limiter) SnapshotFor(projectID string, now time.Time) (ProjectSnapshot, bool) {
	if l == nil || projectID == "" {
		return ProjectSnapshot{}, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	ts, ok := l.log[projectID]
	if !ok {
		return ProjectSnapshot{}, false
	}
	minuteCutoff := now.Add(-1 * time.Minute)
	hourCutoff := now.Add(-1 * time.Hour)
	s := ProjectSnapshot{ProjectID: projectID}
	for _, t := range ts {
		if t.After(hourCutoff) {
			s.HourCount++
		}
		if t.After(minuteCutoff) {
			s.MinuteCount++
		}
	}
	return s, true
}

// pruneOlderThan drops timestamps older than cutoff, preserving order.
// Returns a newly-allocated slice only when pruning actually happens,
// otherwise the input is returned as-is.
func pruneOlderThan(ts []time.Time, cutoff time.Time) []time.Time {
	// Fast path: nothing to prune.
	if len(ts) == 0 || !ts[0].Before(cutoff) {
		return ts
	}
	// Find first timestamp >= cutoff. Slices are append-ordered
	// (monotonic), so a linear walk is fine.
	i := 0
	for i < len(ts) && ts[i].Before(cutoff) {
		i++
	}
	return ts[i:]
}
