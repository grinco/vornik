// Package persistence — knowledge-graph interfaces.
//
// Entity / Edge / Mention persistence powering the LLM-extracted KG from memory chunks. See https://docs.vornik.io
// Split from interfaces.go on 2026-05-28 to keep each domain in
// its own file. Same package; no API change — pure file-org.
package persistence

import (
	"context"
)

// KnowledgeEntityRepository persists typed nouns derived from the
// chunk corpus. Phase 43+ of the knowledge-graph memory roadmap.
type KnowledgeEntityRepository interface {
	// Insert writes a new entity. The unique (project_id, type,
	// canonical_name) constraint means duplicates land as
	// ErrDuplicateKey — the resolver short-circuits before Insert
	// when a match exists.
	Insert(ctx context.Context, e *KnowledgeEntity) error

	// Get returns one entity by id; nil + nil when not found.
	Get(ctx context.Context, id string) (*KnowledgeEntity, error)

	// GetByCanonical returns the entity with this (project, type,
	// canonical_name) tuple, or nil + nil when not found.
	// Powers the resolver's pre-LLM exact-match path.
	GetByCanonical(ctx context.Context, projectID, entityType, canonicalName string) (*KnowledgeEntity, error)

	// List returns entities matching the filter. Default
	// lifecycle = ['published'].
	List(ctx context.Context, filter KnowledgeEntityFilter) ([]*KnowledgeEntity, error)

	// SimilarByEmbedding returns top-K entities of the same type
	// ranked by vector cosine distance. Powers the resolver's
	// catalog input + the searcher's "find entity" UX. Returns
	// empty slice when pgvector isn't installed.
	SimilarByEmbedding(ctx context.Context, projectID, entityType string, embedding []float32, limit int) ([]*KnowledgeEntity, error)

	// UpdateLifecycle flips an entity's lifecycle_state, leaving
	// every other column intact. Cascade worker (Phase 50) calls
	// this when a chunk transition fans out to derived entities.
	UpdateLifecycle(ctx context.Context, id, newState string) error

	// AddAlias appends one alias atomically (JSONB || merge).
	// Resolver merge path uses this when a candidate name is a
	// known variant of an existing entity.
	AddAlias(ctx context.Context, id, alias string) error
}

// KnowledgeEdgeRepository persists typed relationships between
// entities.
type KnowledgeEdgeRepository interface {
	// UpsertEdge inserts a new edge OR merges into an existing
	// edge with the same (project, from, predicate, to) tuple.
	// On merge: source_chunks gets the new chunk appended (no
	// duplicates); properties merge JSONB-style; confidence +
	// faithfulness take the max of old vs new.
	UpsertEdge(ctx context.Context, e *KnowledgeEdge) error

	// Get returns one edge by id; nil + nil when not found.
	Get(ctx context.Context, id string) (*KnowledgeEdge, error)

	// List returns edges matching the filter.
	List(ctx context.Context, filter KnowledgeEdgeFilter) ([]*KnowledgeEdge, error)

	// EdgesForEntity returns all edges touching entityID
	// (incoming + outgoing). Powers the entity detail page +
	// 1-hop subgraph view.
	EdgesForEntity(ctx context.Context, entityID string, limit int) ([]*KnowledgeEdge, error)

	// UpdateLifecycle flips an edge's lifecycle_state.
	UpdateLifecycle(ctx context.Context, id, newState string) error

	// DropChunkFromSources removes a chunk_id from every edge's
	// source_chunks array. Cascade worker calls this when a chunk
	// is quarantined / refuted; edges left with empty source_chunks
	// get lifecycle flipped to quarantined.
	DropChunkFromSources(ctx context.Context, chunkID string) (int, error)
}

