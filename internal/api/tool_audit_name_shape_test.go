package api

import "testing"

// TestClassifyToolNameShape_RealProductionNames — regression for "a malformed
// tool name is persisted".
//
// Every name here was observed in tool_audit_log between 2026-08-06 and
// 2026-09-03. All were correctly REFUSED by the gates; all were stored with
// outcome_class = ”, so the class of failure that means "a model is leaking
// reasoning markers into its tool calls" could not be counted, alerted on, or
// found.
func TestClassifyToolNameShape_RealProductionNames(t *testing.T) {
	for _, name := range []string{
		`file_write</think>allowed_paths`,
		`file_edit</think><tool_call>file_append_string`,
		`file_append_string<tool_call>file_append_string`,
		`file_read(path=.tool_results/call_6khsi6l0.txt)<tool_call>glob(pattern=project/.autonomy/PROJECT_CONTEXT.md)`,
		`file_read<tool_call>path`,
		`file_write＿json<tool_call>callname-placeholder-write final</think><tool_call>write_to_file`,
		`file_write_tool?>`,
		`file_write<tool_call>path`,
		`ls -la project/</arg_value>`,
		`mcp__scraper__ical_events<tool_call>url`,
		`memory_search<tool_call>query`,
		`mcp__vornik__document_get_metadata_pull</think><tool_call>mcp__vornik_architecture`,
	} {
		if got := classifyToolNameShape(name); got != ToolOutcomeClassMalformedName {
			t.Errorf("classifyToolNameShape(%q) = %q, want %q — this refusal stays invisible to "+
				"the outcome_class index", name, got, ToolOutcomeClassMalformedName)
		}
	}
}

// THE HALF THAT MATTERS MORE. A false positive here misclassifies a WORKING
// tool, which is worse than missing a novel leak: the point is to make a known
// failure visible, not to police naming. Every one of these is a real tool name
// from this deployment.
func TestClassifyToolNameShape_LegitimateNamesAreUntouched(t *testing.T) {
	for _, name := range []string{
		"file_read",
		"file_write",
		"run_shell",
		"memory_search",
		"tool_result_read",
		"current_time",
		"read_many_files",
		// MCP names: the vendor chooses these, and they already carry
		// camelCase, digits and hyphens in the server segment.
		"mcp__google-workspace__drive_search",
		"mcp__google-workspace__calendar_listEvents",
		"mcp__atlassian__searchJiraIssuesUsingJql",
		"mcp__atlassian__getAccessibleAtlassianResources",
		"mcp__vornik__recall",
		"mcp__plugin_vornik-companion_vornik__remember",
		// A server this daemon has never seen must also pass — the check
		// holds no vocabulary, deliberately, so tomorrow's server is safe.
		"mcp__some-future-vendor__doThing2",
	} {
		if got := classifyToolNameShape(name); got != "" {
			t.Errorf("classifyToolNameShape(%q) = %q, want \"\" — a working tool was "+
				"classified as a model leaking markup", name, got)
		}
	}
}

// Shapes the pre-2026-09-03 denylist admitted, now classified because the
// grammar is the providers' function-name grammar rather than a list of
// observations (agent-tool declaration design §5): one case per excluded
// class, plus the length boundary, so a later relaxation fails a named case.
func TestClassifyToolNameShape_ExcludedClassesByGrammar(t *testing.T) {
	long := make([]byte, 129)
	for i := range long {
		long[i] = 'a'
	}
	for _, name := range []string{
		"mcp__scraper__web_fetch:fetch", // colon
		"mcp__some/path/tool",           // slash
		"file.read",                     // dot
		"fil€_read",                     // non-ASCII
		"a@b", "a+b",                    // the wider class round 1 proposed
		string(long), // 129 chars: past both providers' limit
	} {
		if got := classifyToolNameShape(name); got != ToolOutcomeClassMalformedName {
			t.Errorf("classifyToolNameShape(%q) = %q, want %q", name, got, ToolOutcomeClassMalformedName)
		}
	}
	if got := classifyToolNameShape(string(long[:128])); got != "" {
		t.Errorf("128 characters is inside the grammar; got %q", got)
	}
}

// Absent and malformed are different. An ingest with no name at all is a client
// bug to find elsewhere, not a model leaking markup.
func TestClassifyToolNameShape_EmptyIsNotMalformed(t *testing.T) {
	if got := classifyToolNameShape(""); got != "" {
		t.Errorf("classifyToolNameShape(\"\") = %q, want \"\"", got)
	}
}
