package api

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Measured on the bench instance 2026-08-18/19: the dev-swarm tester emitted a
// schema-valid result carrying testing.cases in 4 of 50 attempts (8%) across
// the whole retry ladder (test → test_shape_retry → test_model_fallback). Every
// other attempt failed the passed_requires_pinned_validation plausibility rule
// with "field testing.cases is missing or empty", which floors dev-pipeline's
// pinned_case_validation score at missing_contract.
//
// It is not a model limitation. The schema said cases was OPTIONAL: the testing
// object listed `required: [passed]` only, so a result of {"passed": true} was
// fully schema-conformant and the model was doing exactly what the machine
// contract permitted. The obligation lived only in prose and in a plausibility
// rule evaluated AFTER generation, costing a retry each time instead of
// constraining the decoder.
//
// role.OutputSchema.ToToolSpec feeds ToJSONSchema straight through as the
// emit_<role>_result tool's parameters, so `required` is what actually binds at
// tool-call decoding time. Naming a field in a plausibility `require:` does not.
//
// Both fields are unconditionally emittable, which is why requiring them is
// correct rather than merely convenient: the case `status` enum includes
// `missing` ("no test exists for it yet"), so a tester that ran nothing still
// has a truthful entry for every pinned id, and pinned_cases_validated is a
// bool that is always either true or false.
func TestShippedConfigs_TesterSchemaRequiresPinnedCaseEvidence(t *testing.T) {
	for _, swarm := range []string{"dev-swarm.md", "basic-swarm.md"} {
		t.Run(swarm, func(t *testing.T) {
			node := testerTestingNode(t, shippedConfigsDir+"/swarms/"+swarm)
			if node == nil {
				t.Skipf("%s has no tester role with a testing output schema", swarm)
			}
			req := map[string]bool{}
			for _, r := range asStrings(node["required"]) {
				req[r] = true
			}
			props, _ := node["properties"].(map[string]any)
			for _, field := range []string{"cases", "pinned_cases_validated"} {
				if _, declared := props[field]; !declared {
					continue // that swarm's tester does not do pinned-case validation
				}
				if !req[field] {
					t.Errorf("testing.%s is declared but not in `required` — the "+
						"emit_tester_result tool schema therefore permits omitting it, "+
						"which is what the model did in 46 of 50 measured attempts; the "+
						"plausibility rule only catches it afterwards, one retry later", field)
				}
			}
		})
	}
}

// testerTestingNode returns the tester role's outputSchema.properties.testing
// node from a swarm file's YAML front matter, or nil when absent.
func testerTestingNode(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	body := string(raw)
	if !strings.HasPrefix(body, "---") {
		t.Fatalf("%s has no YAML front matter", path)
	}
	end := strings.Index(body[3:], "\n---")
	if end < 0 {
		t.Fatalf("%s front matter is unterminated", path)
	}
	var fm struct {
		Roles []map[string]any `yaml:"roles"`
	}
	if err := yaml.Unmarshal([]byte(body[3:end+3]), &fm); err != nil {
		t.Fatalf("parse %s front matter: %v", path, err)
	}
	for _, role := range fm.Roles {
		if name, _ := role["name"].(string); name != "tester" {
			continue
		}
		schema, _ := role["outputSchema"].(map[string]any)
		props, _ := schema["properties"].(map[string]any)
		node, _ := props["testing"].(map[string]any)
		return node
	}
	return nil
}

func asStrings(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, item := range list {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
