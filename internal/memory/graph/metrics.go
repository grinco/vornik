package graph

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

const (
	graphNamespace = "vornik"
	graphSubsystem = "memory_graph"
)

// Metrics holds Prometheus metrics for the knowledge-graph
// extraction pipeline. Defined in this package (not in
// internal/memory) to keep the graph package import-cycle-free
// — the existing memory.Metrics already provides ingest /
// embedding / search; this struct adds graph-specific surfaces
// with the `vornik_memory_graph_*` prefix.
//
// Nil-safe: every consumer (Pipeline, Worker, Resolver,
// Validator) checks for nil before recording. Tests construct
// pipelines without a Metrics field; production wires one via
// the container.
type Metrics struct {
	// Worker-level outcome of one chunk pass through the pipeline.
	// Labels: status (success | failed).
	ChunksExtractedTotal *prometheus.CounterVec
	// Per-chunk wall time (extractor → resolver → relationship →
	// validator → DB writes). Histogram with buckets matching
	// realistic Bedrock latencies (~5-300s per chunk, dominated by
	// the relationship 120b call).
	ExtractionDuration prometheus.Histogram
	// Circuit-breaker events. The worker pauses for a cooldown
	// after CircuitBreakerThreshold consecutive failed ticks; this
	// counter increments each time the breaker trips.
	CircuitTrippedTotal prometheus.Counter

	// Per-stage token attribution for cost dashboards. Labels:
	// stage (extractor|resolver|relationship|validator), kind
	// (input|output).
	StageTokensTotal *prometheus.CounterVec
	// Per-chunk extractor outcome counter. Labels: outcome —
	// one of ExtractOutcome* constants (empty_response,
	// dropped_all_invalid, produced). Critical for measuring
	// the 2026-05-25 audit's "67% of chunks produced zero
	// entities" finding over time — without this, the empty-
	// rate trend is invisible to operators tuning prompts or
	// model picks.
	ExtractorOutcomesTotal *prometheus.CounterVec

	// Resolver-side telemetry. Labels: outcome (short_circuit | llm
	// | ambiguous). Operators tune the embedding/Levenshtein gates
	// by watching the short_circuit ratio — it's the single biggest
	// cost lever.
	ResolverDecisionsTotal *prometheus.CounterVec
	// Edges proposed by the relationship stage that fell below the
	// validator threshold. High counts mean the prompt is over-
	// generating; sustained zero means the threshold is too lax.
	//
	// Sibling vec ValidatorDropsByReasonTotal carries the per-
	// reason breakdown (missing_score vs below_threshold). Kept
	// as a scalar so existing Grafana queries on
	// `validator_dropped_total` keep working without a sum().
	ValidatorDroppedTotal prometheus.Counter
	// Per-reason validator drop counts. Labels: reason — one of
	// ValidatorDropReason* constants (missing_score,
	// below_threshold). Sum across labels equals
	// ValidatorDroppedTotal. Lets dashboards isolate truncated-
	// LLM-output drops from genuinely-low-faithfulness drops:
	// the former is a re-prompt candidate, the latter is the
	// validator working as intended.
	ValidatorDropsByReasonTotal *prometheus.CounterVec
	// Per-reason drop counts from validateProposals (the cheap
	// pre-validator pass inside the relationship extractor).
	// Labels: reason — one of DropReason* constants
	// (empty_endpoint, self_loop, unknown_from, unknown_to,
	// unknown_predicate, empty_evidence, evidence_not_in_chunk,
	// duplicate_triple). The sum across labels equals the
	// pre-2026-05-25 single ProposedDropped counter; the
	// breakdown lets dashboards spot which rule dominates.
	// Audit context: 2026-05-25 KG audit found 48% of entities
	// isolated; this metric is the prerequisite for measuring
	// future improvements quantitatively.
	RelationshipDroppedTotal *prometheus.CounterVec

	// Idempotency hits — should be ≪ ChunksExtractedTotal in
	// steady state. SameChunkDedupTotal increments when a chunk
	// has two candidates with identical (type, name) and we reuse
	// the first id; DupKeyRecoveredTotal increments when an Insert
	// races with another caller and we recover via GetByCanonical.
	// Both being non-zero is fine; both rapidly growing means a
	// resolver-shortlist regression.
	SameChunkDedupTotal  prometheus.Counter
	DupKeyRecoveredTotal prometheus.Counter

	// Catalog gauges, refreshed periodically by the worker via
	// ChunkGraphExtractionRepository.Stats. Labels on
	// EntitiesByType: type (PERSON|VENDOR|...). Operators read
	// these to see whether the graph is growing.
	ChunksPending  prometheus.Gauge
	ChunksDone     prometheus.Gauge
	EntitiesTotal  prometheus.Gauge
	EdgesTotal     prometheus.Gauge
	MentionsTotal  prometheus.Gauge
	EntitiesByType *prometheus.GaugeVec
}

