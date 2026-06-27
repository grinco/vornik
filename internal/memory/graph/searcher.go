package graph

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"vornik.io/vornik/internal/persistence"
)

// KnowledgeGraphSearcher is the READ surface over the knowledge
// graph. The write path (extraction pipeline) populates
// knowledge_entities / knowledge_edges / entity_mentions; this is
// the complementary read path that turns those rows back into
// answers for operators (UI) and the lead (RAG context).
//
// Every method is PROJECT-SCOPED. A graph read must never surface
// an entity, edge, or chunk from a different project — a cross-
// project leak would be a security bug equivalent to the chunk
// retrieval path leaking across projects. The implementation
// enforces this defensively: the underlying repos' Get(id) /
// EdgesForEntity(id) take only an id (no project filter at the SQL
// layer), so the Searcher re-checks ProjectID on every row it
// returns and drops anything that doesn't belong to the requested
// project.
//
// See https://docs.vornik.io §6.
type KnowledgeGraphSearcher interface {
	// FindEntities returns entities ranked by name + (optional)
	// embedding match, scoped to projectID and filtered to types
	// when types is non-empty. Published lifecycle only.
	FindEntities(ctx context.Context, projectID, query string, types []string, limit int) ([]*persistence.KnowledgeEntity, error)

	// GetEntity loads one entity plus its outgoing and incoming
	// 1-hop edges. Returns (nil, nil, nil, nil) when the entity
	// doesn't exist OR belongs to a different project — the caller
	// can't distinguish "missing" from "not yours", which is the
	// correct posture for a cross-tenant read.
	GetEntity(ctx context.Context, projectID, entityID string) (entity *persistence.KnowledgeEntity, outgoing, incoming []*persistence.KnowledgeEdge, err error)

	// ChunksMentioning returns the chunks that mention entityID,
	// scoped to projectID and filtered by repoScope using the same
	// migration-75 semantics as chunk retrieval (empty repoScope =
	// project-wide; non-empty = that scope OR cross-cutting '*' OR
	// uncategorized NULL). Newest chunk first.
	ChunksMentioning(ctx context.Context, projectID, entityID, repoScope string, limit int) ([]MentionedChunk, error)

	// Subgraph returns the entities + edges within `hops` of the
	// seed entities, all scoped to projectID. Bounded: refuses
	// hops > maxSubgraphHops to keep the recursive expansion
	// cheap. hops <= 0 is treated as 1.
	Subgraph(ctx context.Context, projectID string, seedIDs []string, hops int) (*Subgraph, error)
}

// maxSubgraphHops bounds Subgraph expansion. The LLD (§6.1) caps
// at depth 3; deeper neighbourhoods explode combinatorially and
// aren't a UX we render.
const maxSubgraphHops = 3

// Compile-time assertion that *Searcher implements the interface.
var _ KnowledgeGraphSearcher = (*Searcher)(nil)

// entityReader is the narrow read slice of
// persistence.KnowledgeEntityRepository the searcher needs. Kept
// as an interface so tests inject fakes without a DB.
type entityReader interface {
	Get(ctx context.Context, id string) (*persistence.KnowledgeEntity, error)
	List(ctx context.Context, filter persistence.KnowledgeEntityFilter) ([]*persistence.KnowledgeEntity, error)
	SimilarByEmbedding(ctx context.Context, projectID, entityType string, embedding []float32, limit int) ([]*persistence.KnowledgeEntity, error)
}

// edgeReader is the narrow read slice of
// persistence.KnowledgeEdgeRepository the searcher needs.
type edgeReader interface {
	EdgesForEntity(ctx context.Context, entityID string, limit int) ([]*persistence.KnowledgeEdge, error)
}

// mentionReader is the narrow read slice of
// persistence.EntityMentionRepository the searcher needs.
type mentionReader interface {
	ListByEntity(ctx context.Context, entityID string, limit int) ([]*persistence.EntityMention, error)
}

// MentionedChunk is the projection ChunksMentioning returns: the
// chunk row joined with the mention's surface text. ProjectID and
// RepoScope ride along so the caller can audit the scope decision;
// the searcher has already enforced it before returning.
type MentionedChunk struct {
	ChunkID    string
	ProjectID  string
	SourceName string
	Content    string
	RepoScope  string
	// Surface is the text the extractor matched for this entity in
	// the chunk (entity_mentions.surface). May be empty when the
	// extractor didn't return offsets.
	Surface string
}

