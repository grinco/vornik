package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"vornik.io/vornik/internal/counterfactual"
	"vornik.io/vornik/internal/mcp"
	"vornik.io/vornik/internal/persistence"
)

// companionActorKind returns the LLD-22 §"Provenance and audit"
// actor_kind tag for a companion-originated retrieval or ingest:
// `companion:<client_kind>` so dashboards can split recalls by
// client (claude-code / codex / gemini-cli / opencode / …). Legacy
// keys with an empty ClientKind fall back to plain "companion" so
// pre-2026.6.x rows and bare-bones keys keep working. Caller MUST
// pass a non-nil key; nil panics rather than silently writing an
// untagged audit row.
func companionActorKind(key *persistence.APIKey) string {
	if key.ClientKind == "" {
		return "companion"
	}
	return "companion:" + key.ClientKind
}

// recordCompanionToolAudit writes one tool_audit_log row per
// companion MCP tool call (B-17). Gives operators a unified
// "everything I called" view in the same table the agent-side
// surface uses, so /ui/admin/audit + the chat-audit drill-downs
// pick up companion calls without a parallel table.
//
// The row is intentionally thin:
//   - tool_input captures only the request-shape summary (tool name
//   - argument byte length) so the audit table doesn't accumulate
//     full deposit content (up to 64 KiB per remember()).
//   - tool_output captures status + result length. Full details
//     stay in memory_retrieval_audit / memory_ingest_audit / tasks.
//   - task_id is synthesised as "companion:<api_key_id>" so rows
//     group by operator session; execution_id is fresh per call so
//     each row stands alone.
//
// Nil-safe: when the tool-audit repo isn't wired the function is a
// no-op. Failures are logged but never bubble — the audit row must
// not block the tool reply.
func (s *Server) recordCompanionToolAudit(ctx context.Context, key *persistence.APIKey, toolName string, args json.RawMessage, result string, toolErr error, dur time.Duration) {
	if s.toolAuditRepo == nil || key == nil {
		return
	}
	const toolNamespace = "mcp__plugin_vornik-companion_vornik__"
	outcome := "ok"
	if toolErr != nil {
		outcome = "error: " + toolErr.Error()
		if len(outcome) > 512 {
			outcome = outcome[:512]
		}
	}
	taskID := "companion:" + key.ID
	entry := &persistence.ToolAuditEntry{
		ProjectID:   key.ProjectID,
		TaskID:      taskID,
		ExecutionID: persistence.GenerateID("compex"),
		ToolName:    toolNamespace + toolName,
		ToolInput:   fmt.Sprintf("args_bytes=%d", len(args)),
		ToolOutput:  fmt.Sprintf("status=%s result_bytes=%d", outcome, len(result)),
		DurationMs:  persistence.ClampToolAuditDurationMs(dur.Milliseconds()),
	}
	if err := s.toolAuditRepo.Log(ctx, entry); err != nil {
		s.logger.Warn().
			Err(err).
			Str("tool_name", entry.ToolName).
			Str("project_id", entry.ProjectID).
			Msg("companion tool-audit write failed (call already succeeded)")
	}
}

// LLD 22 — Companion RAG: `recall` + `remember` MCP tools.
//
// The companion server's first six tools (delegate/status/result/
// cancel/list/catalog) are stateless RPC on the task queue. These two
// are stateful: they read from and write to project memory. To keep
// the api package free of a hard dependency on internal/memory (which
// would force every Server consumer to wire the memory subsystem),
// the surface here is an interface — MemoryCompanionAdapter — that the
// daemon implements by composing memory.Searcher + memory.Pipeline.
//
// Authorisation is checked here, not in the adapter: the bearer key's
// MemoryRead / MemoryWrite booleans gate access at the tool boundary.
// The adapter is trusted (it runs in-process).

