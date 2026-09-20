package api

import (
	"fmt"
)

// The staged-artifact size guard — 2026-09-15, task_20260915020853_5cdd2cf3345dfa84.
//
// A companion-architectural-review was delegated with two artifacts totalling
// 248 KB (a 126 KB diff plus a 123 KB design document). The reviewer ran four
// iterations, spent about $0.27, hit its prompt-token budget, and failed the
// run on a shape contract — "no file matching artifacts/out/review.md was
// written". Net result: full cost, no review, and an error naming the missing
// output rather than the reason there was none.
//
// prompt_tokens_estimate=93346 against context_size=100000 was known at
// PREFLIGHT on iteration 1, inside the container, and the run proceeded anyway.
// The caller could have split the upload for nothing had it been told at
// delegate time, which is the only moment the split is cheap.
//
// The refusal sits beside the require_input_artifacts and network-incapability
// guards, in the same shape and for the same reason: a warning attached to a
// SUCCESSFUL delegation is a control that reports a problem while still doing
// the wrong thing, and by then the host has already told its user the work is
// under way.

// artifactTokenBytesPerToken is the agent's own estimator: entrypoint.sh sizes
// a request as (bytes + 2) / 3. Reusing its arithmetic rather than a tokenizer
// keeps the two from disagreeing about the same bytes — and it is the number
// that actually gated the observed run.
const artifactTokenBytesPerToken = 3

// artifactContextUsableFraction is how much of the window staged artifacts may
// claim. The agent's compaction budget is (context - max_tokens - 2048) * 80%,
// so artifacts filling the whole window leave nothing for the system prompt,
// the tool catalogue, the conversation, or the answer. 0.5 is deliberately
// below what would just barely fit: an artifact set that needs every remaining
// token produces a review that had no budget left to think.
const artifactContextUsableFraction = 0.5

// checkStagedArtifactBudget refuses a delegation whose staged artifacts cannot
// fit the reviewer's context, with the measured numbers and the remedy.
//
// Returns nil when it cannot measure — an unknown context size is not a licence
// to refuse, and this guard must never be the reason a correct delegation
// fails.
func checkStagedArtifactBudget(workflow string, artifactBytes, contextSize, maxTokens int) error {
	if artifactBytes <= 0 || contextSize <= 0 {
		return nil
	}
	usable := contextSize - maxTokens
	if usable <= 0 {
		usable = contextSize
	}
	ceiling := int(float64(usable) * artifactContextUsableFraction)
	estimate := (artifactBytes + artifactTokenBytesPerToken - 1) / artifactTokenBytesPerToken
	if estimate <= ceiling {
		return nil
	}
	return fmt.Errorf(
		"ARTIFACTS_EXCEED_CONTEXT: %d KB of staged artifacts is about %d prompt tokens, "+
			"against a usable budget of %d of the reviewer's %d-token context for workflow %q. "+
			"The run would spend a full agent budget and then fail on its output contract "+
			"without producing a review (observed 2026-09-15). "+
			"Split the upload — one delegation per subsystem or per file — and quote what the "+
			"agent needs from the larger documents inline in the prompt instead of attaching them",
		artifactBytes/1024, estimate, ceiling, contextSize, workflow)
}
