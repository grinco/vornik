package registry

import (
	"strings"
	"testing"
)

// Regression, 2026-08-20. The registry validated that every gate-referenced
// path was DECLARED in the producing role's outputSchema — `DeclaresPath`,
// which walks `properties`. It never checked `required`.
//
// `properties` is documentation. `required` is what BINDS: role.OutputSchema
// feeds it through as the emit_<role>_result tool's parameters, so a field
// absent from `required` is one the model may legitimately omit.
//
// dev-pipeline gates on `review.all_done` in two of three conditions.
// `all_done` sat in `properties` but not `required`, so the guard passed and
// the model omitted it — 83 of 97 review steps in the 2026.8.8 profiling arm
// emitted {"review":{"approved":true,"feedback":"..."}} with no all_done,
// matched no gate condition, and had their approval discarded.
//
// Second occurrence of the class. The first was testing.passed/testing.cases
// (2026-08-19), whose fix — adding the field to `required` — moved violations
// 37/50 to 0/6. A guard that checks declaration and not obligation cannot
// catch either.

func gateSchema(required []string) *OutputSchema {
	return &OutputSchema{
		Type:     "object",
		Required: []string{"review"},
		Properties: map[string]*OutputSchema{
			"review": {
				Type:     "object",
				Required: required,
				Properties: map[string]*OutputSchema{
					"approved": {Type: "bool"},
					"all_done": {Type: "bool"},
					"feedback": {Type: "string"},
				},
			},
		},
	}
}

func TestRequiresPath_declaredButOptionalIsNotRequired(t *testing.T) {
	s := gateSchema([]string{"approved"})

	if !s.DeclaresPath("review.all_done") {
		t.Fatal("fixture wrong: all_done should be declared under properties")
	}
	if s.RequiresPath("review.all_done") {
		t.Error("all_done is only in properties, not required — RequiresPath must say so, " +
			"because `required` is what binds at decode time")
	}
	if !s.RequiresPath("review.approved") {
		t.Error("approved IS required and must be reported as such")
	}
}

func TestRequiresPath_everySegmentMustBeRequired(t *testing.T) {
	// `review` itself optional at the root: the model may omit the whole
	// object, so nothing beneath it is obliged either.
	s := gateSchema([]string{"approved", "all_done"})
	s.Required = nil

	if s.RequiresPath("review.approved") {
		t.Error("the parent object is optional, so a required child is still omittable — " +
			"every segment on the path must be required")
	}
}

func TestRequiresPath_nilAndEmpty(t *testing.T) {
	var s *OutputSchema
	if s.RequiresPath("review.approved") {
		t.Error("a nil schema requires nothing")
	}
	if !gateSchema(nil).RequiresPath("") {
		t.Error("the empty path is trivially satisfied, matching DeclaresPath")
	}
}

// And the load-time refusal itself: an optional gate path must strip the
// project, not load it. Without this the RequiresPath helper could exist and
// be wired nowhere — which is exactly how DeclaresPath's weaker check went
// unnoticed.
func TestStripInvalidProjects_RefusesOptionalGatePath(t *testing.T) {
	tmp := t.TempDir()
	mustWriteAll(t, tmp, map[string]string{
		"swarms/s.md": `---
swarmId: "s"
roles:
  - name: "reviewer"
    runtime: { image: "x" }
    injectSchemaIntoPrompt: true
    outputSchema:
      type: object
      required: [review]
      properties:
        review:
          type: object
          required: [approved]
          properties:
            approved: { type: bool }
            all_done: { type: bool }
---
`,
		"workflows/w.md": `---
workflowId: "w"
entrypoint: "review"
steps:
  review:
    type: "agent"
    role: "reviewer"
    prompt: "review"
    gates:
      - condition: "review.approved == true && review.all_done == true"
        target: "complete"
    on_fail: "failed"
terminals:
  complete: { status: "COMPLETED" }
  failed:   { status: "FAILED" }
---
`,
		"projects/p.yaml": `projectId: "p"
swarmId: "s"
defaultWorkflowId: "w"
`,
	})

	r := New()
	if err := r.Stage(tmp); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	verr := r.StripInvalidFromStaged()
	if verr == nil {
		t.Fatal("a gate reading an OPTIONAL schema path loaded cleanly; the model may " +
			"omit that field and the gate would then match nothing")
	}
	msg := verr.Error()
	for _, want := range []string{"review.all_done", "reviewer", "does NOT require"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal does not mention %q — the operator must be able to fix it "+
				"without re-deriving the failing case: %s", want, msg)
		}
	}
}
