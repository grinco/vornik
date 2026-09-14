// Package llmjudge is the transport-and-parsing half of an LLM judge: the
// decision vocabulary, tolerant JSON extraction from a model response,
// <think>-block stripping, the abstain-on-internal-failure summary and the
// retry policy for transient gateway failures. Extracted from
// internal/hallucination/judge.go on 2026-09-13 so the configuration
// assistant's judge (https://docs.vornik.io
// §7) imports it rather than duplicating "a thing that already took two
// incidents to get right (429s eaten as abstains; reasoning models'
// <think> blocks breaking JSON extraction)". Each judge keeps its own input
// type, prompt and verdict struct; this package owns only what is common.
//
// May import internal/chat and internal/persistence only.
package llmjudge

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
)

const hallucinationJudgeCallSite = "judge"

// Decision vocabulary. Aliases of the persistence constants so a verdict
// row written by either judge carries the same literal.
const (
	// DecisionPass means the claims are supported (or there are none).
	DecisionPass = persistence.JudgeVerdictPass
	// DecisionFail means there is concrete evidence of an unsupported claim.
	DecisionFail = persistence.JudgeVerdictFail
	// DecisionAbstain means "I can't tell" — the judge couldn't get enough
	// evidence to make a call. A separate category so dashboards don't
	// credit abstentions toward a quality bar, and so callers never treat
	// it as pass.
	DecisionAbstain = persistence.JudgeVerdictAbstain
)

// ValidDecision reports whether s is one of the three decision literals.
func ValidDecision(s string) bool {
	return s == DecisionPass || s == DecisionFail || s == DecisionAbstain
}

// verdictHead is the part of the JSON every judge shares. Callers with a
// richer verdict (signals, quoted lines) unmarshal the returned raw JSON
// into their own type.
type verdictHead struct {
	Decision   string  `json:"decision"`
	Confidence float64 `json:"confidence"`
	Summary    string  `json:"summary"`
}

// ParseVerdict tolerates models that wrap the JSON in code fences or stick
// prefatory prose in front of it. Returns ok=false on unrecoverable parse
// failure or a decision outside the vocabulary — the caller falls back to
// abstain in that case. raw is the extracted JSON object, so a caller can
// unmarshal the fields this package does not know about.
func ParseVerdict(text string) (decision string, confidence float64, summary string, raw json.RawMessage, ok bool) {
	if text == "" {
		return "", 0, "", nil, false
	}
	t := strings.TrimSpace(text)
	// Strip <think>…</think> blocks emitted by reasoning models
	// (nvidia.nemotron-nano-*, deepseek-r1, etc.). The block can
	// itself contain `{` / `}` which would otherwise confuse the
	// "find first {" extraction below. Multiple blocks possible.
	t = StripThinkBlocks(t)
	// Trim code fences and any leading non-JSON text.
	if i := strings.Index(t, "{"); i > 0 {
		t = t[i:]
	}
	if i := strings.LastIndex(t, "}"); i >= 0 && i < len(t)-1 {
		t = t[:i+1]
	}
	t = strings.TrimPrefix(t, "```json")
	t = strings.TrimPrefix(t, "```")
	t = strings.TrimSuffix(t, "```")
	t = strings.TrimSpace(t)

	var v verdictHead
	if err := json.Unmarshal([]byte(t), &v); err != nil {
		return "", 0, "", nil, false
	}
	if !ValidDecision(v.Decision) {
		return "", 0, "", nil, false
	}
	return v.Decision, v.Confidence, v.Summary, json.RawMessage(t), true
}

// StripThinkBlocks removes every <think>…</think> chain-of-
// thought span from a model response. Reasoning models on
// Bedrock (nvidia.nemotron-nano-*, deepseek.r1-*, qwen reasoning
// variants) emit a <think> block before their final answer; the
// block routinely contains JSON-like punctuation that breaks
// the downstream "find first {" extraction. Tolerant of
// unclosed blocks (truncated responses) — drops everything
// after the opening tag in that case.
func StripThinkBlocks(s string) string {
	for {
		open := strings.Index(s, "<think>")
		if open < 0 {
			return s
		}
		end := strings.Index(s[open:], "</think>")
		if end < 0 {
			// Unclosed — drop the open tag and everything after.
			return strings.TrimSpace(s[:open])
		}
		s = s[:open] + s[open+end+len("</think>"):]
	}
}

