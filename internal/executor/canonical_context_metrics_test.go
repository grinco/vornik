package executor

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestRecordCanonicalContextLoaded pins the label set + zero-
// dimension defense the metric promises operators. Empty source
// is a no-op so a future caller that forgets to populate Source
// doesn't generate a junk series.
func TestRecordCanonicalContextLoaded(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	m.RecordCanonicalContextLoaded("assistant", "dot_autonomy")
	m.RecordCanonicalContextLoaded("assistant", "dot_autonomy")
	m.RecordCanonicalContextLoaded("janka", "mixed")
	m.RecordCanonicalContextLoaded("ignored", "")      // skipped — empty source
	m.RecordCanonicalContextLoaded("", "dot_autonomy") // empty project still records — project label IS valid

	if got := testutil.ToFloat64(m.CanonicalContextLoadedTotal.WithLabelValues("assistant", "dot_autonomy")); got != 2 {
		t.Errorf("assistant/dot_autonomy = %v, want 2", got)
	}
	if got := testutil.ToFloat64(m.CanonicalContextLoadedTotal.WithLabelValues("janka", "mixed")); got != 1 {
		t.Errorf("janka/mixed = %v, want 1", got)
	}
	// Empty-source guard: no series should have been created.
	if got := testutil.ToFloat64(m.CanonicalContextLoadedTotal.WithLabelValues("ignored", "")); got != 0 {
		t.Errorf("empty source should not record; got %v", got)
	}
}

// TestRecordCanonicalContextTruncated pins the per-file
// truncation counter shape.
func TestRecordCanonicalContextTruncated(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	m.RecordCanonicalContextTruncated("assistant", "project")
	m.RecordCanonicalContextTruncated("assistant", "project")
	m.RecordCanonicalContextTruncated("assistant", "user")
	m.RecordCanonicalContextTruncated("janka", "") // skipped — empty file

	if got := testutil.ToFloat64(m.CanonicalContextTruncatedTotal.WithLabelValues("assistant", "project")); got != 2 {
		t.Errorf("assistant/project = %v, want 2", got)
	}
	if got := testutil.ToFloat64(m.CanonicalContextTruncatedTotal.WithLabelValues("assistant", "user")); got != 1 {
		t.Errorf("assistant/user = %v, want 1", got)
	}
}

// TestRecordCanonicalContext_NilMetrics is the nil-safety pin.
// Workspace prep on a no-metrics deployment still calls the
// recorders; they must no-op cleanly.
func TestRecordCanonicalContext_NilMetrics(t *testing.T) {
	var m *Metrics // nil
	m.RecordCanonicalContextLoaded("x", "dot_autonomy")
	m.RecordCanonicalContextTruncated("x", "project")
}
