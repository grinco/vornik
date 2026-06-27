package service

import (
	"context"
	"strings"
	"time"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/memoryfirewall"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/ui"
)

// memorySearchAdapter wraps memory.Searcher so it satisfies api.MemorySearcher.
type memorySearchAdapter struct {
	s *memory.Searcher
}

// newMemorySearchAdapter creates a new adapter.
func newMemorySearchAdapter(s *memory.Searcher) api.MemorySearcher {
	return &memorySearchAdapter{s: s}
}

// memoryCompanionAdapter composes memory.Searcher + memory.Pipeline +
// memory.Repository to satisfy api.MemoryCompanionAdapter (LLD 22).
// The companion MCP server's `recall` / `remember` / `recent_memory`
// tools call through this adapter; recall delegates to
// Searcher.Search; remember goes through Pipeline.IngestCompanionNote
// (gates); recent_memory uses Repository.ListRecentChunks.
type memoryCompanionAdapter struct {
	s    *memory.Searcher
	p    *memory.Pipeline
	repo *memory.Repository
}

// newMemoryCompanionAdapter wires the companion adapter. All three
// dependencies are required — a half-wired adapter would silently
// degrade one of the tools at runtime, so the constructor returns
// nil when any is missing and the daemon's container_http.go skips
// api.WithMemoryCompanionAdapter entirely.
func newMemoryCompanionAdapter(s *memory.Searcher, p *memory.Pipeline, repo *memory.Repository) api.MemoryCompanionAdapter {
	if s == nil || p == nil || repo == nil {
		return nil
	}
	return &memoryCompanionAdapter{s: s, p: p, repo: repo}
}

// Recall delegates to memory.Searcher.Search. v1 ignores
// FromDate/ToDate from RecallOptions — the simple Search() variant
// is sufficient for the initial tool surface; pushing the temporal
// filter into SQL is a follow-up. ActorKind/ActorID are accepted but
// not currently propagated into the retrieval-audit row (Searcher's
// existing audit-write path doesn't take an actor parameter yet);
// the LLD 22 migration adds the columns and the next slice fills
// them.
func (a *memoryCompanionAdapter) Recall(ctx context.Context, projectID, query string, opts api.RecallOptions) ([]api.MemorySearchResult, error) {
	if a == nil || a.s == nil {
		return nil, nil
	}
	// LLD 22: stamp actor metadata onto the context so Searcher's
	// audit-row construction can split agent vs companion recalls
	// without changing Search()'s signature. Empty actor strings
	// fall back to the agent-side default ("agent" + Role), so
	// non-companion callers see no behaviour change.
	if opts.ActorKind != "" || opts.ActorID != "" {
		ctx = memory.WithRetrievalContext(ctx, &memory.RetrievalContext{
			ActorKind: opts.ActorKind,
			ActorID:   opts.ActorID,
		})
	}
	// Call RecallWithContext (firewall-aware path). The companion
	// key carries the most actor metadata of any caller: ClientKind
	// (claude-code / gemini-code-cli / etc.) maps to the firewall's
	// Role; KeyID maps to OperatorID. The 2026-05-29 firewall
	// default-on routing means legacy SearchWithOptions ALSO flows
	// through the firewall, but RecallWithContext lets us attach
	// dispatcher metadata to the audit row.
	searchOpts := memory.SearchOptions{
		Limit:       opts.Limit,
		FromDate:    opts.FromDate,
		ToDate:      opts.ToDate,
		RepoScope:   opts.RepoScope,
		StrictScope: opts.StrictScope,
	}
	reqCtx := memoryfirewall.RequestContext{
		Role:       opts.ActorKind, // "companion:claude-code" etc.
		OperatorID: opts.ActorID,   // the companion API-key id
		Purpose:    memoryfirewall.PurposeOperational,
	}
	// Non-interactive context-assembly callers (the pre-delegation recall
	// hint) set Sufficient to route through scored-sufficiency + reranking;
	// everyone else stays on the fast single-shot RRF path.
	var results []memory.SearchResult
	var err error
	if opts.Sufficient {
		results, err = a.s.RecallSufficient(ctx, projectID, query, searchOpts, reqCtx)
	} else {
		results, err = a.s.RecallWithContext(ctx, projectID, query, searchOpts, reqCtx)
	}
	if err != nil {
		return nil, err
	}
	out := make([]api.MemorySearchResult, len(results))
	for i, r := range results {
		out[i] = api.MemorySearchResult{
			ChunkID:      r.ChunkID,
			ProjectID:    r.ProjectID,
			TaskID:       r.TaskID,
			SourceName:   r.SourceName,
			Content:      r.Content,
			Score:        r.Score,
			RepoScope:    r.RepoScope,
			ContentClass: r.ContentClass,
		}
		if r.IsAlive != nil {
			b := *r.IsAlive
			out[i].IsAlive = &b
		}
		if r.LastCheckedAt != nil {
			s := r.LastCheckedAt.UTC().Format(time.RFC3339)
			out[i].LastCheckedAt = &s
		}
	}
	return out, nil
}

