package memory

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

const (
	memNamespace = "vornik"
	memSubsystem = "memory"
)

// Metrics holds all Prometheus metrics for the memory subsystem.
type Metrics struct {
	// Ingestion
	ChunksIngestedTotal *prometheus.CounterVec
	IngestErrorsTotal   *prometheus.CounterVec
	IngestDuration      *prometheus.HistogramVec

	// Embedding pipeline
	EmbedBatchesTotal     *prometheus.CounterVec
	EmbeddingsStoredTotal *prometheus.CounterVec
	EmbedDuration         prometheus.Histogram

	// Search
	SearchesTotal        *prometheus.CounterVec
	SearchResultsTotal   *prometheus.CounterVec
	SearchDuration       *prometheus.HistogramVec
	SearchRerankDuration prometheus.Histogram

	// State gauges (updated periodically by the Manager)
	ChunksTotal *prometheus.GaugeVec
	QueueDepth  *prometheus.GaugeVec
	WorkerUp    prometheus.Gauge

	// Ingest queue (Phase 1 of memory hardening — see
	// https://docs.vornik.io). Distinct
	// from the embedding QueueDepth above: this counts producer-side
	// pending ingests waiting for the IngestWorker to drain them.
	IngestQueueDepth                 *prometheus.GaugeVec
	IngestQueueProcessedTotal        *prometheus.CounterVec
	IngestQueueTerminalFailuresTotal *prometheus.CounterVec
	IngestQueueCircuitTripped        prometheus.Counter
	// IngestQueueStaleProcessing surfaces rows stuck in 'processing'
	// past the stale threshold (default 5 min). Under healthy
	// operation this should always read 0; a non-zero value means
	// either a worker is wedged or the startup sweep hasn't run
	// since a crash. Unlabeled gauge — the per-project breakdown
	// isn't useful when the issue is operational, and an alert
	// firing on the total is easier to reason about.
	IngestQueueStaleProcessing prometheus.Gauge
	// IngestEnqueueFallbackTotal counts the times workflow.go fell
	// back to synchronous IngestText because the queue Enqueue
	// failed. Pre-fix this was a silent path with only a Warn log;
	// the counter makes the failure visible and pageable.
	IngestEnqueueFallbackTotal *prometheus.CounterVec

	// TitleBackfill metrics surface the auto-retry loop that fills
	// in chunks whose inline titler call failed. Steady-state these
	// should idle near zero — a sustained non-zero remaining gauge
	// indicates the LLM endpoint is failing every title attempt.
	TitleBackfillTicksTotal      *prometheus.CounterVec
	TitleBackfillChunksTotal     *prometheus.CounterVec
	TitleBackfillRemainingChunks prometheus.Gauge

	// ClassifyBackfill metrics surface the auto-retry loop that
	// classifies chunks the deterministic role-map left unclassified
	// (e.g. producer_role empty, or role not in the map). Same shape
	// as the titler's metrics — steady-state idles near zero.
	ClassifyBackfillTicksTotal      *prometheus.CounterVec
	ClassifyBackfillChunksTotal     *prometheus.CounterVec
	ClassifyBackfillRemainingChunks prometheus.Gauge

	// Consolidate metrics surface the periodic LLM-free gist loop.
	// Same labelled-tick shape as the backfill loops above so
	// existing dashboards extend without bespoke panels.
	ConsolidateTicksTotal      *prometheus.CounterVec // outcome=idle|progressed|errored
	ConsolidateProjectsTotal   *prometheus.CounterVec // project,outcome=ok|errored
	ConsolidateDurationSeconds *prometheus.HistogramVec

	// HygieneCandidates counts the instinct-layer Consumer C retrieval
	// hints surfaced per project (kind=boost|prune). Advisory only —
	// counting a candidate NEVER implies a delete. Absent when the
	// memory_hygiene consumer gate is off (no hints fetched).
	HygieneCandidates *prometheus.CounterVec // project,kind=boost|prune

	// LLM-tier narrative pass metrics. Same label shapes as the
	// LLM-free tier above so the panels extend trivially with
	// just a metric-name swap.
	NarrativeTicksTotal      *prometheus.CounterVec // outcome=idle|progressed|errored
	NarrativeProjectsTotal   *prometheus.CounterVec // project,outcome=ok|errored
	NarrativeDurationSeconds *prometheus.HistogramVec

	// Pipeline (Phase 2 — gates + quarantine routing).
	// labelled by project_id and the gate name that fired (or
	// content_class for admitted).
	PipelineAdmittedTotal    *prometheus.CounterVec
	PipelineQuarantinedTotal *prometheus.CounterVec
	PipelineRejectsTotal     *prometheus.CounterVec

	// EpochAdmittedChunksTotal is the per-epoch rollup of admitted
	// chunks, incremented by counts.Admitted when an ingest run closes
	// its epoch (rag-ingest LLD §, "snapshot" step). Labelled by
	// project_id; the per-content_class breakdown lives in
	// PipelineAdmittedTotal, which counts each chunk at admit time.
	EpochAdmittedChunksTotal *prometheus.CounterVec
	// Phase 17 — counts admit-with-shadow-signal events from
	// claim_audit_overlap. Phase 19 will route these to the
	// shadow lifecycle; the counter exists so operators can
	// observe partial-grounding rate before the lifecycle ships.
	PipelineShadowSignalTotal *prometheus.CounterVec

	// PipelineRoleOfRecordVerifiedTotal counts chunks marked verified via
	// the role_of_record shortcut — i.e. admitted at validation_status=
	// 'verified' WITHOUT the LLM validator, on the strength of the
	// producing role's class eligibility. Labelled by project_id,
	// content_class, and producer_role so operators can audit how much of
	// the verified corpus bypassed the validator (memory LLD review batch
	// 4 — validator-bypass observability).
	PipelineRoleOfRecordVerifiedTotal *prometheus.CounterVec

	// URL liveness recheck — labelled by project + alive=true|false.
	// Drives operator dashboards for "dead-URL rate" and powers the
	// alert "any chunk's URL just went dead" so memory hits stay
	// trustworthy as the indexed corpus ages.
	URLLivenessTotal *prometheus.CounterVec
}

