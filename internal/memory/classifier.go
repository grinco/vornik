package memory

import (
	"context"
	"fmt"
	"strings"
	"time"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
)

// classifierRole is the value stored in task_llm_usage.role for every
// LLM classify call. Mirrors titlerRole / kg_extractor so the spend
// dashboard groups every memory background consumer under one
// "memory_*" prefix.
const classifierRole = "memory_classifier"

// classifierSystemPrompt asks the LLM to pick exactly one
// ContentClass for a chunk. Closed-shape output — one lowercase
// token, no punctuation — so the parse is a strict equality check
// against IsValidClass. We strip on the Go side anyway; the prompt
// is the first line of defence against verbose models.
//
// The class list MUST stay in sync with class.go's ContentClass
// constants. Each is listed with a short purpose line so the model
// has the operator's mental model embedded rather than guessing
// what "diagnostic" means in this corpus.
const classifierSystemPrompt = `You classify text fragments from a project's knowledge base into ONE content class.

Available classes (output exactly one of these tokens, lowercase, nothing else):

- research        : investigation, scraping output, scoring, prior-art synthesis
- spec            : formal specifications, plans, requirements, structured proposals
- decision        : authoritative rulings, approvals, design choices, go/no-go calls
- commit_msg      : short after-the-fact records of completed work (commits, fills, executions)
- diagnostic      : failure dumps, error reports, test outcomes, debugging output
- external_fetch  : content fetched from a third party (web pages, OCR'd images, API responses)
- summary         : digests, weekly/daily reports, cross-portal roll-ups
- unclassified    : when none of the above clearly fits, or the fragment is too short / incoherent

Rules:
- Output a single line: just the class token, lowercase, no punctuation, no preamble.
- If the fragment is incoherent, very short, or genuinely ambiguous, output: unclassified
- Do NOT invent new class names. Pick from the eight above only.

Examples:
INPUT: "## Q3 Sales Forecast\nSales team forecasted 12% YoY growth..."
OUTPUT: research

INPUT: "Approved: switch the retry layer to a single coordinator. Reason: ..."
OUTPUT: decision

INPUT: "fix(executor): handle nil pipeline in runVerifiers — was panicking when ..."
OUTPUT: commit_msg

INPUT: "Traceback (most recent call last):\n  File \"foo.py\" ..."
OUTPUT: diagnostic

INPUT: "Fetched https://example.com/jobs.json — 47 listings, status 200"
OUTPUT: external_fetch`

// Classifier turns a chunk's content + light provenance hints into
// one of the eight ContentClass values via an LLM. Used by
// vornikctl memory reclassify --use-llm to handle chunks that the
// deterministic role map left unclassified. Mirrors Titler in shape
// (chat.Provider + per-call model override + retry/timeout knobs
// + optional usage recording).
//
// Failure mode: any LLM error, empty response, or invalid class
// returns ClassUnclassified + an error. The caller's safe default
// is "leave the chunk where it was"; this surface exists to LIFT
// classifications, not to demote them.
type Classifier struct {
	// Client is the chat provider. Required.
	Client chat.Provider
	// Model is the model identifier passed via ModelOverridable when
	// the provider supports it. Empty leaves the provider's own
	// default in place.
	Model string
	// MaxAttempts caps retries on transient LLM errors. 0 → 2.
	// Matches Titler's retry budget — classification is similarly
	// not load-bearing for any single chunk.
	MaxAttempts int
	// MaxPreviewBytes truncates the chunk content before sending.
	// 0 → 2 KiB. Mirrors Titler's cap; classification rarely needs
	// more than the first few paragraphs to pick a class.
	MaxPreviewBytes int
	// Timeout per LLM call. 0 → 30s. Same rationale as Titler.
	Timeout time.Duration

	// LLMUsage records one task_llm_usage row per call so the
	// operator UI's spend dashboards attribute classifier cost to
	// the "memory_classifier" role. Optional — nil-safe.
	LLMUsage UsageRecorder
	// Pricing computes USD from the model's token counts. nil →
	// cost_usd is stamped 0 (token volume still visible).
	Pricing PricingTable
	// Cache memoises (model, system+user prompt) → raw response so
	// `vornikctl memory reclassify` reruns skip the upstream LLM
	// call. Optional — nil disables. See llm-caching-design.md
	// Phase E.
	Cache ResponseCache
}