// RecentMemory delegates to memory.Repository.ListRecentChunks and
// translates the row shape across the api/memory boundary. The
// repoScope arg, when non-empty, restricts the digest to chunks
// matching scope OR '*' OR NULL (migration 75 default). When
// strictScope is true AND repoScope is non-empty, the NULL-scope
// fallthrough is dropped so the digest contains only properly-
// scoped chunks (2026-05-28 investigation closure).
//
// IngestStatus on each entry is derived from the row's embedding
// presence + content_class: "ready" when both are present;
// "pending_embedding" when no embedding yet; "pending_classification"
// when embedding present but class is empty / "unclassified".
func (a *memoryCompanionAdapter) RecentMemory(ctx context.Context, projectID string, limit int, repoScope string, strictScope, onlyUntagged bool) ([]api.RecentMemoryEntry, error) {
	if a == nil || a.s == nil {
		return nil, nil
	}
	// SECURITY: route the digest through the firewall, mirroring
	// Recall. Calling ListRecentChunksWithOptions directly was a
	// Policy-Aware Memory Firewall bypass — a chunk recall drops
	// under enforce (credentials / refuted validation_status /
	// expired firewall policy) still surfaced verbatim here.
	// Searcher.RecentWithContext reuses the same applyFirewall
	// machinery (LoadChunkPolicies → resolveMode → Evaluator.Decide
	// → audit Enqueue → enforce-drop / advisory-annotate).
	//
	// Build the RequestContext the same way Recall does: the
	// companion actor metadata is stamped on ctx by the handler via
	// RetrievalContext (ActorKind="companion:<client_kind>",
	// ActorID=key.ID); fall back to PurposeOperational so the
	// firewall decision matches recall's purpose even when the ctx
	// carries no actor bag.
	reqCtx := memoryfirewall.RequestContext{Purpose: memoryfirewall.PurposeOperational}
	if rc := memory.RetrievalContextFromContext(ctx); rc.ActorKind != "" {
		reqCtx.Role = rc.ActorKind
		reqCtx.OperatorID = rc.ActorID
	}
	rows, err := a.s.RecentWithContext(ctx, projectID, limit, repoScope, strictScope, onlyUntagged, reqCtx)
	if err != nil {
		return nil, err
	}
	out := make([]api.RecentMemoryEntry, len(rows))
	for i, r := range rows {
		out[i] = api.RecentMemoryEntry{
			ChunkID:       r.ChunkID,
			TaskID:        r.TaskID,
			SourceName:    r.SourceName,
			ContentClass:  r.ContentClass,
			Content:       r.Content,
			CreatedAt:     r.CreatedAt.UTC().Format(time.RFC3339),
			RepoScope:     r.RepoScope,
			IngestStatus:  deriveIngestStatus(r.HasEmbedding, r.ContentClass),
			PolicyWarning: r.PolicyWarning,
		}
	}
	return out, nil
}

// ListRepoScopes is a thin pass-through to the memory repo's
// scope-inventory query. NULL-scoped chunks are emitted under the
// empty-string bucket so the operator's client sees the leak surface.
func (a *memoryCompanionAdapter) ListRepoScopes(ctx context.Context, projectID string) ([]api.RepoScopeCount, error) {
	if a == nil || a.repo == nil {
		return nil, nil
	}
	rows, err := a.repo.ListRepoScopes(ctx, projectID)
	if err != nil {
		return nil, err
	}
	out := make([]api.RepoScopeCount, len(rows))
	for i, r := range rows {
		out[i] = api.RepoScopeCount{Scope: r.Scope, Chunks: r.Chunks}
	}
	return out, nil
}

// deriveIngestStatus maps (embedding-present, content_class) onto
// the three IngestStatus constants. Centralised so the test that
// pins the contract has a single function to exercise.
func deriveIngestStatus(hasEmbedding bool, contentClass string) string {
	if !hasEmbedding {
		return api.IngestStatusPendingEmbedding
	}
	if contentClass == "" || contentClass == "unclassified" {
		return api.IngestStatusPendingClassification
	}
	return api.IngestStatusReady
}

// Remember runs the companion deposit through the gate pipeline and
// maps the resulting IngestStats to the api-layer RememberResult.
// "Decision" is the dominant outcome — a single deposit produces one
// candidate, so exactly one of Admitted/Quarantined/Rejected is
// non-zero.
func (a *memoryCompanionAdapter) Remember(ctx context.Context, in api.RememberInput) (api.RememberResult, error) {
	if a == nil || a.p == nil {
		return api.RememberResult{}, nil
	}
	res, err := a.p.IngestCompanionNote(
		ctx,
		in.ProjectID, in.ClientKind, in.KeyID,
		in.SourceName, in.Content,
		memory.ContentClass(in.Class),
		in.TTLDays,
		in.RepoScope,
	)
	if err != nil {
		return api.RememberResult{}, err
	}
	decision := "REJECTED"
	switch {
	case res.Stats.Admitted > 0:
		decision = "ALLOW"
	case res.Stats.Quarantined > 0:
		decision = "QUARANTINED"
	}
	return api.RememberResult{
		Decision:    decision,
		ArtifactID:  res.ArtifactID,
		Admitted:    res.Stats.Admitted,
		Quarantined: res.Stats.Quarantined,
		Rejected:    res.Stats.Rejected,
		GatesFailed: res.Stats.GatesFailed,
	}, nil
}