// ChunkLookup resolves chunk ids to their scoped row. The searcher
// uses it for ChunksMentioning so it can enforce project + repo_scope
// without depending on internal/memory (which would create an import
// cycle: memory → graph → memory). The postgres implementation lives
// alongside the other KG repos. Implementations MUST only return
// chunks that are visible in the chunk-retrieval path (published
// lifecycle, non-refuted) so the graph read posture matches chunk
// reads.
type ChunkLookup interface {
	// LookupChunks loads the given chunk ids for projectID, applying
	// repoScope migration-75 filtering. Chunks not in projectID are
	// silently dropped. The returned slice preserves no particular
	// order; the caller re-sorts.
	LookupChunks(ctx context.Context, projectID string, chunkIDs []string, repoScope string) ([]MentionedChunk, error)
}

// Subgraph is the bounded neighbourhood returned by Subgraph: the
// entities reachable within N hops of the seeds plus the edges that
// connect them. Both slices are project-scoped.
type Subgraph struct {
	ProjectID string
	Entities  []*persistence.KnowledgeEntity
	Edges     []*persistence.KnowledgeEdge
	// Hops is the (clamped) traversal depth that produced this
	// result — useful for the UI to label "1-hop neighbourhood".
	Hops int
}

// Searcher implements KnowledgeGraphSearcher over the existing KG
// repos. It is read-only and stateless beyond its repo handles.
type Searcher struct {
	entities entityReader
	edges    edgeReader
	mentions mentionReader
	chunks   ChunkLookup
	// embed is optional. When wired, FindEntities blends an
	// embedding-similarity shortlist with the name-prefix matches so
	// "the vendor we picked" finds "ACME Blinds Inc" even without a
	// literal substring hit. Nil → name-match only (still correct,
	// just less fuzzy).
	embed EmbedFn
}

// NewSearcher builds a Searcher. entities/edges/mentions are
// required; chunks and embed are optional (ChunksMentioning returns
// an error when chunks is nil; FindEntities degrades to name-match
// when embed is nil).
func NewSearcher(
	entities entityReader,
	edges edgeReader,
	mentions mentionReader,
	chunks ChunkLookup,
	embed EmbedFn,
) *Searcher {
	return &Searcher{
		entities: entities,
		edges:    edges,
		mentions: mentions,
		chunks:   chunks,
		embed:    embed,
	}
}

