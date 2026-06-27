package dispatcher

// In-content reasoning-tag stripping for assistant text. Some models
// (gpt-oss, DeepSeek-R1, Qwen reasoning variants) emit <think>… or
// <reasoning>… blocks alongside their final answer; this file
// removes them before user-visible text leaves the dispatcher.

import (
	"regexp"
	"strings"

	"vornik.io/vornik/internal/chat"
)

// reasoningTagRE matches in-content reasoning blocks that some models
// (gpt-oss, DeepSeek-R1, Qwen reasoning variants) emit alongside their
// final answer. They're meant for display-capable clients that render
// reasoning collapsed; in our plain-text Telegram path they leak into
// the visible message. Non-greedy so multiple blocks in one response
// are stripped independently. (?s) makes . match newlines.
var reasoningTagRE = regexp.MustCompile(`(?s)<(think|thinking|reasoning)>.*?</(think|thinking|reasoning)>\s*`)

// stripReasoning removes inline reasoning blocks from an assistant
// message body and trims the result. Applied only to user-facing text,
// not to stored conversation history — tool-call rounds sometimes rely
// on the raw content for state. Returns the cleaned text.
func stripReasoning(s string) string {
	return strings.TrimSpace(reasoningTagRE.ReplaceAllString(s, ""))
}

// stripReasoningStreaming is the chunk-boundary-safe variant used while a
// response is still being streamed. It strips any already-closed reasoning
// blocks, then hides everything from the first *unclosed* opening tag
// onward — the matching close hasn't arrived yet, so showing the raw
// opener would leak the reasoning prefix to the user.
//
// Callers pass the full accumulated buffer each time (same contract as
// chat.StreamCallback). As more of the stream arrives, previously hidden
// content becomes visible once its close tag lands.
func stripReasoningStreaming(accumulated string) string {
	s := reasoningTagRE.ReplaceAllString(accumulated, "")
	// After the regex replace, any remaining full "<think" / "<reasoning"
	// substring is an unclosed open tag. Truncate at the earliest one.
	earliest := len(s)
	for _, prefix := range []string{"<think", "<reasoning"} {
		if idx := strings.Index(s, prefix); idx >= 0 && idx < earliest {
			earliest = idx
		}
	}
	s = s[:earliest]
	// Additionally, a partial opener like "<thi" or just a bare "<" can
	// sit at the tail when a chunk boundary splits the tag name. If the
	// last "<" has no matching ">" and what follows it is a prefix of
	// any reasoning-tag name, truncate there too.
	if lt := strings.LastIndex(s, "<"); lt >= 0 {
		after := s[lt+1:]
		if !strings.Contains(after, ">") {
			for _, name := range []string{"think", "thinking", "reasoning"} {
				if strings.HasPrefix(name, after) {
					s = s[:lt]
					break
				}
			}
		}
	}
	return strings.TrimSpace(s)
}

// wrapStreamCallbackStripping returns a callback that forwards accumulated
// text to the underlying callback only after running the streaming-safe
// reasoning stripper over it. An empty stripped result is suppressed
// (calling the underlying callback with "" would replace any prior user-
// visible text with a blank).
func wrapStreamCallbackStripping(underlying chat.StreamCallback) chat.StreamCallback {
	if underlying == nil {
		return nil
	}
	return func(accumulated string) {
		text := stripReasoningStreaming(accumulated)
		if text == "" {
			return
		}
		underlying(text)
	}
}
