package memory

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
)

// titlerRole is the value stored in task_llm_usage.role for every
// title-generation call. Matches the underscore convention used by
// the KG stages ("kg_extractor", "kg_resolver", …) so the spend
// dashboard groups all memory background consumers together.
const titlerRole = "memory_titler"

// UsageRecorder is the narrow interface the Titler needs from
// persistence.TaskLLMUsageRepository — only Record. Defined locally
// (mirroring graph.UsageRecorder) so the memory package does not
// pull in the full repository interface and so tests can supply a
// fake without depending on postgres.
type UsageRecorder interface {
	Record(ctx context.Context, u *persistence.TaskLLMUsage) error
}

// PricingTable mirrors graph.PricingTable / *pricing.Table.CostUSD.
// Local definition keeps the memory package independent of the
// pricing package's full surface; production wires *pricing.Table
// directly with no adapter.
type PricingTable interface {
	CostUSD(model string, promptTokens, completionTokens int) float64
}

// titlerSystemPrompt instructs the LLM to emit a short topic label.
// Closed-shape output — 3–7 words, no quotes, no trailing period —
// so the response parses without any JSON ceremony. We strip on the
// Go side anyway; the prompt is the first line of defence.
const titlerSystemPrompt = `You generate short, human-readable topic labels for fragments of text from a project's knowledge base.

Rules:
- Output a single line: 3 to 7 words.
- Title-case the label (e.g. "Quarterly Sales Forecast Methodology").
- No quotation marks, no trailing punctuation, no preamble.
- Describe the SUBJECT MATTER of the fragment, not its file name or its format.
- If the fragment is incoherent or empty, output exactly: Untitled Fragment

Examples:
INPUT: "# Q3 Pipeline Review\nThe sales team forecasted ..."
OUTPUT: Q3 Sales Pipeline Review

INPUT: "Helm chart values for the staging cluster including resource limits..."
OUTPUT: Staging Cluster Helm Values

INPUT: "kubectl logs my-pod --since=10m | grep ERROR"
OUTPUT: Pod Error Log Filtering`

// Titler turns a chunk preview into a short topic label using an LLM.
// Display-only — failures fall back to whatever the caller has on hand
// (markdown heading, source filename). Mirrors graph.Extractor in shape
// so operators get a consistent mental model: chat.Provider + per-call
// model override via chat.ModelOverridable.
type Titler struct {
	// Client is the chat provider. Required.
	Client chat.Provider
	// Model is the model identifier passed via ModelOverridable when
	// the provider supports it. Empty leaves the provider's own
	// default in place.
	Model string
	// MaxAttempts caps retries on transient LLM errors. 0 → 2.
	// Smaller than the extractor (which retries 3×) because titles
	// are not load-bearing — a failed title falls back gracefully.
	MaxAttempts int
	// MaxPreviewBytes truncates the chunk before sending. 0 → 2 KiB.
	// The viz preview is already capped at 600 chars upstream, but
	// the backfill path reads `content` directly so this is the
	// real bound.
	MaxPreviewBytes int
	// Timeout per LLM call. 0 → 30s. gpt-oss:120b can run a few
	// seconds per call on a busy gateway; cap so a stuck endpoint
	// doesn't stall the ingest worker indefinitely.
	Timeout time.Duration

	// LLMUsage records one task_llm_usage row per successful title
	// call so the operator UI's spend dashboards (/ui/spend, the
	// per-project cost breakdown) attribute titler cost to the
	// "memory_titler" role. Optional — nil-safe; tests leave it
	// nil and production wires *postgres.TaskLLMUsageRepository.
	LLMUsage UsageRecorder
	// Pricing computes USD from the model's token counts. nil →
	// cost_usd is stamped 0 so the row still lands (token volume
	// remains visible) on un-priced models.
	Pricing PricingTable
	// Cache memoises (model, system+user prompt) → raw response so
	// re-runs (e.g. vornikctl memory backfill-titles after restart)
	// skip the upstream LLM call. Optional — nil disables. See
	// llm-caching-design.md Phase E.
	Cache ResponseCache
}

