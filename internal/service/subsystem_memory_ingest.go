package service

// MemoryIngestSubsystem is the second concrete extraction
// proving the Subsystem pattern (audit agent's #3, second
// slice). Owns the memory-ingest worker fleet's lifecycle:
// the embed worker (via Manager.Start), the queue ingest
// worker, the title backfiller, the classifier backfiller,
// and the two consolidation workers (LLM-free + LLM-tier).
//
// Pre-extraction these lived imperatively in container.go's
// Run() — six separate `if x != nil { ... }` blocks each
// minting an elector + bootstrapping + launching goroutines.
// All six move here; container.go's Run loses ~100 lines.
//
// Construction stays in container_scheduler.go because the
// scheduler-init flow needs the Indexer wired before the
// executor is built. The Subsystem reads pre-constructed
// state from the container at Build time + drives the
// lifecycle at Start/Stop.

import (
	"context"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/leaderelection"
	"vornik.io/vornik/internal/memory"
)

// MemoryIngestSubsystem owns the lifecycle of every memory-
// ingest goroutine. The construction (Manager, worker pool,
// backfillers) stays in container_scheduler.go for now —
// the scheduler-init flow needs the Indexer before the
// executor is built, so re-routing through Subsystem.Build
// would force a wiring reorder we don't need yet. Build
// reads the pre-constructed state from the container.
type MemoryIngestSubsystem struct {
	logger zerolog.Logger

	// Manager owns the embed worker + state-gauge goroutine.
	// Nil-safe: memory disabled deployments leave it nil.
	manager *memory.Manager

	// ingestWorker drains the project_ingest_queue. Nil when
	// the queue isn't wired (legacy synchronous-only path).
	ingestWorker *memory.IngestWorker

	// titleBackfiller is the periodic loop that re-runs the
	// inline titler on NULL-titled chunks. Nil when titler
	// is disabled.
	titleBackfiller *memory.TitleBackfiller
	titleElector    *leaderelection.Elector
	titleInterval   time.Duration
	titleBatchSize  int

	// classifyBackfiller re-classifies "unclassified" chunks
	// asynchronously. Nil unless Memory.Classifier.Enabled +
	// ChatClient wired.
	classifyBackfiller *memory.ClassifyBackfiller
	classifyElector    *leaderelection.Elector
	classifyInterval   time.Duration
	classifyBatchSize  int

	// consolidateWorker is the LLM-free per-project gist
	// loop. Nil when wholesale-disabled
	// (ConsolidateIntervalSeconds < 0).
	consolidateWorker   *memory.ConsolidateWorker
	consolidateElector  *leaderelection.Elector
	consolidateInterval time.Duration

	// llmConsolidateWorker is the opt-in LLM-tier narrative
	// pass on top of the LLM-free gist. Nil unless
	// Memory.LLMConsolidateEnabled.
	llmConsolidateWorker   *memory.LLMConsolidateWorker
	llmConsolidateElector  *leaderelection.Elector
	llmConsolidateInterval time.Duration
}

// NewMemoryIngestSubsystem returns a fresh subsystem instance.
// Construction happens in Build; this returns the zero value so
// the container can register the subsystem before the
// scheduler-init memory block has run.
func NewMemoryIngestSubsystem() *MemoryIngestSubsystem {
	return &MemoryIngestSubsystem{}
}

// Name implements Subsystem.
func (s *MemoryIngestSubsystem) Name() string { return "memory_ingest" }

// Build reads pre-constructed memory state from the container.
// Returns a skip sentinel when the memory subsystem is disabled
// (manager nil) — preserves the pre-extraction nil-check
// behaviour. A real error here means the wiring is buggy, not
// configured-off.
func (s *MemoryIngestSubsystem) Build(deps *BuildDeps) error {
	if deps == nil || deps.Container == nil {
		return SubsystemSkipped("nil deps")
	}
	c := deps.Container
	s.logger = c.Logger.With().Str("subsystem", s.Name()).Logger()

	if c.memoryManager == nil {
		return SubsystemSkipped("memory manager not configured")
	}
	s.manager = c.memoryManager
	s.ingestWorker = c.ingestWorker
	s.titleBackfiller = c.memoryTitleBackfiller
	s.classifyBackfiller = c.memoryClassifyBackfiller
	s.consolidateWorker = c.memoryConsolidateWorker
	s.llmConsolidateWorker = c.memoryLLMConsolidateWorker

	// Resolve cadence + batch defaults at Build time so Start
	// doesn't need to re-read config. Mirrors the pre-extraction
	// container.go defaults exactly.
	if s.titleBackfiller != nil {
		s.titleInterval = time.Duration(c.Config.Memory.Titler.AutoBackfillIntervalSeconds) * time.Second
		if c.Config.Memory.Titler.AutoBackfillIntervalSeconds == 0 {
			s.titleInterval = 5 * time.Minute
		}
		s.titleBatchSize = c.Config.Memory.Titler.AutoBackfillBatchSize
		if s.titleBatchSize == 0 {
			s.titleBatchSize = 25
		}
	}
	if s.classifyBackfiller != nil && c.Config.Memory.Classifier.AutoBackfillIntervalSeconds > 0 {
		s.classifyInterval = time.Duration(c.Config.Memory.Classifier.AutoBackfillIntervalSeconds) * time.Second
		s.classifyBatchSize = c.Config.Memory.Classifier.AutoBackfillBatchSize
		if s.classifyBatchSize == 0 {
			s.classifyBatchSize = 25
		}
	}
	// Consolidate intervals resolved at Build time so Start
	// doesn't depend on containerFromDetectorCtx returning a
	// real Container — defends against the regression where a
	// nil-container test ctx would have left the workers
	// running at zero-interval (tight loop). Defaults mirror
	// the pre-extraction container.go logic exactly.
	if s.consolidateWorker != nil {
		s.consolidateInterval = time.Duration(c.Config.Memory.ConsolidateIntervalSeconds) * time.Second
		if c.Config.Memory.ConsolidateIntervalSeconds == 0 {
			s.consolidateInterval = 10 * time.Minute
		}
	}
	if s.llmConsolidateWorker != nil {
		s.llmConsolidateInterval = time.Duration(c.Config.Memory.LLMConsolidateIntervalSeconds) * time.Second
		if c.Config.Memory.LLMConsolidateIntervalSeconds == 0 {
			s.llmConsolidateInterval = time.Hour
		}
	}
	return nil
}

