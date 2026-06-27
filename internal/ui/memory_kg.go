package ui

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"vornik.io/vornik/internal/memory/graph"
	"vornik.io/vornik/internal/persistence"
)

// KnowledgeGraphReader is the narrow READ surface the UI uses to
// render the entity browser, entity detail, and subgraph pages.
// *graph.Searcher satisfies it. Hidden behind an interface so the
// ui package's handler tests can inject a fake without a DB.
//
// Every method is project-scoped; the implementation enforces the
// same project + repo_scope + cross-project isolation as chunk
// retrieval (see internal/memory/graph/searcher.go). The UI passes
// the project from the URL path so an operator can't pivot to
// another project's graph by tampering with an entity id.
//
// see https://docs.vornik.io §7.
type KnowledgeGraphReader interface {
	FindEntities(ctx context.Context, projectID, query string, types []string, limit int) ([]*persistence.KnowledgeEntity, error)
	GetEntity(ctx context.Context, projectID, entityID string) (*persistence.KnowledgeEntity, []*persistence.KnowledgeEdge, []*persistence.KnowledgeEdge, error)
	ChunksMentioning(ctx context.Context, projectID, entityID, repoScope string, limit int) ([]graph.MentionedChunk, error)
	Subgraph(ctx context.Context, projectID string, seedIDs []string, hops int) (*graph.Subgraph, error)
}

// kgEntityRow is a UI-boundary projection of a graph entity for the
// browser + detail pages. Counts are populated only on the detail
// page (the browser would need an N+1 to fill them, so it shows the
// name + type + description and links into detail for the counts).
type kgEntityRow struct {
	ID            string
	Type          string
	CanonicalName string
	Description   string
	Aliases       []string
	Edges         int
	Chunks        int
}

// kgEdgeRow is a UI-boundary projection of a graph edge.
type kgEdgeRow struct {
	ID         string
	FromEntity string
	ToEntity   string
	Predicate  string
	// Resolved display names for the endpoints (best-effort; falls
	// back to the id when the searcher doesn't surface the name).
	FromName     string
	ToName       string
	Confidence   float32
	SourceChunks []string
}

// kgChunkRow is a UI-boundary projection of a mentioning chunk.
type kgChunkRow struct {
	ChunkID    string
	SourceName string
	Preview    string
	RepoScope  string
	Surface    string
}

