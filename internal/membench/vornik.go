package membench

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// The vornik adapter (design §5.3). Drives the companion MCP surface at
// /api/v1/mcp/companion — `remember` for ingest, `recall` for retrieval — over
// HTTP, the same transport the external adapter uses. One client code path for
// both systems is what stops an asymmetry hiding in the harness itself.

// rememberMaxContentBytes mirrors the daemon's per-deposit cap
// (internal/api/companion_mcp_memory.go). A haystack session larger than this
// must be SPLIT, never truncated: dropping the tail removes distractors and
// makes the item easier, which would inflate our score for a reason invisible in
// the results.
const rememberMaxContentBytes = 64 * 1024

// VornikConfig configures the adapter.
type VornikConfig struct {
	// BaseURL is the daemon root, e.g. http://192.0.2.10:8080.
	BaseURL string
	// Token is a companion key with memory_read + memory_write.
	Token string
	// Client allows a test server's client to be injected. Nil uses a default
	// with a generous timeout — ingest of a large haystack is not fast.
	Client *http.Client
	// ExtractionModel is recorded in the comparability key. The adapter cannot
	// discover it, so the caller supplies what it configured.
	ExtractionModel string
}

// VornikSystem implements MemorySystem against a running daemon.
type VornikSystem struct {
	cfg    VornikConfig
	client *http.Client
}

// NewVornikSystem builds the adapter.
func NewVornikSystem(cfg VornikConfig) *VornikSystem {
	c := cfg.Client
	if c == nil {
		c = &http.Client{Timeout: 5 * time.Minute}
	}
	return &VornikSystem{cfg: cfg, client: c}
}

// Name identifies the system in results and manifests.
func (v *VornikSystem) Name() string { return "vornik" }

// Prepare is a no-op: a repo_scope needs no creation, it comes into existence
// with the first chunk stamped with it. Kept to satisfy MemorySystem so the
// runner treats both systems identically.
func (v *VornikSystem) Prepare(_ context.Context, _ string) error { return nil }

// Teardown is likewise a no-op here. Scope cleanup happens once per run at the
// database level (the destructive-run guard in §5.8 is what makes that safe),
// not per item — 500 per-item deletions would dominate the run.
func (v *VornikSystem) Teardown(_ context.Context, _ string) error { return nil }

// Config reports the effective configuration for the comparability key. Only
// what the caller told us; the companion surface does not expose the daemon's
// model wiring, and inventing a value would be worse than admitting the gap.
func (v *VornikSystem) Config(_ context.Context) (string, error) {
	return v.cfg.ExtractionModel, nil
}

// Ingest deposits one item's haystack into the scope.
//
// Each Item becomes one or more `remember` calls: one when it fits the 64 KiB
// cap, several when it does not. The Context line is prepended to the body so
// both systems receive identical provenance framing rather than one of them
// getting a bare document.
func (v *VornikSystem) Ingest(ctx context.Context, scope string, items []Item) (IngestStats, error) {
	var stats IngestStats
	stats.ChunksStored = -1 // the companion surface does not report chunk counts
	start := time.Now()

	for _, item := range items {
		body := item.Content
		if item.Context != "" {
			body = item.Context + "\n\n" + item.Content
		}
		parts := splitForDeposit(body, rememberMaxContentBytes)
		if len(parts) > 1 {
			stats.Splits += len(parts) - 1
		}
		for i, part := range parts {
			args := map[string]any{
				"content":     part,
				"source_name": item.DocumentID,
				"repo_scope":  scope,
				"class":       "research",
			}
			if !item.EventTime.IsZero() {
				args["event_time"] = item.EventTime.UTC().Format(time.RFC3339)
			}
			var reply rememberReply
			if err := v.call(ctx, "remember", args, &reply); err != nil {
				return stats, fmt.Errorf("remember %s part %d: %w", item.DocumentID, i, err)
			}
			stats.Deposits++
			stats.Bytes += len(part)
			if reply.Rejected > 0 || strings.EqualFold(reply.Decision, "REJECTED") {
				stats.Rejected++
				// Count the bytes, not just the deposit: haystack loss is measured
				// in content, and one rejected 60 KiB session matters far more than
				// one rejected 200-byte one.
				stats.RejectedBytes += len(part)
			}
		}
	}
	stats.Latency = time.Since(start)
	return stats, nil
}