// Correct soft-refutes the chunks best matching in.WrongClaim and,
// when in.Correction is non-empty, deposits it as an authoritative
// verified chunk. Backs the companion `memory_correct` MCP tool. The
// Corrector is built per-call from the adapter's repo + searcher (the
// same wiring container_*.go uses for the dispatcher/UI surfaces) so
// the adapter doesn't need to carry an extra field. RefuteByClaim and
// InsertCorrection both pin the project-scope guard internally.
func (a *memoryCompanionAdapter) Correct(ctx context.Context, in api.CorrectInput) (api.CorrectResult, error) {
	if a == nil || a.repo == nil || a.s == nil {
		return api.CorrectResult{}, nil
	}
	corrector := memory.NewCorrector(a.repo, a.s)
	var out api.CorrectResult
	if len(in.ChunkIDs) > 0 {
		// Surgical by-id refute — flips exactly the named chunks,
		// never the claim-search top matches (which may be the
		// authoritative corrections).
		n, err := corrector.RefuteByIDs(ctx, in.ProjectID, in.ChunkIDs)
		if err != nil {
			return api.CorrectResult{}, err
		}
		out.ByID = true
		out.RefutedCount = n
		out.Refuted = make([]api.RefutedChunkInfo, 0, len(in.ChunkIDs))
		for _, id := range in.ChunkIDs {
			out.Refuted = append(out.Refuted, api.RefutedChunkInfo{ChunkID: id})
		}
	} else {
		refuted, err := corrector.RefuteByClaim(ctx, in.ProjectID, in.WrongClaim, in.MaxRefutes)
		if err != nil {
			return api.CorrectResult{}, err
		}
		out.Refuted = make([]api.RefutedChunkInfo, 0, len(refuted))
		for _, r := range refuted {
			out.Refuted = append(out.Refuted, api.RefutedChunkInfo{
				ChunkID:    r.ID,
				SourceName: r.SourceName,
				Preview:    r.Preview,
				Score:      r.Score,
			})
		}
	}
	if in.Correction != "" {
		id, ierr := corrector.InsertCorrection(ctx, in.ProjectID, in.Correction, in.RepoScope)
		if ierr != nil {
			// The refute already landed; surface the insert failure but
			// don't discard the refute result — return what we have.
			return out, ierr
		}
		out.CorrectionChunkID = id
	}
	return out, nil
}

// extractedDocumentIndexerAdapter wraps memory.Indexer so it
// satisfies api.ExtractedDocumentIndexer. The adapter translates
// api.ExtractedSectionInput to memory.ExtractedSection — separate
// shapes at the boundary so the api package doesn't import memory.
type extractedDocumentIndexerAdapter struct {
	idx *memory.Indexer
}

func newExtractedDocumentIndexerAdapter(idx *memory.Indexer) api.ExtractedDocumentIndexer {
	if idx == nil {
		return nil
	}
	return &extractedDocumentIndexerAdapter{idx: idx}
}

// IngestExtractedSections translates the api-layer section shape
// into the memory-layer shape and forwards to memory.Indexer.
// Returns (chunksIngested, err) verbatim.
func (a *extractedDocumentIndexerAdapter) IngestExtractedSections(
	ctx context.Context,
	projectID, taskID, sourceArtifactID, extractedDocumentID string,
	sections []api.ExtractedSectionInput,
) (int, error) {
	if a == nil || a.idx == nil {
		return 0, nil
	}
	mem := make([]memory.ExtractedSection, 0, len(sections))
	for _, s := range sections {
		mem = append(mem, memory.ExtractedSection{
			SectionID:  s.SectionID,
			SourceName: s.SourceName,
			Content:    s.Content,
		})
	}
	return a.idx.IngestExtractedSections(ctx, projectID, taskID, sourceArtifactID, extractedDocumentID, mem)
}

// memoryFirewallEditorAdapter satisfies api.MemoryFirewallEditor.
// Translates the api package's wire-shape ChunkPolicyRow into
// the memory package's struct so the handler can edit per-chunk
// policy without dragging the memory package into the api
// import graph.
type memoryFirewallEditorAdapter struct {
	repo *memory.Repository
}

func newMemoryFirewallEditorAdapter(r *memory.Repository) api.MemoryFirewallEditor {
	if r == nil {
		return nil
	}
	return &memoryFirewallEditorAdapter{repo: r}
}

func (a *memoryFirewallEditorAdapter) LoadChunkPolicies(ctx context.Context, chunkIDs []string) (map[string]api.ChunkPolicyRow, error) {
	if a == nil || a.repo == nil {
		return nil, nil
	}
	internal, err := a.repo.LoadChunkPolicies(ctx, chunkIDs)
	if err != nil {
		return nil, err
	}
	out := make(map[string]api.ChunkPolicyRow, len(internal))
	for id, r := range internal {
		out[id] = chunkPolicyRowMemoryToAPI(r)
	}
	return out, nil
}

func (a *memoryFirewallEditorAdapter) UpdateChunkPolicy(ctx context.Context, row api.ChunkPolicyRow) (int64, error) {
	if a == nil || a.repo == nil {
		return 0, nil
	}
	return a.repo.UpdateChunkPolicy(ctx, chunkPolicyRowAPIToMemory(row))
}