// MemoryCompanionAdapter is the slice of memory functionality the
// companion server needs. Deliberately narrow so future memory-
// subsystem refactors don't ripple into the api package.
type MemoryCompanionAdapter interface {
	// Recall performs a semantic search over the project's chunks
	// and returns ranked hits. Implementations are expected to apply
	// the embedder + reranker + MMR + audit-trail pipeline the same
	// way the agent-side memory_search tool does.
	Recall(ctx context.Context, projectID, query string, opts RecallOptions) ([]MemorySearchResult, error)

	// Remember ingests a companion-deposited note through the standard
	// gate pipeline. Returns the gate decision plus the synthetic
	// artifactID stamped on every chunk from this deposit.
	Remember(ctx context.Context, in RememberInput) (RememberResult, error)

	// RecentMemory returns the most recently created chunks for a
	// project, newest-first. Used by the `recent_memory` MCP tool
	// (LLD 22 Phase 2) to enrich the SessionStart digest with "here's
	// what this project recently learned" context. limit ≤ 0 → 5;
	// adapters clamp the upper bound (50 today). Skips expired chunks.
	//
	// strictScope toggles the migration-grace policy that ordinarily
	// includes NULL-scoped chunks in every scope-filtered query (the
	// `OR repo_scope IS NULL` clause). When true AND repoScope is
	// non-empty, NULL-scoped chunks are excluded — operators get a
	// real strict-scope view without retagging the leak surface.
	RecentMemory(ctx context.Context, projectID string, limit int, repoScope string, strictScope, onlyUntagged bool) ([]RecentMemoryEntry, error)

	// ListRepoScopes returns the distinct repo_scope values in the
	// project with chunk counts per scope. Powers the companion
	// `list_scopes` MCP tool so a client can enumerate the available
	// scopes without externally-supplied knowledge. NULL-scoped
	// chunks surface under the empty-string bucket so operators see
	// the leak surface visually.
	ListRepoScopes(ctx context.Context, projectID string) ([]RepoScopeCount, error)

	// Correct soft-refutes the stored chunks that best match a wrong
	// claim (flipping them to validation_status='refuted', which the
	// retrieval layer already excludes) and, when a correction is
	// supplied, deposits it as an authoritative verified chunk. Powers
	// the companion `memory_correct` MCP tool — the programmatic
	// equivalent of the dispatcher's chat-side memory_correct, so a
	// host LLM can demote stale/contradicted notes instead of letting
	// them keep surfacing alongside newer truth. Scoped to one project
	// by the caller (the companion key's own project).
	Correct(ctx context.Context, in CorrectInput) (CorrectResult, error)
}

// RepoScopeCount is one row of the scope inventory the
// `list_scopes` MCP tool returns. Scope is the empty string for
// NULL-scoped chunks (the migration-grace bucket); any other value
// is the operator-authored scope token.
type RepoScopeCount struct {
	Scope  string `json:"scope"`
	Chunks int    `json:"chunks"`
}

// RecentMemoryEntry mirrors memory.RecentChunkRow at the api package
// boundary. SourceName carries "companion:<client_kind>:..." for
// companion-origin rows, role names for agent-origin rows.
type RecentMemoryEntry struct {
	ChunkID      string `json:"chunk_id"`
	TaskID       string `json:"task_id,omitempty"`
	SourceName   string `json:"source_name"`
	ContentClass string `json:"content_class,omitempty"`
	Content      string `json:"content"`
	CreatedAt    string `json:"created_at"` // RFC3339

	// RepoScope is the chunk's repo-scope token (migration 75).
	// Empty string surfaces a NULL-scoped chunk so the operator
	// can see the migration-grace leak surface vs. a properly
	// scoped row.
	RepoScope string `json:"repo_scope,omitempty"`

	// IngestStatus tells the client whether the chunk is fully
	// searchable. One of:
	//   - "ready" — embedded AND classified.
	//   - "pending_embedding" — chunk persisted but no embedding
	//     yet (the async Worker hasn't drained it). Recall will
	//     miss it on the vector half until it lands.
	//   - "pending_classification" — embedding present, content_class
	//     still "unclassified" (the role-map didn't have a mapping
	//     and the async ClassifyBackfiller hasn't run yet).
	// Lets a client distinguish "I just deposited it; give the worker
	// a moment" from "this content genuinely isn't here".
	IngestStatus string `json:"ingest_status,omitempty"`

	// PolicyWarning is populated under the firewall's advisory mode
	// when the chunk would have been blocked: "<decision>: <reason>".
	// Mirrors recall's advisory signal so the recent_memory digest
	// can't surface a blocked chunk silently. Empty under enforce
	// (the row is dropped instead) and off (nothing blocked).
	PolicyWarning string `json:"policy_warning,omitempty"`
}

// IngestStatus constants. Kept here so callers don't have to
// guess the string values. The companion MCP tool description
// enumerates them so the host LLM picks them up too.
const (
	IngestStatusReady                 = "ready"
	IngestStatusPendingEmbedding      = "pending_embedding"
	IngestStatusPendingClassification = "pending_classification"
)

