package memory

import (
	"context"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/memoryfirewall"
	"vornik.io/vornik/internal/persistence"
)

// Searcher runs hybrid (or FTS-only) memory searches scoped to a project.
type Searcher struct {
	cfg      Config
	repo     *Repository
	embedder *Embedder
	metrics  *Metrics
	// queryEmbedTimeout bounds the per-recall query-embedding call so a
	// slow/contended embedding backend can't hang interactive recall for
	// the embedder's full 60s HTTP timeout. On timeout/failure the search
	// degrades to the keyword arm (observably — see embedQueryWithTimeout).
	// Defaults to defaultQueryEmbedTimeout; set in NewSearcher.
	queryEmbedTimeout time.Duration
	auditRepo         persistence.MemoryRetrievalAuditRepository // optional; nil disables retrieval audit
	logger            zerolog.Logger
	// epochSource (optional) returns the active epoch IDs for a
	// project. When set, the search query filters chunks to those
	// in the active set OR with epoch_id IS NULL (legacy chunks
	// from before Phase 0 backfill stay searchable). Nil keeps
	// the pre-Phase-3 behaviour (no epoch filter).
	epochSource func(ctx context.Context, projectID string) ([]string, error)
	// reranker (optional) re-orders the top-K hybrid results by
	// LLM-scored relevance. Nil = NoopReranker (RRF ordering).
	// Failures inside the reranker degrade to RRF too, so the
	// surface is non-fatal end-to-end.
	reranker Reranker
	// mmrLambda controls the diversity/relevance trade-off of the
	// post-rerank MMR pass. 0 disables MMR entirely (default).
	// 0.7–0.8 is the sweet spot once enabled.
	mmrLambda float64
	// queryExpander (optional) widens the FTS query with terms drawn
	// from the knowledge graph. Nil disables expansion (default).
	queryExpander QueryExpander
	// firewall (optional) installs the Policy-Aware Memory
	// Firewall hooks. Nil = legacy behaviour. SetFirewall
	// installs after construction so the container can wire
	// it once the audit writer + evaluator are built.
	firewall *FirewallDeps
	// sufficiency governs scored-sufficiency iterative retrieval
	// (RecallSufficient). Zero value = disabled → single-shot.
	sufficiency SufficiencyConfig
}

// defaultQueryEmbedTimeout bounds the query-embedding call on the
// interactive recall path. The embedder's HTTP client allows 60s
// (fine for the ingest worker's batches), but a recall that waits
// that long is worse than useless — the operator-facing budget is
// sub-second. 5s is generous for a healthy local embedder (~0.1s
// warm) yet fails fast to keyword search when the backend is
// contended or cold, which is the 45-57s hang reported 2026-05-30.
const defaultQueryEmbedTimeout = 5 * time.Second

// NewSearcher creates a Searcher.
func NewSearcher(cfg Config, repo *Repository, embedder *Embedder) *Searcher {
	return &Searcher{
		cfg:               cfg,
		repo:              repo,
		embedder:          embedder,
		queryEmbedTimeout: defaultQueryEmbedTimeout,
		logger:            zerolog.Nop(),
	}
}

// SetAuditRepo wires the per-search retrieval audit repo. Nil
// disables the audit (preserving pre-feature behaviour for
// deployments that haven't applied the schema migration).
func (s *Searcher) SetAuditRepo(repo persistence.MemoryRetrievalAuditRepository) {
	if s != nil {
		s.auditRepo = repo
	}
}

// SetLogger wires a logger for audit-write failures. Audit failures
// are logged at warn (the search itself succeeded) and never
// propagate to the caller.
func (s *Searcher) SetLogger(l zerolog.Logger) {
	if s != nil {
		s.logger = l
	}
}

// SetEpochSource wires the active-epoch resolver. When set, search
// queries restrict chunks to the project's active epoch set (or
// epoch_id IS NULL for legacy chunks from before Phase 0). Nil
// disables — backwards-compatible default.
func (s *Searcher) SetEpochSource(fn func(ctx context.Context, projectID string) ([]string, error)) {
	if s != nil {
		s.epochSource = fn
	}
}

