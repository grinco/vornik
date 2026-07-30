package api

// Doctor check: model_calls_live.
//
// INCIDENT 2026-07-30, customer deployment. `vornikctl doctor` reported "all 11
// role-pinned model(s) healthy" while six memory-pipeline models failed 100% of their
// calls for hours — roughly 500 `context deadline exceeded` per hour — and memory
// ingestion was completely stalled. The operator ran the one check they trust and it told
// them nothing was wrong.
//
// model_health could not have caught it, and that is structural rather than a bug:
//
//   - it enumerates models pinned by swarm ROLES, and the memory workers (classifier,
//     titler, reranker, graph extractor/resolver/validator) are daemon-level config;
//   - its sources cannot see the failures anyway — execution_step_outcomes needs an
//     execution step and there isn't one, while task_llm_usage is a SPEND table with no
//     error column, so a call that times out writes nothing at all.
//
// This check has no enumeration blind spot BY CONSTRUCTION: it reads outcomes recorded at
// the provider wrapper every model call already passes through (chat.CallStats), so a
// call site cannot be missed because nobody remembered to list it.
//
// HORIZON, stated in the finding itself: process-lifetime, reset on restart. model_health
// deliberately avoids that in favour of a DB window that survives a bounce, which is the
// right choice for "has this model been bad over 24h" and the wrong one for "is this
// model failing right now" — the question an operator actually asks when they run doctor
// after noticing trouble. The two are complementary, and the message says which it is so
// nobody mistakes it for history.

import (
	"fmt"
	"sort"
	"strings"
)

const (
	// modelCallsLiveMinSamples is the smallest sample count worth judging. Below this,
	// one or two bad calls should not trip an alarm — same reasoning as
	// modelHealthMinSamples, and an operator who learns to ignore a noisy check is
	// worse off than one with no check.
	modelCallsLiveMinSamples = 5
	// modelCallsLiveFailureRate is the failed fraction at/above which a
	// (model, call_site) pair is flagged.
	modelCallsLiveFailureRate = 0.5
	// modelCallsLiveMaxReported bounds the rendered list so one broken gateway cannot
	// produce a wall of text. Snapshot is sorted worst-first, so the cap keeps the
	// entries that matter.
	modelCallsLiveMaxReported = 6
)

// checkModelCallsLive flags (model, call_site) pairs whose calls are failing now.
//
// Read-only, no --fix: the remedy is always an operator decision — repoint a model, raise
// a timeout, or relieve whatever is saturating the endpoint — and none of those is safe to
// automate under a live deployment.
func (h *DoctorHandlers) checkModelCallsLive() DoctorCheck {
	name := "model_calls_live"

	snap := h.callStats.Snapshot()
	if len(snap) == 0 {
		return DoctorCheck{
			Name:    name,
			Status:  "OK",
			Message: "no model calls observed since daemon start; nothing to assess",
		}
	}

	var (
		flagged  []string
		assessed int
		total    int
		failures int
	)
	for _, s := range snap {
		total += s.Calls
		failures += s.Failures
		if s.Calls < modelCallsLiveMinSamples {
			continue
		}
		assessed++
		if s.FailureRate() < modelCallsLiveFailureRate {
			continue
		}
		entry := fmt.Sprintf("%s via %s: %d/%d calls failing (%.0f%%)",
			s.Model, s.CallSite, s.Failures, s.Calls, s.FailureRate()*100)
		if s.LastError != "" {
			entry += " — last error: " + truncateDoctorError(s.LastError)
		}
		flagged = append(flagged, entry)
	}

	if len(flagged) == 0 {
		return DoctorCheck{
			Name:   name,
			Status: "OK",
			Message: fmt.Sprintf(
				"%d model/call-site pair(s) assessed since daemon start, %d call(s) total, %d failed — none above the %.0f%% failure threshold",
				assessed, total, failures, modelCallsLiveFailureRate*100),
		}
	}

	sort.Strings(flagged)
	if len(flagged) > modelCallsLiveMaxReported {
		dropped := len(flagged) - modelCallsLiveMaxReported
		flagged = flagged[:modelCallsLiveMaxReported]
		flagged = append(flagged, fmt.Sprintf("…and %d more pair(s) above the threshold", dropped))
	}

	return DoctorCheck{
		Name:   name,
		Status: "WARNING",
		Message: "model calls are failing since daemon start — every call site is covered here, " +
			"including the memory workers that model_health does not enumerate:\n  " +
			strings.Join(flagged, "\n  ") +
			"\n  A worker failing at this rate stalls the pipeline it feeds (memory " +
			"classification, titles, graph extraction) without failing any task, so it is " +
			"invisible everywhere else. Check whether the endpoint is saturated before " +
			"repointing the model: calls that are fast in isolation but time out under the " +
			"daemon's own concurrency mean congestion, not a bad model.",
	}
}

// truncateDoctorError bounds an upstream error string so one verbose provider cannot
// dominate the report.
func truncateDoctorError(s string) string {
	const limit = 120
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}

// modelHealthHealthySummary renders model_health's all-clear.
//
// It used to read "all N role-pinned model(s) healthy". Literally true — it had assessed
// exactly the role-pinned models — but on 2026-07-30 an operator read it as "the models
// are fine" while six models it never looked at were failing every call. A check must not
// imply coverage it does not have, so the summary now names its own scope and points at
// the check that covers the rest.
func modelHealthHealthySummary(count int) string {
	return fmt.Sprintf(
		"all %d role-pinned model(s) healthy over the recent window; models used outside swarm roles "+
			"(memory classifier/titler/reranker/graph) are NOT covered here — see model_calls_live",
		count)
}