// RecallOptions carry the optional filters Searcher applies. Only
// Limit is required; everything else nil-defaults sensibly.
type RecallOptions struct {
	// Limit caps the number of returned hits. The adapter is
	// expected to clamp to a reasonable upper bound (50 today).
	Limit int
	// FromDate / ToDate clip to a temporal window. Zero values mean
	// "no temporal filter on this side."
	FromDate time.Time
	ToDate   time.Time
	// ActorKind / ActorID are stamped on the per-search audit row
	// so post-hoc queries can split agent recalls from companion
	// recalls without LIKE 'companion:%' on source_name.
	ActorKind string
	ActorID   string
	// RepoScope — migration 75. When non-empty, the searcher
	// filters chunks to those matching this scope OR '*' OR
	// NULL. Empty = no scope filter (returns project-wide).
	// Filter implementation lands in slice B-3; this slice
	// merely lets the value travel from MCP → adapter →
	// searcher so B-3 can flip the switch in one place.
	RepoScope string
	// StrictScope toggles the migration-grace policy that ordinarily
	// includes NULL-scoped chunks in every scope-filtered query (the
	// `OR repo_scope IS NULL` clause). When true AND RepoScope is
	// non-empty, NULL-scoped chunks are excluded — operators get a
	// real strict-scope view without retagging the leak surface.
	// 2026-05-28 investigation closure (see CHANGELOG).
	StrictScope bool
	// Sufficient routes this recall through scored-sufficiency retrieval
	// (widen-and-retry + LLM reranking) instead of the single-shot path.
	// Set only on non-interactive context-assembly callers (the
	// pre-delegation recall hint) so the rerank latency is scoped away from
	// interactive recall. Inert unless the reranker + sufficiency are
	// enabled in daemon config.
	Sufficient bool
}

// RememberInput bundles every field the companion handler resolves
// before calling into the adapter. ClientKind + KeyID feed synthetic
// provenance; ProjectID scopes the deposit; the rest is verbatim
// caller content. Class + TTLDays are LLD-22's per-deposit overrides
// that let the host LLM deposit as `spec` / `decision` / etc. with a
// custom TTL instead of the role-default `companion_note` / 30 days.
type RememberInput struct {
	ProjectID  string
	ClientKind string
	KeyID      string
	SourceName string
	Content    string
	// Class is the LLD-22 `class` arg. Empty leaves the role-map
	// classifier in charge (companion-prefixed producer role lands
	// on `companion_note`). Non-empty replaces it; the gate stack's
	// policy_match gate still validates the class is known.
	Class string
	// TTLDays is the LLD-22 `ttl_days` arg. Zero leaves the class
	// policy default in place; positive value overrides on a
	// per-deposit basis.
	TTLDays int
	// RepoScope partitions the deposit within the project's RAG
	// (migration 75). Empty = uncategorized; "*" = cross-cutting;
	// any other string = repo token. Lets one operator's many
	// repos share a project without polluting each other's recall
	// results.
	RepoScope string
}

// NOTE: the LLD-22 `tags` arg is intentionally NOT a field on
// RememberInput. v1 encodes tags into SourceName as a `;tags=a,b`
// suffix at the handler boundary (see companionToolRemember), so by
// the time the adapter sees the deposit the tags already live in
// SourceName. Promoting tags to a first-class column is a Phase-2
// follow-on; until then a separate field here would be an
// accepted-but-unused gap of exactly the kind the 2026-05-29 audit
// flagged.

// RememberResult mirrors the pipeline's IngestStats plus the
// synthetic artifact ID. RememberDecision is the dominant outcome
// across the gate stack (a single deposit always produces exactly
// one candidate today, so one decision describes everything).
type RememberResult struct {
	Decision    string // ALLOW | QUARANTINED | REJECTED
	ArtifactID  string
	Admitted    int
	Quarantined int
	Rejected    int
	GatesFailed []string
}

// ---- tool: recall ----------------------------------------------------

type recallArgs struct {
	Query    string  `json:"query"`
	Limit    int     `json:"limit"`
	FromDate string  `json:"from_date"`
	ToDate   string  `json:"to_date"`
	MinScore float64 `json:"min_score"`
	Class    string  `json:"class"`
	// RepoScope — migration 75. When set, recall filters to chunks
	// matching this scope OR '*' OR NULL. When empty, returns
	// everything (project-wide search). Pass "all" or omit to
	// query the entire project; pass a specific token to scope.
	RepoScope string `json:"repo_scope"`
	// StrictScope — when true AND RepoScope is non-empty, drops the
	// `OR repo_scope IS NULL` migration-grace clause so NULL-scoped
	// chunks DON'T leak into the result set. Use this when you want
	// to verify "is anything scoped to X" without the noise of
	// uncategorized chunks. 2026-05-28 investigation closure.
	StrictScope bool `json:"strict_scope"`
}

