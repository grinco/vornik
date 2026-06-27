package graph

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"

	"vornik.io/vornik/internal/chat"
)

//go:embed validator_prompt.txt
var validatorSystemPrompt string

// ValidatorThreshold is the default cutoff: edges scoring below
// this drop with a validation_dropped audit row. Per LLD §4.4
// the operator-tuneable bar is 0.7 — anything below requires "a
// leap of inference" the pipeline doesn't tolerate.
const ValidatorThreshold = 0.7

// ValidatedEdge wraps an EdgeProposal with the validator's score
// + the keep/drop decision. The orchestrator passes Kept ones to
// KnowledgeEdgeRepository.UpsertEdge with Faithfulness stamped
// from Score; Dropped ones land in an audit table for forensics.
type ValidatedEdge struct {
	Proposal EdgeProposal
	Score    float32
	Reason   string
	Kept     bool
}

// ValidateMetrics carries token + accept/reject counts back to
// the orchestrator. ScoreSum / ScoreCount let dashboards plot
// average faithfulness over time. DropsByReason breaks the
// Dropped aggregate into the two distinct failure modes — sum
// of values equals Dropped.
//
// Reason keys are the ValidatorDropReason* constants below.
// Audit context (2026-05-25): the relationship-stage per-reason
// metric (commit 62c3e50) made cosmetic-only drops measurable;
// this surface mirrors it on the LLM-faithfulness side so
// dashboards can spot when the validator silently truncates its
// JSON output (missing_score) vs when it genuinely scores low
// (below_threshold).
type ValidateMetrics struct {
	Model            string
	PromptTokens     int
	CompletionTokens int
	Kept             int
	Dropped          int
	ScoreSum         float32
	ScoreCount       int
	DropsByReason    map[string]int
}

// ValidatorDropReason* are the stable identifiers under which
// Validate reports per-rule drop counts. Closed set so the
// Prometheus label cardinality stays bounded.
const (
	// ValidatorDropReasonMissingScore — the LLM didn't echo
	// back a score for this proposal's ID. Failure mode:
	// truncated JSON output, rare but real on long batches.
	// Today fail-closed (drop with reason); the metric makes
	// the rate visible so re-prompt logic can be data-driven.
	ValidatorDropReasonMissingScore = "missing_score"
	// ValidatorDropReasonBelowThreshold — the LLM scored the
	// proposal but the score fell under the faithfulness cut
	// (default 0.7 per LLD §4.4). The "expected" drop path.
	ValidatorDropReasonBelowThreshold = "below_threshold"
)

// Validator implements Stage 4 of the KG pipeline: faithfulness
// scoring of relationship proposals against the source chunk.
//
// Critically separate from RelationshipExtractor — having one
// model propose AND approve is the most common hallucination
// failure mode in extraction pipelines. The validator gets a
// fresh look at the same chunk + just the proposed edges, not
// the full reasoning that produced them.
type Validator struct {
	Client      chat.Provider
	Model       string
	MaxAttempts int
	// Threshold overrides the default cutoff. 0 → ValidatorThreshold.
	Threshold float32
	// MaxChunkBytes mirrors the other stages. 0 → 8 KiB.
	MaxChunkBytes int
}

// NewValidator returns a Validator with safe defaults.
func NewValidator(client chat.Provider, model string) *Validator {
	return &Validator{Client: client, Model: model, MaxAttempts: 3, Threshold: ValidatorThreshold, MaxChunkBytes: 8 * 1024}
}

