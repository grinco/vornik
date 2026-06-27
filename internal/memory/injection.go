package memory

import "strings"

// Prompt-injection / context-manipulation detection for the ingest
// pipeline (security LLD review batch 2). The quality gates catch
// secrets, dedup, and truncation, but NOT content crafted to manipulate
// a future agent that retrieves the chunk — "ignore previous
// instructions", chat-template control tokens, "reveal your system
// prompt", etc. A poisoned memory chunk is a stored-injection vector:
// it's written once and replayed into every agent that recalls it.
//
// This is a deterministic, heuristic first line (no LLM cost on the
// ingest hot path). The detector is intentionally CONSERVATIVE — a
// memory corpus legitimately discusses prompts and LLMs, so the signals
// are specific multi-word imperatives and chat-template markers rather
// than single keywords. An optional LLM-classifier stage can layer on
// later behind the same gate action; the gate already supports a
// detect-only mode for measuring the false-positive rate before any
// project flips it to quarantine.

// injectionPhrases are lower-cased substrings whose presence strongly
// signals an instruction-override attempt. Kept specific to avoid
// false-positives on benign LLM/prompt discussion.
var injectionPhrases = []string{
	"ignore previous instructions",
	"ignore all previous instructions",
	"ignore the above instructions",
	"disregard previous instructions",
	"disregard the above",
	"disregard all previous",
	"forget previous instructions",
	"forget everything above",
	"forget all previous",
	"override your instructions",
	"ignore your instructions",
	"ignore your system prompt",
	"reveal your system prompt",
	"reveal your instructions",
	"print your system prompt",
	"print your instructions",
	"repeat your system prompt",
	"your real instructions are",
	"new instructions:",
	"do not follow the above",
	"ignore the system prompt",
}

// injectionTokens are exact substrings (case-sensitive) for chat-template
// / role-control markers that have no business inside ingested content
// and are a classic injection delimiter.
var injectionTokens = []string{
	"<|im_start|>",
	"<|im_end|>",
	"<|system|>",
	"[INST]",
	"[/INST]",
	"<<SYS>>",
	"</s><s>",
}

// DetectPromptInjection scans content for prompt-injection / context-
// manipulation signals and returns the matched signal labels (empty when
// clean). Matching is bounded and allocation-light: one lower-cased copy
// for the phrase pass, raw content for the token pass.
func DetectPromptInjection(content string) []string {
	if content == "" {
		return nil
	}
	var hits []string
	lower := strings.ToLower(content)
	for _, p := range injectionPhrases {
		if strings.Contains(lower, p) {
			hits = append(hits, p)
		}
	}
	for _, tok := range injectionTokens {
		if strings.Contains(content, tok) {
			hits = append(hits, tok)
		}
	}
	return hits
}
