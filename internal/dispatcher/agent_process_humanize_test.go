package dispatcher

import "testing"

// TestHumanizeToolName covers the streaming status-marker rendering.
// Stable phrasing matters because the marker is what the operator
// reads in Telegram while the agent is mid-turn — pre-fix the raw
// `[running: create_task]` marker docked against streamed text on
// either side (producing e.g. "[running: create_task]ask to..." where
// "ask" is the tail of the LLM's "task to..."). The humanizer + the
// `\n\n` framing in ProcessStreaming together solve that.
func TestHumanizeToolName(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"known: create_task", "create_task", "creating task"},
		{"known: send_artifact", "send_artifact", "sending artifact"},
		{"known: send_message", "send_message", "sending message"},
		{"known: memory_search", "memory_search", "searching memory"},
		{"known: memory_correct", "memory_correct", "correcting memory"},
		{"known: summarize_thread", "summarize_thread", "summarizing thread"},
		{"known: get_conversation_window", "get_conversation_window", "loading conversation"},
		{"known: file_read", "file_read", "reading file"},
		{"known: file_write", "file_write", "writing file"},
		{"known: file_edit", "file_edit", "editing file"},
		{"known: read_many_files", "read_many_files", "reading files"},
		{"known: run_shell", "run_shell", "running shell command"},
		{"known: grep", "grep", "searching files"},
		{"known: glob", "glob", "listing files"},
		{"known: current_time", "current_time", "checking time"},
		{"mcp: server/tool split", "mcp__scraper__web_fetch", "using scraper/web_fetch"},
		{"mcp: no double-underscore separator", "mcp__bareserver", "using bareserver"},
		{"unknown tool: underscores → spaces", "some_new_tool", "some new tool"},
		{"unknown tool: single word", "ping", "ping"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := humanizeToolName(tc.in)
			if got != tc.want {
				t.Errorf("humanizeToolName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestToolStatusMarker(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"memory search", "memory_search", "[🧠 searching memory]"},
		{"memory correct", "memory_correct", "[🧠 correcting memory]"},
		{"mcp split", "mcp__scraper__web_fetch", "[🔌 using scraper/web_fetch]"},
		{"task", "create_task", "[📋 creating task]"},
		{"unknown", "some_new_tool", "[⏳ some new tool]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := toolStatusMarker(tc.in)
			if got != tc.want {
				t.Errorf("toolStatusMarker(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
