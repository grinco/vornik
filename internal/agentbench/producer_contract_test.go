package agentbench

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"vornik.io/vornik/internal/registry"
)

// Regression, measured 2026-08-17: the first gate-bootstrap arm scored
// 0.000/missing_contract on ALL 20 gate tasks.
//
// The pinned_case_validation scorer reads analysis.test_case_ids and
// analysis.test_cases_pinned from the PRODUCER step's result.json
// (internal/quality/execution_score.go producerEnvelope). The dev-swarm analyst
// — the producer every gate task names — declared NEITHER field in its
// outputSchema. The metric was unsatisfiable by construction: the model had no
// field to comply with, so it could never emit the evidence, and the tester's
// passed_requires_pinned_validation could never be met either because it
// validates against ids the analyst had no way to publish.
//
// Same class as the plausibility defect fixed earlier the same day: a contract
// enforced in one place that the agent is never told about. Here it was worse —
// the schema did not merely omit the guidance, it omitted the fields.
//
// This asserts the SHIPPED configuration can satisfy the SHIPPED scorer, so a
// future scoring contract cannot land without the role that must feed it.
func TestGateTaskProducerRolesCanSatisfyTheScorer(t *testing.T) {
	// The field names the scorer unmarshals. Keep in step with producerEnvelope.
	const (
		fieldCaseIDs = "test_case_ids"
		fieldPinned  = "test_cases_pinned"
	)

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
			if task.Scoring == nil || task.Scoring.ProducerStep == "" {
				continue
			}
			// Find the role that runs the producer step. Gate tasks in the
			// shipped sets run dev-pipeline, whose analyze step is the analyst.
			role := producerRoleFor(swarms, task.Scoring.ProducerStep)
			if role == nil {
				t.Errorf("task %q scores on producer step %q but no shipped role produces it",
					task.ID, task.Scoring.ProducerStep)
				continue
			}
			checked++
			props := analysisProperties(role)
			for _, field := range []string{fieldCaseIDs, fieldPinned} {
				if _, ok := props[field]; !ok {
					t.Errorf("task %q scores with kind %q, but producer role %q does not declare "+
						"analysis.%s in its outputSchema — the scorer reads that field, so the "+
						"metric is unsatisfiable and every run floors at missing_contract",
						task.ID, task.Scoring.Kind, role.Name, field)
				}
			}
		}
	}
	if checked == 0 {
		t.Error("no shipped task declares a scoring producer step — this guard is watching " +
			"nothing; if the contract moved, move the guard with it")
	}
}

// producerRoleFor returns the role that runs the producing step in the swarm
// the shipped gate tasks actually execute under.
//
// SCOPED TO dev-swarm ON PURPOSE. The bench runs `--swarm dev-swarm`, and
// several shipped swarms define a role called "analyst" — ranging over a map
// would pick an arbitrary one and make this guard non-deterministic, passing or
// failing on Go's map iteration order rather than on the configuration. If the
// bench's swarm changes, change it here too and the guard keeps meaning
// something.
const producerSwarmID = "dev-swarm"

func producerRoleFor(swarms map[string]*registry.Swarm, step string) *registry.SwarmRole {
	want := map[string]string{"analyze": "analyst", "test": "tester"}[step]
	if want == "" {
		want = step
	}
	sw := swarms[producerSwarmID]
	if sw == nil {
		return nil
	}
	for i := range sw.Roles {
		if sw.Roles[i].Name == want && sw.Roles[i].OutputSchema != nil {
			return &sw.Roles[i]
		}
	}
	return nil
}

func analysisProperties(role *registry.SwarmRole) map[string]any {
	if role == nil || role.OutputSchema == nil {
		return nil
	}
	raw, err := json.Marshal(role.OutputSchema)
	if err != nil {
		return nil
	}
	var shape struct {
		Properties struct {
			Analysis struct {
				Properties map[string]any `json:"properties"`
			} `json:"analysis"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &shape); err != nil {
		return nil
	}
	return shape.Properties.Analysis.Properties
}
