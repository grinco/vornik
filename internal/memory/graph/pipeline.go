package graph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"vornik.io/vornik/internal/persistence"
)

// Pipeline orchestrates the four KG extraction stages
// (extractor → resolver → relationship → validator) into a
// single per-chunk run, owning all DB writes.
//
// Failure semantics: any stage failure returns an error WITHOUT
// flipping the chunk's needs_graph_extraction flag (the caller —
// the ingest worker — sees the error and leaves the flag set for
// the next pass). Partial DB writes are tolerated: entity inserts
// from a half-finished prior run are picked up by the resolver
// stage as "match" hits on the next attempt, so re-runs converge
// rather than duplicate.
//
// Cost discipline: each stage gets its own chat.Provider model
// pin — typically gpt-oss:20b for extractor / resolver /
// validator and gpt-oss:120b for the relationship extractor (per
// LLD §4.4a). The orchestrator doesn't care which models;
// per-stage Metrics carry the served model so dashboards
// attribute spend correctly.
type Pipeline struct {
	Extractor *Extractor
	Resolver  *Resolver
	Relations *RelationshipExtractor
	Validator *Validator

	Entities persistence.KnowledgeEntityRepository
	Edges    persistence.KnowledgeEdgeRepository
	Mentions persistence.EntityMentionRepository

	// Embedder populates the canonical_name vector on newly-
	// inserted entities so future SimilarByEmbedding shortlists
	// can match them. Optional — when nil, new entities land
	// with NULL embeddings and the resolver falls back to its
	// List() shortlist path on subsequent chunks.
	Embedder EmbedFn

	// Metrics is the optional Prometheus sink. Nil-safe: every
	// emit checks for nil first. Production wires *graph.Metrics;
	// tests usually leave this nil.
	Metrics *Metrics

	// LLMUsage records one task_llm_usage row per stage per
	// chunk so KG extraction spend lands on the same dashboards
	// as worker / dispatcher / judge spend. Optional — nil-safe.
	// Pricing is wired separately so cost_usd computes against
	// the same table the rest of the system uses.
	LLMUsage UsageRecorder
	// Pricing computes USD from token counts. nil → cost_usd is
	// stamped 0 and the row still lands so the spend dashboard
	// shows token volume even on un-priced models.
	Pricing PricingTable
}

// UsageRecorder is the narrow interface the KG pipeline needs
// from persistence.TaskLLMUsageRepository — only Record. Defined
// locally so this package doesn't drag the full repo interface
// (which the orchestrator doesn't otherwise reference) into its
// dependency graph.
type UsageRecorder interface {
	Record(ctx context.Context, u *persistence.TaskLLMUsage) error
}

// PricingTable is the narrow interface for computing per-call
// cost. Mirrors *pricing.Table.CostUSD so production wires
// directly with no adapter, and tests can supply their own.
type PricingTable interface {
	CostUSD(model string, promptTokens, completionTokens int) float64
}

// PipelineMetrics aggregates per-stage telemetry for one chunk
// run. Exposed via the worker so dashboards can plot chunks/sec,
// per-stage tokens, drop ratios, and short-circuit ratio over
// time. Per-stage *Metrics fields may be nil when that stage
// short-circuited (e.g. zero candidates → resolver/relationship
// nil; zero proposals → validator nil).
type PipelineMetrics struct {
	Extract   *ExtractMetrics
	Resolve   *ResolveMetrics
	Relations *RelationshipMetrics
	Validate  *ValidateMetrics

	EntitiesCreated   int
	EntitiesMatched   int
	EntitiesAmbiguous int
	MentionsWritten   int
	EdgesUpserted     int
	EdgesDropped      int
}

// chunkInput is the minimal slice of memory.MemoryChunk the
// pipeline reads. Defining it here (instead of importing
// memory.MemoryChunk) avoids a circular dependency between
// `internal/memory` and `internal/memory/graph` once the worker
// in `internal/memory` calls into this package.
type ChunkInput struct {
	ID        string
	ProjectID string
	Content   string
}