func chunkPolicyRowMemoryToAPI(r memory.ChunkPolicyRow) api.ChunkPolicyRow {
	return api.ChunkPolicyRow{
		ChunkID:            r.ChunkID,
		TenantID:           r.TenantID,
		SensitivityTier:    r.SensitivityTier,
		ProvenanceSource:   r.ProvenanceSource,
		ProvenanceProducer: r.ProvenanceProducer,
		ProvenanceTrust:    r.ProvenanceTrust,
		ProvenanceURL:      r.ProvenanceURL,
		FirewallExpiresAt:  r.FirewallExpiresAt,
		PermittedRoles:     r.PermittedRoles,
		AllowedPurposes:    r.AllowedPurposes,
		PolicyDigest:       r.PolicyDigest,
		ContentClass:       r.ContentClass,
		ValidationStatus:   r.ValidationStatus,
	}
}

func chunkPolicyRowAPIToMemory(r api.ChunkPolicyRow) memory.ChunkPolicyRow {
	return memory.ChunkPolicyRow{
		ChunkID:            r.ChunkID,
		TenantID:           r.TenantID,
		SensitivityTier:    r.SensitivityTier,
		ProvenanceSource:   r.ProvenanceSource,
		ProvenanceProducer: r.ProvenanceProducer,
		ProvenanceTrust:    r.ProvenanceTrust,
		ProvenanceURL:      r.ProvenanceURL,
		FirewallExpiresAt:  r.FirewallExpiresAt,
		PermittedRoles:     r.PermittedRoles,
		AllowedPurposes:    r.AllowedPurposes,
		PolicyDigest:       r.PolicyDigest,
		ContentClass:       r.ContentClass,
		ValidationStatus:   r.ValidationStatus,
	}
}

// uiFirewallEditorAdapter satisfies ui.FirewallEditor by
// delegating to the same memory.Repository the api adapter uses.
// Separate ui-package interface keeps the UI free of an
// internal/api dependency.
type uiFirewallEditorAdapter struct {
	repo *memory.Repository
}

func newUIFirewallEditorAdapter(r *memory.Repository) ui.FirewallEditor {
	if r == nil {
		return nil
	}
	return &uiFirewallEditorAdapter{repo: r}
}

func (a *uiFirewallEditorAdapter) LoadChunkPolicies(ctx context.Context, chunkIDs []string) (map[string]ui.FirewallChunkRow, error) {
	if a == nil || a.repo == nil {
		return nil, nil
	}
	internal, err := a.repo.LoadChunkPolicies(ctx, chunkIDs)
	if err != nil {
		return nil, err
	}
	out := make(map[string]ui.FirewallChunkRow, len(internal))
	for id, r := range internal {
		out[id] = ui.FirewallChunkRow{
			ChunkID:            r.ChunkID,
			TenantID:           r.TenantID,
			SensitivityTier:    r.SensitivityTier,
			ProvenanceSource:   r.ProvenanceSource,
			ProvenanceProducer: r.ProvenanceProducer,
			ProvenanceTrust:    r.ProvenanceTrust,
			ProvenanceURL:      r.ProvenanceURL,
			FirewallExpiresAt:  r.FirewallExpiresAt,
			PermittedRoles:     r.PermittedRoles,
			AllowedPurposes:    r.AllowedPurposes,
			PolicyDigest:       r.PolicyDigest,
			ContentClass:       r.ContentClass,
			ValidationStatus:   r.ValidationStatus,
		}
	}
	return out, nil
}

// Search delegates to the underlying memory.Searcher and converts the result
// type from memory.SearchResult to api.MemorySearchResult.
//
// Reads any RetrievalContext stamped on ctx (the REST handler
// sets ActorKind=rest_api + ActorID=apikey_id at memory_handlers.go)
// and threads it into the firewall's RequestContext so the
// audit row records the API-key caller. Phase B's default-on
// firewall routing covers the case where the ctx doesn't carry
// the metadata — the audit row still lands, just without
// per-caller attribution.
func (a *memorySearchAdapter) Search(ctx context.Context, projectID, query string, limit int) ([]api.MemorySearchResult, error) {
	reqCtx := memoryfirewall.RequestContext{Purpose: memoryfirewall.PurposeOperational}
	if rc := memory.RetrievalContextFromContext(ctx); rc.ActorKind != "" {
		reqCtx.Role = rc.ActorKind
		reqCtx.OperatorID = rc.ActorID
	}
	results, err := a.s.RecallWithContext(ctx, projectID, query, memory.SearchOptions{Limit: limit}, reqCtx)
	if err != nil {
		return nil, err
	}
	out := make([]api.MemorySearchResult, len(results))
	for i, r := range results {
		out[i] = api.MemorySearchResult{
			ChunkID:    r.ChunkID,
			ProjectID:  r.ProjectID,
			TaskID:     r.TaskID,
			SourceName: r.SourceName,
			Content:    r.Content,
			Score:      r.Score,
		}
		// URL liveness flags. nil-passthrough preserves the
		// "never checked" signal; consuming agents distinguish
		// nil (no signal) from *false (confirmed dead).
		if r.IsAlive != nil {
			b := *r.IsAlive
			out[i].IsAlive = &b
		}
		if r.LastCheckedAt != nil {
			s := r.LastCheckedAt.UTC().Format(time.RFC3339)
			out[i].LastCheckedAt = &s
		}
	}
	return out, nil
}

