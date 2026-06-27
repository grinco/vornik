// Package metricstest provides shared helpers for unit tests that exercise
// Prometheus-metric constructors.
package metricstest

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// IsolateDefaultRegistry swaps the process-global prometheus.DefaultRegisterer
// for a fresh, isolated registry for the duration of the test, restoring the
// original on cleanup.
//
// Use it in the "constructs with the default registerer" subtests that pass a
// nil registerer (so the constructor falls back to the global default). Those
// subtests are otherwise unsafe under `go test -count>1`: the real default
// registry persists across passes, so the second construction re-registers the
// same collectors and panics with "duplicate metrics collector registration
// attempted". This helper keeps the nil-fallback branch covered while making
// the test count-safe — extracted after the pattern recurred across six
// packages (observability, executor, queue, runtime, scheduler, memory).
func IsolateDefaultRegistry(t *testing.T) {
	t.Helper()
	orig := prometheus.DefaultRegisterer
	prometheus.DefaultRegisterer = prometheus.NewRegistry()
	t.Cleanup(func() { prometheus.DefaultRegisterer = orig })
}
