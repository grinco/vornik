package chat

import "testing"

// Operator report 2026-08-17: the Telegram bot could not deliver a completed
// job's artifacts. The chat audit showed why — every attempt died with
// `gateway error 400: invalid tool call arguments`, so list_artifacts never
// ran.
//
// The captured arguments below are verbatim from chat_audit_log. minimax-m3
// emits SEVERAL tool invocations concatenated into ONE arguments string:
//
//	{"task_id":"…093f"}{"task_id":"…9d21"}{"task_id":"…"}
//
// Note the ids DIFFER. That rules out a streaming accumulator doubling one
// value — the model asked for three status checks and they were flattened into
// one call. 4 of 52 audited tool calls (7.7%) were affected, all minimax-m3,
// across two different tools, so this is a model-shaped fault rather than a
// tool-specific one. Same family as the existing rescue for
// hallucinated-JSON-as-tool-name from minimax on the Bedrock path.
//
// Splitting preserves intent: three checks stay three checks.
func TestSplitConcatenatedToolCallArgs(t *testing.T) {
	const idA = "task_20260817211602_369e9d87747b093f"
	const idB = "task_20260817203651_9ee028e72b0e9d21"

	tests := []struct {
		name      string
		in        []ToolCall
		wantNames []string
		wantArgs  []string
	}{
		{
			name: "the captured list_artifacts call: same object twice",
			in: []ToolCall{{
				ID: "c1", Type: "function",
				Function: FunctionCall{
					Name:      "list_artifacts",
					Arguments: `{"task_id":"` + idA + `"}{"task_id":"` + idA + `"}`,
				},
			}},
			wantNames: []string{"list_artifacts", "list_artifacts"},
			wantArgs: []string{
				`{"task_id":"` + idA + `"}`,
				`{"task_id":"` + idA + `"}`,
			},
		},
		{
			// The decisive case: DIFFERENT ids. Keeping only the first would
			// silently answer about one task when three were asked about.
			name: "the captured get_task_status call: three distinct ids",
			in: []ToolCall{{
				ID: "c2", Type: "function",
				Function: FunctionCall{
					Name:      "get_task_status",
					Arguments: `{"task_id":"` + idA + `"}{"task_id":"` + idB + `"}`,
				},
			}},
			wantNames: []string{"get_task_status", "get_task_status"},
			wantArgs: []string{
				`{"task_id":"` + idA + `"}`,
				`{"task_id":"` + idB + `"}`,
			},
		},
		{
			name: "a well-formed single call is untouched",
			in: []ToolCall{{
				ID: "c3", Type: "function",
				Function: FunctionCall{Name: "list_tasks", Arguments: `{"limit":20}`},
			}},
			wantNames: []string{"list_tasks"},
			wantArgs:  []string{`{"limit":20}`},
		},
		{
			// Nested braces must not be mistaken for a second top-level object.
			name: "nested objects are one call",
			in: []ToolCall{{
				ID: "c4", Type: "function",
				Function: FunctionCall{Name: "search", Arguments: `{"filter":{"a":{"b":1}},"n":2}`},
			}},
			wantNames: []string{"search"},
			wantArgs:  []string{`{"filter":{"a":{"b":1}},"n":2}`},
		},
		{
			// A brace inside a string literal is not structure.
			name: "braces inside strings do not split",
			in: []ToolCall{{
				ID: "c5", Type: "function",
				Function: FunctionCall{Name: "echo", Arguments: `{"text":"}{ literal"}`},
			}},
			wantNames: []string{"echo"},
			wantArgs:  []string{`{"text":"}{ literal"}`},
		},
		{
			name:      "empty arguments are left alone",
			in:        []ToolCall{{ID: "c6", Type: "function", Function: FunctionCall{Name: "ping"}}},
			wantNames: []string{"ping"},
			wantArgs:  []string{""},
		},
		{
			// Genuinely unparseable input must pass through untouched rather
			// than be silently dropped — the caller's existing error path is
			// more useful than a vanished call.
			name: "malformed non-JSON is passed through",
			in: []ToolCall{{
				ID: "c7", Type: "function",
				Function: FunctionCall{Name: "broken", Arguments: `not json at all`},
			}},
			wantNames: []string{"broken"},
			wantArgs:  []string{`not json at all`},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := splitConcatenatedToolCallArgs(tc.in)
			if len(got) != len(tc.wantNames) {
				t.Fatalf("got %d call(s), want %d: %#v", len(got), len(tc.wantNames), got)
			}
			for i := range got {
				if got[i].Function.Name != tc.wantNames[i] {
					t.Errorf("call %d name = %q, want %q", i, got[i].Function.Name, tc.wantNames[i])
				}
				if got[i].Function.Arguments != tc.wantArgs[i] {
					t.Errorf("call %d args = %q, want %q", i, got[i].Function.Arguments, tc.wantArgs[i])
				}
			}
			// Split calls must not share an ID: the OpenAI contract pairs each
			// tool result to its call by id, and duplicates would make two
			// results collide on one call.
			seen := map[string]bool{}
			for _, c := range got {
				if c.ID == "" {
					continue
				}
				if seen[c.ID] {
					t.Errorf("duplicate tool_call id %q after split — results would collide", c.ID)
				}
				seen[c.ID] = true
			}
		})
	}
}