// RunChunk runs the full pipeline against one chunk. Returns
// per-stage metrics on success, partial metrics on the first
// stage error (later stages stay nil so the caller can tell
// where the run aborted).
func (p *Pipeline) RunChunk(ctx context.Context, chunk ChunkInput) (*PipelineMetrics, error) {
	if p == nil {
		return nil, fmt.Errorf("Pipeline.RunChunk: nil pipeline")
	}
	if err := p.validateWiring(); err != nil {
		return nil, err
	}
	if chunk.ID == "" || chunk.ProjectID == "" {
		return nil, fmt.Errorf("Pipeline.RunChunk: chunk id + project id required")
	}
	metrics := &PipelineMetrics{}

	// Stage 1 — entity extraction.
	cands, em, err := p.Extractor.Extract(ctx, chunk.Content)
	metrics.Extract = em
	if err != nil {
		return metrics, fmt.Errorf("extractor: %w", err)
	}
	// Emit the extractor metrics IMMEDIATELY — including the empty-
	// extract short-circuit path below, which would otherwise skip
	// emitStageTokens entirely and leave both the extractor token
	// counter AND the outcome counter blank for the audit-dominant
	// "67% empty extraction" case. The later emitStageTokens skips
	// m.Extract on the produced path (see emitStageTokens) so no
	// double-counting.
	p.emitExtractorMetrics(metrics)
	if len(cands) == 0 {
		return metrics, nil
	}

	// Stage 2 — resolve candidates against catalog.
	resolutions, rm, err := p.Resolver.Resolve(ctx, chunk.ProjectID, cands)
	metrics.Resolve = rm
	if err != nil {
		return metrics, fmt.Errorf("resolver: %w", err)
	}

	// Per-candidate entity ids end up here whether matched, newly
	// created, or ambiguous-quarantined. Nil entries mean we
	// couldn't land an entity row at all (rare — only on Insert
	// failure mid-run).
	entityIDByCand := make([]string, len(cands))
	resolvedForRel := make([]ResolvedEntity, 0, len(cands))
	resolvedSeen := make(map[string]struct{}, len(cands))

	// Same-chunk dedup: if the resolver returns "new" for two
	// candidates with the same (type, canonical_name), we must
	// only insert once. The second occurrence reuses the first's
	// entity id. Without this guard the second insert hits the
	// (project_id, type, canonical_name) unique constraint, which
	// fails the chunk and forces a retry. Live evidence:
	// chunk 41b1bf83… mentioned "Vadim Grinco" twice and the
	// resolver tagged both as new.
	type sameChunkKey struct{ Type, Name string }
	sameChunkIDs := make(map[sameChunkKey]string, len(cands))

	for i, c := range cands {
		res := resolutions[i]
		switch res.Decision {
		case "match":
			entityIDByCand[i] = res.MatchID
			metrics.EntitiesMatched++
			for _, alias := range res.MergeAliases {
				if alias == "" {
					continue
				}
				_ = p.Entities.AddAlias(ctx, res.MatchID, alias)
			}
		case "new":
			key := sameChunkKey{Type: c.Type, Name: c.Name}
			if existingID, dup := sameChunkIDs[key]; dup {
				entityIDByCand[i] = existingID
				metrics.EntitiesMatched++
				if p.Metrics != nil {
					p.Metrics.SameChunkDedupTotal.Inc()
				}
				break
			}
			id, err := p.insertEntityIdempotent(ctx, chunk.ProjectID, c, "published")
			if err != nil {
				return metrics, fmt.Errorf("insert new entity %q: %w", c.Name, err)
			}
			entityIDByCand[i] = id
			sameChunkIDs[key] = id
			metrics.EntitiesCreated++
		default: // "ambiguous" or unknown
			key := sameChunkKey{Type: c.Type, Name: c.Name}
			if existingID, dup := sameChunkIDs[key]; dup {
				entityIDByCand[i] = existingID
				metrics.EntitiesMatched++
				if p.Metrics != nil {
					p.Metrics.SameChunkDedupTotal.Inc()
				}
				break
			}
			id, err := p.insertEntityIdempotent(ctx, chunk.ProjectID, c, "quarantined")
			if err != nil {
				return metrics, fmt.Errorf("insert ambiguous entity %q: %w", c.Name, err)
			}
			entityIDByCand[i] = id
			sameChunkIDs[key] = id
			metrics.EntitiesAmbiguous++
		}

		// Mention row — only when the candidate carries a valid
		// span. Out-of-range offsets were already zeroed by the
		// extractor's validateCandidates.
		if entityIDByCand[i] != "" && (c.CharEnd > c.CharStart) {
			mention := &persistence.EntityMention{
				ChunkID: chunk.ID, EntityID: entityIDByCand[i],
				CharStart: c.CharStart, Surface: c.Surface,
			}
			ce := c.CharEnd
			mention.CharEnd = &ce
			if err := p.Mentions.Insert(ctx, mention); err == nil {
				metrics.MentionsWritten++
			}
		}

		// Build the relationship-stage entity list, deduped.
		if id := entityIDByCand[i]; id != "" {
			if _, dup := resolvedSeen[id]; !dup {
				resolvedSeen[id] = struct{}{}
				resolvedForRel = append(resolvedForRel, ResolvedEntity{
					ID: id, Type: c.Type, CanonicalName: c.Name,
				})
			}
		}
	}

	// Skip the heavy 120b call when there's nothing to relate.
	if len(resolvedForRel) < 2 {
		return metrics, nil
	}

	// Stage 3 — propose edges.
	proposals, relMetrics, err := p.Relations.Extract(ctx, chunk.Content, resolvedForRel)
	metrics.Relations = relMetrics
	if err != nil {
		return metrics, fmt.Errorf("relationship: %w", err)
	}
	if len(proposals) == 0 {
		return metrics, nil
	}

	// Stage 4 — validate proposals against the chunk.
	validated, vm, err := p.Validator.Validate(ctx, chunk.Content, proposals)
	metrics.Validate = vm
	if err != nil {
		return metrics, fmt.Errorf("validator: %w", err)
	}

	// Persist kept edges via UpsertEdge — duplicate triples in
	// later chunks will collapse and just append source_chunks.
	for _, ve := range validated {
		if !ve.Kept {
			metrics.EdgesDropped++
			if p.Metrics != nil {
				p.Metrics.ValidatorDroppedTotal.Inc()
			}
			continue
		}
		score := ve.Score
		edge := &persistence.KnowledgeEdge{
			ProjectID:    chunk.ProjectID,
			FromEntity:   ve.Proposal.From,
			ToEntity:     ve.Proposal.To,
			Predicate:    ve.Proposal.Predicate,
			Properties:   ve.Proposal.Properties,
			SourceChunks: []string{chunk.ID},
			ExtractedBy:  metrics.relationshipModel(),
			Confidence:   score,
			Faithfulness: &score,
		}
		if err := p.Edges.UpsertEdge(ctx, edge); err != nil {
			return metrics, fmt.Errorf("upsert edge %s→%s: %w",
				ve.Proposal.From, ve.Proposal.To, err)
		}
		metrics.EdgesUpserted++
	}
	p.emitStageTokens(metrics)
	p.recordStageUsage(ctx, chunk, metrics)
	return metrics, nil
}

