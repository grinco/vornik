package executor

import (
	"regexp"
	"strings"
)

// reasoningTagRE matches in-content reasoning blocks that gpt-oss,
// DeepSeek-R1, and Qwen reasoning variants emit alongside their final
// answer. They're meant for chat clients that render reasoning
// collapsed; in agent result.json they leak into handover to the next
// role and into the user-visible final message. Non-greedy so multiple
// blocks in one response are stripped independently. (?s) makes . match
// newlines.
var reasoningTagRE = regexp.MustCompile(`(?s)<(think|thinking|reasoning)>.*?</(think|thinking|reasoning)>\s*`)

// stripReasoning removes inline reasoning blocks from an agent message
// and trims the result. Applied when extracting the "message" field
// from an agent's result.json so downstream consumers (next-step
// handover, gate evaluation, final user-facing output) never see the
// raw chain-of-thought.
func stripReasoning(s string) string {
	return strings.TrimSpace(reasoningTagRE.ReplaceAllString(s, ""))
}
