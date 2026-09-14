package configassist

import (
	"strings"
	"testing"
)

func TestMostRestrictive(t *testing.T) {
	if MostRestrictive() != ClassE {
		t.Fatal("nothing classified must be E (deny by default)")
	}
	if got := MostRestrictive(ClassA, ClassB2, ClassA); got != ClassB2 {
		t.Fatalf("got %s", got)
	}
	if got := MostRestrictive(ClassA, ClassE, ClassC); got != ClassE {
		t.Fatalf("got %s", got)
	}
	if Rank(ClassB1) <= Rank(ClassB2) {
		t.Fatal("B1 (unwrapped steering prose) must rank above B2")
	}
}

// Test 13 (design §9): a bundle touching class A and class E is class E —
// refused ENTIRE, never split. Test 14: an unclassifiable key is E, not A.
func TestClassify_MostRestrictiveWinsAndUnknownIsE(t *testing.T) {
	bc := ClassifyChanges([]Change{
		{File: "projects/assistant.yaml", Key: "autonomy.feeds.0.cadence", Before: "1h", After: "2h"},
		{File: "swarms/dev.md", Key: "roles.reviewer.allowedTools", Before: "[a]", After: "[a,b]"},
	})
	if bc.Class != ClassE {
		t.Fatalf("A+E bundle must be E, got %s: %v", bc.Class, bc.Reasons())
	}
	if bc.Touches[0].Class != ClassE {
		t.Fatalf("touches must be sorted most-restrictive first: %v", bc.Reasons())
	}
	bc = ClassifyChanges([]Change{{File: "projects/x.yaml", Key: "some.new.knob", Before: "1", After: "2"}})
	if bc.Class != ClassE || !strings.Contains(bc.Touches[0].Reason, "unclassifiable") {
		t.Fatalf("unknown key must be E: %v", bc.Reasons())
	}
}

// Design §6 table and review R7 directions.
func TestClassify_RuleTable(t *testing.T) {
	cases := []struct {
		key, before, after, want string
	}{
		{"autonomy.feeds.0.cadence", "1h", "2h", ClassA},   // longer cadence: fewer ticks
		{"autonomy.feeds.0.cadence", "1h", "1m", ClassD},   // shorter: sixtyfold more ticks (design §7 worked case)
		{"autonomy.feeds.0.cadence", "60d", "30d", ClassD}, // day suffix parses
		{"autonomy.maxTasksPerHour", "10", "4", ClassA},
		{"autonomy.maxTasksPerHour", "4", "10", ClassD},
		{"budget.daily_hard_usd", "3", "5", ClassD},
		{"budget.daily_hard_usd", "5", "3", ClassD}, // spend keys are D both ways (never auto-apply)
		{"steps.review.timeout", "10m", "5m", ClassA},
		{"steps.review.timeout", "5m", "10m", ClassD}, // increases are D (R7)
		{"steps.implement.retryPolicy.maxRetries", "1", "3", ClassD},
		{"steps.implement.retryPolicy.maxRetries", "3", "1", ClassA},
		{"steps.newstep.role", "", "coder", ClassC},
		{"steps.review.on_success", "done", "implement", ClassC},
		{"entrypoint", "plan", "implement", ClassC},
		{"roles.reviewer.model", "a", "b", ClassD},
		{"autonomy.goal", "x", "y", ClassB1},
		{"steps.plan.prompt", "x", "y", ClassB1},
		{"roles.lead.systemPrompt", "x", "y", ClassB1},
		{"description", "x", "y", ClassB2},
		{"PROJECT_CONTEXT.md", "x", "y", ClassB2},
		{"permissions.allowedTools", "[a]", "[a,b]", ClassE},
		{"roles.reviewer.allowed_tools", "", "[x]", ClassE}, // casing collision lands as E
		{"mcp.servers", "[]", "[x]", ClassE},
		{"permissions.secrets", "[]", "[x]", ClassE},
		{"slack.sender_allowlist", "[]", "[x]", ClassE},
		{"autonomy.requireApproval", "true", "false", ClassE},
		{"autonomy.requireApproval", "false", "true", ClassA},
		{"retention.tasks_days", "60", "30", ClassA},
		{"hallucinationJudge.model", "a", "b", ClassD},
		{"trading.entry_policy.max_positions", "6", "8", ClassE},
		{"steps.plan.something_new", "", "a long natural language sentence with many words in it to look like prose", ClassB1},
	}
	for _, tc := range cases {
		got := ClassifyChanges([]Change{{File: "f", Key: tc.key, Before: tc.before, After: tc.after}})
		if got.Class != tc.want {
			t.Errorf("%s %q→%q: got %s, want %s (%s)", tc.key, tc.before, tc.after, got.Class, tc.want, got.Touches[0].Reason)
		}
	}
}

func TestClassifyFileEvent(t *testing.T) {
	if got := ClassifyFileEvent("workflows/new.md", true); got.Class != ClassC {
		t.Fatalf("new workflow = %s", got.Class)
	}
	if got := ClassifyFileEvent("projects/x.yaml", true); got.Class != ClassC {
		t.Fatalf("new project = %s", got.Class)
	}
	if got := ClassifyFileEvent("projects/x/PROJECT_CONTEXT.md", true); got.Class != ClassB2 {
		t.Fatalf("new project prose = %s", got.Class)
	}
	if got := ClassifyFileEvent("secrets/thing", true); got.Class != ClassE {
		t.Fatalf("new file outside the tree = %s", got.Class)
	}
	if got := ClassifyFileEvent("projects/x.yaml", false); got.Class != ClassA {
		t.Fatalf("replaced file event itself is A; keys decide: %s", got.Class)
	}
}