// NewTitler builds a Titler with sane defaults.
func NewTitler(client chat.Provider, model string) *Titler {
	return &Titler{
		Client:          client,
		Model:           model,
		MaxAttempts:     2,
		MaxPreviewBytes: 2 * 1024,
		Timeout:         30 * time.Second,
	}
}

// Title returns a short topic label for the given chunk text. On any
// failure (LLM error, empty/garbled response, nil receiver) it returns
// "" so callers can fall back without branching on err. Errors are
// still returned so the backfill CLI can report them — ingest can
// safely discard.
//
// projectID and chunkID are stamped on the task_llm_usage row when
// LLMUsage is configured. Both may be "" — in that case usage
// recording is skipped (useful for tests). The row carries TaskID=NULL
// (background consumer, like the KG pipeline) and ExecutionID=NULL;
// project_id + step_id (= chunkID) are the load-bearing attribution
// labels on the spend dashboard.
func (t *Titler) Title(ctx context.Context, content, projectID, chunkID string) (string, error) {
	if t == nil || t.Client == nil {
		return "", fmt.Errorf("Titler.Title: client not configured")
	}
	body := strings.TrimSpace(content)
	if body == "" {
		return "", nil
	}
	cap := t.MaxPreviewBytes
	if cap <= 0 {
		cap = 2 * 1024
	}
	if len(body) > cap {
		body = truncateUTF8Bytes(body, cap)
	}

	timeout := t.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	callCtx = chat.WithCallSite(callCtx, "memory.titler")
	defer cancel()

	msgs := []chat.Message{
		{Role: "system", Content: titlerSystemPrompt},
		{Role: "user", Content: "FRAGMENT:\n" + body},
	}
	client := pickModelForTitler(t.Client, t.Model)

	// Phase E response cache: skip the LLM entirely on a hit. The
	// stored content is the raw model output; cleanTitle runs again
	// here so cleaner-rule edits remain effective on cached rows.
	// Hit counts persist via llm_response_cache.hit_count (surfaced
	// through CacheStats) — no separate Prometheus counter, mirroring
	// the Phase D embedding cache.
	cacheKey := ""
	if t.Cache != nil {
		cacheKey = ResponseCacheKey(t.Model, ResponseCachePurposeTitler, msgs)
		if raw, _, _, hit, _ := t.Cache.Get(callCtx, cacheKey); hit {
			if cleaned := cleanTitle(raw); cleaned != "" {
				return cleaned, nil
			}
			// Cached row produces nothing usable — fall through and
			// re-call the LLM. Could happen if cleanTitle rules
			// tightened after the row was written.
		}
	}

	maxAttempts := t.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 2
	}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		resp, err := client.Complete(callCtx, msgs)
		if err == nil && resp != nil && len(resp.Choices) > 0 {
			raw := resp.Choices[0].Message.Content
			cleaned := cleanTitle(raw)
			if cleaned != "" {
				// Bill before returning so the row lands even if the
				// caller discards the result. Non-fatal — dashboards
				// can miss a row, but the title still goes back.
				t.recordUsage(ctx, resp, projectID, chunkID)
				if t.Cache != nil && cacheKey != "" {
					_ = t.Cache.Put(ctx, cacheKey, t.Model, ResponseCachePurposeTitler,
						raw, resp.Usage.PromptTokens, resp.Usage.CompletionTokens)
				}
				return cleaned, nil
			}
			// We DID spend tokens on this call; record them so the
			// dashboard reflects the real bill, then surface the
			// "unusable" error to the caller.
			t.recordUsage(ctx, resp, projectID, chunkID)
			lastErr = fmt.Errorf("titler: empty/unusable response")
			break
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("titler: no choices in response")
		}
		if callCtx.Err() != nil {
			return "", lastErr
		}
		if attempt == maxAttempts {
			break
		}
		select {
		case <-callCtx.Done():
			return "", callCtx.Err()
		case <-time.After(time.Duration(attempt) * 500 * time.Millisecond):
		}
	}
	return "", lastErr
}

func truncateUTF8Bytes(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	end := maxBytes
	for end > 0 && !utf8.ValidString(s[:end]) {
		end--
	}
	return s[:end]
}

