package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/llmspend"
)

// Reranker scores a candidate set of SearchResults against a query and
// returns them sorted best-first. Pulled out as an interface so the
// Searcher stays oblivious to the scoring strategy — production wires
// an LLM-backed implementation; tests can substitute deterministic
// stubs without dragging in chat provider machinery.
//
// Failure mode: a reranker must NEVER drop results. On error or
// timeout, return the input slice unchanged + nil error. Search
// quality degrades to plain RRF but the caller still gets answers.
type Reranker interface {
	Rerank(ctx context.Context, query string, results []SearchResult) ([]SearchResult, error)
}

// NewConfiguredReranker builds the Reranker the service container wires onto
// the Searcher from daemon config. Returns a NoopReranker (RRF ordering, the
// safe default) when reranking is disabled or no chat client is available;
// otherwise an LLMReranker. Keeping this here (rather than inline in the
// container) lets the memory package stay free of the config package and makes
// the enable/disable decision unit-testable.
//
// Wiring a non-Noop reranker has two effects: it re-orders every recall by
// LLM-scored relevance (one extra LLM call per search, bounded by the timeout
// and degrading to RRF on failure), AND it activates scored-sufficiency
// retrieval, whose absolute score floor is only meaningful against calibrated
// reranker scores.
// Options are variadic so the existing call shape keeps working; cost
// accounting is opt-in via WithRerankerSpend and applies only to the
// LLM-backed path, since a NoopReranker never bills anything.
func NewConfiguredReranker(enabled bool, client chat.Provider, model string, maxCandidates, timeoutSeconds, maxSnippetBytes int, logger zerolog.Logger, opts ...RerankerOption) Reranker {
	if !enabled || client == nil {
		return NoopReranker{}
	}
	r := &LLMReranker{
		Client:          client,
		Model:           model,
		Timeout:         time.Duration(timeoutSeconds) * time.Second,
		MaxCandidates:   maxCandidates,
		MaxSnippetBytes: maxSnippetBytes,
		Logger:          logger,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(r)
		}
	}
	return r
}

// RerankerOption customises an LLMReranker at construction. Mirrors the
// memetic package's WithInstincts / WithApplicationWriter shape rather
// than growing an already-long positional parameter list.
type RerankerOption func(*LLMReranker)

// WithRerankerSpend wires the billing recorder. Takes an llmspend.Recorder
// rather than a repo + pricing pair, matching every other component: the seam
// owns the row shape, so the caller only decides WHO pays.
func WithRerankerSpend(r llmspend.Recorder) RerankerOption {
	return func(lr *LLMReranker) { lr.spend = r }
}

// NoopReranker preserves RRF ordering. The default — wired when the
// service container has no chat provider for reranking, or when the
// operator opts out for latency reasons.
type NoopReranker struct{}

// Rerank returns the input unchanged.
func (NoopReranker) Rerank(_ context.Context, _ string, results []SearchResult) ([]SearchResult, error) {
	return results, nil
}

// LLMReranker calls a chat provider with a relevance-scoring prompt and
// re-sorts results by the returned per-result score. Designed for the
// "rerank top-20 → top-10" pattern: one LLM call per query, regardless
// of result count. Cost scales with candidate text length; the Limit
// fields cap it.
type LLMReranker struct {
	// Client is the chat provider. Required.
	Client chat.Provider
	// Model overrides the provider's default when non-empty and the
	// provider implements chat.ModelOverridable. Optional.
	Model string
	// Timeout per call. 0 → 15s. Reranker latency is on the search
	// critical path so the cap is tighter than the titler's.
	Timeout time.Duration
	// MaxCandidates caps the number of inputs sent to the LLM. The
	// top-K post-RRF; results beyond K pass through unchanged at the
	// tail. 0 → 20.
	MaxCandidates int
	// MaxSnippetBytes truncates each candidate's content before
	// sending. 0 → 600 (matches the viz preview cap).
	MaxSnippetBytes int
	// Logger captures rerank-time warnings (LLM timeout, parse failure
	// → degrade to RRF). Optional.
	Logger zerolog.Logger
	// LLMUsage records one task_llm_usage row per BILLED rerank call.
	// nil disables recording (failing to bill is dashboard fidelity,
	// not correctness); production wires
	// *postgres.TaskLLMUsageRepository. Mirrors Titler.LLMUsage.
	spend llmspend.Recorder
	// Pricing computes USD from the model's token counts. nil → the
	// row still lands with CostUSD 0 so the call remains visible in
	// the call-count rollup even when the model is unpriced.
	Pricing PricingTable
}