// Validate scores each proposal against the chunk and returns
// the per-proposal verdict (in input order). nil proposals
// short-circuits to (nil, empty metrics, nil) — no LLM call.
//
// The validator NEVER reorders, drops, or mutates EdgeProposal
// content; it only stamps Score / Reason / Kept. That keeps
// orchestrator zip logic dead simple (output[i] corresponds to
// input[i]).
func (v *Validator) Validate(ctx context.Context, content string, proposals []EdgeProposal) ([]ValidatedEdge, *ValidateMetrics, error) {
	if v == nil || v.Client == nil {
		return nil, nil, fmt.Errorf("Validator.Validate: client not configured")
	}
	metrics := &ValidateMetrics{Model: v.Model}
	if len(proposals) == 0 {
		return nil, metrics, nil
	}

	threshold := v.Threshold
	if threshold <= 0 {
		threshold = ValidatorThreshold
	}
	cap := v.MaxChunkBytes
	if cap <= 0 {
		cap = 8 * 1024
	}
	body := content
	if len(body) > cap {
		body = truncateUTF8Bytes(body, cap)
	}

	type promptItem struct {
		ID        string          `json:"id"`
		From      string          `json:"from"`
		To        string          `json:"to"`
		Predicate string          `json:"predicate"`
		Evidence  string          `json:"evidence"`
		Props     json.RawMessage `json:"properties,omitempty"`
	}
	items := make([]promptItem, len(proposals))
	for i, p := range proposals {
		items[i] = promptItem{
			ID: proposalID(i), From: p.From, To: p.To, Predicate: p.Predicate,
			Evidence: p.Evidence, Props: p.Properties,
		}
	}
	itemsJSON, _ := json.Marshal(items)
	user := "CHUNK:\n" + body + "\n\nPROPOSED RELATIONSHIPS:\n" + string(itemsJSON)

	maxAttempts := v.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	client := pickModel(v.Client, v.Model)

	resp, err := completeWithRetry(ctx, client, []chat.Message{
		{Role: "system", Content: validatorSystemPrompt},
		{Role: "user", Content: user},
	}, maxAttempts)
	if err != nil {
		return nil, metrics, fmt.Errorf("validator LLM call failed: %w", err)
	}
	metrics.PromptTokens = resp.Usage.PromptTokens
	metrics.CompletionTokens = resp.Usage.CompletionTokens
	if resp.Model != "" {
		metrics.Model = resp.Model
	}
	if len(resp.Choices) == 0 {
		return nil, metrics, fmt.Errorf("validator returned no choices")
	}
	raw := stripJSONFence(resp.Choices[0].Message.Content)
	scored, err := parseValidatorOutput(raw)
	if err != nil {
		return nil, metrics, fmt.Errorf("validator JSON parse: %w", err)
	}

	scoreByID := make(map[string]validatorScore, len(scored))
	for _, s := range scored {
		scoreByID[s.ID] = s
	}

	out := make([]ValidatedEdge, len(proposals))
	byReason := map[string]int{}
	for i, p := range proposals {
		ve := ValidatedEdge{Proposal: p}
		s, ok := scoreByID[proposalID(i)]
		if !ok {
			// Model dropped this slot. Default to "drop with
			// reason" rather than silently keeping it — fail
			// closed for faithfulness gating. The per-reason
			// counter quantifies how often this happens so a
			// future re-prompt path can be tuned against data.
			ve.Score = 0
			ve.Reason = "validator omitted score for this proposal"
			ve.Kept = false
			byReason[ValidatorDropReasonMissingScore]++
		} else {
			score := clampScore(s.Score)
			ve.Score = score
			ve.Reason = strings.TrimSpace(s.Reason)
			ve.Kept = score >= threshold
			if !ve.Kept {
				byReason[ValidatorDropReasonBelowThreshold]++
			}
		}
		if ve.Kept {
			metrics.Kept++
		} else {
			metrics.Dropped++
		}
		metrics.ScoreSum += ve.Score
		metrics.ScoreCount++
		out[i] = ve
	}
	metrics.DropsByReason = byReason
	return out, metrics, nil
}

type validatorScore struct {
	ID     string  `json:"id"`
	Score  float32 `json:"score"`
	Reason string  `json:"reason"`
}

func parseValidatorOutput(raw string) ([]validatorScore, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	if raw[0] == '[' {
		var arr []validatorScore
		if err := json.Unmarshal([]byte(raw), &arr); err != nil {
			return nil, err
		}
		return arr, nil
	}
	var wrap struct {
		Scores  []validatorScore `json:"scores"`
		Results []validatorScore `json:"results"`
	}
	if err := json.Unmarshal([]byte(raw), &wrap); err != nil {
		return nil, err
	}
	if len(wrap.Scores) > 0 {
		return wrap.Scores, nil
	}
	return wrap.Results, nil
}

// clampScore enforces 0..1. Some models emit on a 0..100 scale
// (>5 strongly suggests percent, since a 0..1 score above 5 is
// nonsensical); clamp small overshoots and small undershoots to
// the 0..1 range so one misbehaved row doesn't poison the rest.
func clampScore(s float32) float32 {
	if s > 5 && s <= 100 {
		s = s / 100
	}
	if s < 0 {
		return 0
	}
	if s > 1 {
		return 1
	}
	return s
}

// proposalID stamps a stable per-call ID the LLM echoes back so
// we can correlate scores by id even when the model emits them
// out of order.
func proposalID(i int) string {
	return fmt.Sprintf("prop-%d", i)
}