type recallHit struct {
	ChunkID       string  `json:"chunk_id"`
	Score         float64 `json:"score"`
	Content       string  `json:"content"`
	SourceName    string  `json:"source_name"`
	TaskID        string  `json:"task_id,omitempty"`
	ContentClass  string  `json:"content_class,omitempty"`
	IsAlive       *bool   `json:"is_alive,omitempty"`
	LastCheckedAt *string `json:"last_checked_at,omitempty"`
	// RepoScope surfaces the chunk's actual repo-scope token so
	// the client can disambiguate a properly-scoped match from a
	// NULL-scoped leak (the migration-grace fallthrough). Empty
	// string here = NULL-scoped (chunk was never tagged). Any
	// non-empty value is the operator-supplied scope token, or
	// '*' for cross-cutting. 2026-05-28 investigation closure.
	RepoScope string `json:"repo_scope,omitempty"`
}

type recallResult struct {
	ProjectID string      `json:"project_id"`
	Query     string      `json:"query"`
	Hits      []recallHit `json:"hits"`
	Returned  int         `json:"returned"`
	ElapsedMS int64       `json:"elapsed_ms"`
}

func (s *Server) companionToolRecall(ctx context.Context, key *persistence.APIKey, raw json.RawMessage) (string, error) {
	if !key.MemoryRead {
		return "", errors.New("this key lacks memory_read; ask the operator for `vornikctl companion grant --memory-read`")
	}
	if s.memoryCompanion == nil {
		return "", errors.New("memory subsystem not wired on this daemon")
	}

	var args recallArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	args.Query = strings.TrimSpace(args.Query)
	args.Class = strings.TrimSpace(args.Class)
	if args.Query == "" {
		return "", errors.New("query is required")
	}
	if args.Limit <= 0 {
		args.Limit = 10
	}
	if args.Limit > 50 {
		args.Limit = 50
	}
	opts := RecallOptions{
		Limit:       args.Limit,
		ActorKind:   companionActorKind(key),
		ActorID:     key.ID,
		RepoScope:   strings.TrimSpace(args.RepoScope),
		StrictScope: args.StrictScope,
	}
	if args.FromDate != "" {
		t, err := time.Parse(time.RFC3339, args.FromDate)
		if err != nil {
			return "", fmt.Errorf("from_date: %w", err)
		}
		opts.FromDate = t
	}
	if args.ToDate != "" {
		t, err := time.Parse(time.RFC3339, args.ToDate)
		if err != nil {
			return "", fmt.Errorf("to_date: %w", err)
		}
		opts.ToDate = t
	}

	start := time.Now()
	results, err := s.memoryCompanion.Recall(ctx, key.ProjectID, args.Query, opts)
	if err != nil {
		return "", fmt.Errorf("recall failed: %w", err)
	}

	// Counterfactual chunk exclusion (Phase C v2
	// VariableMemoryChunkExcluded). Operator-supplied list of
	// chunk IDs to drop from recall — answers "what would the
	// answer be if THIS memory wasn't retrieved?". No-op for
	// non-counterfactual tasks (overrides.ExcludedChunks is nil
	// when the payload has no counterfactual block).
	var cfExclude counterfactual.Payload
	if taskID, ok := ctx.Value(mcp.TaskIDHeaderKey{}).(string); ok && taskID != "" && s.taskRepo != nil {
		if t, terr := s.taskRepo.Get(ctx, taskID); terr == nil && t != nil {
			cfExclude = counterfactual.ExtractPayload(t.Payload)
		}
	}

	hits := make([]recallHit, 0, len(results))
	for _, r := range results {
		if args.MinScore > 0 && r.Score < args.MinScore {
			continue
		}
		if cfExclude.IsChunkExcluded(r.ChunkID) {
			continue
		}
		// LLD-22 `class` filter. MemorySearchResult now carries the
		// chunk's ContentClass (populated by the search SQL), so a
		// caller-supplied class narrows the result set to exactly that
		// class. Case-insensitive match. A result whose ContentClass
		// is empty (older repos without the column) is dropped under
		// an explicit filter rather than leaked — the caller asked for
		// a specific class and an unlabeled chunk isn't a confirmed
		// match.
		if args.Class != "" && !strings.EqualFold(r.ContentClass, args.Class) {
			continue
		}
		hits = append(hits, recallHit{
			ChunkID:       r.ChunkID,
			Score:         r.Score,
			Content:       r.Content,
			SourceName:    r.SourceName,
			TaskID:        r.TaskID,
			ContentClass:  r.ContentClass,
			IsAlive:       r.IsAlive,
			LastCheckedAt: r.LastCheckedAt,
			RepoScope:     r.RepoScope,
		})
	}
	out := recallResult{
		ProjectID: key.ProjectID,
		Query:     args.Query,
		Hits:      hits,
		Returned:  len(hits),
		ElapsedMS: time.Since(start).Milliseconds(),
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	return string(b), nil
}

// ---- tool: recent_memory --------------------------------------------

type recentMemoryArgs struct {
	Limit int `json:"limit"`
	// RepoScope filters the digest to one repo's chunks plus '*'
	// + NULL. Empty = project-wide. Migration 75. Filter logic
	// lands in B-3; this slice wires the surface.
	RepoScope string `json:"repo_scope"`
	// StrictScope — when true AND RepoScope is non-empty, drops the
	// migration-grace `OR repo_scope IS NULL` clause so NULL-scoped
	// chunks don't leak into the digest. 2026-05-28 investigation
	// closure.
	StrictScope bool `json:"strict_scope"`
	// OnlyUntagged — when true, returns ONLY NULL-scoped ("untagged")
	// chunks (ignores RepoScope/StrictScope). This is the retag-triage
	// selector: list_scopes reports the untagged count, this enumerates
	// its contents/sources so the operator knows what to retag.
	OnlyUntagged bool `json:"only_untagged"`
}

type recentMemoryEntry struct {
	ChunkID      string `json:"chunk_id"`
	TaskID       string `json:"task_id,omitempty"`
	SourceName   string `json:"source_name"`
	ContentClass string `json:"content_class,omitempty"`
	Snippet      string `json:"snippet"`
	CreatedAt    string `json:"created_at"`
	// RepoScope surfaces the chunk's actual scope token so the
	// client can spot NULL-scoped leak-throughs. Empty = NULL-scoped.
	RepoScope string `json:"repo_scope,omitempty"`
	// IngestStatus tells the client whether the chunk is fully
	// searchable: "ready", "pending_embedding", or
	// "pending_classification". See api.IngestStatus* constants.
	IngestStatus string `json:"ingest_status,omitempty"`
}

type recentMemoryResult struct {
	ProjectID string              `json:"project_id"`
	Entries   []recentMemoryEntry `json:"entries"`
	Returned  int                 `json:"returned"`
}

const recentMemorySnippetChars = 240

func (s *Server) companionToolRecentMemory(ctx context.Context, key *persistence.APIKey, raw json.RawMessage) (string, error) {
	if !key.MemoryRead {
		return "", errors.New("this key lacks memory_read; ask the operator for `vornikctl companion grant --memory-read`")
	}
	if s.memoryCompanion == nil {
		return "", errors.New("memory subsystem not wired on this daemon")
	}

	var args recentMemoryArgs
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return "", fmt.Errorf("invalid arguments: %w", err)
		}
	}
	if args.Limit <= 0 {
		args.Limit = 5
	}
	if args.Limit > 20 {
		// Cap below the adapter's clamp so the digest payload stays small.
		args.Limit = 20
	}

	entries, err := s.memoryCompanion.RecentMemory(ctx, key.ProjectID, args.Limit, strings.TrimSpace(args.RepoScope), args.StrictScope, args.OnlyUntagged)
	if err != nil {
		return "", fmt.Errorf("recent_memory failed: %w", err)
	}

	out := recentMemoryResult{
		ProjectID: key.ProjectID,
		Entries:   make([]recentMemoryEntry, 0, len(entries)),
	}
	for _, e := range entries {
		snippet := e.Content
		if len(snippet) > recentMemorySnippetChars {
			snippet = snippet[:recentMemorySnippetChars] + "…"
		}
		out.Entries = append(out.Entries, recentMemoryEntry{
			ChunkID:      e.ChunkID,
			TaskID:       e.TaskID,
			SourceName:   e.SourceName,
			ContentClass: e.ContentClass,
			Snippet:      snippet,
			CreatedAt:    e.CreatedAt,
			RepoScope:    e.RepoScope,
			IngestStatus: e.IngestStatus,
		})
	}
	out.Returned = len(out.Entries)
	b, _ := json.MarshalIndent(out, "", "  ")
	return string(b), nil
}