// FindEntities — see LLD §6.1. Ranks name matches first (List with
// NameLike + type filter is already project-scoped at the SQL
// layer), then folds in an embedding shortlist when an embedder is
// wired. Dedup by id, stable order: name matches before
// embedding-only matches.
func (s *Searcher) FindEntities(ctx context.Context, projectID, query string, types []string, limit int) ([]*persistence.KnowledgeEntity, error) {
	if projectID == "" {
		return nil, fmt.Errorf("graph.FindEntities: projectID required")
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	query = strings.TrimSpace(query)

	seen := make(map[string]struct{})
	out := make([]*persistence.KnowledgeEntity, 0, limit)

	// 1. Name match (always; SQL-scoped to projectID + published).
	nameMatches, err := s.entities.List(ctx, persistence.KnowledgeEntityFilter{
		ProjectID: projectID,
		Types:     types,
		NameLike:  query,
		Limit:     limit,
	})
	if err != nil {
		return nil, fmt.Errorf("graph.FindEntities name match: %w", err)
	}
	for _, e := range nameMatches {
		if e == nil || e.ProjectID != projectID {
			continue // defensive: List is scoped, but never trust a row's project
		}
		if _, dup := seen[e.ID]; dup {
			continue
		}
		seen[e.ID] = struct{}{}
		out = append(out, e)
		if len(out) >= limit {
			return out, nil
		}
	}

	// 2. Embedding shortlist (optional). One query per requested
	// type — the repo's SimilarByEmbedding is per-type. When no
	// type filter is given we skip the embedding pass: a typeless
	// vector search over the whole catalog isn't what the repo
	// exposes, and the name pass already covered the common UX.
	if s.embed != nil && query != "" && len(types) > 0 {
		vecs, embErr := s.embed(ctx, []string{query})
		if embErr == nil && len(vecs) == 1 && len(vecs[0]) > 0 {
			for _, t := range types {
				sim, simErr := s.entities.SimilarByEmbedding(ctx, projectID, t, vecs[0], limit)
				if simErr != nil {
					return nil, fmt.Errorf("graph.FindEntities embedding match: %w", simErr)
				}
				for _, e := range sim {
					if e == nil || e.ProjectID != projectID {
						continue
					}
					if _, dup := seen[e.ID]; dup {
						continue
					}
					seen[e.ID] = struct{}{}
					out = append(out, e)
					if len(out) >= limit {
						return out, nil
					}
				}
			}
		}
	}

	return out, nil
}

// GetEntity — see LLD §6.1. Loads the entity, verifies it belongs
// to projectID (cross-project guard), then splits its 1-hop edges
// into outgoing (from == entity) and incoming (to == entity). Edges
// are additionally project-checked.
func (s *Searcher) GetEntity(ctx context.Context, projectID, entityID string) (*persistence.KnowledgeEntity, []*persistence.KnowledgeEdge, []*persistence.KnowledgeEdge, error) {
	if projectID == "" || entityID == "" {
		return nil, nil, nil, fmt.Errorf("graph.GetEntity: projectID + entityID required")
	}
	ent, err := s.entities.Get(ctx, entityID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("graph.GetEntity: %w", err)
	}
	// Not-found and cross-project both collapse to "nil entity" so a
	// caller can't probe another project's id space for hits.
	if ent == nil || ent.ProjectID != projectID {
		return nil, nil, nil, nil
	}

	edges, err := s.edges.EdgesForEntity(ctx, entityID, 0)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("graph.GetEntity edges: %w", err)
	}
	var outgoing, incoming []*persistence.KnowledgeEdge
	for _, e := range edges {
		if e == nil || e.ProjectID != projectID {
			continue // defensive cross-project guard on the edge rows too
		}
		switch entityID {
		case e.FromEntity:
			outgoing = append(outgoing, e)
		case e.ToEntity:
			incoming = append(incoming, e)
		}
	}
	return ent, outgoing, incoming, nil
}

// ChunksMentioning — see LLD §6.1 / §7.1. Resolves the mention
// rows for entityID, then hands the chunk ids to the ChunkLookup
// which enforces project + repo_scope. The mention list is
// ordered newest-chunk-first by the repo; we re-apply that order
// after the lookup (which doesn't guarantee order).
//
// Project scope is enforced TWICE: the entity is confirmed to
// belong to projectID before we trust its mentions, and the
// ChunkLookup drops any chunk not in projectID. This double check
// means a mention row pointing at a foreign chunk (which shouldn't
// happen, but the table has no project_id column to enforce it)
// can't leak content cross-project.
func (s *Searcher) ChunksMentioning(ctx context.Context, projectID, entityID, repoScope string, limit int) ([]MentionedChunk, error) {
	if projectID == "" || entityID == "" {
		return nil, fmt.Errorf("graph.ChunksMentioning: projectID + entityID required")
	}
	if s.chunks == nil {
		return nil, fmt.Errorf("graph.ChunksMentioning: chunk lookup not wired")
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}

	// Confirm the entity is ours before trusting its mention rows.
	ent, err := s.entities.Get(ctx, entityID)
	if err != nil {
		return nil, fmt.Errorf("graph.ChunksMentioning entity: %w", err)
	}
	if ent == nil || ent.ProjectID != projectID {
		return nil, nil // not ours / missing → no chunks, no leak
	}

	mentions, err := s.mentions.ListByEntity(ctx, entityID, limit)
	if err != nil {
		return nil, fmt.Errorf("graph.ChunksMentioning mentions: %w", err)
	}
	if len(mentions) == 0 {
		return nil, nil
	}

	// Preserve the repo's newest-first ordering + capture surface
	// text per chunk. Dedup chunk ids (an entity can be mentioned
	// multiple times in one chunk at different offsets).
	order := make([]string, 0, len(mentions))
	surfaceByChunk := make(map[string]string, len(mentions))
	for _, m := range mentions {
		if _, ok := surfaceByChunk[m.ChunkID]; !ok {
			order = append(order, m.ChunkID)
		}
		if m.Surface != "" && surfaceByChunk[m.ChunkID] == "" {
			surfaceByChunk[m.ChunkID] = m.Surface
		} else if _, ok := surfaceByChunk[m.ChunkID]; !ok {
			surfaceByChunk[m.ChunkID] = ""
		}
	}

	resolved, err := s.chunks.LookupChunks(ctx, projectID, order, repoScope)
	if err != nil {
		return nil, fmt.Errorf("graph.ChunksMentioning lookup: %w", err)
	}

	byID := make(map[string]MentionedChunk, len(resolved))
	for _, c := range resolved {
		byID[c.ChunkID] = c
	}
	out := make([]MentionedChunk, 0, len(resolved))
	for _, id := range order {
		c, ok := byID[id]
		if !ok {
			continue // dropped by scope/lifecycle filter — correct
		}
		c.Surface = surfaceByChunk[id]
		out = append(out, c)
	}
	return out, nil
}