// NewClassifier builds a Classifier with sane defaults.
func NewClassifier(client chat.Provider, model string) *Classifier {
	return &Classifier{
		Client:          client,
		Model:           model,
		MaxAttempts:     2,
		MaxPreviewBytes: 2 * 1024,
		Timeout:         30 * time.Second,
	}
}

// Classify returns the best-fit ContentClass for the given chunk
// fragment. Provenance hints (sourceName, producerRole) are included
// in the user message so the LLM can pick up filename conventions
// and producer identity when the content alone is ambiguous. Returns
// ClassUnclassified + a non-nil error on any failure path (LLM
// error, empty response, unknown class) so callers can branch on the
// error if they want to distinguish "model said unclassified" from
// "model didn't respond at all".
//
// projectID + chunkID are stamped on the task_llm_usage row when
// LLMUsage is wired. Both may be "" — usage recording is skipped in
// that case (useful for tests).
func (c *Classifier) Classify(
	ctx context.Context,
	content, sourceName, producerRole, projectID, chunkID string,
) (ContentClass, error) {
	if c == nil || c.Client == nil {
		return ClassUnclassified, fmt.Errorf("Classifier.Classify: client not configured")
	}
	body := strings.TrimSpace(content)
	if body == "" {
		return ClassUnclassified, nil
	}
	cap := c.MaxPreviewBytes
	if cap <= 0 {
		cap = 2 * 1024
	}
	if len(body) > cap {
		body = truncateUTF8Bytes(body, cap)
	}

	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	callCtx = chat.WithCallSite(callCtx, "memory.classifier")
	defer cancel()

	user := buildClassifierUserPrompt(body, sourceName, producerRole)
	msgs := []chat.Message{
		{Role: "system", Content: classifierSystemPrompt},
		{Role: "user", Content: user},
	}
	client := pickModelForTitler(c.Client, c.Model) // shared helper from titler.go

	// Phase E response cache: skip the LLM on a hit. The cached
	// content is the raw model output; parseClassifierResponse runs
	// again on hit so parser changes remain effective.
	cacheKey := ""
	if c.Cache != nil {
		cacheKey = ResponseCacheKey(c.Model, ResponseCachePurposeClassifier, msgs)
		if raw, _, _, hit, _ := c.Cache.Get(callCtx, cacheKey); hit {
			if cleaned, ok := parseClassifierResponse(raw); ok {
				return cleaned, nil
			}
			// Cached row no longer parseable — fall through.
		}
	}

	maxAttempts := c.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 2
	}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		resp, err := client.Complete(callCtx, msgs)
		if err == nil && resp != nil && len(resp.Choices) > 0 {
			raw := resp.Choices[0].Message.Content
			cleaned, ok := parseClassifierResponse(raw)
			if ok {
				// Bill the successful call so the dashboard reflects
				// the real spend; matches Titler's order.
				c.recordUsage(ctx, resp, projectID, chunkID)
				if c.Cache != nil && cacheKey != "" {
					_ = c.Cache.Put(ctx, cacheKey, c.Model, ResponseCachePurposeClassifier,
						raw, resp.Usage.PromptTokens, resp.Usage.CompletionTokens)
				}
				return cleaned, nil
			}
			// Tokens were spent on an unusable answer; record cost
			// so the dashboard isn't undercounted.
			c.recordUsage(ctx, resp, projectID, chunkID)
			lastErr = fmt.Errorf("classifier: unrecognised class in response: %q",
				truncForClassifier(raw, 80))
			break
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("classifier: no choices in response")
		}
		if callCtx.Err() != nil {
			return ClassUnclassified, lastErr
		}
		if attempt == maxAttempts {
			break
		}
		select {
		case <-callCtx.Done():
			return ClassUnclassified, callCtx.Err()
		case <-time.After(time.Duration(attempt) * 500 * time.Millisecond):
		}
	}
	return ClassUnclassified, lastErr
}