// ---- tool: list_scopes ----------------------------------------------

type listScopesEntry struct {
	Scope  string `json:"scope"`
	Chunks int    `json:"chunks"`
}

type listScopesResult struct {
	ProjectID string            `json:"project_id"`
	Scopes    []listScopesEntry `json:"scopes"`
	Returned  int               `json:"returned"`
}

// companionToolListScopes returns the distinct repo_scope values in
// the project's RAG memory, each with a chunk count. NULL-scoped
// chunks surface under the empty-string bucket so operators see
// the leak surface visually. 2026-05-28 investigation closure.
func (s *Server) companionToolListScopes(ctx context.Context, key *persistence.APIKey) (string, error) {
	if !key.MemoryRead {
		return "", errors.New("this key lacks memory_read; ask the operator for `vornikctl companion grant --memory-read`")
	}
	if s.memoryCompanion == nil {
		return "", errors.New("memory subsystem not wired on this daemon")
	}
	rows, err := s.memoryCompanion.ListRepoScopes(ctx, key.ProjectID)
	if err != nil {
		return "", fmt.Errorf("list_scopes failed: %w", err)
	}
	out := listScopesResult{
		ProjectID: key.ProjectID,
		Scopes:    make([]listScopesEntry, 0, len(rows)),
	}
	for _, r := range rows {
		// staticcheck S1016: both types have identical fields, so
		// the conversion is preferred over a struct-literal copy.
		out.Scopes = append(out.Scopes, listScopesEntry(r))
	}
	out.Returned = len(out.Scopes)
	b, _ := json.MarshalIndent(out, "", "  ")
	return string(b), nil
}