// NewMetrics registers and returns all memory metrics.
func NewMetrics(registerer prometheus.Registerer) *Metrics {
	if registerer == nil {
		registerer = prometheus.DefaultRegisterer
	}
	return &Metrics{
		ChunksIngestedTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: memNamespace,
				Subsystem: memSubsystem,
				Name:      "chunks_ingested_total",
				Help:      "Total chunks written to project memory.",
			},
			[]string{"project_id"},
		),
		IngestErrorsTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: memNamespace,
				Subsystem: memSubsystem,
				Name:      "ingest_errors_total",
				Help:      "Total chunk ingestion failures.",
			},
			[]string{"project_id"},
		),
		IngestDuration: promauto.With(registerer).NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace: memNamespace,
				Subsystem: memSubsystem,
				Name:      "ingest_duration_seconds",
				Help:      "Time to chunk and insert one artifact into memory.",
				Buckets:   []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5},
			},
			[]string{"project_id"},
		),
		EmbedBatchesTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: memNamespace,
				Subsystem: memSubsystem,
				Name:      "embed_batches_total",
				Help:      "Total embedding API batch calls, labelled by outcome.",
			},
			[]string{"status"}, // success | error
		),
		EmbeddingsStoredTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: memNamespace,
				Subsystem: memSubsystem,
				Name:      "embeddings_stored_total",
				Help:      "Total vector embeddings written to the database.",
			},
			[]string{"project_id"},
		),
		EmbedDuration: promauto.With(registerer).NewHistogram(
			prometheus.HistogramOpts{
				Namespace: memNamespace,
				Subsystem: memSubsystem,
				Name:      "embed_duration_seconds",
				Help:      "Latency of one embedding batch API call.",
				Buckets:   []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30},
			},
		),
		SearchesTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: memNamespace,
				Subsystem: memSubsystem,
				Name:      "searches_total",
				Help:      "Total memory search queries.",
			},
			[]string{"project_id", "mode"}, // mode: hybrid | keyword
		),
		SearchResultsTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: memNamespace,
				Subsystem: memSubsystem,
				Name:      "search_results_total",
				Help:      "Total result chunks returned across all searches.",
			},
			[]string{"project_id", "mode"},
		),
		SearchDuration: promauto.With(registerer).NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace: memNamespace,
				Subsystem: memSubsystem,
				Name:      "search_duration_seconds",
				Help:      "Memory search query latency.",
				Buckets:   []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1},
			},
			[]string{"mode"},
		),
		SearchRerankDuration: promauto.With(registerer).NewHistogram(
			prometheus.HistogramOpts{
				Namespace: memNamespace,
				Subsystem: memSubsystem,
				Name:      "search_rerank_duration_seconds",
				Help:      "Latency of the optional LLM rerank pass over top-K hybrid results.",
				Buckets:   []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10},
			},
		),
		ChunksTotal: promauto.With(registerer).NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: memNamespace,
				Subsystem: memSubsystem,
				Name:      "chunks_total",
				Help:      "Current number of stored memory chunks per project.",
			},
			[]string{"project_id"},
		),
		QueueDepth: promauto.With(registerer).NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: memNamespace,
				Subsystem: memSubsystem,
				Name:      "queue_depth",
				Help:      "Number of chunks awaiting embedding per project.",
			},
			[]string{"project_id"},
		),
		WorkerUp: promauto.With(registerer).NewGauge(
			prometheus.GaugeOpts{
				Namespace: memNamespace,
				Subsystem: memSubsystem,
				Name:      "worker_up",
				Help:      "1 if the background embed worker is running, 0 otherwise.",
			},
		),
		IngestQueueDepth: promauto.With(registerer).NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: memNamespace,
				Subsystem: memSubsystem,
				Name:      "ingest_queue_depth",
				Help:      "Pending ingest-queue rows per project (queued + processing).",
			},
			[]string{"project_id"},
		),
		IngestQueueProcessedTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: memNamespace,
				Subsystem: memSubsystem,
				Name:      "ingest_queue_processed_total",
				Help:      "Successfully drained ingest-queue items.",
			},
			[]string{"project_id"},
		),
		IngestQueueTerminalFailuresTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: memNamespace,
				Subsystem: memSubsystem,
				Name:      "ingest_queue_terminal_failures_total",
				Help:      "Ingest-queue items that exhausted retry budget and went terminal-failed.",
			},
			[]string{"project_id"},
		),
		IngestQueueCircuitTripped: promauto.With(registerer).NewCounter(
			prometheus.CounterOpts{
				Namespace: memNamespace,
				Subsystem: memSubsystem,
				Name:      "ingest_queue_circuit_tripped_total",
				Help:      "Times the ingest worker's failure circuit-breaker has tripped.",
			},
		),
		IngestQueueStaleProcessing: promauto.With(registerer).NewGauge(
			prometheus.GaugeOpts{
				Namespace: memNamespace,
				Subsystem: memSubsystem,
				Name:      "ingest_queue_stale_processing",
				Help:      "Ingest-queue rows stuck in 'processing' past the stale threshold (default 5 minutes). Healthy=0; non-zero indicates either a wedged worker or unresolved post-crash state.",
			},
		),
		IngestEnqueueFallbackTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: memNamespace,
				Subsystem: memSubsystem,
				Name:      "ingest_enqueue_fallback_total",
				Help:      "Times the producer side fell back to synchronous IngestText because the queue Enqueue failed. Bypasses Phase 2 pipeline gates — non-zero values should be investigated.",
			},
			[]string{"project_id"},
		),
		TitleBackfillTicksTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: memNamespace,
				Subsystem: memSubsystem,
				Name:      "title_backfill_ticks_total",
				Help:      "Auto title-backfill ticks, labelled by outcome (idle | progressed | errored).",
			},
			[]string{"outcome"},
		),
		TitleBackfillChunksTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: memNamespace,
				Subsystem: memSubsystem,
				Name:      "title_backfill_chunks_total",
				Help:      "Chunks the auto title-backfill loop processed, labelled by per-chunk status (succeeded | failed | skipped).",
			},
			[]string{"status"},
		),
		TitleBackfillRemainingChunks: promauto.With(registerer).NewGauge(
			prometheus.GaugeOpts{
				Namespace: memNamespace,
				Subsystem: memSubsystem,
				Name:      "title_backfill_remaining_chunks",
				Help:      "Chunks still awaiting a title (content_title IS NULL). Healthy steady state is 0 — a sustained non-zero reading means the titler LLM is failing every attempt.",
			},
		),
		ClassifyBackfillTicksTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: memNamespace,
				Subsystem: memSubsystem,
				Name:      "classify_backfill_ticks_total",
				Help:      "Auto classify-backfill ticks, labelled by outcome (idle | progressed | errored).",
			},
			[]string{"outcome"},
		),
		ClassifyBackfillChunksTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: memNamespace,
				Subsystem: memSubsystem,
				Name:      "classify_backfill_chunks_total",
				Help:      "Chunks the auto classify-backfill loop processed, labelled by per-chunk status (succeeded | failed | skipped).",
			},
			[]string{"status"},
		),
		ClassifyBackfillRemainingChunks: promauto.With(registerer).NewGauge(
			prometheus.GaugeOpts{
				Namespace: memNamespace,
				Subsystem: memSubsystem,
				Name:      "classify_backfill_remaining_chunks",
				Help:      "Chunks still awaiting classification (content_class IS NULL or 'unclassified'). Healthy steady state is 0 — sustained non-zero means the classifier LLM is failing every attempt.",
			},
		),
		ConsolidateTicksTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: memNamespace,
				Subsystem: memSubsystem,
				Name:      "consolidate_ticks_total",
				Help:      "Periodic LLM-free gist ticks, labelled by outcome (idle | progressed | errored).",
			},
			[]string{"outcome"},
		),
		ConsolidateProjectsTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: memNamespace,
				Subsystem: memSubsystem,
				Name:      "consolidate_projects_total",
				Help:      "Projects processed by the gist loop, labelled by outcome (ok | errored).",
			},
			[]string{"project_id", "outcome"},
		),
		ConsolidateDurationSeconds: promauto.With(registerer).NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace: memNamespace,
				Subsystem: memSubsystem,
				Name:      "consolidate_duration_seconds",
				Help:      "Per-project gist duration. Healthy is sub-second; chunks-scanned drives runtime linearly.",
				Buckets:   []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5, 10},
			},
			[]string{"project_id"},
		),
		HygieneCandidates: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: memNamespace,
				Subsystem: memSubsystem,
				Name:      "instinct_hygiene_candidates_total",
				Help:      "Instinct-layer Consumer C retrieval hints surfaced per project (kind=boost|prune). Advisory; never implies a delete.",
			},
			[]string{"project_id", "kind"},
		),
		NarrativeTicksTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: memNamespace,
				Subsystem: memSubsystem,
				Name:      "narrative_ticks_total",
				Help:      "Periodic LLM-tier narrative ticks, labelled by outcome (idle | progressed | errored).",
			},
			[]string{"outcome"},
		),
		NarrativeProjectsTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: memNamespace,
				Subsystem: memSubsystem,
				Name:      "narrative_projects_total",
				Help:      "Projects processed by the narrative loop, labelled by outcome (ok | errored).",
			},
			[]string{"project_id", "outcome"},
		),
		NarrativeDurationSeconds: promauto.With(registerer).NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace: memNamespace,
				Subsystem: memSubsystem,
				Name:      "narrative_duration_seconds",
				Help:      "Per-project narrative duration. Healthy under 30s; LLM endpoint latency dominates.",
				Buckets:   []float64{0.5, 1, 2, 5, 10, 20, 30, 60},
			},
			[]string{"project_id"},
		),
		PipelineAdmittedTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: memNamespace,
				Subsystem: memSubsystem,
				Name:      "pipeline_admitted_total",
				Help:      "Chunks admitted by the ingest pipeline, labelled by content_class.",
			},
			[]string{"project_id", "content_class"},
		),
		EpochAdmittedChunksTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: memNamespace,
				Subsystem: memSubsystem,
				Name:      "epoch_admitted_chunks_total",
				Help:      "Admitted chunks rolled up per closed corpus epoch (incremented by the epoch's admitted count at snapshot/close), labelled by project_id. Per-content_class detail lives in pipeline_admitted_total.",
			},
			[]string{"project_id"},
		),
		PipelineQuarantinedTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: memNamespace,
				Subsystem: memSubsystem,
				Name:      "pipeline_quarantined_total",
				Help:      "Chunks routed to quarantine, labelled by failed_gate.",
			},
			[]string{"project_id", "failed_gate"},
		),
		PipelineRejectsTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: memNamespace,
				Subsystem: memSubsystem,
				Name:      "pipeline_rejects_total",
				Help:      "Candidates refused by the pipeline before storage (gate=schema_match etc).",
			},
			[]string{"project_id", "failed_gate"},
		),
		PipelineShadowSignalTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: memNamespace,
				Subsystem: memSubsystem,
				Name:      "pipeline_shadow_signal_total",
				Help:      "Admitted chunks flagged for shadow lifecycle (partial claim/audit overlap). Phase 19 consumes the flag; this counter is observable from Phase 17.",
			},
			[]string{"project_id", "gate"},
		),
		PipelineRoleOfRecordVerifiedTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: memNamespace,
				Subsystem: memSubsystem,
				Name:      "pipeline_role_of_record_verified_total",
				Help:      "Chunks marked verified via the role_of_record shortcut (admitted at validation_status=verified WITHOUT the LLM validator), labelled by content_class and producer_role. Audits validator-bypass volume.",
			},
			[]string{"project_id", "content_class", "producer_role"},
		),
		URLLivenessTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: memNamespace,
				Subsystem: memSubsystem,
				Name:      "url_liveness_total",
				Help:      "URL liveness recheck outcomes, labelled by project and alive=true|false.",
			},
			[]string{"project", "alive"},
		),
	}
}
