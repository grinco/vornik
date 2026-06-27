package memory

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/testutil/metricstest"
)

func TestNewMetrics(t *testing.T) {
	t.Run("creates metrics with default registerer", func(t *testing.T) {
		metricstest.IsolateDefaultRegistry(t) // count-safe nil-fallback coverage
		m := NewMetrics(nil)
		require.NotNil(t, m)
		assert.NotNil(t, m.ChunksIngestedTotal)
		assert.NotNil(t, m.IngestErrorsTotal)
		assert.NotNil(t, m.IngestDuration)
		assert.NotNil(t, m.EmbedBatchesTotal)
		assert.NotNil(t, m.EmbeddingsStoredTotal)
		assert.NotNil(t, m.EmbedDuration)
		assert.NotNil(t, m.SearchesTotal)
		assert.NotNil(t, m.SearchResultsTotal)
		assert.NotNil(t, m.SearchDuration)
		assert.NotNil(t, m.ChunksTotal)
		assert.NotNil(t, m.QueueDepth)
		assert.NotNil(t, m.WorkerUp)
		assert.NotNil(t, m.IngestQueueDepth)
		assert.NotNil(t, m.IngestQueueProcessedTotal)
		assert.NotNil(t, m.IngestQueueTerminalFailuresTotal)
		assert.NotNil(t, m.IngestQueueCircuitTripped)
		assert.NotNil(t, m.IngestQueueStaleProcessing)
		assert.NotNil(t, m.IngestEnqueueFallbackTotal)
		assert.NotNil(t, m.TitleBackfillTicksTotal)
		assert.NotNil(t, m.TitleBackfillChunksTotal)
		assert.NotNil(t, m.TitleBackfillRemainingChunks)
		assert.NotNil(t, m.PipelineAdmittedTotal)
		assert.NotNil(t, m.PipelineQuarantinedTotal)
		assert.NotNil(t, m.PipelineRejectsTotal)
		assert.NotNil(t, m.PipelineShadowSignalTotal)
	})

	t.Run("creates metrics with custom registry", func(t *testing.T) {
		registry := prometheus.NewRegistry()
		m := NewMetrics(registry)
		require.NotNil(t, m)
		// Verify metrics were registered into our custom registry
		count, err := registry.Gather()
		require.NoError(t, err)
		// Should have some metrics registered (covers registration path)
		assert.Greater(t, len(count), 0)
	})

	t.Run("metrics can be incremented without panic", func(t *testing.T) {
		registry := prometheus.NewRegistry()
		m := NewMetrics(registry)

		assert.NotPanics(t, func() {
			m.ChunksIngestedTotal.WithLabelValues("proj-1").Inc()
			m.SearchesTotal.WithLabelValues("proj-1", "hybrid").Inc()
			m.SearchResultsTotal.WithLabelValues("proj-1", "hybrid").Inc()
			m.EmbedBatchesTotal.WithLabelValues("success").Inc()
			m.ChunksTotal.WithLabelValues("proj-1").Set(100)
			m.WorkerUp.Set(1)
			m.IngestQueueCircuitTripped.Inc()
			m.IngestQueueStaleProcessing.Set(0)
		})
	})

	t.Run("counter metrics track values correctly", func(t *testing.T) {
		registry := prometheus.NewRegistry()
		m := NewMetrics(registry)

		m.ChunksIngestedTotal.WithLabelValues("proj-1").Inc()
		m.ChunksIngestedTotal.WithLabelValues("proj-1").Inc()
		m.ChunksIngestedTotal.WithLabelValues("proj-2").Inc()

		count := testutil.ToFloat64(m.ChunksIngestedTotal.WithLabelValues("proj-1"))
		assert.Equal(t, 2.0, count)

		count = testutil.ToFloat64(m.ChunksIngestedTotal.WithLabelValues("proj-2"))
		assert.Equal(t, 1.0, count)
	})

	t.Run("gauge metrics track values correctly", func(t *testing.T) {
		registry := prometheus.NewRegistry()
		m := NewMetrics(registry)

		m.ChunksTotal.WithLabelValues("proj-1").Set(42)
		m.QueueDepth.WithLabelValues("proj-1").Set(5)

		count := testutil.ToFloat64(m.ChunksTotal.WithLabelValues("proj-1"))
		assert.Equal(t, 42.0, count)

		count = testutil.ToFloat64(m.QueueDepth.WithLabelValues("proj-1"))
		assert.Equal(t, 5.0, count)
	})

	t.Run("histogram metrics observe without panic", func(t *testing.T) {
		registry := prometheus.NewRegistry()
		m := NewMetrics(registry)

		assert.NotPanics(t, func() {
			m.IngestDuration.WithLabelValues("proj-1").Observe(0.1)
			m.IngestDuration.WithLabelValues("proj-1").Observe(0.5)
			m.EmbedDuration.Observe(1.2)
			m.SearchDuration.WithLabelValues("hybrid").Observe(0.05)
		})
	})

	t.Run("unlabeled counter and gauge work", func(t *testing.T) {
		registry := prometheus.NewRegistry()
		m := NewMetrics(registry)

		assert.NotPanics(t, func() {
			m.IngestQueueCircuitTripped.Inc()
			m.IngestQueueCircuitTripped.Add(2)
			m.WorkerUp.Set(1)
			m.IngestQueueStaleProcessing.Set(3)
		})

		count := testutil.ToFloat64(m.IngestQueueCircuitTripped)
		assert.Equal(t, 3.0, count)

		count = testutil.ToFloat64(m.WorkerUp)
		assert.Equal(t, 1.0, count)

		count = testutil.ToFloat64(m.IngestQueueStaleProcessing)
		assert.Equal(t, 3.0, count)
	})
}