// ---- tool: remember --------------------------------------------------

const rememberMaxContentBytes = 64 * 1024 // see LLD 22 §Rate limits

type rememberArgs struct {
	Content    string `json:"content"`
	SourceName string `json:"source_name"`
	// Class — LLD-22 § "Tool surface". One of the ContentClass
	// values (e.g. "spec", "decision", "diagnostic", …). Empty
	// leaves the role-map classifier in charge (companion-origin
	// deposits default to `companion_note`).
	Class string `json:"class"`
	// TTLDays — LLD-22 § "Tool surface". Per-deposit override of
	// the class-policy default TTL. <= 0 means "use class default";
	// positive value pins `expires_at = NOW() + ttl_days * 24h`.
	TTLDays int `json:"ttl_days"`
	// RepoScope — migration 75. Partitions the deposit within the
	// project's RAG so the operator's many repos don't pollute
	// each other's recall results. Empty = uncategorized; "*" =
	// cross-cutting; any other string = repo token (typically the
	// git remote URL <host>/<path> or repo basename).
	RepoScope string `json:"repo_scope"`
	// Tags — LLD-22 § "Tool surface". Optional free-form labels
	// (max 10 entries × 32 chars). v1 encodes them as a comma-joined
	// suffix on source_name so they're queryable via LIKE without a
	// migration; the encoding is a deliberate one-way door.
	Tags []string `json:"tags"`
}

// rememberMaxTags / rememberMaxTagLen bound the `tags` arg per
// LLD-22 § "Tool surface" ("[string], optional, max 10 entries × 32
// chars each"). Over-long entries are truncated; the slice is capped
// at the first rememberMaxTags entries. Bounding here keeps the
// source_name suffix small.
const (
	rememberMaxTags   = 10
	rememberMaxTagLen = 32
)