// buildClassifierUserPrompt assembles the user-side message. Source
// + role go on labelled lines so the model treats them as routing
// hints rather than as part of the content body. Empty hints are
// omitted so the prompt doesn't carry "Source: " on its own line.
func buildClassifierUserPrompt(content, sourceName, producerRole string) string {
	var b strings.Builder
	if s := strings.TrimSpace(sourceName); s != "" {
		b.WriteString("Source: ")
		b.WriteString(s)
		b.WriteByte('\n')
	}
	if r := strings.TrimSpace(producerRole); r != "" {
		b.WriteString("Producer role: ")
		b.WriteString(r)
		b.WriteByte('\n')
	}
	if b.Len() > 0 {
		b.WriteByte('\n')
	}
	b.WriteString("FRAGMENT:\n")
	b.WriteString(content)
	return b.String()
}

// parseClassifierResponse extracts a known ContentClass from a raw
// LLM response. Returns (ClassUnclassified, false) when the response
// can't be mapped to a built-in class — the caller treats that as a
// retryable failure. Tolerant of common LLM noise: code fences,
// surrounding quotes, trailing periods, "OUTPUT:" prefixes.
func parseClassifierResponse(raw string) (ContentClass, bool) {
	s := strings.TrimSpace(raw)
	// Strip code fences.
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)
	// First line only — guards against verbose "Reason: ..." trailers.
	if idx := strings.IndexAny(s, "\r\n"); idx >= 0 {
		s = s[:idx]
	}
	s = strings.TrimSpace(s)
	// Strip optional "OUTPUT:"/"CLASS:" prefix.
	for _, p := range []string{"OUTPUT:", "Output:", "CLASS:", "Class:", "class:"} {
		if strings.HasPrefix(s, p) {
			s = strings.TrimSpace(strings.TrimPrefix(s, p))
		}
	}
	// Strip surrounding quotes / backticks / trailing punctuation.
	s = strings.Trim(s, `"'`+"`")
	s = strings.TrimRight(s, ".")
	s = strings.TrimSpace(s)
	s = strings.ToLower(s)
	if !IsValidClass(s) {
		return ClassUnclassified, false
	}
	return ContentClass(s), true
}

// recordUsage persists one task_llm_usage row for a billed call.
// Skipped when LLMUsage is nil, when both attribution labels are
// empty (test paths), or when the response carries zero tokens.
// Errors are swallowed: failing to bill is dashboard fidelity, not
// correctness.
func (c *Classifier) recordUsage(ctx context.Context, resp *chat.ChatResponse, projectID, chunkID string) {
	if c == nil || c.LLMUsage == nil || resp == nil {
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
		model = c.Model
	}
	var costUSD float64
	if c.Pricing != nil {
		costUSD = c.Pricing.CostUSD(model, prompt, completion)
	}
	row := &persistence.TaskLLMUsage{
		ID:                  persistence.GenerateID("llm"),
		ProjectID:           projectID,
		TaskID:              nil,
		ExecutionID:         nil,
		StepID:              chunkID,
		Role:                classifierRole,
		Model:               model,
		PromptTokens:        int64(prompt),
		CompletionTokens:    int64(completion),
		Iterations:          1,
		CostUSD:             costUSD,
		Source:              persistence.TaskLLMUsageSourceMemoryClassifier,
		CacheCreationTokens: int64(resp.Usage.CacheCreationTokens),
		CacheReadTokens:     int64(resp.Usage.CacheReadTokens),
	}
	_ = c.LLMUsage.Record(ctx, row)
}

func truncForClassifier(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