func TestMetrics_MultiLabelCounters(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	t.Run("search metrics with mode label", func(t *testing.T) {
		m.SearchesTotal.WithLabelValues("proj-1", "hybrid").Inc()
		m.SearchesTotal.WithLabelValues("proj-1", "hybrid").Inc()
		m.SearchesTotal.WithLabelValues("proj-1", "keyword").Inc()

		count := testutil.ToFloat64(m.SearchesTotal.WithLabelValues("proj-1", "hybrid"))
		assert.Equal(t, 2.0, count)

		count = testutil.ToFloat64(m.SearchesTotal.WithLabelValues("proj-1", "keyword"))
		assert.Equal(t, 1.0, count)
	})

	t.Run("pipeline metrics with project_id and gate/content_class", func(t *testing.T) {
		m.PipelineAdmittedTotal.WithLabelValues("proj-1", "text").Inc()
		m.PipelineQuarantinedTotal.WithLabelValues("proj-1", "rate_limit").Inc()
		m.PipelineRejectsTotal.WithLabelValues("proj-1", "schema_match").Inc()
		m.PipelineShadowSignalTotal.WithLabelValues("proj-1", "claim_audit_overlap").Inc()

		count := testutil.ToFloat64(m.PipelineAdmittedTotal.WithLabelValues("proj-1", "text"))
		assert.Equal(t, 1.0, count)

		count = testutil.ToFloat64(m.PipelineQuarantinedTotal.WithLabelValues("proj-1", "rate_limit"))
		assert.Equal(t, 1.0, count)
	})
}

func TestMetrics_TitleBackfill(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	m.TitleBackfillTicksTotal.WithLabelValues("idle").Inc()
	m.TitleBackfillChunksTotal.WithLabelValues("succeeded").Add(5)
	m.TitleBackfillRemainingChunks.Set(10)

	count := testutil.ToFloat64(m.TitleBackfillTicksTotal.WithLabelValues("idle"))
	assert.Equal(t, 1.0, count)

	count = testutil.ToFloat64(m.TitleBackfillChunksTotal.WithLabelValues("succeeded"))
	assert.Equal(t, 5.0, count)

	count = testutil.ToFloat64(m.TitleBackfillRemainingChunks)
	assert.Equal(t, 10.0, count)
}

func TestMetrics_IngestQueueMetrics(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	t.Run("processed and terminal failure counters are isolated per project", func(t *testing.T) {
		m.IngestQueueProcessedTotal.WithLabelValues("proj-1").Add(2)
		m.IngestQueueProcessedTotal.WithLabelValues("proj-2").Inc()
		m.IngestQueueTerminalFailuresTotal.WithLabelValues("proj-1").Inc()

		count := testutil.ToFloat64(m.IngestQueueProcessedTotal.WithLabelValues("proj-1"))
		assert.Equal(t, 2.0, count)
		count = testutil.ToFloat64(m.IngestQueueProcessedTotal.WithLabelValues("proj-2"))
		assert.Equal(t, 1.0, count)
		count = testutil.ToFloat64(m.IngestQueueTerminalFailuresTotal.WithLabelValues("proj-1"))
		assert.Equal(t, 1.0, count)
	})

	t.Run("enqueue fallback counter and queue depth gauge track values", func(t *testing.T) {
		m.IngestEnqueueFallbackTotal.WithLabelValues("proj-1").Add(3)
		m.IngestQueueDepth.WithLabelValues("proj-1").Set(7)

		count := testutil.ToFloat64(m.IngestEnqueueFallbackTotal.WithLabelValues("proj-1"))
		assert.Equal(t, 3.0, count)
		count = testutil.ToFloat64(m.IngestQueueDepth.WithLabelValues("proj-1"))
		assert.Equal(t, 7.0, count)
	})
}