// rerankerRole is the value stored in task_llm_usage.role for every
// LLM rerank call. Mirrors titlerRole / classifierRole so the spend
// breakdown groups retrieval-side cost under its own name.
const rerankerRole = "memory_reranker"

// rerankSystemPrompt instructs the model to emit relevance scores as a
// strict JSON object so we can parse without ceremony. Closed-shape:
// "scores" maps the per-result index (0-based) to a float in [0,1].
const rerankSystemPrompt = `You score the relevance of retrieved memory chunks against a search query.

Rules:
- Output a single JSON object on one line: {"scores":{"0":0.92,"1":0.71,...}}
- One entry per candidate. Index is the candidate's position (0-based).
- Score in [0.0, 1.0]: 1.0 = directly answers the query; 0.0 = unrelated.
- No prose, no markdown fences, no trailing commentary.`

// Rerank scores the top MaxCandidates of results and returns them sorted
// by the LLM's relevance score. Tail (beyond MaxCandidates) is appended
// unchanged. On any failure (timeout, parse error, empty input) it
// returns the input slice unchanged so the search request still
// succeeds.
func (r *LLMReranker) Rerank(ctx context.Context, query string, results []SearchResult) ([]SearchResult, error) {
	if r == nil || r.Client == nil || len(results) < 2 || strings.TrimSpace(query) == "" {
		return results, nil
	}

	k := r.MaxCandidates
	if k <= 0 {
		k = 20
	}
	if k > len(results) {
		k = len(results)
	}
	head := results[:k]
	tail := results[k:]

	snippetCap := r.MaxSnippetBytes
	if snippetCap <= 0 {
		snippetCap = 600
	}

	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	callCtx = chat.WithCallSite(callCtx, "memory.reranker")
	defer cancel()

	userPrompt := buildRerankPrompt(query, head, snippetCap)
	msgs := []chat.Message{
		{Role: "system", Content: rerankSystemPrompt},
		{Role: "user", Content: userPrompt},
	}
	client := pickModelForTitler(r.Client, r.Model) // same override helper

	resp, err := client.Complete(callCtx, msgs)
	// Bill BEFORE classifying the result. The provider charged for the
	// call the moment it returned, so recording after a success check
	// would make every degrade-to-RRF path a silent spender — the exact
	// laundering that hid ~83% of the KG extractor's spend behind
	// "nothing found" (investigated 2026-07-31). recordUsage tolerates a
	// nil resp. Parent ctx, not callCtx: callCtx carries the rerank
	// timeout and may already be expired when a slow call returns.
	billTo, ambiguous := rerankProjectID(head)
	if ambiguous {
		r.Logger.Warn().Str("attributed_to", billTo).Int("candidates", len(head)).
			Msg("memory: rerank candidates span multiple projects — cost attribution is ambiguous, billing the first")
	}
	r.recordUsage(ctx, resp, billTo)
	if err != nil || resp == nil || len(resp.Choices) == 0 {
		r.Logger.Warn().Err(err).Int("candidates", len(head)).
			Msg("memory: reranker LLM call failed — degrading to RRF ordering")
		return results, nil
	}
	scores, perr := parseRerankScores(resp.Choices[0].Message.Content, len(head))
	if perr != nil {
		r.Logger.Warn().Err(perr).
			Str("raw", truncate(resp.Choices[0].Message.Content, 200)).
			Msg("memory: reranker parse failed — degrading to RRF ordering")
		return results, nil
	}

	// Stable sort head by score desc. Ties preserve the RRF order.
	indexed := make([]struct {
		i     int
		score float64
	}, len(head))
	for i := range head {
		indexed[i] = struct {
			i     int
			score float64
		}{i, scores[i]}
	}
	sort.SliceStable(indexed, func(a, b int) bool {
		return indexed[a].score > indexed[b].score
	})
	reordered := make([]SearchResult, 0, len(results))
	for _, ix := range indexed {
		row := head[ix.i]
		row.Score = ix.score
		reordered = append(reordered, row)
	}
	reordered = append(reordered, tail...)
	return reordered, nil
}

// buildRerankPrompt assembles the user-side prompt: the query + an
// indexed list of candidate snippets. Each candidate is one block
// prefixed with `[i]` so the LLM can refer to them by index.
func buildRerankPrompt(query string, results []SearchResult, snippetCap int) string {
	var b strings.Builder
	b.WriteString("QUERY:\n")
	b.WriteString(query)
	b.WriteString("\n\nCANDIDATES:\n")
	for i, r := range results {
		snippet := r.Content
		if len(snippet) > snippetCap {
			snippet = snippet[:snippetCap]
		}
		fmt.Fprintf(&b, "[%d] (source: %s)\n%s\n\n", i, r.SourceName, snippet)
	}
	return b.String()
}

