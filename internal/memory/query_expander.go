package memory

import (
	"context"
	"encoding/json"
	"strings"

	"vornik.io/vornik/internal/persistence"
)

// QueryExpander widens a search query with terms drawn from the
// knowledge graph or any other lexical source. Wired by the service
// container so the memory package stays independent of the graph
// package's full surface; tests can supply deterministic stubs.
//
// Returns a slice of extra terms (canonical names, aliases, related
// entity names) that the searcher OR-tokenises onto the original
// query. Empty slice / nil = no expansion.
type QueryExpander interface {
	Expand(ctx context.Context, projectID, query string) []string
}

// KGQueryExpander walks the knowledge graph: for every entity whose
// alias or canonical name matches a token in the query, append the
// canonical names of one-hop neighbours. The expansion is bounded so
// a high-degree entity doesn't balloon the query.
type KGQueryExpander struct {
	// Entities reads knowledge_entities. Required.
	Entities persistence.KnowledgeEntityRepository
	// Edges reads knowledge_edges. Required.
	Edges persistence.KnowledgeEdgeRepository
	// MaxSeeds caps how many entities a single query string can match
	// as seed nodes. 0 → 3. The first MaxSeeds tokens that resolve
	// against the entity table become seeds; subsequent tokens are
	// ignored so a noisy query doesn't fan out to dozens of seeds.
	MaxSeeds int
	// MaxNeighbors caps the per-seed 1-hop neighbour expansion.
	// 0 → 5. Tight cap because each neighbour appends one term to
	// the search-side tsquery; too many noises the relevance signal.
	MaxNeighbors int
}

// Expand walks the graph and returns the extra terms. Failure-tolerant:
// any DB error returns an empty expansion + no error so the search
// still completes with the original query.
func (e *KGQueryExpander) Expand(ctx context.Context, projectID, query string) []string {
	if e == nil || e.Entities == nil || e.Edges == nil || projectID == "" {
		return nil
	}
	maxSeeds := e.MaxSeeds
	if maxSeeds <= 0 {
		maxSeeds = 3
	}
	maxNeighbors := e.MaxNeighbors
	if maxNeighbors <= 0 {
		maxNeighbors = 5
	}

	tokens := tokenizeQueryForExpansion(query)
	if len(tokens) == 0 {
		return nil
	}

	seeds := make([]*persistence.KnowledgeEntity, 0, maxSeeds)
	seenSeedID := make(map[string]struct{})
	for _, tok := range tokens {
		if len(seeds) >= maxSeeds {
			break
		}
		// One DB hop per token. List with a tight limit so a noisy
		// catalog can't dominate the request budget.
		ents, err := e.Entities.List(ctx, persistence.KnowledgeEntityFilter{
			ProjectID: projectID,
			NameLike:  tok,
			Limit:     3,
		})
		if err != nil {
			continue
		}
		for _, ent := range ents {
			if ent == nil {
				continue
			}
			if _, dup := seenSeedID[ent.ID]; dup {
				continue
			}
			seenSeedID[ent.ID] = struct{}{}
			seeds = append(seeds, ent)
			if len(seeds) >= maxSeeds {
				break
			}
		}
	}

	if len(seeds) == 0 {
		return nil
	}

	terms := make(map[string]struct{})
	// Aliases of each seed are themselves expansion candidates so the
	// user's query catches chunks indexed under a sibling name.
	// Aliases are stored as JSONB so we decode lazily; a malformed
	// blob just skips that seed's aliases rather than abort expansion.
	for _, s := range seeds {
		for _, a := range decodeAliases(s.Aliases) {
			a = strings.TrimSpace(a)
			if a != "" {
				terms[strings.ToLower(a)] = struct{}{}
			}
		}
	}
	// One-hop neighbours via EdgesForEntity. Cap per seed so a
	// well-connected entity (say "PostgreSQL") doesn't bury the
	// query under hundreds of neighbour names.
	for _, s := range seeds {
		edges, err := e.Edges.EdgesForEntity(ctx, s.ID, maxNeighbors*2)
		if err != nil {
			continue
		}
		neighborIDs := make([]string, 0, maxNeighbors)
		for _, edge := range edges {
			if edge == nil {
				continue
			}
			other := edge.ToEntity
			if edge.ToEntity == s.ID {
				other = edge.FromEntity
			}
			if other == "" {
				continue
			}
			neighborIDs = append(neighborIDs, other)
			if len(neighborIDs) >= maxNeighbors {
				break
			}
		}
		for _, nid := range neighborIDs {
			ent, err := e.Entities.Get(ctx, nid)
			if err != nil || ent == nil {
				continue
			}
			name := strings.TrimSpace(ent.CanonicalName)
			if name != "" {
				terms[strings.ToLower(name)] = struct{}{}
			}
		}
	}

	out := make([]string, 0, len(terms))
	for t := range terms {
		out = append(out, t)
	}
	return out
}

// decodeAliases unpacks the JSONB aliases array from KnowledgeEntity.
// Empty input or malformed JSON returns nil — expansion gracefully
// degrades to "no aliases for this seed".
func decodeAliases(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

// tokenizeQueryForExpansion is a tiny tokenizer used to pick seed
// candidates: lowercase, alphanumeric tokens of length ≥3. Distinct
// from tokenSet (mmr.go) because callers need the order preserved so
// MaxSeeds is deterministic.
func tokenizeQueryForExpansion(query string) []string {
	var out []string
	var b strings.Builder
	flush := func() {
		if b.Len() >= 3 {
			out = append(out, b.String())
		}
		b.Reset()
	}
	for _, r := range query {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else if r >= 'A' && r <= 'Z' {
			b.WriteRune(r + ('a' - 'A'))
		} else {
			flush()
		}
	}
	flush()
	return out
}

// mergeExpansionIntoQuery returns the FTS-friendly query string,
// appending each unique expansion term once. Lowercases everything so
// duplicate detection works regardless of source casing. Returns the
// original query unchanged when expansion is empty.
func mergeExpansionIntoQuery(query string, expansion []string) string {
	if len(expansion) == 0 {
		return query
	}
	seen := make(map[string]struct{})
	for _, tok := range tokenizeQueryForExpansion(query) {
		seen[tok] = struct{}{}
	}
	var b strings.Builder
	b.WriteString(query)
	for _, term := range expansion {
		term = strings.TrimSpace(strings.ToLower(term))
		if term == "" {
			continue
		}
		// Add the whole term as a phrase; tokenize for dedup so
		// "Postgres" already in the query doesn't get appended.
		toks := tokenizeQueryForExpansion(term)
		fresh := false
		for _, tok := range toks {
			if _, dup := seen[tok]; !dup {
				fresh = true
				seen[tok] = struct{}{}
			}
		}
		if fresh {
			b.WriteByte(' ')
			b.WriteString(term)
		}
	}
	return b.String()
}