// uiMemorySearchAdapter wraps memory.Searcher so it satisfies
// ui.MemorySearcher. Distinct from memorySearchAdapter because the
// result type lives in the ui package (mirrored at the package
// boundary to keep ui free of memory-package imports).
type uiMemorySearchAdapter struct {
	s *memory.Searcher
}

func newUIMemorySearchAdapter(s *memory.Searcher) ui.MemorySearcher {
	return &uiMemorySearchAdapter{s: s}
}

func (a *uiMemorySearchAdapter) Search(ctx context.Context, projectID, query string, limit int) ([]ui.MemorySearchResult, error) {
	return a.SearchWithScope(ctx, projectID, query, limit, "")
}

func (a *uiMemorySearchAdapter) SearchWithScope(ctx context.Context, projectID, query string, limit int, repoScope string) ([]ui.MemorySearchResult, error) {
	// Strict scope when the operator picked a scope from the
	// /ui/memory dropdown: the picker promises "I'm filtering to
	// X" and lenient mode (the legacy NULL leak-through) makes
	// that claim a lie. Empty repoScope short-circuits to
	// project-wide search so strict has no effect there.
	//
	// Reads RetrievalContext stamped by the UI handler
	// (internal/ui/memory.go uses ActorKind="ui"+ActorID=sessionID
	// pattern) and threads it into the firewall RequestContext.
	reqCtx := memoryfirewall.RequestContext{Purpose: memoryfirewall.PurposeOperational}
	if rc := memory.RetrievalContextFromContext(ctx); rc.ActorKind != "" {
		reqCtx.Role = rc.ActorKind
		reqCtx.OperatorID = rc.ActorID
	}
	results, err := a.s.RecallWithContext(ctx, projectID, query, memory.SearchOptions{
		Limit:       limit,
		RepoScope:   repoScope,
		StrictScope: repoScope != "",
	}, reqCtx)
	if err != nil {
		return nil, err
	}
	out := make([]ui.MemorySearchResult, len(results))
	for i, r := range results {
		out[i] = ui.MemorySearchResult{
			ChunkID:    r.ChunkID,
			ProjectID:  r.ProjectID,
			TaskID:     r.TaskID,
			SourceName: r.SourceName,
			Content:    r.Content,
			Score:      r.Score,
			RepoScope:  r.RepoScope,
		}
	}
	return out, nil
}

func (a *uiMemorySearchAdapter) ListRepoScopes(ctx context.Context, projectID string) ([]ui.MemoryRepoScope, error) {
	if a.s == nil {
		return nil, nil
	}
	rows, err := a.s.ListRepoScopes(ctx, projectID)
	if err != nil {
		return nil, err
	}
	out := make([]ui.MemoryRepoScope, len(rows))
	for i, r := range rows {
		out[i] = ui.MemoryRepoScope{Scope: r.Scope, Chunks: r.Chunks}
	}
	return out, nil
}

// memoryStatsAdapter wraps memory.Manager.Stats so it satisfies
// api.MemoryStatsProvider. Kept separate from the search adapter so
// the API surface stays narrow (stats endpoint doesn't depend on the
// embedder being healthy, only on the DB).
type memoryStatsAdapter struct {
	m *memory.Manager
}

// newMemoryStatsAdapter creates a new adapter.
func newMemoryStatsAdapter(m *memory.Manager) api.MemoryStatsProvider {
	return &memoryStatsAdapter{m: m}
}

// memoryCacheStatsAdapter satisfies api.MemoryCacheStatsProvider
// by pulling stats off the manager's Embedder.Cache (Phase D) +
// ResponseCache (Phase E). Both fields nil-safe — Enabled=false
// on the result signals "feature disabled" to the CLI vs an empty
// table.
type memoryCacheStatsAdapter struct {
	m *memory.Manager
}

func newMemoryCacheStatsAdapter(m *memory.Manager) api.MemoryCacheStatsProvider {
	return &memoryCacheStatsAdapter{m: m}
}

func (a *memoryCacheStatsAdapter) EmbeddingCacheStats(ctx context.Context) (api.EmbeddingCacheStatsResult, error) {
	if a == nil || a.m == nil || a.m.Embedder == nil || a.m.Embedder.Cache == nil {
		return api.EmbeddingCacheStatsResult{Enabled: false}, nil
	}
	src, ok := a.m.Embedder.Cache.(interface {
		CacheStats(ctx context.Context) (memory.EmbeddingCacheStats, error)
	})
	if !ok {
		return api.EmbeddingCacheStatsResult{Enabled: false}, nil
	}
	s, err := src.CacheStats(ctx)
	if err != nil {
		return api.EmbeddingCacheStatsResult{Enabled: true}, err
	}
	return api.EmbeddingCacheStatsResult{
		Enabled:        true,
		RowCount:       s.RowCount,
		ApproxBytes:    s.ApproxBytes,
		DistinctModels: s.DistinctModels,
	}, nil
}

