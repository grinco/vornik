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
func NewConfiguredReranker(enabled bool, client chat.Provider, model string, maxCandidates, timeoutSeconds, maxSnippetBytes int, logger zerolog.Logger) Reranker {
	if !enabled || client == nil {
		return NoopReranker{}
	}
	return &LLMReranker{
		Client:          client,
		Model:           model,
		Timeout:         time.Duration(timeoutSeconds) * time.Second,
		MaxCandidates:   maxCandidates,
		MaxSnippetBytes: maxSnippetBytes,
		Logger:          logger,
	}
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
}

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