// Recall retrieves against the scope.
//
// strict_scope is pinned TRUE unconditionally. The default scoped query includes
// NULL-scoped chunks via the migration-grace `OR repo_scope IS NULL` clause,
// which would leak one benchmark item's haystack into another item's recall and
// silently score cross-contaminated retrieval (design §5.5).
func (v *VornikSystem) Recall(ctx context.Context, scope string, q Query) (Recalled, error) {
	args := map[string]any{
		"query":        q.Text,
		"repo_scope":   scope,
		"strict_scope": true,
		// The PRODUCTION context-assembly path: scored-sufficiency widening plus
		// the LLM reranker. Operator decision 2026-08-11 — the gate protects what
		// AGENTS retrieve, not the interactive operator recall path.
		//
		// Without this the harness measured single-shot RRF while the reranker never
		// fired (rerankOn := opts.Rerank && s.rerankerActive(), and the interactive
		// surface leaves Rerank false for latency). A gate on the wrong path can go
		// green while the path that matters regresses.
		"sufficient": true,
	}
	if q.MaxTokens > 0 {
		// The recall tool takes a result count, not a token budget. Convert with
		// a conservative chars-per-token estimate and record the budget too, so
		// the manifest shows the conversion happened rather than implying the
		// systems were asked in the same units.
		args["limit"] = tokenBudgetToLimit(q.MaxTokens)
		args["max_tokens"] = q.MaxTokens
	}
	if !q.From.IsZero() {
		args["from_date"] = q.From.UTC().Format(time.RFC3339)
	}
	if !q.To.IsZero() {
		args["to_date"] = q.To.UTC().Format(time.RFC3339)
	}

	start := time.Now()
	var reply recallReply
	if err := v.call(ctx, "recall", args, &reply); err != nil {
		return Recalled{}, err
	}
	out := Recalled{Latency: time.Since(start)}
	for _, h := range reply.Hits {
		out.Hits = append(out.Hits, Hit{
			// SourceName is the DOCUMENT identity the dataset labels; chunk_id is
			// not comparable to a gold document id.
			SourceID: h.SourceName,
			Text:     h.Content,
			Score:    h.Score,
		})
		out.Tokens += approxTokens(h.Content)
	}
	return out, nil
}

// splitForDeposit cuts s into pieces no larger than limit bytes, preferring a
// newline boundary so a split lands between turns rather than mid-word.
func splitForDeposit(s string, limit int) []string {
	if len(s) <= limit {
		return []string{s}
	}
	var parts []string
	for len(s) > limit {
		cut := strings.LastIndex(s[:limit], "\n")
		if cut <= 0 {
			// No boundary available; cut at the cap rather than emitting an
			// over-cap deposit the daemon would refuse outright.
			cut = limit
		}
		parts = append(parts, s[:cut])
		s = strings.TrimPrefix(s[cut:], "\n")
	}
	if s != "" {
		parts = append(parts, s)
	}
	return parts
}

// approxTokens estimates tokens from bytes at 4 bytes/token — the conventional
// English approximation. Only used for tier-3 reporting of budget utilisation,
// never for scoring, so the imprecision cannot move an accuracy number.
func approxTokens(s string) int { return (len(s) + 3) / 4 }

// tokenBudgetToLimit converts a token budget into a chunk count for a surface
// that only accepts top-k. Assumes ~512-token chunks (the configured default),
// with a floor of 1 so a small budget still asks for something.
func tokenBudgetToLimit(maxTokens int) int {
	n := maxTokens / 512
	if n < 1 {
		return 1
	}
	return n
}

type rememberReply struct {
	Decision string `json:"decision"`
	Admitted int    `json:"admitted"`
	Rejected int    `json:"rejected"`
}

type recallReply struct {
	Hits []struct {
		SourceName string  `json:"source_name"`
		Content    string  `json:"content"`
		Score      float64 `json:"score"`
	} `json:"hits"`
}

// call performs one JSON-RPC tools/call against the companion endpoint and
// decodes the tool's inner JSON payload into out.
func (v *VornikSystem) call(ctx context.Context, tool string, args map[string]any, out any) error {
	reqBody, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params":  map[string]any{"name": tool, "arguments": args},
	})
	if err != nil {
		return fmt.Errorf("marshal %s request: %w", tool, err)
	}

	url := strings.TrimRight(v.cfg.BaseURL, "/") + "/api/v1/mcp/companion"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("build %s request: %w", tool, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if v.cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+v.cfg.Token)
	}

	resp, err := v.client.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", tool, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// 429 and 402 are capacity refusals: terminal, never retried. Every other
		// non-200 is a plain error, because conflating a transient 500 with quota
		// exhaustion would abort a whole run over one blip.
		if resp.StatusCode == http.StatusTooManyRequests ||
			resp.StatusCode == http.StatusPaymentRequired {
			return errors.Join(
				fmt.Errorf("%s: http %d", tool, resp.StatusCode),
				ErrQuotaExhausted,
			)
		}
		return fmt.Errorf("%s: http %d", tool, resp.StatusCode)
	}

	var env struct {
		Error  *json.RawMessage `json:"error"`
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return fmt.Errorf("decode %s response: %w", tool, err)
	}
	if env.Error != nil {
		return fmt.Errorf("%s: json-rpc error: %s", tool, string(*env.Error))
	}
	if len(env.Result.Content) == 0 {
		return fmt.Errorf("%s: empty response content", tool)
	}
	text := env.Result.Content[0].Text
	if env.Result.IsError {
		// Surfaced, never treated as an empty result: reporting a permission
		// failure as "retrieved nothing" would score a harness fault as a
		// retrieval failure.
		return fmt.Errorf("%s: %s", tool, text)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal([]byte(text), out); err != nil {
		return fmt.Errorf("decode %s payload: %w", tool, err)
	}
	return nil
}