func (a *memoryCacheStatsAdapter) ResponseCacheStats(ctx context.Context) (api.ResponseCacheStatsResult, error) {
	if a == nil || a.m == nil || a.m.ResponseCache == nil {
		return api.ResponseCacheStatsResult{Enabled: false}, nil
	}
	src, ok := a.m.ResponseCache.(interface {
		CacheStats(ctx context.Context) (memory.ResponseCacheStats, error)
	})
	if !ok {
		return api.ResponseCacheStatsResult{Enabled: false}, nil
	}
	s, err := src.CacheStats(ctx)
	if err != nil {
		return api.ResponseCacheStatsResult{Enabled: true}, err
	}
	return api.ResponseCacheStatsResult{
		Enabled:          true,
		RowCount:         s.RowCount,
		ApproxBytes:      s.ApproxBytes,
		DistinctPurposes: s.DistinctPurposes,
		TotalHits:        s.TotalHits,
		TotalSavingsUSD:  s.TotalSavingsUSD,
	}, nil
}

// vectorVizAdapter wraps memory.VizSource so it satisfies the
// ui.VectorVizSource interface. The interface lives in the UI
// package so the UI's tests don't pull memory's pgvector wiring.
type vectorVizAdapter struct {
	src *memory.VizSource
}

func newVectorVizAdapter(src *memory.VizSource) ui.VectorVizSource {
	return &vectorVizAdapter{src: src}
}

// embeddingCacheStatsAdapter wraps the memory.EmbedCache impl's
// CacheStats method to satisfy ui.EmbeddingCacheStatsSource. The
// memory package's NewEmbeddingCache returns the EmbedCache
// interface; CacheStats is on the concrete *embeddingCacheRepo,
// so we type-assert + degrade to nil when the cache wasn't wired
// (the daemon may have memory enabled without postgres-backed
// caching).
type embeddingCacheStatsAdapter struct {
	src interface {
		CacheStats(ctx context.Context) (memory.EmbeddingCacheStats, error)
	}
}

// newEmbeddingCacheStatsAdapter returns nil when the EmbedCache
// doesn't expose CacheStats (e.g. an in-memory test fake or a
// disabled cache). nil-safe at the WithEmbeddingCacheStatsSource
// boundary — the UI panel renders the disabled placeholder.
func newEmbeddingCacheStatsAdapter(ec memory.EmbedCache) ui.EmbeddingCacheStatsSource {
	if ec == nil {
		return nil
	}
	src, ok := ec.(interface {
		CacheStats(ctx context.Context) (memory.EmbeddingCacheStats, error)
	})
	if !ok {
		return nil
	}
	return &embeddingCacheStatsAdapter{src: src}
}

func (a *embeddingCacheStatsAdapter) CacheStats(ctx context.Context) (ui.EmbeddingCacheStats, error) {
	if a == nil || a.src == nil {
		return ui.EmbeddingCacheStats{}, nil
	}
	s, err := a.src.CacheStats(ctx)
	if err != nil {
		return ui.EmbeddingCacheStats{}, err
	}
	return ui.EmbeddingCacheStats{
		RowCount:       s.RowCount,
		ApproxBytes:    s.ApproxBytes,
		DistinctModels: s.DistinctModels,
	}, nil
}

// responseCacheStatsAdapter mirrors embeddingCacheStatsAdapter for
// the Phase E llm_response_cache table. Same shape: type-assert
// onto the CacheStats method, degrade to nil when the cache wasn't
// wired so the UI panel renders the disabled placeholder.
type responseCacheStatsAdapter struct {
	src interface {
		CacheStats(ctx context.Context) (memory.ResponseCacheStats, error)
	}
}

func newResponseCacheStatsAdapter(rc memory.ResponseCache) ui.ResponseCacheStatsSource {
	if rc == nil {
		return nil
	}
	src, ok := rc.(interface {
		CacheStats(ctx context.Context) (memory.ResponseCacheStats, error)
	})
	if !ok {
		return nil
	}
	return &responseCacheStatsAdapter{src: src}
}

func (a *responseCacheStatsAdapter) CacheStats(ctx context.Context) (ui.ResponseCacheStats, error) {
	if a == nil || a.src == nil {
		return ui.ResponseCacheStats{}, nil
	}
	s, err := a.src.CacheStats(ctx)
	if err != nil {
		return ui.ResponseCacheStats{}, err
	}
	return ui.ResponseCacheStats{
		RowCount:         s.RowCount,
		ApproxBytes:      s.ApproxBytes,
		DistinctPurposes: s.DistinctPurposes,
		TotalHits:        s.TotalHits,
		TotalSavingsUSD:  s.TotalSavingsUSD,
	}, nil
}

// memoryEvictorAdapter wraps memory.Corrector + memory.Repository
// so the combined surface satisfies ui.MemoryEvictor without the
// ui package importing internal/memory directly. HardEvict delegates
// to the Corrector (which already pins the project-scope IDOR guard
// + audit-row contract); ListEvictionAudits goes straight to the
// Repository.
type memoryEvictorAdapter struct {
	c    *memory.Corrector
	repo *memory.Repository
}

func newMemoryEvictorAdapter(c *memory.Corrector, repo *memory.Repository) ui.MemoryEvictor {
	return &memoryEvictorAdapter{c: c, repo: repo}
}