// Subgraph — see LLD §6.1 / §7.3. BFS from the seeds out to `hops`
// levels. Every entity and edge is project-checked; foreign rows
// are dropped (so a seed that names a foreign entity simply yields
// nothing for that seed). The expansion is bounded by maxSubgraphHops
// and by the per-entity edge cap inside EdgesForEntity.
func (s *Searcher) Subgraph(ctx context.Context, projectID string, seedIDs []string, hops int) (*Subgraph, error) {
	if projectID == "" {
		return nil, fmt.Errorf("graph.Subgraph: projectID required")
	}
	if hops <= 0 {
		hops = 1
	}
	if hops > maxSubgraphHops {
		hops = maxSubgraphHops
	}

	entityByID := make(map[string]*persistence.KnowledgeEntity)
	edgeByID := make(map[string]*persistence.KnowledgeEdge)

	// Frontier starts at the seed entities that actually belong to
	// the project. `loaded` tracks every entity id we've already
	// pulled so we don't re-query in a cyclic graph.
	frontier := make([]string, 0, len(seedIDs))
	loaded := make(map[string]struct{})
	for _, id := range seedIDs {
		if id == "" {
			continue
		}
		if _, dup := loaded[id]; dup {
			continue
		}
		ent, err := s.entities.Get(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("graph.Subgraph seed %s: %w", id, err)
		}
		loaded[id] = struct{}{}
		if ent == nil || ent.ProjectID != projectID {
			continue // foreign / missing seed: drop silently
		}
		entityByID[id] = ent
		frontier = append(frontier, id)
	}

	for depth := 0; depth < hops && len(frontier) > 0; depth++ {
		next := make([]string, 0)
		for _, id := range frontier {
			edges, err := s.edges.EdgesForEntity(ctx, id, 0)
			if err != nil {
				return nil, fmt.Errorf("graph.Subgraph edges %s: %w", id, err)
			}
			for _, e := range edges {
				if e == nil || e.ProjectID != projectID {
					continue // cross-project guard
				}
				edgeByID[e.ID] = e
				for _, neighbour := range []string{e.FromEntity, e.ToEntity} {
					if neighbour == "" {
						continue
					}
					if _, ok := loaded[neighbour]; ok {
						continue
					}
					ent, err := s.entities.Get(ctx, neighbour)
					if err != nil {
						return nil, fmt.Errorf("graph.Subgraph neighbour %s: %w", neighbour, err)
					}
					loaded[neighbour] = struct{}{}
					if ent == nil || ent.ProjectID != projectID {
						continue
					}
					entityByID[neighbour] = ent
					next = append(next, neighbour)
				}
			}
		}
		frontier = next
	}

	sg := &Subgraph{ProjectID: projectID, Hops: hops}
	for _, e := range entityByID {
		sg.Entities = append(sg.Entities, e)
	}
	for _, e := range edgeByID {
		sg.Edges = append(sg.Edges, e)
	}
	// Stable output: entities by canonical name, edges by id. The
	// UI + tests want deterministic ordering.
	sort.Slice(sg.Entities, func(i, j int) bool {
		return sg.Entities[i].CanonicalName < sg.Entities[j].CanonicalName
	})
	sort.Slice(sg.Edges, func(i, j int) bool {
		return sg.Edges[i].ID < sg.Edges[j].ID
	})
	return sg, nil
}
