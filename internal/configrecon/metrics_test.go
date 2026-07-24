package configrecon

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestMetrics_IncRecordsPerNormalizer asserts the counter is registered and
// increments under the normalizer label.
func TestMetrics_IncRecordsPerNormalizer(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	m.Inc("agent-image-qualify")
	m.Inc("agent-image-qualify")

	const want = `
# HELP vornik_config_mirror_normalized_total Count of mirror-seam canonical-field normalizations, by normalizer. The rel path is in the structured log (not a label) to bound cardinality.
# TYPE vornik_config_mirror_normalized_total counter
vornik_config_mirror_normalized_total{normalizer="agent-image-qualify"} 2
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want),
		"vornik_config_mirror_normalized_total"); err != nil {
		t.Errorf("metric mismatch: %v", err)
	}
}

// TestMetrics_NilSafe asserts Inc is a no-op on a nil handle / nil vec (the
// mirror seam calls it unconditionally even when metrics aren't wired).
func TestMetrics_NilSafe(_ *testing.T) {
	var m *Metrics
	m.Inc("x") // must not panic

	empty := &Metrics{}
	empty.Inc("x") // nil NormalizedTotal — must not panic
}