// recordStageUsage writes one task_llm_usage row per stage that
// actually called the LLM. Mirrors the judge runner's pattern:
// project_id + step_id (= chunk_id) are the load-bearing labels;
// task_id stays NULL because the KG worker is a background drain
// loop, not a per-task surface. Source = "kg_extraction" so the
// spend dashboard can isolate KG cost from agent / judge cost.
//
// Errors land in the worker's logger (via the LLMUsage
// implementation's own logging) but never abort the pipeline —
// the chunk is already extracted; failing to bill it is a
// dashboard-fidelity issue, not a correctness issue.
func (p *Pipeline) recordStageUsage(ctx context.Context, chunk ChunkInput, m *PipelineMetrics) {
	if p.LLMUsage == nil || m == nil {
		return
	}
	stages := []struct {
		role  string
		stage stageMetrics
	}{
		{"kg_extractor", m.Extract.tokens()},
		{"kg_resolver", m.Resolve.tokens()},
		{"kg_relationship", m.Relations.tokens()},
		{"kg_validator", m.Validate.tokens()},
	}
	stepID := chunk.ID
	for _, s := range stages {
		if s.stage.zero() {
			continue
		}
		var costUSD float64
		if p.Pricing != nil {
			costUSD = p.Pricing.CostUSD(s.stage.model, s.stage.prompt, s.stage.completion)
		}
		row := &persistence.TaskLLMUsage{
			ID:               persistence.GenerateID("llm"),
			ProjectID:        chunk.ProjectID,
			TaskID:           nil, // background pipeline, not task-scoped
			ExecutionID:      nil,
			StepID:           stepID,
			Role:             s.role,
			Model:            s.stage.model,
			PromptTokens:     int64(s.stage.prompt),
			CompletionTokens: int64(s.stage.completion),
			Iterations:       1,
			CostUSD:          costUSD,
			Source:           persistence.TaskLLMUsageSourceKGExtraction,
		}
		_ = p.LLMUsage.Record(ctx, row)
	}
}