func TestMetrics_AdditionalLabelCombinations(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	t.Run("embed batches error status increments", func(t *testing.T) {
		m.EmbedBatchesTotal.WithLabelValues("error").Add(2)

		count := testutil.ToFloat64(m.EmbedBatchesTotal.WithLabelValues("error"))
		assert.Equal(t, 2.0, count)
	})

	t.Run("search duration supports keyword mode", func(t *testing.T) {
		m.SearchDuration.WithLabelValues("keyword").Observe(0.004)

		err := testutil.CollectAndCompare(
			m.SearchDuration,
			strings.NewReader(`
# HELP vornik_memory_search_duration_seconds Memory search query latency.
# TYPE vornik_memory_search_duration_seconds histogram
vornik_memory_search_duration_seconds_bucket{mode="keyword",le="0.001"} 0
vornik_memory_search_duration_seconds_bucket{mode="keyword",le="0.005"} 1
vornik_memory_search_duration_seconds_bucket{mode="keyword",le="0.01"} 1
vornik_memory_search_duration_seconds_bucket{mode="keyword",le="0.025"} 1
vornik_memory_search_duration_seconds_bucket{mode="keyword",le="0.05"} 1
vornik_memory_search_duration_seconds_bucket{mode="keyword",le="0.1"} 1
vornik_memory_search_duration_seconds_bucket{mode="keyword",le="0.25"} 1
vornik_memory_search_duration_seconds_bucket{mode="keyword",le="0.5"} 1
vornik_memory_search_duration_seconds_bucket{mode="keyword",le="1"} 1
vornik_memory_search_duration_seconds_bucket{mode="keyword",le="+Inf"} 1
vornik_memory_search_duration_seconds_sum{mode="keyword"} 0.004
vornik_memory_search_duration_seconds_count{mode="keyword"} 1
`),
		)
		require.NoError(t, err)
	})
}

func TestMetrics_HistogramBucketBoundaries(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	t.Run("search duration boundary values are placed in expected buckets", func(t *testing.T) {
		m.SearchDuration.WithLabelValues("keyword").Observe(0.001)
		m.SearchDuration.WithLabelValues("keyword").Observe(0.0010001)
		m.SearchDuration.WithLabelValues("keyword").Observe(0.005)
		m.SearchDuration.WithLabelValues("keyword").Observe(0.007)

		err := testutil.CollectAndCompare(
			m.SearchDuration,
			strings.NewReader(`
# HELP vornik_memory_search_duration_seconds Memory search query latency.
# TYPE vornik_memory_search_duration_seconds histogram
vornik_memory_search_duration_seconds_bucket{mode="keyword",le="0.001"} 1
vornik_memory_search_duration_seconds_bucket{mode="keyword",le="0.005"} 3
vornik_memory_search_duration_seconds_bucket{mode="keyword",le="0.01"} 4
vornik_memory_search_duration_seconds_bucket{mode="keyword",le="0.025"} 4
vornik_memory_search_duration_seconds_bucket{mode="keyword",le="0.05"} 4
vornik_memory_search_duration_seconds_bucket{mode="keyword",le="0.1"} 4
vornik_memory_search_duration_seconds_bucket{mode="keyword",le="0.25"} 4
vornik_memory_search_duration_seconds_bucket{mode="keyword",le="0.5"} 4
vornik_memory_search_duration_seconds_bucket{mode="keyword",le="1"} 4
vornik_memory_search_duration_seconds_bucket{mode="keyword",le="+Inf"} 4
vornik_memory_search_duration_seconds_sum{mode="keyword"} 0.014000100000000001
vornik_memory_search_duration_seconds_count{mode="keyword"} 4
`),
		)
		require.NoError(t, err)
	})

	t.Run("embed duration exact bucket edge is inclusive", func(t *testing.T) {
		m.EmbedDuration.Observe(0.05)
		m.EmbedDuration.Observe(0.050001)

		err := testutil.CollectAndCompare(
			m.EmbedDuration,
			strings.NewReader(`
# HELP vornik_memory_embed_duration_seconds Latency of one embedding batch API call.
# TYPE vornik_memory_embed_duration_seconds histogram
vornik_memory_embed_duration_seconds_bucket{le="0.05"} 1
vornik_memory_embed_duration_seconds_bucket{le="0.1"} 2
vornik_memory_embed_duration_seconds_bucket{le="0.25"} 2
vornik_memory_embed_duration_seconds_bucket{le="0.5"} 2
vornik_memory_embed_duration_seconds_bucket{le="1"} 2
vornik_memory_embed_duration_seconds_bucket{le="2"} 2
vornik_memory_embed_duration_seconds_bucket{le="5"} 2
vornik_memory_embed_duration_seconds_bucket{le="10"} 2
vornik_memory_embed_duration_seconds_bucket{le="30"} 2
vornik_memory_embed_duration_seconds_bucket{le="+Inf"} 2
vornik_memory_embed_duration_seconds_sum 0.100001
vornik_memory_embed_duration_seconds_count 2
`),
		)
		require.NoError(t, err)
	})
}