// SetQueryExpander wires an optional QueryExpander. When set, the
// searcher widens the keyword side of search with entity aliases /
// neighbour names from the knowledge graph before fetching. Embedding
// side uses the original query so the semantic signal isn't diluted
// by appended terms.
func (s *Searcher) SetQueryExpander(qx QueryExpander) {
	if s != nil {
		s.queryExpander = qx
	}
}

// SetMMRLambda enables MMR diversification over the post-rerank result
// set. lambda <= 0 disables (default). 0.7–0.8 typically improves
// top-K precision on prose-heavy corpora without hurting recall. The
// MMR pass runs over the already-reranked slice, then the searcher
// truncates to the caller's limit.
func (s *Searcher) SetMMRLambda(lambda float64) {
	if s != nil {
		s.mmrLambda = lambda
	}
}

// SetReranker wires an optional Reranker over hybrid search results.
// Nil-safe: a nil receiver, or nil reranker, keeps the default RRF
// ordering. The reranker itself is allowed to degrade to its input on
// failure (no errors bubble), so wiring this is non-fatal under any
// reranker outage.
func (s *Searcher) SetReranker(r Reranker) {
	if s != nil {
		s.reranker = r
	}
}

// retrievalContextKey is the Go context key for per-search caller
// attribution. Agents calling memory_search via MCP run inside an
// executor step that knows (task_id, execution_id, step_id, role);
// piping that through ctx without changing Search's signature keeps
// the chat-side and CLI-side call sites unchanged.
type retrievalContextKey struct{}

// RetrievalContext is the attribution bag the searcher writes onto
// each audit row. All fields optional — populated by callers that
// have the data, omitted by chat / CLI / programmatic searches.
type RetrievalContext struct {
	TaskID      string
	ExecutionID string
	StepID      string
	Role        string
	// ActorKind / ActorID split agent vs companion recalls (LLD 22).
	// Companion callers (the recall MCP tool, the recall_hint
	// internal call) set ActorKind="companion:<client_kind>" + ActorID=key.ID
	// — the per-client suffix (claude-code / codex / gemini-cli / …)
	// lets dashboards split recalls by client without joining
	// api_keys. Legacy keys with an empty ClientKind degrade to
	// plain "companion". Agent callers usually leave both empty —
	// the searcher then auto-populates ActorKind="agent" +
	// ActorID=Role on the audit row, preserving the indexable split
	// without each agent call site having to think about it.
	ActorKind string
	ActorID   string
}

// WithRetrievalContext stamps a RetrievalContext onto ctx. Returns
// the original ctx unchanged when rc is nil so the caller doesn't
// have to nil-check before threading.
func WithRetrievalContext(ctx context.Context, rc *RetrievalContext) context.Context {
	if rc == nil {
		return ctx
	}
	return context.WithValue(ctx, retrievalContextKey{}, rc)
}

// retrievalContextFromContext reads the bag stamped by
// WithRetrievalContext, or returns a zero value when none is set.
func retrievalContextFromContext(ctx context.Context) RetrievalContext {
	if v, ok := ctx.Value(retrievalContextKey{}).(*RetrievalContext); ok && v != nil {
		return *v
	}
	return RetrievalContext{}
}

// RetrievalContextFromContext is the exported view of the same
// reader. Lets callers in other packages (api, ui handlers, tests
// that exercise B-15's "actor stamped before search" contract)
// confirm the bag is in place without re-implementing the lookup.
// Returns a value copy so callers can't mutate the stamped struct.
func RetrievalContextFromContext(ctx context.Context) RetrievalContext {
	return retrievalContextFromContext(ctx)
}

// setMetrics attaches a Metrics instance to the Searcher.
func (s *Searcher) setMetrics(m *Metrics) { s.metrics = m }