func (a *memoryEvictorAdapter) HardEvict(ctx context.Context, projectID string, chunkIDs []string, reason, evictedBy string) (int, error) {
	if a == nil || a.c == nil {
		return 0, nil
	}
	rows, err := a.c.HardEvict(ctx, projectID, chunkIDs, reason, evictedBy)
	if err != nil {
		return 0, err
	}
	return len(rows), nil
}

func (a *memoryEvictorAdapter) ListEvictionAudits(ctx context.Context, projectID string, limit int) ([]ui.MemoryEvictionAuditRow, error) {
	if a == nil || a.repo == nil {
		return nil, nil
	}
	src, err := a.repo.ListEvictionAudits(ctx, projectID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]ui.MemoryEvictionAuditRow, len(src))
	for i, e := range src {
		out[i] = ui.MemoryEvictionAuditRow{
			ID:           e.ID,
			ChunkID:      e.ChunkID,
			ContentHash:  e.ContentHash,
			SourceName:   e.SourceName,
			ContentClass: e.ContentClass,
			ProducerRole: e.ProducerRole,
			Reason:       e.Reason,
			EvictedBy:    e.EvictedBy,
			EvictedAt:    e.EvictedAt,
		}
	}
	return out, nil
}

// pipelineDryRunAdapter wraps memory.Pipeline so it satisfies the
// ui.PipelineDryRunner interface. The pipeline inspector's
// "test it" form lands in DryRun via this adapter.
type pipelineDryRunAdapter struct {
	p *memory.Pipeline
}

func newPipelineDryRunAdapter(p *memory.Pipeline) ui.PipelineDryRunner {
	return &pipelineDryRunAdapter{p: p}
}

func (a *pipelineDryRunAdapter) DryRun(projectID, sourceName, producerRole, content string) ui.DryRunResult {
	return a.dryRunResultToUI(a.p.DryRun(projectID, sourceName, producerRole, content))
}

func (a *pipelineDryRunAdapter) DryRunWithExecution(projectID, sourceName, producerRole, executionID, content string) ui.DryRunResult {
	return a.dryRunResultToUI(a.p.DryRunWithExecution(projectID, sourceName, producerRole, executionID, content))
}

func (a *pipelineDryRunAdapter) dryRunResultToUI(r memory.DryRunResult) ui.DryRunResult {
	out := ui.DryRunResult{
		Final:                gateOutcomeToUI(r.Final),
		Trail:                gateTrailToUI(r.Trail),
		Class:                string(r.Class),
		TTLDays:              int(r.Policy.TTL.Hours() / 24),
		DefaultConfidence:    r.Policy.DefaultConfidence,
		RoleOfRecordEligible: r.RoleOfRecordEligible,
		PostRedactContent:    r.PostRedactContent,
	}
	if len(r.Claims) > 0 {
		out.Claims = make([]ui.DryRunClaim, len(r.Claims))
		for i, c := range r.Claims {
			out.Claims[i] = ui.DryRunClaim{
				Category:   string(c.Category),
				Value:      c.Value,
				Found:      c.Found,
				AuditRowID: c.AuditRowID,
			}
		}
	}
	return out
}

// newAuditLookupFunc builds the memory.AuditLookupFunc the
// pipeline uses to score extracted claims against tool_audit_log.
// One DB round-trip per ingest candidate (filtered by
// execution_id), then in-memory soft matching against each row's
// tool_input + tool_output. Soft = case-folded substring with a
// token-Jaccard fallback; see memory.SoftMatchClaim. Captures the
// match score on ClaimMatch.MatchScore so operators can tell a sharp
// hit from a fuzzy one in the inspector.
//
// The pipeline already gates this on a non-empty execution_id, so
// the typical execution has dozens-to-hundreds of audit rows; the
// LIMIT 1000 cap is a defensive ceiling against runaway
// long-running executions, not a tuning knob.
func newAuditLookupFunc(repo persistence.ToolAuditRepository) memory.AuditLookupFunc {
	return func(ctx context.Context, executionID string, claims []memory.Claim) ([]memory.ClaimMatch, error) {
		if repo == nil || executionID == "" || len(claims) == 0 {
			return nil, nil
		}
		execID := executionID
		rows, err := repo.List(ctx, persistence.ToolAuditFilter{
			ExecutionID: &execID,
			PageSize:    1000,
		})
		if err != nil {
			return nil, err
		}
		out := make([]memory.ClaimMatch, len(claims))
		for i, cl := range claims {
			out[i] = memory.ClaimMatch{Claim: cl}
			needle := strings.TrimSpace(cl.Value)
			if needle == "" {
				continue
			}
			for _, r := range rows {
				if r == nil {
					continue
				}
				// Try each side; pick the stronger score.
				inMatched, inScore := memory.SoftMatchClaim(needle, r.ToolInput, 0)
				outMatched, outScore := memory.SoftMatchClaim(needle, r.ToolOutput, 0)
				if !inMatched && !outMatched {
					continue
				}
				best := inScore
				if outScore > best {
					best = outScore
				}
				out[i].Found = true
				out[i].AuditRowID = r.ID
				out[i].MatchScore = best
				break
			}
		}
		return out, nil
	}
}