// memoryStatsReply is the shape of GET /api/v1/memory/stats.
type memoryStatsReply struct {
	Projects []struct {
		ProjectID      string `json:"projectId"`
		ChunksTotal    int    `json:"chunksTotal"`
		ChunksEmbedded int    `json:"chunksEmbedded"`
		QueueDepth     int    `json:"queueDepth"`
	} `json:"projects"`
	// Embedder is the daemon's RESOLVED embedder. Absent on a daemon that cannot
	// report one, which the caller must treat as unverified rather than as none.
	Embedder *struct {
		Provider   string `json:"provider"`
		Model      string `json:"model"`
		Dimensions int    `json:"dimensions"`
	} `json:"embedder"`
}

// ObservedEmbedder reports the embedder the DAEMON says it is using, satisfying
// EmbedderReporter.
//
// This is the fact the harness could not previously obtain: the companion surface
// exposed no model wiring, so a run's embedder was whatever an operator typed, and
// a titan arm and a cohere arm of an embedder comparison produced byte-identical
// metrics with nothing able to say which vectors either had queried.
//
// Returns empty with no error when the daemon does not report one — an older daemon
// is unverified, not misconfigured, and the comparability key marks it partial.
func (v *VornikSystem) ObservedEmbedder(ctx context.Context) (string, error) {
	out, err := v.fetchStats(ctx)
	if err != nil {
		return "", err
	}
	if out.Embedder == nil || strings.TrimSpace(out.Embedder.Model) == "" {
		return "", nil
	}
	e := out.Embedder
	id := strings.TrimSpace(e.Model)
	if p := strings.TrimSpace(e.Provider); p != "" {
		id = p + "/" + id
	}
	// The dimension travels with the id because two different models can share a
	// width, so the width alone cannot tell them apart — but a width that CHANGED
	// is a different vector space and must break comparability.
	if e.Dimensions > 0 {
		id = fmt.Sprintf("%s@%dd", id, e.Dimensions)
	}
	return id, nil
}

// fetchStats performs GET /api/v1/memory/stats once.
func (v *VornikSystem) fetchStats(ctx context.Context) (memoryStatsReply, error) {
	var out memoryStatsReply
	url := strings.TrimRight(v.cfg.BaseURL, "/") + "/api/v1/memory/stats"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return out, fmt.Errorf("build stats request: %w", err)
	}
	if v.cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+v.cfg.Token)
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return out, fmt.Errorf("memory stats: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("memory stats: http %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return out, fmt.Errorf("decode stats: %w", err)
	}
	return out, nil
}

// EmbeddingReadiness reports the fraction of this daemon's chunks that are
// semantically searchable, satisfying EmbeddingReadinessReporter.
//
// The harness ingests a corpus and immediately recalls it, but embedding is
// queued asynchronously. Without this, a run over a cold corpus reports
// keyword-only retrieval scores that look exactly like semantic ones — the first
// live run scored with 126 of 3,187 chunks embedded and nothing said so.
//
// Summed across projects rather than filtered to one: the adapter addresses the
// daemon by companion key, which binds it to a single project anyway, and summing
// keeps the number meaningful if that ever stops being true.
func (v *VornikSystem) EmbeddingReadiness(ctx context.Context) (float64, error) {
	url := strings.TrimRight(v.cfg.BaseURL, "/") + "/api/v1/memory/stats"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, fmt.Errorf("build stats request: %w", err)
	}
	if v.cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+v.cfg.Token)
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("memory stats: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("memory stats: http %d", resp.StatusCode)
	}
	var out memoryStatsReply
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, fmt.Errorf("decode memory stats: %w", err)
	}
	var total, embedded int
	for _, p := range out.Projects {
		total += p.ChunksTotal
		embedded += p.ChunksEmbedded
	}
	if total == 0 {
		// No content is not "0% ready" — there is nothing to be ready. Reporting 0
		// would make an empty store look like a broken embedder.
		return 0, fmt.Errorf("memory stats: no chunks stored")
	}
	return float64(embedded) / float64(total), nil
}
