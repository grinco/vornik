package configassist

import (
	"strings"
	"testing"
)

const goodProject = "projectId: assistant\nswarmId: s\ndefaultWorkflowId: w\nautonomy:\n  enabled: true\n  goal: g\n  feeds:\n    - slug: news\n      cadence: 1h\n    - slug: events\n      cadence: 60d\n"

func TestFlatten_FeedsIndexBySlug(t *testing.T) {
	before := Flatten([]byte(goodProject))
	if before["autonomy.feeds.news.cadence"] != "1h" || before["autonomy.feeds.events.cadence"] != "60d" {
		t.Fatalf("flatten: %v", before)
	}
	// Inserting a feed before "events" must not make "events" look changed.
	after := Flatten([]byte(strings.Replace(goodProject, "    - slug: events", "    - slug: sport\n      cadence: 2h\n    - slug: events", 1)))
	changes := DiffFlat("projects/assistant.yaml", before, after)
	if len(changes) != 2 {
		t.Fatalf("changes = %+v (want only the inserted feed's two keys)", changes)
	}
	if !strings.Contains(Flatten([]byte("a: [b"))["<unparseable>"], "yaml") {
		t.Fatal("unparseable yaml must flatten to a sentinel key")
	}
}

func TestChangesForFile_MarkdownBodyAndFrontmatter(t *testing.T) {
	before := "---\nworkflowId: w\nsteps:\n  plan:\n    role: lead\n---\n### plan\nold prompt\n"
	after := "---\nworkflowId: w\nsteps:\n  plan:\n    role: lead\n    timeout: 5m\n---\n### plan\nnew prompt\n"
	ch := ChangesForFile("workflows/w.md", []byte(before), []byte(after))
	keys := map[string]string{}
	for _, c := range ch {
		keys[c.Key] = c.After
	}
	if keys["steps.plan.timeout"] != "5m" {
		t.Fatalf("frontmatter change missing: %+v", ch)
	}
	if _, ok := keys["steps.<body>.prompt"]; !ok {
		t.Fatalf("workflow body change must classify as a step prompt (B1): %+v", ch)
	}
	if ClassifyChanges(ch).Class != ClassD {
		t.Fatalf("timeout set + prompt edit = D (most restrictive), got %s", ClassifyChanges(ch).Class)
	}
	ch = ChangesForFile("projects/x/PROJECT_CONTEXT.md", []byte("a"), []byte("b"))
	if len(ch) != 1 || ClassifyChanges(ch).Class != ClassB2 {
		t.Fatalf("project prose must be B2: %+v", ch)
	}
}

func TestUnifiedDiff_AndChangedLines(t *testing.T) {
	d := UnifiedDiff("projects/a.yaml", "a: 1\nb: 2\nc: 3\n", "a: 1\nb: 5\nc: 3\nd: 4\n")
	if !strings.Contains(d, "-b: 2") || !strings.Contains(d, "+b: 5") || !strings.Contains(d, "+d: 4") {
		t.Fatalf("diff:\n%s", d)
	}
	lines := ChangedLines(d)
	if len(lines) != 3 || lines[0] != "b: 2" || lines[1] != "b: 5" || lines[2] != "d: 4" {
		t.Fatalf("changed lines = %v", lines)
	}
	if !strings.Contains(UnifiedDiff("n.yaml", "", "x: 1\n"), "+x: 1") {
		t.Fatal("create diff")
	}
}

// Test 3 (design §9): an edit introducing an unknown project key is refused
// WITH the skew diagnosis, and it is a refusal (nothing is filed — the
// engine never reaches the ledger on a non-nil gate).
func TestGateSchemas_UnknownKeyIsSkewRefusal(t *testing.T) {
	r := GateSchemas([]Op{{Op: "replace", Path: "projects/assistant.yaml", Content: goodProject + "autonomy_feedz: 1\n"}})
	if r == nil || r.Code != RefuseSkew {
		t.Fatalf("want skew refusal, got %v", r)
	}
	if !strings.Contains(r.Error(), "autonomy_feedz") || !strings.Contains(r.Message, "binary first") {
		t.Fatalf("refusal must name the key and the deploy-ordering rule: %v", r)
	}
	// A misspelling of a known key carries the suggestion.
	r = GateSchemas([]Op{{Op: "replace", Path: "projects/assistant.yaml", Content: strings.Replace(goodProject, "defaultWorkflowId", "default_workflow_id", 1)}})
	if r == nil || !strings.Contains(r.Error(), "did you mean") {
		t.Fatalf("misspelling must suggest the accepted spelling: %v", r)
	}
	// Feeds rules: duplicate slug and non-positive cadence.
	r = GateSchemas([]Op{{Op: "replace", Path: "projects/assistant.yaml", Content: strings.Replace(goodProject, "slug: events", "slug: news", 1)}})
	if r == nil || r.Code != RefuseSchema || !strings.Contains(r.Error(), "duplicate feed slug") {
		t.Fatalf("duplicate slug: %v", r)
	}
	r = GateSchemas([]Op{{Op: "replace", Path: "projects/assistant.yaml", Content: strings.Replace(goodProject, "cadence: 1h", "cadence: 0s", 1)}})
	if r == nil || !strings.Contains(r.Error(), "not a positive duration") {
		t.Fatalf("zero cadence: %v", r)
	}
	if r := GateSchemas([]Op{{Op: "replace", Path: "projects/assistant.yaml", Content: goodProject}}); r != nil {
		t.Fatalf("good project must pass: %v", r)
	}
	// Workflow validation: malformed frontmatter is a schema refusal.
	r = GateSchemas([]Op{{Op: "create", Path: "workflows/w.md", Content: "no frontmatter\n"}})
	if r == nil || r.Code != RefuseSchema {
		t.Fatalf("bad workflow: %v", r)
	}
	r = GateSchemas([]Op{{Op: "create", Path: "swarms/s.md", Content: "---\nswarmId: [\n---\n"}})
	if r == nil || r.Code != RefuseSchema {
		t.Fatalf("bad swarm: %v", r)
	}
}

