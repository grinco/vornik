package replay

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics is the Phase C operator surface for the failure-forensics
// fork primitive. One counter today; future replay-side telemetry
// (e.g. timeline-page render duration, fork-outcome rollup once we
// trace forks through to terminal status) lands on this struct so
// the registration site stays one block.
type Metrics struct {
	// ForksTotal counts every fork attempt by outcome. Labels:
	//   - "created"           — fork task created successfully
	//   - "validation_failed" — bad step_id, missing fields
	//   - "source_not_found"  — source execution doesn't exist
	//   - "step_missing"      — chosen step has no recorded outcome
	//   - "error"             — DB error or other unexpected fault
	// Operators watch the ratio of created vs the four refusal
	// labels to spot UI bugs (lots of validation_failed means
	// the modal is letting bad inputs through) and operator
	// confusion (lots of step_missing means people are trying to
	// fork from steps that didn't run, which suggests the UI
	// isn't disabling those buttons).
	ForksTotal *prometheus.CounterVec
}

// NewMetrics constructs the replay metrics surface. Pass the
// daemon's prometheus.Registerer; nil falls back to
// prometheus.DefaultRegisterer so a plain `NewMetrics(nil)` works
// for tests that don't care about scraping.
func NewMetrics(registerer prometheus.Registerer) *Metrics {
	if registerer == nil {
		registerer = prometheus.DefaultRegisterer
	}
	return &Metrics{
		ForksTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "vornik",
				Subsystem: "replay",
				Name:      "forks_total",
				Help:      "Number of fork-from-step attempts, labelled by outcome.",
			},
			[]string{"outcome"},
		),
	}
}

// recordForkOutcome bumps the ForksTotal counter for `outcome` if
// Metrics is wired. Nil-safe so production paths that omit
// metrics wiring (or test paths that don't care) stay quiet.
func (m *Metrics) recordForkOutcome(outcome string) {
	if m == nil || m.ForksTotal == nil {
		return
	}
	m.ForksTotal.WithLabelValues(outcome).Inc()
}