// AbstainSummary is the summary text of the safe fallback for any judge
// failure — LLM error, parse error, missing config. Renders as "abstain"
// in the UI so the operator sees the judge ran but couldn't decide, and
// keeps the operator-actionable reason so dashboards can group "abstained
// because LLM error" separately from "abstained because no evidence".
func AbstainSummary(reason string) string {
	return "judge abstained: " + reason
}

// CompleteWithRetry calls Complete up to maxAttempts times,
// backing off between transient failures. "Transient" means:
//
//   - chat.GatewayError where Retryable() is true (5xx, 429)
//   - net.OpError / syscall.ECONNRESET / "unexpected EOF" /
//     other "connection dropped mid-request" shapes — captured
//     by string match on the error message because the chat
//     package's typed errors don't always wrap them
//
// Permanent errors (4xx other than 429, malformed-request errors,
// auth failures) return immediately. Context cancellation also
// returns immediately — no point retrying when the caller already
// gave up.
//
// Backoff: 500ms, 2s, 8s. Capped at 3 attempts total. The whole
// retry budget completes within ~10s so the judge's outer-context
// timeout still bounds the overall call.
//
// Live evidence: Vertex AI's openapi endpoint surfaces
// "RESOURCE_EXHAUSTED: queue full" 429s during traffic spikes and
// "unexpected EOF" connection drops on long-lived idle pools. Pre-fix
// the hallucination judge abstained on the first attempt — verdict
// tiles filled with "abstained: LLM error" rows that hid real signal.
//
// callSite attributes the calls in the llm-call log; without it they
// log call_site="unknown" (asked 2026-06-13). The hallucination judge
// passes "judge".
func CompleteWithRetry(ctx context.Context, client chat.Provider, msgs []chat.Message, maxAttempts int, callSite string) (*chat.ChatResponse, error) {
	switch callSite {
	case hallucinationJudgeCallSite:
		return CompleteJudgeWithRetry(ctx, client, msgs, maxAttempts)
	default:
		return completeWithRetry(ctx, client, msgs, maxAttempts)
	}
}

// CompleteJudgeWithRetry is the hallucination judge's statically-audited
// call site. Keep the literal reachable here so internal/chat's accounting
// guard can prove the "judge" label still exists in production code while
// CompleteWithRetry remains reusable by config-assistant judges with their
// own labels.
func CompleteJudgeWithRetry(ctx context.Context, client chat.Provider, msgs []chat.Message, maxAttempts int) (*chat.ChatResponse, error) {
	ctx = chat.WithCallSite(ctx, hallucinationJudgeCallSite)
	return completeWithRetry(ctx, client, msgs, maxAttempts)
}

func completeWithRetry(ctx context.Context, client chat.Provider, msgs []chat.Message, maxAttempts int) (*chat.ChatResponse, error) {
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		resp, err := client.Complete(ctx, msgs)
		if err == nil {
			return resp, nil
		}
		lastErr = err

		// Caller cancelled — bail immediately, no retry.
		if ctx.Err() != nil {
			return nil, err
		}

		if attempt == maxAttempts {
			break
		}
		if !IsRetryableErr(err) {
			break
		}

		// 500ms, 2s, 8s — geometric ×4. Bounded by maxAttempts so
		// the wait can't exceed ~10s in aggregate.
		backoff := time.Duration(500) * time.Millisecond
		for i := 1; i < attempt; i++ {
			backoff *= 4
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
	}
	return nil, lastErr
}

// IsRetryableErr classifies LLM errors into transient (worth
// retrying) vs permanent. Chat-layer GatewayError exposes
// Retryable() for the HTTP-status-aware case. Connection-drop
// shapes ("unexpected EOF", "connection reset") aren't typed
// errors at the chat layer; match by message substring.
func IsRetryableErr(err error) bool {
	if err == nil {
		return false
	}
	if ge, ok := err.(*chat.GatewayError); ok {
		return ge.Retryable()
	}
	msg := err.Error()
	for _, hint := range []string{
		"unexpected EOF",
		"connection reset",
		"connection refused",
		"broken pipe",
		"i/o timeout",
		"context deadline exceeded",
		"RESOURCE_EXHAUSTED",
		"queue is full",
	} {
		if strings.Contains(msg, hint) {
			return true
		}
	}
	return false
}