// SearchOptions narrows a memory search beyond query+limit. Added
// in 2026.6.0 as the external-research retrofit — temporal filters
// answer "what did the assistant know last week" without dragging
// in every prior matching chunk. Zero-valued FromDate/ToDate are
// no-ops on that side; both empty means "behave like the legacy
// Search(query, limit) call".
type SearchOptions struct {
	Limit    int       // 0 falls back to the Search() default (10)
	FromDate time.Time // chunks created on/after; zero = no lower bound
	ToDate   time.Time // chunks created on/before; zero = no upper bound
	// RepoScope — migration 75. When non-empty, the SQL filters
	// chunks to those matching this scope OR repo_scope = '*' (cross-
	// cutting) OR repo_scope IS NULL (uncategorized — kept visible
	// during the transition window so legacy chunks don't vanish
	// before a bulk-retag CLI ships). Empty = no scope filter
	// (project-wide search).
	RepoScope string
	// StrictScope — when true AND RepoScope is non-empty, drops the
	// `OR repo_scope IS NULL` fallthrough so the result set really
	// contains only chunks scoped to RepoScope OR cross-cutting '*'.
	// The lenient default (false) preserves the migration-window
	// semantics: legacy NULL chunks remain visible to every scoped
	// caller until they're bulk-retagged via `vornikctl memory scope
	// retag`. Operator-facing surfaces like the /ui/memory scope
	// picker set this true so what they see matches what they
	// filtered. RecallTool defaults to false so host LLMs still
	// reach pre-migration deposits.
	StrictScope bool
	// Rerank opts this search into the LLM reranker pass (and the wider
	// candidate fetch it needs). Default false: the interactive paths
	// (memory_search tool, companion recall) stay on fast RRF ordering and
	// never pay the rerank LLM latency. Only the scored-sufficiency /
	// context-assembly path (RecallSufficient) sets it true, so reranking is
	// scoped to where the extra quality is worth the seconds. No-op unless a
	// real (non-Noop) reranker is wired.
	Rerank bool
}

// SearchWithOptions is the parameterised form of Search.
//
// 2026-05-29 default-on change: when the firewall is wired,
// results now flow through applyFirewall with an EMPTY
// RequestContext (no operator_id / role / purpose). Under
// default policies that allows everything but writes an
// audit row per result so operators see what the firewall
// would block under stricter policies. Callers that have
// operator metadata to attach should use RecallWithContext
// directly.
func (s *Searcher) SearchWithOptions(ctx context.Context, projectID, query string, opts SearchOptions) ([]SearchResult, error) {
	results, err := s.searchInternal(ctx, projectID, query, opts)
	if err != nil {
		return nil, err
	}
	return s.applyFirewall(ctx, projectID, results, memoryfirewall.RequestContext{}), nil
}

// Search executes a hybrid (or FTS-only) search for the given query scoped
// to projectID. limit controls the maximum number of results returned.
// When the embedding endpoint is not configured or unavailable, the search
// falls back to full-text search automatically. Same firewall-default-on
// semantics as SearchWithOptions.
func (s *Searcher) Search(ctx context.Context, projectID, query string, limit int) ([]SearchResult, error) {
	return s.SearchWithOptions(ctx, projectID, query, SearchOptions{Limit: limit})
}

// ListRepoScopes returns the distinct repo_scope values in a
// project's memory, with chunk counts. Thin pass-through to the
// underlying repository so callers don't have to thread the
// Repository handle separately. nil repo (e.g. memory disabled)
// returns nil, nil — same shape as Search() under that branch.
func (s *Searcher) ListRepoScopes(ctx context.Context, projectID string) ([]RepoScopeCount, error) {
	if s == nil || s.repo == nil {
		return nil, nil
	}
	return s.repo.ListRepoScopes(ctx, projectID)
}

// searchInternal is the shared implementation behind both Search
// and SearchWithOptions. The opts.FromDate/ToDate hand off to the
// repository layer where they fold into the WHERE clause.
// embedQueryWithTimeout embeds the recall query under a bounded
// deadline (queryEmbedTimeout) so a slow or contended embedding backend
// can't hang interactive recall for the embedder's full 60s HTTP
// timeout. Returns nil (degrade to keyword-only search) when the
// embedder is unconfigured/absent, or on timeout / error / empty
// result — logging the degrade at warn so it is visible rather than
// silent. The 2026-05-30 "recall hangs ~50s then returns ~nothing"
// report traced to the previous inline call passing the full request
// context and discarding the result, so a contended-backend timeout
// was both slow and invisible.
func (s *Searcher) embedQueryWithTimeout(ctx context.Context, projectID, query string) []float32 {
	if s.cfg.EmbeddingEndpoint == "" || s.embedder == nil {
		return nil
	}
	timeout := s.queryEmbedTimeout
	if timeout <= 0 {
		timeout = defaultQueryEmbedTimeout
	}
	embedCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	vecs, err := s.embedder.Embed(embedCtx, []string{query})
	if err == nil && len(vecs) > 0 && len(vecs[0]) > 0 {
		return vecs[0]
	}
	// Embed() returns (nil, nil) on a non-fatal degrade (network error,
	// non-200, timeout), so an empty result — with or without err — is
	// the degrade signal. Make it observable; the caller continues on
	// the keyword arm.
	ev := s.logger.Warn().Str("project_id", projectID).Dur("waited", time.Since(start))
	if err != nil {
		ev = ev.Err(err)
	}
	if embedCtx.Err() != nil {
		ev = ev.Str("cause", "query_embed_timeout").Dur("timeout", timeout)
	}
	ev.Msg("memory: query embedding unavailable; degrading recall to keyword-only")
	return nil
}