// NewMetrics registers and returns a Metrics struct.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	if reg == nil {
		reg = prometheus.DefaultRegisterer
	}
	return &Metrics{
		ChunksExtractedTotal: promauto.With(reg).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: graphNamespace,
				Subsystem: graphSubsystem,
				Name:      "chunks_extracted_total",
				Help:      "Chunks processed by the KG extraction worker, labelled by outcome.",
			},
			[]string{"status"},
		),
		ExtractionDuration: promauto.With(reg).NewHistogram(
			prometheus.HistogramOpts{
				Namespace: graphNamespace,
				Subsystem: graphSubsystem,
				Name:      "extraction_duration_seconds",
				Help:      "End-to-end pipeline wall time per chunk (all four stages plus DB writes).",
				Buckets:   []float64{1, 5, 10, 30, 60, 120, 300, 600},
			},
		),
		CircuitTrippedTotal: promauto.With(reg).NewCounter(
			prometheus.CounterOpts{
				Namespace: graphNamespace,
				Subsystem: graphSubsystem,
				Name:      "circuit_tripped_total",
				Help:      "Times the KG worker's failure circuit-breaker has tripped.",
			},
		),
		StageTokensTotal: promauto.With(reg).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: graphNamespace,
				Subsystem: graphSubsystem,
				Name:      "stage_tokens_total",
				Help:      "Cumulative LLM tokens consumed by each pipeline stage, split by direction.",
			},
			[]string{"stage", "kind"},
		),
		ExtractorOutcomesTotal: promauto.With(reg).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: graphNamespace,
				Subsystem: graphSubsystem,
				Name:      "extractor_outcomes_total",
				Help:      "Per-chunk extractor outcome (empty_response = LLM returned nothing; dropped_all_invalid = all candidates filtered; produced = ≥1 candidate kept).",
			},
			[]string{"outcome"},
		),
		ResolverDecisionsTotal: promauto.With(reg).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: graphNamespace,
				Subsystem: graphSubsystem,
				Name:      "resolver_decisions_total",
				Help:      "Resolver outcomes: short_circuit (gate) vs llm (model) vs ambiguous.",
			},
			[]string{"outcome"},
		),
		ValidatorDroppedTotal: promauto.With(reg).NewCounter(
			prometheus.CounterOpts{
				Namespace: graphNamespace,
				Subsystem: graphSubsystem,
				Name:      "validator_dropped_total",
				Help:      "Edges dropped by the faithfulness validator (aggregate; see validator_drops_by_reason_total for breakdown).",
			},
		),
		ValidatorDropsByReasonTotal: promauto.With(reg).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: graphNamespace,
				Subsystem: graphSubsystem,
				Name:      "validator_drops_by_reason_total",
				Help:      "Validator drops broken down by reason (missing_score = LLM truncated output; below_threshold = genuinely low faithfulness).",
			},
			[]string{"reason"},
		),
		RelationshipDroppedTotal: promauto.With(reg).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: graphNamespace,
				Subsystem: graphSubsystem,
				Name:      "relationship_dropped_total",
				Help:      "Edges dropped by the relationship extractor's cheap pre-validator pass, labelled by drop reason.",
			},
			[]string{"reason"},
		),
		SameChunkDedupTotal: promauto.With(reg).NewCounter(
			prometheus.CounterOpts{
				Namespace: graphNamespace,
				Subsystem: graphSubsystem,
				Name:      "same_chunk_dedup_total",
				Help:      "Same-chunk duplicate (type, canonical_name) candidates collapsed into one entity.",
			},
		),
		DupKeyRecoveredTotal: promauto.With(reg).NewCounter(
			prometheus.CounterOpts{
				Namespace: graphNamespace,
				Subsystem: graphSubsystem,
				Name:      "dup_key_recovered_total",
				Help:      "Cross-chunk Insert collisions recovered via GetByCanonical.",
			},
		),
		ChunksPending: promauto.With(reg).NewGauge(
			prometheus.GaugeOpts{
				Namespace: graphNamespace,
				Subsystem: graphSubsystem,
				Name:      "chunks_pending",
				Help:      "Chunks still flagged needs_graph_extraction = TRUE.",
			},
		),
		ChunksDone: promauto.With(reg).NewGauge(
			prometheus.GaugeOpts{
				Namespace: graphNamespace,
				Subsystem: graphSubsystem,
				Name:      "chunks_done",
				Help:      "Chunks already processed (needs_graph_extraction = FALSE).",
			},
		),
		EntitiesTotal: promauto.With(reg).NewGauge(
			prometheus.GaugeOpts{
				Namespace: graphNamespace,
				Subsystem: graphSubsystem,
				Name:      "entities_total",
				Help:      "Knowledge-graph entity rows.",
			},
		),
		EdgesTotal: promauto.With(reg).NewGauge(
			prometheus.GaugeOpts{
				Namespace: graphNamespace,
				Subsystem: graphSubsystem,
				Name:      "edges_total",
				Help:      "Knowledge-graph edge rows.",
			},
		),
		MentionsTotal: promauto.With(reg).NewGauge(
			prometheus.GaugeOpts{
				Namespace: graphNamespace,
				Subsystem: graphSubsystem,
				Name:      "mentions_total",
				Help:      "entity_mentions rows linking chunks to entities.",
			},
		),
		EntitiesByType: promauto.With(reg).NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: graphNamespace,
				Subsystem: graphSubsystem,
				Name:      "entities_by_type",
				Help:      "Entity counts by closed-vocabulary type (PERSON, VENDOR, …).",
			},
			[]string{"type"},
		),
	}
}