// stageMetrics is the per-stage tuple recordStageUsage needs.
// The four stage-metric structs (ExtractMetrics, ResolveMetrics,
// RelationshipMetrics, ValidateMetrics) carry slightly different
// shapes; the .tokens() methods below normalise them so the
// recorder doesn't care which stage produced the numbers.
type stageMetrics struct {
	model      string
	prompt     int
	completion int
}

func (s stageMetrics) zero() bool {
	return s.prompt == 0 && s.completion == 0
}

func (m *ExtractMetrics) tokens() stageMetrics {
	if m == nil {
		return stageMetrics{}
	}
	return stageMetrics{model: m.Model, prompt: m.PromptTokens, completion: m.CompletionTokens}
}

func (m *ResolveMetrics) tokens() stageMetrics {
	if m == nil {
		return stageMetrics{}
	}
	return stageMetrics{model: m.Model, prompt: m.PromptTokens, completion: m.CompletionTokens}
}

func (m *RelationshipMetrics) tokens() stageMetrics {
	if m == nil {
		return stageMetrics{}
	}
	return stageMetrics{model: m.Model, prompt: m.PromptTokens, completion: m.CompletionTokens}
}

func (m *ValidateMetrics) tokens() stageMetrics {
	if m == nil {
		return stageMetrics{}
	}
	return stageMetrics{model: m.Model, prompt: m.PromptTokens, completion: m.CompletionTokens}
}

// emitExtractorMetrics forwards the extractor stage's tokens +
// per-chunk outcome to Prometheus IMMEDIATELY after Extract
// returns — regardless of whether the chunk produced candidates
// or short-circuited downstream. The audit-dominant case
// (empty extraction) bypasses emitStageTokens entirely, so a
// stage-specific emit at the call site is the only way to keep
// the extractor counters truthful.
//
// Idempotent guards in the rest of the pipeline rely on this
// helper NOT being called twice per chunk; emitStageTokens
// skips Extract when called later (see comment in that
// function).
func (p *Pipeline) emitExtractorMetrics(m *PipelineMetrics) {
	if p.Metrics == nil || m == nil || m.Extract == nil {
		return
	}
	p.Metrics.StageTokensTotal.WithLabelValues("extractor", "input").Add(float64(m.Extract.PromptTokens))
	p.Metrics.StageTokensTotal.WithLabelValues("extractor", "output").Add(float64(m.Extract.CompletionTokens))
	if p.Metrics.ExtractorOutcomesTotal != nil && m.Extract.Outcome != "" {
		p.Metrics.ExtractorOutcomesTotal.WithLabelValues(m.Extract.Outcome).Inc()
	}
}

// emitStageTokens forwards the per-stage token usage from the
// pipeline run into the Prometheus counter. Called once at the
// end of a successful run so partial-failure runs don't double-
// count tokens against stages that didn't fully complete.
func (p *Pipeline) emitStageTokens(m *PipelineMetrics) {
	if p.Metrics == nil || m == nil {
		return
	}
	// Extractor metrics are emitted inline by emitExtractorMetrics
	// right after Extract returns, so the empty-extract short-
	// circuit doesn't bypass them. Skipping here prevents double-
	// counting on the produced path.
	if m.Resolve != nil {
		p.Metrics.StageTokensTotal.WithLabelValues("resolver", "input").Add(float64(m.Resolve.PromptTokens))
		p.Metrics.StageTokensTotal.WithLabelValues("resolver", "output").Add(float64(m.Resolve.CompletionTokens))
		p.Metrics.ResolverDecisionsTotal.WithLabelValues("short_circuit").Add(float64(m.Resolve.ShortCircuited))
		p.Metrics.ResolverDecisionsTotal.WithLabelValues("llm").Add(float64(m.Resolve.LLMResolved))
	}
	if m.Relations != nil {
		p.Metrics.StageTokensTotal.WithLabelValues("relationship", "input").Add(float64(m.Relations.PromptTokens))
		p.Metrics.StageTokensTotal.WithLabelValues("relationship", "output").Add(float64(m.Relations.CompletionTokens))
		// Per-reason drop counters. Sum across labels equals the
		// pre-2026-05-25 single ProposedDropped counter; the
		// breakdown lets dashboards spot which validation rule
		// dominates (and whether future fixes move the
		// distribution as intended).
		if p.Metrics.RelationshipDroppedTotal != nil {
			for reason, n := range m.Relations.DropsByReason {
				if n > 0 {
					p.Metrics.RelationshipDroppedTotal.WithLabelValues(reason).Add(float64(n))
				}
			}
		}
	}
	if m.Validate != nil {
		p.Metrics.StageTokensTotal.WithLabelValues("validator", "input").Add(float64(m.Validate.PromptTokens))
		p.Metrics.StageTokensTotal.WithLabelValues("validator", "output").Add(float64(m.Validate.CompletionTokens))
		// Per-reason validator drops. Mirrors the relationship-stage
		// emission shape (commit 62c3e50). The scalar
		// ValidatorDroppedTotal stays in sync because both increment
		// from the same Dropped counter inside the validator loop.
		if p.Metrics.ValidatorDropsByReasonTotal != nil {
			for reason, n := range m.Validate.DropsByReason {
				if n > 0 {
					p.Metrics.ValidatorDropsByReasonTotal.WithLabelValues(reason).Add(float64(n))
				}
			}
		}
	}
}