func (s *Searcher) searchInternal(ctx context.Context, projectID, query string, opts SearchOptions) ([]SearchResult, error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = 10
	}

	// Attempt to embed the query for semantic search. The embedder
	// always sees the user's original query — adding expansion terms
	// would dilute the semantic signal. Bounded by queryEmbedTimeout
	// so a slow/contended backend degrades to keyword fast, observably.
	queryVec := s.embedQueryWithTimeout(ctx, projectID, query)

	// Keyword-side expansion: walk the knowledge graph to add aliases
	// + one-hop neighbour names to the FTS query. Embedding side is
	// kept unchanged above so this never poisons semantic ranking.
	keywordQuery := query
	if s.queryExpander != nil {
		expansion := s.queryExpander.Expand(ctx, projectID, query)
		keywordQuery = mergeExpansionIntoQuery(query, expansion)
	}

	mode := "keyword"
	if len(queryVec) > 0 {
		mode = "hybrid"
	}

	// Resolve the active epoch set. Empty slice = no active
	// epochs yet → search reads only legacy (epoch_id IS NULL)
	// chunks, preserving today's behaviour for projects that
	// haven't run a pipeline ingest yet. Nil epochSource disables
	// the filter entirely.
	var activeEpochs []string
	if s.epochSource != nil {
		ae, eerr := s.epochSource(ctx, projectID)
		if eerr != nil {
			s.logger.Warn().Err(eerr).
				Str("project_id", projectID).
				Msg("memory: epoch source failed; degrading to no-filter search")
		} else {
			activeEpochs = ae
		}
	}

	start := time.Now()
	// Fetch a wider candidate set when a reranker is wired so the
	// rerank+truncate pass has room to re-order. Without that the
	// reranker only sees `limit` candidates and can't promote a
	// strong result that RRF ranked 11th into the top-10. 3× is
	// the cheapest setting that still gives the rerank meaningful
	// freedom; tunable later via Config if it shows up in latency.
	// Reranking is opt-in per search (opts.Rerank) and scoped to a real
	// reranker — the interactive paths leave it off and stay on fast RRF.
	rerankOn := opts.Rerank && s.rerankerActive()
	fetchLimit := limit
	if rerankOn {
		fetchLimit = limit * 3
		if fetchLimit > 60 {
			fetchLimit = 60
		}
	}
	results, err := s.repo.HybridSearchWithEpochs(ctx, projectID, queryVec, keywordQuery, fetchLimit, activeEpochs, s.epochSource != nil, opts.FromDate, opts.ToDate, opts.RepoScope, opts.StrictScope)
	// Role-based class boost: re-orders by per-role class affinity
	// before the reranker sees the list. The reranker can still
	// overrule the order, but the boost guarantees class-relevant
	// chunks aren't truncated out of the rerank window when the
	// candidate set is wider than the rerank head.
	if err == nil && len(results) > 1 {
		rc := retrievalContextFromContext(ctx)
		if rc.Role != "" {
			results = applyRoleClassBoost(results, rc.Role, 0)
		}
	}
	if err == nil && rerankOn && len(results) > 1 {
		rerankStart := time.Now()
		reordered, rerr := s.reranker.Rerank(ctx, query, results)
		if rerr != nil {
			s.logger.Warn().Err(rerr).
				Str("project_id", projectID).
				Msg("memory: reranker error — keeping RRF order")
		} else {
			results = reordered
		}
		if s.metrics != nil {
			s.metrics.SearchRerankDuration.Observe(time.Since(rerankStart).Seconds())
		}
	}
	// MMR diversification — cheap pure-Go pass over whatever ordering
	// we have (RRF or reranked). Disabled when lambda is 0 (default),
	// or when there are fewer than 3 results (nothing to diversify).
	if err == nil && s.mmrLambda > 0 && len(results) >= 3 {
		results = applyMMR(results, s.mmrLambda)
	}
	// Migration 75 safety net: HybridSearchWithEpochs SQL applies the
	// repo_scope filter when the epoch path is active, but the
	// non-epoch + pgvector-unavailable fallbacks below it do not.
	// Filter post-fetch so every code path honours the caller's scope.
	// Cheap — the candidate set is already truncated to fetchLimit.
	if err == nil && opts.RepoScope != "" && len(results) > 0 {
		filtered := results[:0]
		for _, r := range results {
			// repo_scope from the chunk row is returned via the
			// future SearchResult.RepoScope field (B-3 follow-on
			// when scanSearchResults grows the column). Until then
			// the post-search filter would have nothing to read,
			// so the safety net is a no-op for the fallback paths
			// — the main path's SQL filter is the contract.
			_ = r
			filtered = append(filtered, r)
		}
		results = filtered
	}

	// Truncate post-rerank so the caller sees `limit` results.
	if len(results) > limit {
		results = results[:limit]
	}
	if s.metrics != nil {
		s.metrics.SearchesTotal.WithLabelValues(projectID, mode).Inc()
		s.metrics.SearchDuration.WithLabelValues(mode).Observe(time.Since(start).Seconds())
		if err == nil {
			s.metrics.SearchResultsTotal.WithLabelValues(projectID, mode).Add(float64(len(results)))
		}
	}

	// Best-effort retrieval audit: one row per Search call. Failures
	// are logged but never bubble — the search itself succeeded and
	// the audit is a downstream analytics channel, not a correctness
	// dependency. Skipped on errored searches: those produce no
	// chunks, and an audit row with empty chunk_ids would skew the
	// "TotalSearches" metric without representing useful retrieval.
	if err == nil && s.auditRepo != nil {
		rc := retrievalContextFromContext(ctx)
		chunkIDs := make([]string, 0, len(results))
		for _, r := range results {
			chunkIDs = append(chunkIDs, r.ChunkID)
		}
		audit := &persistence.MemoryRetrievalAudit{
			ProjectID: projectID,
			Query:     query,
			ChunkIDs:  chunkIDs,
		}
		if rc.TaskID != "" {
			tid := rc.TaskID
			audit.TaskID = &tid
		}
		if rc.ExecutionID != "" {
			eid := rc.ExecutionID
			audit.ExecutionID = &eid
		}
		if rc.StepID != "" {
			sid := rc.StepID
			audit.StepID = &sid
		}
		if rc.Role != "" {
			role := rc.Role
			audit.Role = &role
		}
		// Actor split (LLD 22). Companion paths set both fields; the
		// agent path stamps Role but typically not ActorKind, so we
		// derive "agent"/role for backwards-compat on agent calls and
		// only override when the caller is explicit.
		actorKind := rc.ActorKind
		actorID := rc.ActorID
		if actorKind == "" && rc.Role != "" {
			actorKind = "agent"
			if actorID == "" {
				actorID = rc.Role
			}
		}
		if actorKind != "" {
			ak := actorKind
			audit.ActorKind = &ak
		}
		if actorID != "" {
			aid := actorID
			audit.ActorID = &aid
		}
		// Migration 75: record which scope the caller searched so
		// dashboards can report "what scope is being recalled most"
		// + the feedback loop is scope-aware.
		if opts.RepoScope != "" {
			rs := opts.RepoScope
			audit.RepoScope = &rs
		}
		if werr := s.auditRepo.Record(ctx, audit); werr != nil {
			s.logger.Warn().
				Err(werr).
				Str("project_id", projectID).
				Int("results", len(results)).
				Msg("memory: retrieval audit write failed (search itself succeeded)")
		}
	}
	return results, err
}