// parseRerankScores decodes the LLM's JSON response into a []float64
// indexed by candidate position. Missing entries default to 0 — they
// sink in the sort but stay in the result set.
func parseRerankScores(raw string, n int) ([]float64, error) {
	// Strip optional code fences the LLM might emit despite the prompt.
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)

	var payload struct {
		Scores map[string]float64 `json:"scores"`
	}
	if err := json.Unmarshal([]byte(s), &payload); err != nil {
		return nil, fmt.Errorf("rerank decode: %w", err)
	}
	if len(payload.Scores) == 0 {
		return nil, fmt.Errorf("rerank decode: empty scores object")
	}
	out := make([]float64, n)
	for k, v := range payload.Scores {
		var idx int
		if _, err := fmt.Sscanf(k, "%d", &idx); err != nil {
			continue
		}
		if idx < 0 || idx >= n {
			continue
		}
		if v < 0 {
			v = 0
		} else if v > 1 {
			v = 1
		}
		out[idx] = v
	}
	return out, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// rerankProjectID picks the project to attribute rerank spend to, and
// reports whether the candidate set was ambiguous about it.
//
// A search is project-scoped in practice, so the first non-empty
// ProjectID among the candidates is the right label. When none carry
// one the call is simply not recorded (recordUsage returns early on an
// empty projectID) — the row never lands, rather than landing and being
// filtered out at query time.
//
// ambiguous is true when the candidates name more than one distinct
// project. That should be impossible today, but the reranker cannot see
// the query's scope, so if cross-project search is ever added this would
// silently attribute all the spend to whichever project sorted first.
// The caller warns and bills anyway: one imperfectly-attributed row
// beats a lost one, and the warning is the paper trail if the
// assumption ever breaks.
func rerankProjectID(candidates []SearchResult) (projectID string, ambiguous bool) {
	for _, c := range candidates {
		if c.ProjectID == "" {
			continue
		}
		if projectID == "" {
			projectID = c.ProjectID
			continue
		}
		if c.ProjectID != projectID {
			ambiguous = true
		}
	}
	return projectID, ambiguous
}

// recordUsage persists one task_llm_usage row for a billed rerank call.
// Skipped when LLMUsage is nil, when the project cannot be attributed,
// or when the response carries zero tokens (defensive — a provider that
// doesn't populate Usage shouldn't pollute the dashboard with empty
// rows). Errors are swallowed: failing to bill is dashboard fidelity,
// not correctness, and the reranker must never fail a search.
//
// StepID is deliberately empty. The titler and classifier set it to a
// chunk id because each call concerns exactly one chunk; a rerank spans
// the whole candidate set, so there is no single chunk to name.
func (r *LLMReranker) recordUsage(ctx context.Context, resp *chat.ChatResponse, projectID string) {
	if r == nil || resp == nil || projectID == "" {
		return
	}
	model := resp.Model
	if model == "" {
		model = r.Model
	}
	// StepID deliberately empty: a rerank spans the whole candidate set, so no
	// single chunk owns the call.
	r.spend.Record(ctx, llmspend.Input{
		ProjectID:           projectID,
		Model:               model,
		PromptTokens:        resp.Usage.PromptTokens,
		CompletionTokens:    resp.Usage.CompletionTokens,
		CacheCreationTokens: resp.Usage.CacheCreationTokens,
		CacheReadTokens:     resp.Usage.CacheReadTokens,
	})
}

// RerankerStatus reports whether reranking will actually happen, and why not when
// it will not.
//
// Exists because the reranker failed SILENTLY: `reranker.enabled: true` was set,
// SetReranker was called, and across 151,818 production LLM-usage rows not one
// reranker call was ever made — with nothing anywhere stating which gate was
// closed. A feature that can be configured on and still do nothing needs to say so
// at wiring time, or the next person spends hours reading call graphs as well.
func (s *Searcher) RerankerStatus() (active bool, reason string) {
	if s == nil {
		return false, "searcher not built"
	}
	if s.reranker == nil {
		return false, "not wired"
	}
	if _, isNoop := s.reranker.(NoopReranker); isNoop {
		return false, "disabled or no chat client"
	}
	return true, "active"
}
