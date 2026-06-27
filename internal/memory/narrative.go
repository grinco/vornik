package memory

import (
	"context"
	"fmt"
	"strings"
	"time"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
)

// narrativeRole is the value stored in task_llm_usage.role for every
// LLM-tier consolidation call. Matches Titler's underscore convention
// so the spend dashboard groups all memory background consumers
// together.
const narrativeRole = "memory_narrative"

// narrativeSystemPrompt instructs the LLM to write a short summary
// of a project. Closed-shape output — 1-3 sentences, plain prose —
// so the response parses without JSON ceremony.
const narrativeSystemPrompt = `You write short natural-language summaries of a project's knowledge base. You receive:

- A ranked list of the most frequent meaningful terms across the project's chunks.
- A small sample of representative chunk excerpts.

Rules:
- Output 1 to 3 sentences. Plain prose. No bullet points, no headings, no preamble.
- Describe what the PROJECT is about, not the individual chunks. Generalise across the sample.
- Mention 2-4 of the most distinctive concrete topics from the term list when possible.
- If the sample is too thin to summarise honestly, output exactly: Project has too few chunks for a useful summary.

Examples:
INPUT: terms=[trading, ibkr, options, bracket, stop_loss]
       sample="Submitted bracket order for NVDA 100 shares with 2% stop..."
OUTPUT: An automated equities trading project focused on IBKR bracket orders, with emphasis on stop-loss management for high-conviction names.

INPUT: terms=[gmail, label, threading]
       sample="Apply 'Newsletter' label to every message from..."
OUTPUT: An email automation project for routing incoming messages by label and preserving thread context.`

// NarrativeWriter produces a short natural-language summary of a
// project from its term-frequency gist + a sample of chunks. The
// LLM-free Consolidator stays the primary tier; this layer is
// opt-in (operator config) and runs on a slower cadence.
//
// Shape mirrors Titler / Classifier so operators get a consistent
// mental model: chat.Provider + per-call model override via
// chat.ModelOverridable + per-row task_llm_usage attribution.
type NarrativeWriter struct {
	// Client is the chat provider. Required.
	Client chat.Provider
	// Model is the model identifier passed via ModelOverridable
	// when the provider supports it. Empty leaves the provider's
	// own default in place.
	Model string
	// MaxAttempts caps retries on transient LLM errors. 0 → 2.
	MaxAttempts int
	// Timeout per LLM call. 0 → 30s.
	Timeout time.Duration

	// LLMUsage records one task_llm_usage row per successful call
	// so the spend dashboard attributes narrative cost to the
	// "memory_narrative" role. Optional — nil-safe.
	LLMUsage UsageRecorder
	// Pricing computes USD from token counts. nil → cost_usd
	// is stamped 0.
	Pricing PricingTable
}

// NewNarrativeWriter builds one with sensible defaults.
func NewNarrativeWriter(client chat.Provider, model string) *NarrativeWriter {
	return &NarrativeWriter{
		Client:      client,
		Model:       model,
		MaxAttempts: 2,
		Timeout:     30 * time.Second,
	}
}

// Write returns a short summary for a project. terms is the
// ranked list from a recent ProjectGist; sample is up to N
// chunk excerpts joined with newlines. On any failure (nil
// client, empty input, LLM error, blank response) Write
// returns an empty string. Errors are still returned so the
// worker can log + count them; callers that don't care can
// safely discard.
//
// projectID is stamped on the task_llm_usage row when LLMUsage
// is configured. Empty projectID skips the usage record (useful
// for tests).
func (w *NarrativeWriter) Write(ctx context.Context, terms []TermFrequency, sample, projectID string) (string, error) {
	if w == nil || w.Client == nil {
		return "", fmt.Errorf("NarrativeWriter.Write: client not configured")
	}
	user := buildNarrativeUserMessage(terms, sample)
	if strings.TrimSpace(user) == "" {
		return "", nil
	}
	timeout := w.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	callCtx = chat.WithCallSite(callCtx, "memory.narrative")
	defer cancel()

	msgs := []chat.Message{
		{Role: "system", Content: narrativeSystemPrompt},
		{Role: "user", Content: user},
	}
	client := pickModelForNarrative(w.Client, w.Model)

	maxAttempts := w.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 2
	}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		resp, err := client.Complete(callCtx, msgs)
		if err == nil && resp != nil && len(resp.Choices) > 0 {
			cleaned := cleanNarrative(resp.Choices[0].Message.Content)
			if cleaned != "" {
				w.recordUsage(ctx, resp, projectID)
				return cleaned, nil
			}
			// Empty cleaned output — surface as error but record
			// usage; we still paid the tokens.
			w.recordUsage(ctx, resp, projectID)
			return "", fmt.Errorf("NarrativeWriter.Write: empty cleaned response")
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("NarrativeWriter.Write: no choices in response")
		}
	}
	return "", lastErr
}

