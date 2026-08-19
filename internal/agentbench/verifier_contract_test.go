package agentbench

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"vornik.io/vornik/internal/registry"
)

// Sibling of TestGateTaskProducerRolesCanSatisfyTheScorer, measured 2026-08-18.
//
// The producer guard proved the analyst can PUBLISH pinned ids. It says nothing
// about the other half of the fraction: the scorer reads testing.cases[] from
// the VERIFIER step and requires each entry to carry an `id` matching a pinned
// id and a `status` it recognises (decodeVerifierCases + knownCaseStatus in
// internal/quality/execution_score.go). An id the analyst never pinned is not a
// low score — validatePinnedIDs/decodeVerifierCases fail the whole report closed
// to invalid_evidence/unknown_case_id.
//
// The shipped tester declared `cases: {type: array}` — a bare array with no item
// shape and no description. Neither the rendered prompt skeleton (which prints
// `<array>` for an itemless array) nor the provider JSON Schema then carried any
// statement of what an entry contains or that the id space is closed. Scored
// against a real 2026-08-17 tester report and a real 2026-08-18 analyst report,
// that produced invalid_evidence/unknown_case_id: the tester had enumerated more
// cases than the analyst pinned, which floors the metric at zero rather than
// grading it.
//
// This asserts the SHIPPED verifier role describes the evidence the SHIPPED
// scorer consumes, so the graded metric cannot be reintroduced as unreachable.
func TestGateTaskVerifierRolesDescribeTheEvidenceTheScorerReads(t *testing.T) {
	swarms, err := registry.LoadSwarms("../../configs")
	if err != nil {
		t.Fatalf("load shipped swarms: %v", err)
	}

	tasksets, err := filepath.Glob("tasksets/*.json")
	if err != nil || len(tasksets) == 0 {
		t.Fatalf("no tasksets found: %v", err)
	}

	checked := 0
	for _, path := range tasksets {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		var tasks []TaskSpec
		if err := json.Unmarshal(raw, &tasks); err != nil {
			continue // not a task-set shape; other tests cover parsing
		}
		for _, task := range tasks {
			if task.Scoring == nil || task.Scoring.VerifierStep == "" {
				continue
			}
			role := producerRoleFor(swarms, task.Scoring.VerifierStep)
			if role == nil {
				t.Errorf("task %q scores on verifier step %q but no shipped role produces it",
					task.ID, task.Scoring.VerifierStep)
				continue
			}
			checked++
			cases := testingCasesSchema(role)
			if cases == nil {
				t.Errorf("task %q scores with kind %q, but verifier role %q does not declare "+
					"testing.cases in its outputSchema — the scorer reads that field",
					task.ID, task.Scoring.Kind, role.Name)
				continue
			}
			if cases.Items == nil {
				t.Errorf("verifier role %q declares testing.cases as a bare array with no item "+
					"shape — the prompt skeleton renders it as `<array>`, so nothing tells the "+
					"model an entry is {id, status} and the scorer's id space is closed",
					role.Name)
				continue
			}
			item := cases.Items
			for _, field := range []string{"id", "status"} {
				if _, ok := item.Properties[field]; !ok {
					t.Errorf("verifier role %q declares testing.cases[] items without %q — "+
						"decodeVerifierCases reads exactly {id, status}", role.Name, field)
				}
			}
			if len(item.Properties["status"].Enum) == 0 {
				t.Errorf("verifier role %q leaves testing.cases[].status unconstrained — "+
					"knownCaseStatus accepts a closed set and rejects anything else as "+
					"unknown_case_status, zeroing the whole report", role.Name)
			}
			if cases.Description == "" {
				t.Errorf("verifier role %q gives testing.cases no description — the model is "+
					"never told the ids must be exactly the analyst's pinned set, and an "+
					"extra id fails the report closed rather than lowering the score", role.Name)
			}
		}
	}
	if checked == 0 {
		t.Error("no shipped task declares a scoring verifier step — this guard is watching " +
			"nothing; if the contract moved, move the guard with it")
	}
}

// testingCasesSchema returns the role's testing.cases subschema, or nil.
func testingCasesSchema(role *registry.SwarmRole) *registry.OutputSchema {
	if role == nil || role.OutputSchema == nil {
		return nil
	}
	testing, ok := role.OutputSchema.Properties["testing"]
	if !ok || testing == nil {
		return nil
	}
	cases, ok := testing.Properties["cases"]
	if !ok {
		return nil
	}
	return cases
}