func gateActionToString(a memory.GateAction) string {
	switch a {
	case memory.GateAllow:
		return "allow"
	case memory.GateRedact:
		return "redact"
	case memory.GateQuarantine:
		return "quarantine"
	case memory.GateReject:
		return "reject"
	}
	return "unknown"
}

func gateOutcomeToUI(o memory.GateOutcome) ui.DryRunGateOutcome {
	return ui.DryRunGateOutcome{
		Gate:         string(o.Gate),
		Action:       gateActionToString(o.Action),
		Detail:       o.Detail,
		NewContent:   o.NewContent,
		ShadowSignal: o.ShadowSignal,
	}
}

func gateTrailToUI(trail []memory.GateOutcome) []ui.DryRunGateOutcome {
	out := make([]ui.DryRunGateOutcome, len(trail))
	for i, t := range trail {
		out[i] = gateOutcomeToUI(t)
	}
	return out
}

func (a *vectorVizAdapter) SampleProjection(ctxLike interface{ Done() <-chan struct{} }, projectID string, activeEpochs []string, limit int) ([]ui.VizPoint, error) {
	pts, err := a.src.SampleProjection(ctxLike, projectID, activeEpochs, limit)
	if err != nil {
		return nil, err
	}
	out := make([]ui.VizPoint, len(pts))
	for i, p := range pts {
		nbrs := make([]ui.VizNeighbor, len(p.Neighbors))
		for j, n := range p.Neighbors {
			nbrs[j] = ui.VizNeighbor{
				ChunkID:    n.ChunkID,
				Similarity: n.Similarity,
			}
		}
		out[i] = ui.VizPoint{
			X:                p.X,
			Y:                p.Y,
			Z:                p.Z,
			ContentSize:      p.ContentSize,
			ChunkID:          p.ChunkID,
			SourceName:       p.SourceName,
			ContentClass:     p.ContentClass,
			ValidationStatus: p.ValidationStatus,
			ProducerRole:     p.ProducerRole,
			Preview:          p.Preview,
			Neighbors:        nbrs,
		}
	}
	return out, nil
}

// Stats delegates to the underlying Manager and converts the result
// type from memory.ProjectMemoryStats to api.MemoryProjectStats.
func (a *memoryStatsAdapter) Stats(ctx context.Context) ([]api.MemoryProjectStats, error) {
	rows, err := a.m.Stats(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]api.MemoryProjectStats, len(rows))
	for i, row := range rows {
		out[i] = api.MemoryProjectStats{
			ProjectID:      row.ProjectID,
			ChunksTotal:    row.ChunksTotal,
			ChunksEmbedded: row.ChunksEmbedded,
			QueueDepth:     row.QueueDepth,
		}
	}
	return out, nil
}

// memoryTitleBackfillAdapter wraps a memory.TitleBackfiller so it
// satisfies api.MemoryTitleBackfiller. The api package can't import
// memory directly without an import cycle, so the adapter lives here.
type memoryTitleBackfillAdapter struct {
	b *memory.TitleBackfiller
}

func newMemoryTitleBackfillAdapter(b *memory.TitleBackfiller) api.MemoryTitleBackfiller {
	return &memoryTitleBackfillAdapter{b: b}
}

func (a *memoryTitleBackfillAdapter) CountRemaining(ctx context.Context) (int, error) {
	return a.b.CountRemaining(ctx)
}

func (a *memoryTitleBackfillAdapter) BackfillBatch(ctx context.Context, batchSize int) (*api.MemoryTitleBackfillResult, error) {
	res, err := a.b.BackfillBatch(ctx, batchSize)
	if err != nil {
		return nil, err
	}
	return &api.MemoryTitleBackfillResult{
		Processed: res.Processed,
		Succeeded: res.Succeeded,
		Failed:    res.Failed,
		Skipped:   res.Skipped,
		Remaining: res.Remaining,
		Errors:    res.Errors,
	}, nil
}

// memoryClassifyBackfillAdapter wraps a memory.ClassifyBackfiller so
// it satisfies api.MemoryClassifyBackfiller. Same import-cycle
// rationale as the title-backfill adapter above.
type memoryClassifyBackfillAdapter struct {
	b *memory.ClassifyBackfiller
}

func newMemoryClassifyBackfillAdapter(b *memory.ClassifyBackfiller) api.MemoryClassifyBackfiller {
	return &memoryClassifyBackfillAdapter{b: b}
}

func (a *memoryClassifyBackfillAdapter) CountRemaining(ctx context.Context, projectID string) (int, error) {
	return a.b.CountRemaining(ctx, projectID)
}

func (a *memoryClassifyBackfillAdapter) BackfillBatch(ctx context.Context, projectID string, batchSize int) (*api.MemoryClassifyBackfillResult, error) {
	res, err := a.b.BackfillBatch(ctx, projectID, batchSize)
	if err != nil {
		return nil, err
	}
	return &api.MemoryClassifyBackfillResult{
		Processed: res.Processed,
		Succeeded: res.Succeeded,
		Failed:    res.Failed,
		Skipped:   res.Skipped,
		Remaining: res.Remaining,
		Errors:    res.Errors,
	}, nil
}