// recordUsage persists one task_llm_usage row for a billed call.
// Skipped when LLMUsage is nil, when both attribution labels are
// empty (test paths), or when the response carries zero tokens
// (defensive — a provider that doesn't populate Usage shouldn't
// pollute the dashboard with empty rows). Errors are swallowed:
// failing to bill is dashboard fidelity, not correctness.
func (t *Titler) recordUsage(ctx context.Context, resp *chat.ChatResponse, projectID, chunkID string) {
	if t == nil || t.LLMUsage == nil || resp == nil {
		return
	}
	if projectID == "" && chunkID == "" {
		return
	}
	prompt := resp.Usage.PromptTokens
	completion := resp.Usage.CompletionTokens
	if prompt == 0 && completion == 0 {
		return
	}
	model := resp.Model
	if model == "" {
		model = t.Model
	}
	var costUSD float64
	if t.Pricing != nil {
		costUSD = t.Pricing.CostUSD(model, prompt, completion)
	}
	row := &persistence.TaskLLMUsage{
		ID:                  persistence.GenerateID("llm"),
		ProjectID:           projectID,
		TaskID:              nil, // background consumer, no task scope
		ExecutionID:         nil,
		StepID:              chunkID,
		Role:                titlerRole,
		Model:               model,
		PromptTokens:        int64(prompt),
		CompletionTokens:    int64(completion),
		Iterations:          1,
		CostUSD:             costUSD,
		Source:              persistence.TaskLLMUsageSourceMemoryTitler,
		CacheCreationTokens: int64(resp.Usage.CacheCreationTokens),
		CacheReadTokens:     int64(resp.Usage.CacheReadTokens),
	}
	_ = t.LLMUsage.Record(ctx, row)
}

// pickModelForTitler applies a per-call model override when the
// provider supports it. Duplicated from graph.pickModel rather than
// exported across packages — the chat package is the right home for
// this helper, but pulling it up is out of scope.
func pickModelForTitler(client chat.Provider, model string) chat.Provider {
	if model == "" {
		return client
	}
	if mo, ok := client.(chat.ModelOverridable); ok {
		return mo.WithModel(model)
	}
	return client
}

// cleanTitle normalizes an LLM response into a display-safe title.
// Strips code fences, quotes, trailing punctuation, and limits the
// label to a single line of <=80 chars. Returns "" if nothing usable
// remains, which the caller treats as "fall back".
func cleanTitle(raw string) string {
	s := strings.TrimSpace(raw)
	// Strip code fences. We strip the prefix/suffix BEFORE collapsing
	// to one line so fenced responses ("```\nTopic\n```") survive —
	// otherwise the first-line cut hits the empty leading line.
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)
	// First line only — guards against models that bolt on a
	// "Reason: ..." explanation despite the prompt.
	if idx := strings.IndexAny(s, "\r\n"); idx >= 0 {
		s = s[:idx]
	}
	s = strings.TrimSpace(s)
	// Strip surrounding quotes (single, double, smart).
	for _, pair := range [][2]string{
		{`"`, `"`}, {`'`, `'`}, {"“", "”"}, {"‘", "’"},
	} {
		if strings.HasPrefix(s, pair[0]) && strings.HasSuffix(s, pair[1]) && len(s) > len(pair[0])+len(pair[1]) {
			s = strings.TrimSuffix(strings.TrimPrefix(s, pair[0]), pair[1])
			s = strings.TrimSpace(s)
		}
	}
	// Drop a "OUTPUT:" / "Label:" prefix if the model echoes one.
	for _, prefix := range []string{"OUTPUT:", "Output:", "LABEL:", "Label:", "TITLE:", "Title:"} {
		if strings.HasPrefix(s, prefix) {
			s = strings.TrimSpace(strings.TrimPrefix(s, prefix))
		}
	}
	// Strip a trailing period — but keep ?/! since some topics use them.
	s = strings.TrimRight(s, ".")
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// Bound the length — pathological responses don't get to blow up
	// the viz layout.
	if len(s) > 80 {
		s = strings.TrimSpace(s[:80])
	}
	// Reject "responses" that are clearly not a label: anything with
	// no letters at all, or that starts with markdown noise.
	hasLetter := false
	for _, r := range s {
		if unicode.IsLetter(r) {
			hasLetter = true
			break
		}
	}
	if !hasLetter {
		return ""
	}
	return s
}