// Start launches every memory worker + its leader gate. Order:
// Manager first (provides the Indexer the queue worker pulls
// against), then the ingest worker, then the backfillers + gist
// loops. Mirrors the pre-extraction sequence in container.go's
// Run() so behaviour is identical.
func (s *MemoryIngestSubsystem) Start(ctx context.Context) error {
	if s == nil || s.manager == nil {
		return nil
	}
	c := containerFromDetectorCtx(ctx)

	// Manager (embed worker + state-gauge goroutine).
	s.manager.Start(ctx)
	s.logger.Info().Msg("memory manager started")

	// Queue ingest worker. Internally goroutined.
	if s.ingestWorker != nil {
		s.ingestWorker.Start(ctx)
	}

	// Title backfill — auto loop. Leader-gated for HA.
	// Elector is written back to the container so allElectors()
	// (used by the drain loop to release leases before DB close)
	// still sees it. Same pattern for the other three workers
	// below. Once allElectors moves onto a Subsystem-facing
	// surface, the write-back goes away.
	if s.titleBackfiller != nil {
		if c != nil {
			s.titleElector = c.initWorkerElector("memory_title_backfill")
			c.titleBackfillElector = s.titleElector
		}
		if s.titleElector != nil {
			s.titleBackfiller.LeaderGate = s.titleElector
			s.titleElector.BootstrapAcquire(ctx)
			go s.titleElector.Run(ctx)
		}
		go s.titleBackfiller.Run(collectorsCtxFrom(ctx, c), s.titleInterval, s.titleBatchSize)
	}

	// Classifier backfill — opt-in (interval > 0 gates wiring at Build).
	if s.classifyBackfiller != nil && s.classifyInterval > 0 {
		if c != nil {
			s.classifyElector = c.initWorkerElector("memory_classify_backfill")
			c.classifyBackfillElector = s.classifyElector
		}
		if s.classifyElector != nil {
			s.classifyBackfiller.LeaderGate = s.classifyElector
			s.classifyElector.BootstrapAcquire(ctx)
			go s.classifyElector.Run(ctx)
		}
		go s.classifyBackfiller.Run(collectorsCtxFrom(ctx, c), s.classifyInterval, s.classifyBatchSize)
	}

	// LLM-free consolidate loop. Interval was resolved at Build
	// time onto s.consolidateInterval so the worker never runs
	// with a zero interval (which would cause a tight loop).
	if s.consolidateWorker != nil {
		s.consolidateWorker.Interval = s.consolidateInterval
		if c != nil {
			s.consolidateElector = c.initWorkerElector("memory_consolidate")
			c.consolidateElector = s.consolidateElector
		}
		if s.consolidateElector != nil {
			s.consolidateWorker.LeaderGate = s.consolidateElector
			s.consolidateElector.BootstrapAcquire(ctx)
			go s.consolidateElector.Run(ctx)
		}
		go s.consolidateWorker.Run(collectorsCtxFrom(ctx, c))
	}

	// LLM-tier narrative loop. Same shape, longer default
	// cadence (1h vs 10m for LLM-free).
	if s.llmConsolidateWorker != nil {
		s.llmConsolidateWorker.Interval = s.llmConsolidateInterval
		if c != nil {
			s.llmConsolidateElector = c.initWorkerElector("memory_llm_consolidate")
			c.llmConsolidateElector = s.llmConsolidateElector
		}
		if s.llmConsolidateElector != nil {
			s.llmConsolidateWorker.LeaderGate = s.llmConsolidateElector
			s.llmConsolidateElector.BootstrapAcquire(ctx)
			go s.llmConsolidateElector.Run(ctx)
		}
		go s.llmConsolidateWorker.Run(collectorsCtxFrom(ctx, c))
	}

	return nil
}

// Stop drains the ingest worker + manager in shutdown order
// (queue first so no new chunks enqueue into a draining
// manager, then manager so the embed worker flushes its last
// batch). The other workers respect ctx cancellation handed
// down through collectorsCtx; no explicit stop call needed
// for those.
func (s *MemoryIngestSubsystem) Stop(_ context.Context) error {
	if s == nil {
		return nil
	}
	if s.ingestWorker != nil {
		s.ingestWorker.Stop()
		s.logger.Debug().Msg("memory ingest worker stopped")
	}
	if s.manager != nil {
		s.manager.Stop()
		s.logger.Debug().Msg("memory manager stopped")
	}
	return nil
}

// collectorsCtxFrom returns the container's long-running
// collectors ctx if available, otherwise the supplied ctx.
// The pre-extraction code passed `c.collectorsCtx` directly to
// the auto-backfill / consolidate goroutines (vs `ctx` for the
// electors) — preserves that split via this helper. Subsystems
// without container access (test environments) fall back to ctx,
// which is fine for unit tests where there's no separate
// collectors window.
func collectorsCtxFrom(ctx context.Context, c *Container) context.Context {
	if c != nil && c.collectorsCtx != nil {
		return c.collectorsCtx
	}
	return ctx
}