// MemoryEntities renders the entity browser for a project:
// /ui/memory/<project>/entities[?type=VENDOR&q=acme]. Groups
// matching entities by type so the operator scans "all the VENDORs"
// at a glance (LLD §7.1).
func (s *Server) MemoryEntities(w http.ResponseWriter, r *http.Request, projectID string) {
	if projectID == "" {
		httpError(w, http.StatusBadRequest, "project id required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	typeFilter := strings.TrimSpace(r.URL.Query().Get("type"))
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if len(query) > 256 {
		query = query[:256]
	}

	type typeGroup struct {
		Type     string
		Entities []kgEntityRow
	}
	data := struct {
		Title       string
		CurrentPage string
		ProjectID   string
		ProjectName string
		Enabled     bool
		Query       string
		TypeFilter  string
		Types       []string
		Groups      []typeGroup
		Total       int
	}{
		Title:       "Entities — " + projectID,
		CurrentPage: "memory",
		ProjectID:   projectID,
		ProjectName: projectID,
		Types:       knowledgeEntityTypes,
		Query:       query,
		TypeFilter:  typeFilter,
	}
	if s.projectReg != nil {
		if p := s.projectReg.GetProject(projectID); p != nil && p.DisplayName != "" {
			data.ProjectName = p.DisplayName
		}
	}

	if s.kgSearcher == nil {
		s.render(w, "memory_entities.html", data)
		return
	}
	data.Enabled = true

	var types []string
	if typeFilter != "" {
		types = []string{typeFilter}
	}
	entities, err := s.kgSearcher.FindEntities(ctx, projectID, query, types, 200)
	if err != nil {
		s.logger.Warn().Err(err).Str("project_id", projectID).Msg("ui: entity browser FindEntities failed")
		s.render(w, "memory_entities.html", data)
		return
	}

	byType := map[string][]kgEntityRow{}
	for _, e := range entities {
		if e == nil {
			continue
		}
		row := kgEntityRow{
			ID:            e.ID,
			Type:          e.Type,
			CanonicalName: e.CanonicalName,
			Description:   e.Description,
		}
		byType[e.Type] = append(byType[e.Type], row)
		data.Total++
	}
	groupTypes := make([]string, 0, len(byType))
	for t := range byType {
		groupTypes = append(groupTypes, t)
	}
	sort.Strings(groupTypes)
	for _, t := range groupTypes {
		rows := byType[t]
		sort.Slice(rows, func(i, j int) bool { return rows[i].CanonicalName < rows[j].CanonicalName })
		data.Groups = append(data.Groups, typeGroup{Type: t, Entities: rows})
	}
	s.render(w, "memory_entities.html", data)
}

// MemoryEntityDetail renders one entity's detail page:
// /ui/memory/<project>/entities/<entityID>. Shows description,
// aliases, 1-hop edges, and the mentioning chunks (LLD §7.1).
func (s *Server) MemoryEntityDetail(w http.ResponseWriter, r *http.Request, projectID, entityID string) {
	if projectID == "" || entityID == "" {
		httpError(w, http.StatusBadRequest, "project id + entity id required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	data := struct {
		Title       string
		CurrentPage string
		ProjectID   string
		ProjectName string
		Enabled     bool
		Found       bool
		Entity      kgEntityRow
		Outgoing    []kgEdgeRow
		Incoming    []kgEdgeRow
		Chunks      []kgChunkRow
	}{
		Title:       "Entity — " + entityID,
		CurrentPage: "memory",
		ProjectID:   projectID,
		ProjectName: projectID,
	}
	if s.projectReg != nil {
		if p := s.projectReg.GetProject(projectID); p != nil && p.DisplayName != "" {
			data.ProjectName = p.DisplayName
		}
	}
	if s.kgSearcher == nil {
		s.render(w, "memory_entity_detail.html", data)
		return
	}
	data.Enabled = true

	ent, outgoing, incoming, err := s.kgSearcher.GetEntity(ctx, projectID, entityID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "entity lookup failed: "+err.Error())
		return
	}
	if ent == nil {
		// Not found OR cross-project — same 404 either way so an
		// operator can't probe another project's id space.
		s.render(w, "memory_entity_detail.html", data)
		return
	}
	data.Found = true
	data.Entity = kgEntityRow{
		ID:            ent.ID,
		Type:          ent.Type,
		CanonicalName: ent.CanonicalName,
		Description:   ent.Description,
		Aliases:       decodeJSONStringArray(ent.Aliases),
		Edges:         len(outgoing) + len(incoming),
	}

	// Build a name map so edges can show the neighbour's canonical
	// name, not just its opaque id. Best-effort: GetEntity on each
	// distinct neighbour. Bounded by the 1-hop edge cap.
	nameByID := map[string]string{ent.ID: ent.CanonicalName}
	resolveName := func(id string) string {
		if n, ok := nameByID[id]; ok {
			return n
		}
		if ne, _, _, e := s.kgSearcher.GetEntity(ctx, projectID, id); e == nil && ne != nil {
			nameByID[id] = ne.CanonicalName
			return ne.CanonicalName
		}
		nameByID[id] = id
		return id
	}
	data.Outgoing = make([]kgEdgeRow, 0, len(outgoing))
	for _, e := range outgoing {
		data.Outgoing = append(data.Outgoing, kgEdgeRow{
			ID: e.ID, FromEntity: e.FromEntity, ToEntity: e.ToEntity, Predicate: e.Predicate,
			FromName: ent.CanonicalName, ToName: resolveName(e.ToEntity),
			Confidence: e.Confidence, SourceChunks: e.SourceChunks,
		})
	}
	data.Incoming = make([]kgEdgeRow, 0, len(incoming))
	for _, e := range incoming {
		data.Incoming = append(data.Incoming, kgEdgeRow{
			ID: e.ID, FromEntity: e.FromEntity, ToEntity: e.ToEntity, Predicate: e.Predicate,
			FromName: resolveName(e.FromEntity), ToName: ent.CanonicalName,
			Confidence: e.Confidence, SourceChunks: e.SourceChunks,
		})
	}

	chunks, err := s.kgSearcher.ChunksMentioning(ctx, projectID, entityID, "", 50)
	if err != nil {
		s.logger.Warn().Err(err).Str("project_id", projectID).Str("entity_id", entityID).Msg("ui: ChunksMentioning failed")
	} else {
		data.Entity.Chunks = len(chunks)
		data.Chunks = make([]kgChunkRow, 0, len(chunks))
		for _, c := range chunks {
			preview := c.Content
			if len(preview) > 240 {
				preview = preview[:237] + "..."
			}
			data.Chunks = append(data.Chunks, kgChunkRow{
				ChunkID: c.ChunkID, SourceName: c.SourceName, Preview: preview,
				RepoScope: c.RepoScope, Surface: c.Surface,
			})
		}
	}
	s.render(w, "memory_entity_detail.html", data)
}

// kgSVGNode is a positioned node in the subgraph SVG. Positions are
// computed server-side (radial layout around the seed) so the
// template just plots circles + lines — no client layout library.
type kgSVGNode struct {
	ID     string
	Name   string
	Type   string
	X, Y   float64
	IsSeed bool
}

// kgSVGEdge is a positioned edge in the subgraph SVG.
type kgSVGEdge struct {
	X1, Y1, X2, Y2 float64
	Predicate      string
	MidX, MidY     float64
	Colour         string
	Closed         bool // closed-vocabulary predicate → coloured; else gray
}

// MemorySubgraph renders the server-side SVG subgraph viewer for a
// seed entity: /ui/memory/<project>/subgraph/<entityID>. Radial
// layout, closed-vocab edges coloured, free-form gray (LLD §7.3).
// Phone fallback (a flat predicate list) renders below the SVG.
func (s *Server) MemorySubgraph(w http.ResponseWriter, r *http.Request, projectID, entityID string) {
	if projectID == "" || entityID == "" {
		httpError(w, http.StatusBadRequest, "project id + entity id required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	hops := 1
	if hs := r.URL.Query().Get("hops"); hs != "" {
		if n, err := strconv.Atoi(hs); err == nil && n > 0 {
			hops = n
		}
	}

	data := struct {
		Title       string
		CurrentPage string
		ProjectID   string
		ProjectName string
		Enabled     bool
		Found       bool
		SeedID      string
		Hops        int
		Width       int
		Height      int
		Nodes       []kgSVGNode
		Edges       []kgSVGEdge
		// FlatEdges is the phone-fallback list: entity → predicate → entity.
		FlatEdges []kgEdgeRow
	}{
		Title:       "Subgraph — " + entityID,
		CurrentPage: "memory",
		ProjectID:   projectID,
		ProjectName: projectID,
		SeedID:      entityID,
		Hops:        hops,
		Width:       640,
		Height:      480,
	}
	if s.projectReg != nil {
		if p := s.projectReg.GetProject(projectID); p != nil && p.DisplayName != "" {
			data.ProjectName = p.DisplayName
		}
	}
	if s.kgSearcher == nil {
		s.render(w, "memory_subgraph.html", data)
		return
	}
	data.Enabled = true

	sg, err := s.kgSearcher.Subgraph(ctx, projectID, []string{entityID}, hops)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "subgraph failed: "+err.Error())
		return
	}
	if sg == nil || len(sg.Entities) == 0 {
		s.render(w, "memory_subgraph.html", data)
		return
	}
	data.Found = true
	data.Hops = sg.Hops

	nameByID := make(map[string]string, len(sg.Entities))
	posByID := make(map[string][2]float64, len(sg.Entities))
	cx, cy := float64(data.Width)/2, float64(data.Height)/2
	radius := math.Min(cx, cy) - 60

	// Radial layout: seed at centre, everyone else evenly around the
	// ring. Deterministic (entities already sorted by name) so the
	// layout is stable across reloads.
	others := make([]*persistence.KnowledgeEntity, 0, len(sg.Entities))
	for _, e := range sg.Entities {
		nameByID[e.ID] = e.CanonicalName
		if e.ID == entityID {
			posByID[e.ID] = [2]float64{cx, cy}
		} else {
			others = append(others, e)
		}
	}
	for i, e := range others {
		angle := 2 * math.Pi * float64(i) / math.Max(1, float64(len(others)))
		x := cx + radius*math.Cos(angle)
		y := cy + radius*math.Sin(angle)
		posByID[e.ID] = [2]float64{x, y}
	}
	for _, e := range sg.Entities {
		p := posByID[e.ID]
		data.Nodes = append(data.Nodes, kgSVGNode{
			ID: e.ID, Name: e.CanonicalName, Type: e.Type,
			X: p[0], Y: p[1], IsSeed: e.ID == entityID,
		})
	}
	for _, e := range sg.Edges {
		from, fok := posByID[e.FromEntity]
		to, tok := posByID[e.ToEntity]
		if !fok || !tok {
			continue // endpoint outside the rendered node set
		}
		closed := isClosedPredicate(e.Predicate)
		data.Edges = append(data.Edges, kgSVGEdge{
			X1: from[0], Y1: from[1], X2: to[0], Y2: to[1],
			MidX: (from[0] + to[0]) / 2, MidY: (from[1] + to[1]) / 2,
			Predicate: e.Predicate, Colour: predicateColour(e.Predicate, closed), Closed: closed,
		})
		data.FlatEdges = append(data.FlatEdges, kgEdgeRow{
			ID: e.ID, Predicate: e.Predicate,
			FromName: nameOr(nameByID, e.FromEntity), ToName: nameOr(nameByID, e.ToEntity),
			FromEntity: e.FromEntity, ToEntity: e.ToEntity,
		})
	}
	s.render(w, "memory_subgraph.html", data)
}

func nameOr(m map[string]string, id string) string {
	if n, ok := m[id]; ok && n != "" {
		return n
	}
	return id
}

// knowledgeEntityTypes is the closed first-level taxonomy from the
// LLD §3.4, used to populate the browser's type filter.
var knowledgeEntityTypes = []string{
	"PERSON", "ORG", "VENDOR", "PRODUCT", "DECISION", "EVENT",
	"DATE", "PRICE", "LOCATION", "TECHNOLOGY", "FACT", "OTHER",
}

// closedPredicates is the closed-vocabulary relationship set (the
// persistence.Predicate* constants, LLD §3.5). Edges on these
// predicates get a distinct colour in the subgraph SVG; everything
// else is gray.
var closedPredicates = map[string]struct{}{
	persistence.PredicateMentionedIn:  {},
	persistence.PredicateRelatesTo:    {},
	persistence.PredicateQuotedPrice:  {},
	persistence.PredicateChosenOver:   {},
	persistence.PredicateMeasuredAs:   {},
	persistence.PredicateDependsOn:    {},
	persistence.PredicateSupersededBy: {},
	persistence.PredicateLocatedAt:    {},
	persistence.PredicateOwnedBy:      {},
	persistence.PredicateHasDeadline:  {},
}

func isClosedPredicate(p string) bool {
	_, ok := closedPredicates[strings.ToUpper(p)]
	return ok
}

// predicateColour returns a stable colour for an edge by predicate.
// Free-form predicates render gray (matching the LLD §7.3 spec).
func predicateColour(predicate string, closed bool) string {
	if !closed {
		return "#6b7280" // gray-500
	}
	switch strings.ToUpper(predicate) {
	case persistence.PredicateDependsOn, persistence.PredicateRelatesTo:
		return "#34d399" // emerald-400
	case persistence.PredicateOwnedBy, persistence.PredicateChosenOver:
		return "#a78bfa" // violet-400
	case persistence.PredicateHasDeadline, persistence.PredicateQuotedPrice:
		return "#fbbf24" // amber-400
	}
	return "#60a5fa" // blue-400 (other closed-vocab)
}

// decodeJSONStringArray parses a JSONB string array (aliases) into a
// []string. Defensive: returns nil on any malformed input so the
// template renders an empty alias list rather than erroring.
func decodeJSONStringArray(raw []byte) []string {
	s := strings.TrimSpace(string(raw))
	if len(s) < 2 || s[0] != '[' || s[len(s)-1] != ']' {
		return nil
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}
