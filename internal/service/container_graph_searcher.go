package service

import (
	"context"

	"vornik.io/vornik/internal/memory/graph"
)

// newGraphSearcher builds the knowledge-graph READ surface
// (graph.Searcher) from the already-wired KG repos + the raw DB
// handle (for the scoped chunk lookup) + the memory embedder (for
// the FindEntities embedding shortlist). Returns nil when the KG
// repos or DB aren't available so callers can stay nil-safe.
//
// The same searcher instance backs both the dispatcher's
// memory_search overlay (LLD §6.2) and the operator UI entity pages
// (LLD §7) — it's read-only and stateless beyond its repo handles,
// so sharing one is fine.
//
// see https://docs.vornik.io §6.
func (c *Container) newGraphSearcher() *graph.Searcher {
	if c.repos == nil || c.DB == nil {
		return nil
	}
	if c.repos.KnowledgeEntities == nil || c.repos.KnowledgeEdges == nil || c.repos.EntityMentions == nil {
		return nil
	}

	var embed graph.EmbedFn
	if c.memoryManager != nil && c.memoryManager.Embedder != nil {
		mgr := c.memoryManager
		embed = func(ctx context.Context, texts []string) ([][]float32, error) {
			return mgr.Embedder.Embed(ctx, texts)
		}
	}

	return graph.NewSearcher(
		c.repos.KnowledgeEntities,
		c.repos.KnowledgeEdges,
		c.repos.EntityMentions,
		graph.NewSQLChunkLookup(c.DB),
		embed,
	)
}