// normalizeTags trims, drops empties, truncates each entry to
// rememberMaxTagLen, and caps the slice at rememberMaxTags. Returns
// nil when nothing survives so the source_name suffix is omitted.
// Deterministic order (caller order preserved) so the same tag set
// always yields the same source_name (dedup_hash stays stable).
func normalizeTags(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, rememberMaxTags)
	seen := make(map[string]struct{}, len(in))
	for _, raw := range in {
		t := strings.TrimSpace(raw)
		if t == "" {
			continue
		}
		if len(t) > rememberMaxTagLen {
			t = t[:rememberMaxTagLen]
		}
		// Strip commas so the comma-joined encoding stays
		// unambiguous; a tag containing a comma would otherwise
		// split into two on read-back.
		t = strings.ReplaceAll(t, ",", " ")
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
		if len(out) >= rememberMaxTags {
			break
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// applyTagsToSourceName appends the LLD-22 `;tags=a,b` suffix to the
// source name. Empty tags leaves the name untouched. Idempotent
// shape: the suffix is always `;tags=` + comma-joined entries.
func applyTagsToSourceName(sourceName string, tags []string) string {
	if len(tags) == 0 {
		return sourceName
	}
	return sourceName + ";tags=" + strings.Join(tags, ",")
}

type rememberResult struct {
	Decision    string   `json:"decision"`
	ArtifactID  string   `json:"artifact_id"`
	Admitted    int      `json:"admitted"`
	Quarantined int      `json:"quarantined"`
	Rejected    int      `json:"rejected"`
	GatesFailed []string `json:"gates_failed,omitempty"`
}

func (s *Server) companionToolRemember(ctx context.Context, key *persistence.APIKey, raw json.RawMessage) (string, error) {
	if !key.MemoryWrite {
		return "", errors.New("this key lacks memory_write; ask the operator for `vornikctl companion grant --memory-write`")
	}
	if s.memoryCompanion == nil {
		return "", errors.New("memory subsystem not wired on this daemon")
	}

	var args rememberArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	args.Content = strings.TrimSpace(args.Content)
	if args.Content == "" {
		return "", errors.New("content is required")
	}
	if len(args.Content) > rememberMaxContentBytes {
		return "", fmt.Errorf("content exceeds %d bytes; upload as an artifact instead", rememberMaxContentBytes)
	}

	// Per-key budget cap (LLD-22 § "Rate limits & budget"; audit
	// §8.1). Before this gate, BudgetCapUSD was stored + echoed in
	// catalog() but a key with BudgetCapUSD=0.01 could deposit
	// unbounded notes. Mirror the delegate() pre-check (§7.2): sum the
	// key's prior spend via the Phase-1 SumCostByAPIKey infra and
	// refuse once the cap is reached. nil cap = uncapped; nil repo
	// (lean deployments) = skip. Lifetime window (zero since/until).
	// embedding token cost isn't itself ledgered today, so this
	// enforces the cap as a pre-check rather than debiting the
	// deposit's own embedding cost — see the LLD note added in the
	// 2026-05-29 drift-mitigation pass. Fails OPEN on a query error
	// (the deposit is cheap and a transient DB blip shouldn't freeze
	// every remember).
	if key.BudgetCapUSD != nil && s.llmUsageRepo != nil {
		spent, sumErr := s.llmUsageRepo.SumCostByAPIKey(ctx, key.ID, time.Time{}, time.Time{})
		if sumErr != nil {
			s.logger.Warn().Err(sumErr).Str("api_key_id", key.ID).
				Msg("companion remember: budget-cap spend lookup failed; allowing deposit")
		} else if spent >= *key.BudgetCapUSD {
			return "", fmt.Errorf("BUDGET_EXCEEDED: key budget cap $%.4f reached (spent $%.4f); remember refused", *key.BudgetCapUSD, spent)
		}
	}

	sourceName := strings.TrimSpace(args.SourceName)

	// LLD-22 `tags`: encode as a `;tags=a,b` suffix on source_name so
	// they're observable + LIKE-queryable without a migration. When
	// the caller left source_name empty we resolve the role-default
	// here (rather than in the pipeline) so the suffix has a base to
	// attach to and the tags actually land. Bounded to 10 entries ×
	// 32 chars.
	tags := normalizeTags(args.Tags)
	if len(tags) > 0 {
		if sourceName == "" {
			sourceName = "companion:" + key.ClientKind + ":note"
		}
		sourceName = applyTagsToSourceName(sourceName, tags)
	}

	res, err := s.memoryCompanion.Remember(ctx, RememberInput{
		ProjectID:  key.ProjectID,
		ClientKind: key.ClientKind,
		KeyID:      key.ID,
		SourceName: sourceName,
		Content:    args.Content,
		Class:      strings.TrimSpace(args.Class),
		TTLDays:    args.TTLDays,
		RepoScope:  strings.TrimSpace(args.RepoScope),
	})
	if err != nil {
		return "", fmt.Errorf("remember failed: %w", err)
	}
	out := rememberResult(res)
	b, _ := json.MarshalIndent(out, "", "  ")
	return string(b), nil
}

// ---- tool: memory_correct ------------------------------------------

// CorrectInput is the adapter-boundary shape for a companion
// memory_correct call. ProjectID is always the caller key's own
// project (the handler never accepts a caller-supplied project — that
// would breach the per-key project scope). Correction is optional:
// empty means refute-only (demote the wrong claim without depositing a
// replacement — useful when the authoritative version already lives in
// a newer note).
type CorrectInput struct {
	ProjectID  string
	WrongClaim string
	Correction string
	RepoScope  string
	MaxRefutes int
	// ChunkIDs, when non-empty, switches Correct to surgical by-ID
	// refute: exactly these chunks are flipped to refuted (via
	// MarkRefutedByIDs), bypassing the claim search. Use when the
	// caller already knows the stale chunk AND authoritative
	// corrections would otherwise out-rank it in a claim search
	// (refuting "top matches" would demote the corrections instead).
	ChunkIDs []string
}

// RefutedChunkInfo is the per-row record of a chunk that was flipped to
// validation_status='refuted', surfaced back to the caller so it can
// report exactly what got demoted.
type RefutedChunkInfo struct {
	ChunkID    string  `json:"chunk_id"`
	SourceName string  `json:"source_name"`
	Preview    string  `json:"preview"`
	Score      float64 `json:"score"`
}

// CorrectResult is what the adapter returns from Correct: the refuted
// chunks plus the ID of the freshly-deposited correction (empty when
// refute-only).
type CorrectResult struct {
	Refuted           []RefutedChunkInfo
	CorrectionChunkID string
	// ByID is true when the refute used the surgical chunk-id path.
	// RefutedCount is then the number of named chunks actually flipped
	// (≤ len(requested) — already-refuted/wrong-project IDs are
	// skipped). For the claim path ByID is false and len(Refuted) is
	// authoritative.
	ByID         bool
	RefutedCount int
}

type correctArgs struct {
	WrongClaim string   `json:"wrong_claim"`
	Correction string   `json:"correction"`
	MaxRefutes int      `json:"max_refutes"`
	RepoScope  string   `json:"repo_scope"`
	ChunkIDs   []string `json:"chunk_ids"`
}

type correctResultOut struct {
	RefutedCount      int                `json:"refuted_count"`
	Refuted           []RefutedChunkInfo `json:"refuted"`
	CorrectionChunkID string             `json:"correction_chunk_id,omitempty"`
	Note              string             `json:"note,omitempty"`
}

// companionToolMemoryCorrect handles the `memory_correct` MCP tool.
// It is gated on MemoryWrite (it mutates the corpus: refutes matches
// and optionally inserts a correction) and is always scoped to the
// caller key's own project. Mirrors the dispatcher's chat-side
// memory_correct so a host LLM can demote stale/contradicted notes
// programmatically.
func (s *Server) companionToolMemoryCorrect(ctx context.Context, key *persistence.APIKey, raw json.RawMessage) (string, error) {
	if !key.MemoryWrite {
		return "", errors.New("this key lacks memory_write; ask the operator for `vornikctl companion grant --memory-write`")
	}
	if s.memoryCompanion == nil {
		return "", errors.New("memory subsystem not wired on this daemon")
	}
	var args correctArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	args.WrongClaim = strings.TrimSpace(args.WrongClaim)
	args.Correction = strings.TrimSpace(args.Correction)
	chunkIDs := normalizeChunkIDs(args.ChunkIDs)
	// Need a target: either a claim to search, or explicit chunk ids.
	// (A bare correction with neither would deposit a note but demote
	// nothing — use remember() for that.)
	if args.WrongClaim == "" && len(chunkIDs) == 0 {
		return "", errors.New("provide wrong_claim (claim search) or chunk_ids (surgical by-id refute)")
	}
	if len(args.Correction) > rememberMaxContentBytes {
		return "", fmt.Errorf("correction exceeds %d bytes; deposit it via remember and refute separately", rememberMaxContentBytes)
	}

	res, err := s.memoryCompanion.Correct(ctx, CorrectInput{
		ProjectID:  key.ProjectID,
		WrongClaim: args.WrongClaim,
		Correction: args.Correction,
		RepoScope:  strings.TrimSpace(args.RepoScope),
		MaxRefutes: args.MaxRefutes,
		ChunkIDs:   chunkIDs,
	})
	if err != nil {
		return "", fmt.Errorf("memory_correct failed: %w", err)
	}

	out := correctResultOut{
		Refuted:           res.Refuted,
		CorrectionChunkID: res.CorrectionChunkID,
	}
	if out.Refuted == nil {
		out.Refuted = []RefutedChunkInfo{}
	}
	if res.ByID {
		out.RefutedCount = res.RefutedCount
		if res.RefutedCount < len(chunkIDs) {
			out.Note = fmt.Sprintf("flipped %d of %d requested chunk_ids to refuted (others were already refuted/superseded or not in this project)", res.RefutedCount, len(chunkIDs))
		}
	} else {
		out.RefutedCount = len(res.Refuted)
		if out.RefutedCount == 0 {
			out.Note = "no stored chunk matched wrong_claim closely enough to refute — nothing demoted (try chunk_ids for a surgical refute, or rephrase to match how the fact is stored)"
		}
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	return string(b), nil
}

// normalizeChunkIDs trims, drops empties, and dedupes the chunk_ids
// arg so a sloppy caller (trailing spaces, dup entries) can't inflate
// the request or send empty IDs to the refute query.
func normalizeChunkIDs(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, id := range in {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}