// buildNarrativeUserMessage assembles the prompt body sent to the
// LLM. terms goes through a compact ranked-list render
// ("term=count, …"), sample goes verbatim under a clear delimiter.
// Empty terms + empty sample yields "" so Write short-circuits
// to "no input".
func buildNarrativeUserMessage(terms []TermFrequency, sample string) string {
	var b strings.Builder
	if len(terms) > 0 {
		b.WriteString("TOP_TERMS:\n")
		for i, t := range terms {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(t.Term)
			b.WriteString("=")
			fmt.Fprintf(&b, "%d", t.Count)
		}
		b.WriteString("\n")
	}
	if strings.TrimSpace(sample) != "" {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("SAMPLE:\n")
		b.WriteString(sample)
	}
	return strings.TrimSpace(b.String())
}

// cleanNarrative tidies the LLM response: trims whitespace, drops
// surrounding quotes, collapses runs of whitespace. Empty after
// tidying returns "".
func cleanNarrative(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// Strip a single pair of surrounding quotes if the model
	// disregarded "no quotation marks".
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		s = s[1 : len(s)-1]
		s = strings.TrimSpace(s)
	}
	// Collapse internal whitespace runs to a single space; LLMs
	// occasionally emit double-newlines that wreck the UI panel
	// layout.
	fields := strings.Fields(s)
	return strings.Join(fields, " ")
}

// pickModelForNarrative wraps the chat client with the requested
// model when the provider implements ModelOverridable. Mirrors
// Titler's pickModelForTitler.
func pickModelForNarrative(client chat.Provider, model string) chat.Provider {
	if strings.TrimSpace(model) == "" {
		return client
	}
	if ov, ok := client.(chat.ModelOverridable); ok {
		return ov.WithModel(model)
	}
	return client
}

// recordUsage stamps one task_llm_usage row when LLMUsage is
// configured. Best-effort — a failed record is silently dropped
// (the spend dashboard misses a row but the narrative still
// reaches the operator).
func (w *NarrativeWriter) recordUsage(ctx context.Context, resp *chat.ChatResponse, projectID string) {
	if w == nil || w.LLMUsage == nil || resp == nil || projectID == "" {
		return
	}
	pt, ct := resp.Usage.PromptTokens, resp.Usage.CompletionTokens
	if pt == 0 && ct == 0 {
		return
	}
	model := resp.Model
	if model == "" {
		model = w.Model
	}
	cost := 0.0
	if w.Pricing != nil {
		cost = w.Pricing.CostUSD(model, pt, ct)
	}
	row := &persistence.TaskLLMUsage{
		ID:                  persistence.GenerateID("llm"),
		ProjectID:           projectID,
		TaskID:              nil,
		ExecutionID:         nil,
		StepID:              "",
		Role:                narrativeRole,
		Model:               model,
		PromptTokens:        int64(pt),
		CompletionTokens:    int64(ct),
		Iterations:          1,
		CostUSD:             cost,
		Source:              persistence.TaskLLMUsageSourceMemoryNarrative,
		CacheCreationTokens: int64(resp.Usage.CacheCreationTokens),
		CacheReadTokens:     int64(resp.Usage.CacheReadTokens),
	}
	_ = w.LLMUsage.Record(ctx, row)
}
