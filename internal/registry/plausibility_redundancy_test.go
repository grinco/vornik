package registry

import (
	"strings"
	"testing"
)

// Regression, measured 2026-08-18 on the agent benchmark: the assistant-swarm
// planner failed 11 of 11 attempts on a 27B while the researcher — running in
// the SAME executions, one step earlier — passed 11 of 11. The difference was
// the rule, not the model:
//
//	researcher  written_implies_files  require: ["produced_files"]                  11/11 pass
//	planner     written_implies_path   require: ["planning.path","produced_files"]  0/11
//
// The planner demanded the same fact twice: the path of the file it had just
// written. The model wrote a correct 1278-byte plan, and its retries converged
// on produced_files: ["artifacts/out/plan.md"] without ever restating it in
// planning.path — so the harness failed a run whose work had succeeded.
//
// produced_files is the canonical evidence field: verifyRoleClaims checks it
// against the real diff on every step of every deployment. A rule that also
// demands a role-specific copy is asking a model to say one thing in two
// places, and failing it for choosing the standard one.
func TestPlausibilityRulesDoNotDemandTheSameEvidenceTwice(t *testing.T) {
	swarms, err := LoadSwarms("../../configs")
	if err != nil {
		t.Fatalf("load shipped swarms: %v", err)
	}
	checked := 0
	for swarmID, swarm := range swarms {
		if swarm == nil {
			continue
		}
		for i := range swarm.Roles {
			role := &swarm.Roles[i]
			rules := role.PlausibilityRules
			if role.OutputSchema != nil {
				rules = append(rules, role.OutputSchema.DerivePlausibilityRules()...)
			}
			for _, rule := range rules {
				wantsProduced := false
				var pathLike []string
				for _, field := range rule.Require {
					switch {
					case field == "produced_files":
						wantsProduced = true
					case strings.HasSuffix(field, ".path"), strings.HasSuffix(field, ".files"):
						pathLike = append(pathLike, field)
					}
				}
				checked++
				if wantsProduced && len(pathLike) > 0 {
					t.Errorf("swarm %q role %q rule %q requires produced_files AND %v — "+
						"the same evidence twice. A model that reports the file it wrote in "+
						"produced_files (the field verifyRoleClaims actually checks) is failed "+
						"for not restating it. Require it once.",
						swarmID, role.Name, rule.Name, pathLike)
				}
			}
		}
	}
	if checked == 0 {
		t.Error("no plausibility rules found in the shipped swarms — this guard is " +
			"watching nothing; if the rules moved, move the guard with them")
	}
}
