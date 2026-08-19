package chat

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// splitConcatenatedToolCallArgs repairs a provider response in which SEVERAL
// tool invocations were flattened into ONE call's arguments string.
//
// THE FAULT, measured 2026-08-17. minimax-m3 emitted:
//
//	{"task_id":"…093f"}{"task_id":"…9d21"}{"task_id":"…"}
//
// as the arguments of a single get_task_status call. The ids DIFFER, which is
// what rules out a streaming accumulator doubling one value — the model asked
// for three status checks and they arrived collapsed into one. 4 of 52 audited
// tool calls (7.7%) were affected, all minimax-m3, across two different tools.
//
// WHY IT MATTERED. The concatenation is not valid JSON, so the upstream gateway
// rejected the whole request with `400 invalid tool call arguments`. The tool
// never ran, and the operator's Telegram bot could not deliver a completed
// job's artifacts — the failure surfaced as "it simply doesn't work" with no
// indication that a malformed tool call was the cause.
//
// WHY SPLIT RATHER THAN KEEP-FIRST. Keeping the first object would answer about
// one task when three were asked about, silently. Splitting preserves the
// model's intent; the tool loop then executes each call and pairs each result
// to its own id.
//
// Conservative by construction: a well-formed single object, a nested object, a
// brace inside a string literal, and genuinely unparseable text are all left
// exactly as they are. Only input that decodes as two-or-more complete JSON
// values is rewritten, so a provider behaving correctly is never touched.
func splitConcatenatedToolCallArgs(calls []ToolCall) []ToolCall {
	if len(calls) == 0 {
		return calls
	}
	var out []ToolCall
	changed := false
	for _, call := range calls {
		parts, ok := decodeConcatenatedJSONObjects(call.Function.Arguments)
		if !ok || len(parts) < 2 {
			out = append(out, call)
			continue
		}
		changed = true
		for i, part := range parts {
			split := call
			split.Function.Arguments = part
			// Distinct ids: the OpenAI contract pairs each tool result to its
			// call by id, so reusing one would make several results collide on
			// a single call. The first keeps the original id so any log or
			// audit row already referencing it still resolves.
			if i > 0 && call.ID != "" {
				split.ID = fmt.Sprintf("%s-split-%d", call.ID, i)
			}
			out = append(out, split)
		}
	}
	if !changed {
		return calls
	}
	return out
}

// decodeConcatenatedJSONObjects reports the individual JSON values in s when it
// holds more than one, re-encoded compactly.
//
// Uses encoding/json's streaming decoder rather than brace counting: a decoder
// already understands strings, escapes and nesting, so `{"text":"}{ literal"}`
// stays one value and `{"a":{"b":1}}` is not mistaken for two. Returns ok=false
// for anything that is not a clean sequence of complete JSON values, which is
// what keeps malformed input on the caller's existing error path instead of
// being silently reshaped.
func decodeConcatenatedJSONObjects(s string) ([]string, bool) {
	if len(s) == 0 {
		return nil, false
	}
	dec := json.NewDecoder(strings.NewReader(s))
	var parts []string
	for {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			// io.EOF is the clean end; anything else means the tail was not a
			// complete JSON value, so we refuse the whole repair.
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, false
		}
		var buf bytes.Buffer
		if err := json.Compact(&buf, raw); err != nil {
			return nil, false
		}
		parts = append(parts, buf.String())
	}
	if len(parts) < 2 {
		return nil, false
	}
	return parts, true
}