// EntityMentionRepository persists chunk ↔ entity links.
type EntityMentionRepository interface {
	// Insert writes one mention; idempotent on the (chunk_id,
	// entity_id, char_start) primary key — duplicate inserts are
	// silently no-ops.
	Insert(ctx context.Context, m *EntityMention) error

	// ListByEntity returns chunks mentioning entityID, newest
	// chunk first.
	ListByEntity(ctx context.Context, entityID string, limit int) ([]*EntityMention, error)

	// ListByChunk returns the entities a chunk mentions.
	ListByChunk(ctx context.Context, chunkID string) ([]*EntityMention, error)

	// DeleteForChunk removes every mention for one chunk. Used
	// when re-processing a chunk (the pipeline re-runs and
	// produces a fresh entity set).
	DeleteForChunk(ctx context.Context, chunkID string) error
}

// ChunkGraphExtractionRepository is the narrow query surface the
// KG worker uses to drive Stage 0 (chunk selection) of the
// extraction pipeline. Two methods only — pull a batch of flagged
// chunks, then mark each one extracted on success. Failures leave
// needs_graph_extraction = TRUE so the next tick retries; entity
// inserts and edge upserts performed mid-failure are idempotent
// (resolver short-circuits the next time, edges merge source_chunks).
type ChunkGraphExtractionRepository interface {
	// FetchUnextracted returns up to `limit` chunks where
	// needs_graph_extraction = TRUE, oldest first. Reading directly
	// (no row-level lock) is safe because MarkExtracted is the
	// only writer of the flag and the worker is currently single-
	// instance; multi-instance deploys would add SKIP LOCKED.
	FetchUnextracted(ctx context.Context, limit int) ([]ChunkForExtraction, error)

	// MarkExtracted flips needs_graph_extraction = FALSE on
	// successful pipeline run. Caller never invokes this on
	// failure — the flag stays set so the chunk re-enters the
	// next batch.
	MarkExtracted(ctx context.Context, chunkID string) error

	// PendingCount reports how many chunks still need extraction.
	// Powers the worker's "still draining" gauge so dashboards can
	// show backlog burn-down.
	PendingCount(ctx context.Context) (int, error)

	// Stats returns a global snapshot of pipeline progress —
	// pending/done chunk counts plus the resulting entity, edge,
	// and mention totals (and entity counts by type). One round-
	// trip; powers the /ui/memory KG progress widget.
	Stats(ctx context.Context) (*KGStats, error)

	// ReflagChunksMissingEdges flips needs_graph_extraction back
	// to TRUE on every chunk in projectID that produced ZERO
	// published edges — i.e. chunk.id is absent from every
	// knowledge_edges.source_chunks for that project. The KG
	// worker then picks them up on its next tick and re-runs the
	// extraction pipeline with whatever logic is current.
	//
	// Use case: after a pipeline-logic fix (e.g. the 2026-05-25
	// evidence-substring normalisation) the operator wants the
	// existing isolated entities to actually benefit, not just
	// future ingest. Without this surface, the daemon waits for
	// new chunks to arrive before the fix has measurable impact.
	//
	// Project-scoped (not global) so a fix doesn't gratuitously
	// reprocess every project's history. Idempotent: re-running
	// against the same project re-flags the same chunk set,
	// minus any that the latest pass DID manage to extract edges
	// from. Returns the number of rows flipped.
	//
	// `countOnly` when true skips the UPDATE and just returns
	// the count of chunks that WOULD be re-flagged — used by
	// --dry-run and the daemon's CLI probe.
	ReflagChunksMissingEdges(ctx context.Context, projectID string, countOnly bool) (int, error)
}

// KGStats is the snapshot used by the UI / dashboards to render
// "is the KG pipeline draining?". One value per metric of
// interest; per-project breakdown isn't carried because the
// pipeline drains globally with one worker.
type KGStats struct {
	ChunksPending  int
	ChunksDone     int
	Entities       int
	Edges          int
	Mentions       int
	EntitiesByType map[string]int
}

// ChunkForExtraction is the minimal slice of project_memory_chunks
// the worker needs. Distinct from MemoryChunk in internal/memory
// to avoid an import cycle when the worker (in
// internal/memory/graph) wires this repo.
type ChunkForExtraction struct {
	ID        string
	ProjectID string
	Content   string
}
