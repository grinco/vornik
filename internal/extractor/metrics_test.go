package extractor

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"vornik.io/vornik/internal/testutil/metricstest"
)

func TestNewMetrics_RegistersAllSeries(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	if m == nil {
		t.Fatal("NewMetrics returned nil")
	}
	// Emit one sample on each series, then assert the registry sees
	// exactly the three vornik_extract* families.
	m.recordExtraction("pdf", "ok")
	m.observeDuration("pdf", 0.5)
	m.recordDocument("assistant")

	for _, name := range []string{
		"vornik_extractions_total",
		"vornik_extraction_duration_seconds",
		"vornik_extracted_documents_total",
	} {
		if n := testutil.CollectAndCount(reg, name); n == 0 {
			t.Errorf("series %q not registered/emitted", name)
		}
	}
}

func TestNewMetrics_NilRegistererFallsBack(t *testing.T) {
	// A nil registerer must fall back to the default registerer without
	// panicking. We don't assert on the default registry's contents
	// (other tests pollute it); the contract is "doesn't panic, returns
	// a usable value". Isolate the default registry so the fallback
	// re-registers cleanly under `go test -count>1`.
	metricstest.IsolateDefaultRegistry(t)
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("NewMetrics(nil) panicked: %v", r)
		}
	}()
	if NewMetrics(nil) == nil {
		t.Fatal("NewMetrics(nil) returned nil")
	}
}

func TestMetrics_NilSafeHelpers(t *testing.T) {
	// A nil *Metrics (Runner built without metrics) must no-op, not
	// panic — this is the contract the Runner relies on.
	var m *Metrics
	m.recordExtraction("pdf", "ok")
	m.observeDuration("pdf", 1.0)
	m.recordDocument("assistant")

	// A zero-value Metrics (fields nil) is equally safe.
	empty := &Metrics{}
	empty.recordExtraction("pdf", "error")
	empty.observeDuration("pdf", 1.0)
	empty.recordDocument("assistant")
}