func TestMetrics_RegistryContainsAllFamilies(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	m.ChunksIngestedTotal.WithLabelValues("proj-1").Inc()
	m.IngestErrorsTotal.WithLabelValues("proj-1").Inc()
	m.IngestDuration.WithLabelValues("proj-1").Observe(0.01)
	m.EmbedBatchesTotal.WithLabelValues("success").Inc()
	m.EmbeddingsStoredTotal.WithLabelValues("proj-1").Inc()
	m.EmbedDuration.Observe(0.1)
	m.SearchesTotal.WithLabelValues("proj-1", "hybrid").Inc()
	m.SearchResultsTotal.WithLabelValues("proj-1", "hybrid").Add(2)
	m.SearchDuration.WithLabelValues("hybrid").Observe(0.02)
	m.ChunksTotal.WithLabelValues("proj-1").Set(3)
	m.QueueDepth.WithLabelValues("proj-1").Set(1)
	m.WorkerUp.Set(1)
	m.IngestQueueDepth.WithLabelValues("proj-1").Set(2)
	m.IngestQueueProcessedTotal.WithLabelValues("proj-1").Inc()
	m.IngestQueueTerminalFailuresTotal.WithLabelValues("proj-1").Inc()
	m.IngestQueueCircuitTripped.Inc()
	m.IngestQueueStaleProcessing.Set(0)
	m.IngestEnqueueFallbackTotal.WithLabelValues("proj-1").Inc()
	m.TitleBackfillTicksTotal.WithLabelValues("idle").Inc()
	m.TitleBackfillChunksTotal.WithLabelValues("succeeded").Inc()
	m.TitleBackfillRemainingChunks.Set(4)
	m.ClassifyBackfillTicksTotal.WithLabelValues("idle").Inc()
	m.ClassifyBackfillChunksTotal.WithLabelValues("succeeded").Inc()
	m.ClassifyBackfillRemainingChunks.Set(0)
	m.PipelineAdmittedTotal.WithLabelValues("proj-1", "text").Inc()
	m.PipelineQuarantinedTotal.WithLabelValues("proj-1", "schema_match").Inc()
	m.PipelineRejectsTotal.WithLabelValues("proj-1", "truncation").Inc()
	m.PipelineShadowSignalTotal.WithLabelValues("proj-1", "claim_audit_overlap").Inc()

	families, err := registry.Gather()
	require.NoError(t, err)

	names := make(map[string]struct{}, len(families))
	for _, family := range families {
		names[family.GetName()] = struct{}{}
	}

	expectedNames := []string{
		"vornik_memory_chunks_ingested_total",
		"vornik_memory_ingest_errors_total",
		"vornik_memory_ingest_duration_seconds",
		"vornik_memory_embed_batches_total",
		"vornik_memory_embeddings_stored_total",
		"vornik_memory_embed_duration_seconds",
		"vornik_memory_searches_total",
		"vornik_memory_search_results_total",
		"vornik_memory_search_duration_seconds",
		"vornik_memory_search_rerank_duration_seconds",
		"vornik_memory_chunks_total",
		"vornik_memory_queue_depth",
		"vornik_memory_worker_up",
		"vornik_memory_ingest_queue_depth",
		"vornik_memory_ingest_queue_processed_total",
		"vornik_memory_ingest_queue_terminal_failures_total",
		"vornik_memory_ingest_queue_circuit_tripped_total",
		"vornik_memory_ingest_queue_stale_processing",
		"vornik_memory_ingest_enqueue_fallback_total",
		"vornik_memory_title_backfill_ticks_total",
		"vornik_memory_title_backfill_chunks_total",
		"vornik_memory_title_backfill_remaining_chunks",
		"vornik_memory_classify_backfill_ticks_total",
		"vornik_memory_classify_backfill_chunks_total",
		"vornik_memory_classify_backfill_remaining_chunks",
		"vornik_memory_pipeline_admitted_total",
		"vornik_memory_pipeline_quarantined_total",
		"vornik_memory_pipeline_rejects_total",
		"vornik_memory_pipeline_shadow_signal_total",
	}

	for _, metricName := range expectedNames {
		_, ok := names[metricName]
		assert.True(t, ok, "expected metric family %s to be registered", metricName)
	}
	assert.Len(t, names, len(expectedNames))
}
