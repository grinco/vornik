package chat

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
)

// CallStats is a live, in-process tally of model-call outcomes keyed by
// (model, call_site).
//
// INCIDENT 2026-07-30, customer deployment. Six memory-pipeline models failed 100% of
// their calls for hours — roughly 500 `context deadline exceeded` per hour — while
// `vornikctl doctor` reported "all 11 role-pinned model(s) healthy". Memory ingestion was
// stalled and the one check an operator trusts said everything was fine.
//
// The blind spot was structural, not a bug in the check:
//
//   - `model_health` enumerates models pinned by swarm ROLES. The memory workers
//     (classifier, titler, reranker, graph extractor/resolver/validator) are
//     daemon-level config, not swarm roles, so they were never enumerated.
//   - Its data sources cannot see the failures either. `execution_step_outcomes` needs an
//     execution step; there isn't one. `task_llm_usage` is a SPEND table with no error
//     column, so a call that times out writes nothing at all.
//
// So no queryable record existed that those calls were failing. This closes that gap at
// the one place every model call already passes through.
//
// PROCESS-LIFETIME on purpose. The existing check documents why it avoids Prometheus —
// counters reset on restart, and it wants a window that survives a bounce. That
// reasoning is right for "has this model been bad over 24h" and wrong for "is this model
// failing RIGHT NOW", which is the question an operator asks when they run doctor after
// noticing trouble. The two signals are complementary; this is the second one, and
// findings built on it must say "since daemon start" so nobody mistakes it for history.
type CallStats struct {
	mu      sync.Mutex
	entries map[callStatKey]*callStatEntry
}

// callStatsMaxEntries bounds cardinality. Model and call-site are both bounded in
// practice, but the map is written from a shared chokepoint and a future caller could
// derive a call site from something unbounded; a hard cap keeps that from becoming a slow
// leak in a long-lived daemon.
const callStatsMaxEntries = 512

type callStatKey struct{ model, callSite string }

type callStatEntry struct {
	calls     int
	failures  int
	lastErr   string
	lastFail  time.Time
	firstSeen time.Time
}

// CallStat is one (model, call_site) tally, as read by the doctor.
type CallStat struct {
	Model     string
	CallSite  string
	Calls     int
	Failures  int
	LastError string
	LastFail  time.Time
	Since     time.Time
}

// FailureRate is the failed fraction of observed calls. Zero calls reads as 0 rather
// than dividing by zero — a model nobody called is not failing.
func (c CallStat) FailureRate() float64 {
	if c.Calls == 0 {
		return 0
	}
	return float64(c.Failures) / float64(c.Calls)
}

// NewCallStats returns an empty tally.
func NewCallStats() *CallStats {
	return &CallStats{entries: make(map[callStatKey]*callStatEntry)}
}

// Record notes one completed call. A nil err is a success.
//
// context.Canceled is counted as a call but NOT as a failure: it means the CALLER went
// away — config reload, autonomy-loop restart, daemon shutdown — which LoggingProvider
// already distinguishes for logging. Counting it would make every restart look like an
// outage. context.DeadlineExceeded is a genuine failure and is exactly the customer's
// error, so it must count.
//
// Nil-receiver safe: the sink is optional and call sites pass it unconditionally.
func (s *CallStats) Record(model, callSite string, err error) {
	if s == nil {
		return
	}
	if model == "" {
		model = "(unknown)"
	}
	if callSite == "" {
		callSite = "(unknown)"
	}
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries == nil {
		s.entries = make(map[callStatKey]*callStatEntry)
	}
	key := callStatKey{model: model, callSite: callSite}
	e, ok := s.entries[key]
	if !ok {
		if len(s.entries) >= callStatsMaxEntries {
			// At the cap, keep what we have rather than evicting: the entries already
			// present are the ones with history, and a finding needs history to be
			// worth reporting.
			return
		}
		e = &callStatEntry{firstSeen: now}
		s.entries[key] = e
	}
	e.calls++
	if err != nil && !errors.Is(err, context.Canceled) {
		e.failures++
		e.lastErr = err.Error()
		e.lastFail = now
	}
}

// Snapshot returns a copy of the tally, worst failure rate first so a caller rendering a
// bounded list shows the most alarming entries. Returns nil for a nil receiver.
func (s *CallStats) Snapshot() []CallStat {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	out := make([]CallStat, 0, len(s.entries))
	for k, e := range s.entries {
		out = append(out, CallStat{
			Model:     k.model,
			CallSite:  k.callSite,
			Calls:     e.calls,
			Failures:  e.failures,
			LastError: e.lastErr,
			LastFail:  e.lastFail,
			Since:     e.firstSeen,
		})
	}
	s.mu.Unlock()

	sort.Slice(out, func(i, j int) bool {
		ri, rj := out[i].FailureRate(), out[j].FailureRate()
		if ri != rj {
			return ri > rj
		}
		if out[i].Failures != out[j].Failures {
			return out[i].Failures > out[j].Failures
		}
		if out[i].Model != out[j].Model {
			return out[i].Model < out[j].Model
		}
		return out[i].CallSite < out[j].CallSite
	})
	return out
}