// Test 4 (design §9): an edit touching a file outside the declared subject
// is refused; an attempted deletion is named.
func TestGateDeclaredScope(t *testing.T) {
	ops := []Op{{Op: "replace", Path: "projects/a.yaml"}, {Op: "create", Path: "workflows/new.md"}}
	if r := GateDeclaredScope([]string{"projects/a.yaml"}, ops, nil); r == nil || r.Code != RefuseDiffScope || !strings.Contains(r.Error(), "workflows/new.md") {
		t.Fatalf("out-of-scope create must be refused: %v", r)
	}
	if r := GateDeclaredScope([]string{"projects/a.yaml", "./workflows/new.md"}, ops, nil); r != nil {
		t.Fatalf("declared files must pass: %v", r)
	}
	if r := GateDeclaredScope([]string{"projects/a.yaml"}, ops[:1], []string{"projects/b.yaml"}); r == nil || !strings.Contains(r.Error(), "deletion attempted") {
		t.Fatalf("deletion must be named: %v", r)
	}
}

func TestGateWorkspaceContext(t *testing.T) {
	if r := GateWorkspaceContext("assistant", []Op{{Op: "replace", Path: "projects/assistant/PROJECT_CONTEXT.md"}}); r != nil {
		t.Fatalf("own workspace context must pass: %v", r)
	}
	if r := GateWorkspaceContext("assistant", []Op{{Op: "replace", Path: "projects/other/PROJECT_CONTEXT.md"}}); r == nil || !strings.Contains(r.Error(), "other") {
		t.Fatalf("other project context must be refused: %v", r)
	}
	ops := []Op{
		{Op: "replace", Path: "projects/assistant/PROJECT_CONTEXT.md"},
		{Op: "replace", Path: "projects/assistant.yaml"},
	}
	if r := GateWorkspaceContext("assistant", ops); r == nil || !strings.Contains(r.Error(), "may not be mixed") {
		t.Fatalf("mixed workspace/config edit must be refused: %v", r)
	}
}

// Tests 21 (refusal half) and 29: the chat and agent entrypoints refuse B1, C, D
// and E by class and name the entrypoint; operator entrypoints carry every class.
func TestGateEntrypointCeiling(t *testing.T) {
	for _, entrypoint := range []string{EntrypointChat, EntrypointAgent} {
		for _, class := range []string{ClassB1, ClassC, ClassD, ClassE} {
			r := GateEntrypointCeiling(entrypoint, class)
			if r == nil || !strings.Contains(r.Message, entrypoint) || !strings.Contains(r.Message, "class "+class) {
				t.Fatalf("%s/%s must be refused naming the entrypoint and class: %v", entrypoint, class, r)
			}
		}
		for _, class := range []string{ClassA, ClassB2} {
			if r := GateEntrypointCeiling(entrypoint, class); r != nil {
				t.Fatalf("%s/%s must pass: %v", entrypoint, class, r)
			}
		}
	}
	for _, entrypoint := range []string{EntrypointREST, EntrypointCLI, EntrypointConsole} {
		if r := GateEntrypointCeiling(entrypoint, ClassE); r != nil {
			t.Fatalf("operator entrypoint %s must carry class E (human-approved): %v", entrypoint, r)
		}
		if !MayAutoApply(entrypoint) {
			t.Fatalf("%s may auto-apply (A/B with opt-in)", entrypoint)
		}
	}
	if MayAutoApply(EntrypointChat) || MayAutoApply(EntrypointAgent) {
		t.Fatal("raising entrypoints never auto-apply")
	}
}
