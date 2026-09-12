package autonomy

import (
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// FeedHorizon records how much history one FeedObservations pass actually
// looked at. It exists because "no run found" carries two completely
// different meanings and, before 2026-09-11, no caller could tell them
// apart:
//
//   - the page of tasks was NOT full, so the whole of the project's
//     history was examined and this feed genuinely never ran; versus
//   - the page came back full (Saturated), so older history exists and
//     was never looked at — the feed may have run 200 days ago, or may
//     be 200 days overdue, and this pass cannot say which.
//
// Reporting the second as the first is the "examined and clean" vs
// "never examined" conflation this whole surface exists to prevent
// (project rule 4), reproduced inside the detector itself.
type FeedHorizon struct {
	// Tasks is how many tasks this pass was given to inspect.
	Tasks int
	// Oldest is the CreatedAt of the oldest task inspected — the far
	// edge of the examined window. Zero when no tasks were inspected.
	Oldest time.Time
	// Saturated reports that the page returned at least as many tasks as
	// were asked for, i.e. older history exists but was not examined.
	Saturated bool
}

// FeedObservation is one declared feed measured against the task list.
// Lag is the age of the most recent task carrying the feed's slug
// prefix, whatever that task's terminal status: a FAILED run does not
// reset the timer, because a feed retrying in a tight loop is a
// degradation whatever the reason for it.
type FeedObservation struct {
	Slug    string
	Cadence time.Duration
	Lag     time.Duration
	// NeverRan means no task carrying this slug's prefix was found in
	// the inspected history. On its own that is NOT the same as "this
	// feed has never run" — see Unmeasured and Horizon.
	NeverRan bool
	// LagAtLeast is a proven LOWER BOUND on the lag, set only for an
	// Unmeasured feed: nothing carrying the slug appears anywhere in a
	// full page of the project's most recent tasks, so the feed's last
	// run — if it ever had one — is older than Horizon.Oldest. Zero
	// when it does not apply. It is a bound, never a measurement, and
	// callers must render it as one.
	LagAtLeast time.Duration
	// Horizon is the slice of history this observation was measured
	// against, carried so a caller can distinguish "examined and never
	// ran" from "not examined this far back".
	Horizon FeedHorizon
	// Breach is "slow" (lag exceeds cadence), "fast" (the interval
	// between the two most recent runs is under half the cadence), or
	// "" for neither.
	//
	// "fast" is a GAP predicate, deliberately: design §4 defines it as
	// "a new task for a slug whose PREVIOUS run is younger than half its
	// cadence", i.e. the interval between two consecutive runs. Until
	// 2026-09-11 the code compared the age of the single most recent run
	// against half the cadence instead, which made every healthy feed
	// report `fast` for the whole first half of every cadence period —
	// once per tick, forever. A slug with only one observed run has no
	// gap to measure and therefore cannot be a `fast` breach.
	//
	// A feed with no observed run is normally no breach either — absent
	// evidence is not evidence of a breach. The one exception is an
	// Unmeasured feed whose LagAtLeast bound ALREADY exceeds its
	// cadence: nothing carrying the slug appears anywhere in the
	// examined window, and that window is longer than the cadence, so
	// "overdue" is proven rather than assumed. Without that case `slow`
	// — the 4.9x drift class this surface was built for — was
	// unreachable for any feed that had fallen off the end of the page.
	//
	// slow wins over fast when both hold (a feed that ran twice in quick
	// succession and then stopped): currently-overdue is the more
	// actionable half, and the wire shape carries one direction.
	Breach string
}

// Unmeasured reports that NeverRan means "no run inside the examined
// history" rather than "this feed has never run". A caller that renders
// the two identically is reporting "examined and clean" while meaning
// "never examined".
func (o FeedObservation) Unmeasured() bool { return o.NeverRan && o.Horizon.Saturated }

// feedSlugPrefix is the exact prompt prefix a task must carry to count
// as a run of the given slug. Exact, so the slug "news" does not match
// a "czech-news: " prompt.
func feedSlugPrefix(slug string) string { return slug + ": " }

// feedHorizon summarises the page of tasks a FeedObservations pass was
// handed. pageSize is the bound the CALLER asked its repository for; a
// page that came back with at least that many rows is saturated, meaning
// older history exists that this pass did not look at.
func feedHorizon(tasks []*persistence.Task, pageSize int) FeedHorizon {
	h := FeedHorizon{
		Tasks:     len(tasks),
		Saturated: pageSize > 0 && len(tasks) >= pageSize,
	}
	for _, t := range tasks {
		if t == nil || t.CreatedAt.IsZero() {
			continue
		}
		if h.Oldest.IsZero() || t.CreatedAt.Before(h.Oldest) {
			h.Oldest = t.CreatedAt
		}
	}
	return h
}

// FeedObservations measures each declared feed against the tasks it can
// see. Pure: no I/O, no clock of its own — `now` is passed so the caller
// anchors every observation to one consistent instant, matching
// buildStateContext's existing single-`now` discipline.
//
// pageSize is the row bound the caller asked its task repository for, so
// every observation can carry the horizon it was measured against (see
// FeedHorizon). Pass 0 when the task slice is the project's complete
// history and no page bound applied.
//
// Returns nil when no feeds are declared. That is the honesty rule at
// the data layer: a project that declares nothing produces no
// observations, so no caller can render it as OK.
func FeedObservations(
	feeds []registry.ResolvedFeed,
	tasks []*persistence.Task,
	pageSize int,
	now time.Time,
) []FeedObservation {
	if len(feeds) == 0 {
		return nil
	}
	horizon := feedHorizon(tasks, pageSize)
	out := make([]FeedObservation, 0, len(feeds))
	for _, f := range feeds {
		obs := FeedObservation{
			Slug:     f.Slug,
			Cadence:  f.Cadence,
			NeverRan: true,
			Horizon:  horizon,
		}
		prefix := feedSlugPrefix(f.Slug)

		// The two most recent runs of this slug. Two, not one: `fast` is
		// the interval BETWEEN consecutive runs, so one timestamp cannot
		// express it.
		var newest, previous time.Time
		for _, t := range tasks {
			if t == nil {
				continue
			}
			if !strings.HasPrefix(extractPrompt(t.Payload), prefix) {
				continue
			}
			switch {
			case t.CreatedAt.After(newest):
				previous = newest
				newest = t.CreatedAt
			case t.CreatedAt.After(previous):
				previous = t.CreatedAt
			}
		}

		if newest.IsZero() {
			obs.LagAtLeast, obs.Breach = feedUnmeasuredBound(horizon, f.Cadence, now)
			out = append(out, obs)
			continue
		}

		obs.NeverRan = false
		obs.Lag = now.Sub(newest)
		if obs.Lag < 0 {
			obs.Lag = 0
		}
		switch {
		case obs.Lag > f.Cadence:
			obs.Breach = "slow"
		case !previous.IsZero() && newest.Sub(previous) < f.Cadence/2:
			obs.Breach = "fast"
		}
		out = append(out, obs)
	}
	return out
}

// feedUnmeasuredBound is the no-run-found branch. When the page was not
// saturated the whole history was examined and the feed genuinely never
// ran: no bound, no breach. When it WAS saturated, the feed's last run
// is older than the far edge of the examined window, which is a proven
// lower bound on its lag — and a bound already past the cadence proves
// `slow` without needing the run itself.
func feedUnmeasuredBound(h FeedHorizon, cadence time.Duration, now time.Time) (time.Duration, string) {
	if !h.Saturated || h.Oldest.IsZero() {
		return 0, ""
	}
	bound := now.Sub(h.Oldest)
	if bound < 0 {
		bound = 0
	}
	if bound > cadence {
		return bound, "slow"
	}
	return bound, ""
}
