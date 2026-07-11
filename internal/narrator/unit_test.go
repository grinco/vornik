package narrator

import (
	"strings"
	"testing"
)

// --- llm.go helpers -------------------------------------------------

func TestBuildUserMessage_AllFieldsPresent(t *testing.T) {
	msg := buildUserMessage(triggerCompletion,
		templateInput{Role: "researcher", StepIdx: 2, StepTotal: 5, Outcome: "ok", Success: true},
		"step-name", "tool-name")
	for _, want := range []string{
		"EVENT: completion", "ROLE: researcher", "STEP: 2 of 5",
		"<<<UNTRUSTED>>>step-name<<<END_UNTRUSTED>>>",
		"<<<UNTRUSTED>>>tool-name<<<END_UNTRUSTED>>>",
		"OUTCOME: ok", "SUCCESS: true",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q:\n%s", want, msg)
		}
	}
}

func TestBuildUserMessage_MinimalFields_OmitsOptional(t *testing.T) {
	msg := buildUserMessage(triggerStepStarted, templateInput{}, "", "")
	if strings.Contains(msg, "ROLE:") || strings.Contains(msg, "STEP:") ||
		strings.Contains(msg, "STEP_NAME") || strings.Contains(msg, "TOOL_NAME") ||
		strings.Contains(msg, "OUTCOME:") {
		t.Errorf("minimal input should omit every optional field:\n%s", msg)
	}
	if !strings.Contains(msg, "EVENT: step_started") {
		t.Errorf("EVENT line always required:\n%s", msg)
	}
}

func TestBuildUserMessage_StepWithoutTotal(t *testing.T) {
	msg := buildUserMessage(triggerStepStarted, templateInput{StepIdx: 3}, "", "")
	if !strings.Contains(msg, "STEP: 3\n") {
		t.Errorf("expected \"STEP: 3\" without an \"of M\" clause:\n%s", msg)
	}
}

// --- cleanLine --------------------------------------------------------

func TestCleanLine(t *testing.T) {
	cases := map[string]string{
		"":                             "",
		"   ":                          "",
		"Hello world.":                 "Hello world.",
		"\"Quoted line.\"":             "Quoted line.",
		"'Single quoted.'":             "Single quoted.",
		"Line one.\nLine two ignored.": "Line one.",
		"Multi   space   collapse":     "Multi space collapse",
	}
	for in, want := range cases {
		if got := cleanLine(in); got != want {
			t.Errorf("cleanLine(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCleanLine_TruncatesLongResponses(t *testing.T) {
	long := strings.Repeat("word ", 100)
	got := cleanLine(long)
	if len(got) > 220 {
		t.Errorf("cleanLine did not cap length: got %d chars", len(got))
	}
}

// --- pickModel --------------------------------------------------------

func TestPickModel_NonOverridableProviderReturnsUnchanged(t *testing.T) {
	base := &fakeProvider{}
	if pickModel(base, "some-model") != base {
		t.Error("a non-ModelOverridable provider should be returned unchanged")
	}
}

// --- redactLine -------------------------------------------------------

func TestRedactLine_NilScanner_ReturnsUnchanged(t *testing.T) {
	if got := redactLine(nil, "hello"); got != "hello" {
		t.Errorf("redactLine with nil scanner = %q, want unchanged", got)
	}
}

func TestRedactLine_EmptyText_ReturnsEmpty(t *testing.T) {
	if got := redactLine(noopScanner{}, ""); got != "" {
		t.Errorf("redactLine on empty text = %q, want empty", got)
	}
}

func TestRedactLine_NoFindings_ReturnsUnchanged(t *testing.T) {
	if got := redactLine(noopScanner{}, "nothing secret here"); got != "nothing secret here" {
		t.Errorf("redactLine with no findings = %q, want unchanged", got)
	}
}

// --- templates.go edge cases -------------------------------------------

func TestStepCompletedTemplate_ErrorOutcome(t *testing.T) {
	got := stepCompletedTemplate(templateInput{Outcome: "error", StepIdx: 2})
	if !strings.Contains(got, "Ran into a problem") {
		t.Errorf("got %q, want a problem phrasing for a non-ok outcome", got)
	}
}

func TestDefaultTemplate_NoStepIdx(t *testing.T) {
	got := defaultTemplate(templateInput{})
	if got != "Working on the task…" {
		t.Errorf("got %q", got)
	}
}

func TestToolHeartbeatTemplate_EmptyTool(t *testing.T) {
	got := toolHeartbeatTemplate(templateInput{StepIdx: 1})
	if !strings.Contains(got, "Still working") {
		t.Errorf("got %q, want the tool-less heartbeat phrasing", got)
	}
}

func TestHumanizeTool(t *testing.T) {
	cases := map[string]string{
		"":                       "",
		"web_search":             "web search",
		"mcp__filesystem__read":  "read",
		"mcp__server__tool_name": "tool name",
		"already spaced":         "already spaced",
	}
	for in, want := range cases {
		if got := humanizeTool(in); got != want {
			t.Errorf("humanizeTool(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLastIndexAll(t *testing.T) {
	if got := lastIndexAll("a__b__c", "__"); got != 4 {
		t.Errorf("lastIndexAll = %d, want 4", got)
	}
	if got := lastIndexAll("no-sep-here", "__"); got != -1 {
		t.Errorf("lastIndexAll = %d, want -1", got)
	}
}

// --- metrics.go nil-safety --------------------------------------------

func TestMetrics_NilSafe(_ *testing.T) {
	var n *Narrator
	n.metricLine("step", false) // must not panic
	n.metricCapped("lines")
	n.metricDropped("step_started")
	n.metricPanic()

	withNilMetrics := &Narrator{}
	withNilMetrics.metricLine("step", false)
	withNilMetrics.metricCapped("cost")
	withNilMetrics.metricDropped("tool_call_started")
	withNilMetrics.metricPanic()
}

func TestNewMetrics_NilRegistryReturnsNil(t *testing.T) {
	if NewMetrics(nil) != nil {
		t.Error("NewMetrics(nil) should return nil (observability disabled)")
	}
}