// insertEntityIdempotent creates a new knowledge_entities row,
// or recovers the existing row's ID when a concurrent / cross-
// chunk run inserted the same (project_id, type, canonical_name)
// triple first. The unique constraint at the DB level is the
// source of truth; this method makes the orchestrator tolerant
// of races and stale embedding-shortlist results that miss the
// pre-existing row.
//
// The extractor stage labels each candidate with a closed-vocab
// type + canonical name; we stamp lifecycle (published vs
// quarantined) based on resolver decision and embed the canonical
// name when an Embedder is wired so future SimilarByEmbedding
// catches it.
func (p *Pipeline) insertEntityIdempotent(ctx context.Context, projectID string, c Candidate, lifecycle string) (string, error) {
	ent := &persistence.KnowledgeEntity{
		ProjectID:        projectID,
		Type:             c.Type,
		CanonicalName:    c.Name,
		LifecycleState:   lifecycle,
		ValidationStatus: "unverified",
		Confidence:       1.0,
	}
	if c.Subtype != "" {
		props := map[string]string{"subtype": c.Subtype}
		if b, err := json.Marshal(props); err == nil {
			ent.Properties = b
		}
	}
	if p.Embedder != nil {
		if vecs, err := p.Embedder(ctx, []string{c.Name}); err == nil && len(vecs) == 1 {
			ent.Embedding = vecs[0]
		}
	}
	err := p.Entities.Insert(ctx, ent)
	if err == nil {
		return ent.ID, nil
	}
	if !errors.Is(err, persistence.ErrDuplicateKey) {
		return "", err
	}
	// Lost the race — another insert (this batch or a prior run)
	// landed first. Recover its ID via GetByCanonical so callers
	// (mention writes, edge upserts) still get a real entity to
	// reference.
	existing, gerr := p.Entities.GetByCanonical(ctx, projectID, c.Type, c.Name)
	if gerr != nil {
		return "", fmt.Errorf("duplicate-key recovery failed for %q: %w", c.Name, gerr)
	}
	if existing == nil {
		return "", fmt.Errorf("duplicate-key on %q but GetByCanonical returned nil", c.Name)
	}
	if p.Metrics != nil {
		p.Metrics.DupKeyRecoveredTotal.Inc()
	}
	return existing.ID, nil
}

func (p *Pipeline) validateWiring() error {
	switch {
	case p.Extractor == nil:
		return fmt.Errorf("Pipeline: Extractor not wired")
	case p.Resolver == nil:
		return fmt.Errorf("Pipeline: Resolver not wired")
	case p.Relations == nil:
		return fmt.Errorf("Pipeline: Relations not wired")
	case p.Validator == nil:
		return fmt.Errorf("Pipeline: Validator not wired")
	case p.Entities == nil:
		return fmt.Errorf("Pipeline: Entities repo not wired")
	case p.Edges == nil:
		return fmt.Errorf("Pipeline: Edges repo not wired")
	case p.Mentions == nil:
		return fmt.Errorf("Pipeline: Mentions repo not wired")
	}
	return nil
}

// relationshipModel reports the relationship stage's served
// model (when known) so edge rows attribute extracted_by
// correctly. Empty when the stage short-circuited or the
// provider didn't surface a model.
func (m *PipelineMetrics) relationshipModel() string {
	if m == nil || m.Relations == nil {
		return ""
	}
	return m.Relations.Model
}
