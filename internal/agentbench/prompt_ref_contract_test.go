package agentbench

import (
	"fmt"
	"strings"
	"testing"

	"vornik.io/vornik/internal/quality"
	"vornik.io/vornik/internal/registry"
)

// Test 46 (agent-quality-benchmark design, amendment 2026-09-18, rewritten
// after F5 of review-20260918-384e).
//
// The mechanism rests on ONE invariant: the value interpolated into the
// verifier's prompt is the value the scorer divides by. An earlier draft
// asserted only that the prompt named the configured producer STEP. That is
// not enough — the scorer reads a specific FIELD PATH, and the executor's
// resolver will happily resolve whatever dotted path the prompt carries, so a
// prompt naming the right step with the wrong field hands the verifier one
// value while the scorer reads another and a step-only assertion passes
// throughout.
//
// So this asserts the COMPLETE reference, step AND field path, and it takes
// the field path from quality.PinnedProducerFieldPath rather than a literal —
// the constant is bound to producerEnvelope by
// TestPinnedProducerFieldPathMatchesEnvelope in the quality package, so this
// is a chain to the scorer's real behaviour, not a literal compared to itself.
func TestScoringVerifierPromptReferencesTheScorersProducerField(t *testing.T) {
	// LoadWorkflows takes the configs ROOT and resolves workflows/ itself.
	workflows, err := registry.LoadWorkflows("../../configs")
	if err != nil {
		t.Fatalf("load shipped workflows: %v", err)
	}

	checked := 0
	for id, wf := range workflows {
		p := wf.QualityScoring
		if p == nil || p.Kind != quality.ScoreKindPinnedCaseValidation {
			continue
		}
		verifier, ok := wf.Steps[p.VerifierStep]
		if !ok {
			t.Errorf("%s: verifierStep %q is not a step", id, p.VerifierStep)
			continue
		}
		want := fmt.Sprintf("${outputs.%s.%s}", p.ProducerStep, quality.PinnedProducerFieldPath)
		if !strings.Contains(verifier.Prompt, want) {
			t.Errorf("%s: verifier step %q prompt does not carry %s\n"+
				"the verifier must be handed the same value the scorer divides by, "+
				"not told to re-read it from a prose file", id, p.VerifierStep, want)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no shipped workflow declares pinned_case_validation; this contract is unenforced")
	}
}

// D5's anti-no-op guard. Pinning the case-id enum walks
// testing -> cases -> items -> id in the verifier role's declared schema. If a
// future edit renames or restructures that path, pinCaseIDEnum returns the
// schema unchanged and the constraint silently stops being applied — the
// agent would go back to emitting whatever ids it likes and nothing would say
// so. This asserts the SHIPPED verifier role still exposes a pinnable id node.
func TestShippedVerifierRoleHasAPinnableCaseIDNode(t *testing.T) {
	workflows, err := registry.LoadWorkflows("../../configs")
	if err != nil {
		t.Fatalf("load shipped workflows: %v", err)
	}
	swarms, err := registry.LoadSwarms("../../configs")
	if err != nil {
		t.Fatalf("load shipped swarms: %v", err)
	}

	checked := 0
	for id, wf := range workflows {
		p := wf.QualityScoring
		if p == nil || p.Kind != quality.ScoreKindPinnedCaseValidation {
			continue
		}
		verifier, ok := wf.Steps[p.VerifierStep]
		if !ok {
			continue
		}
		role := findRole(swarms, verifier.Role)
		if role == nil {
			t.Errorf("%s: verifier role %q not found in any shipped swarm", id, verifier.Role)
			continue
		}
		if role.OutputSchema == nil {
			t.Errorf("%s: verifier role %q declares no outputSchema", id, verifier.Role)
			continue
		}
		node := role.OutputSchema.Properties["testing"]
		if node == nil {
			t.Errorf("%s: role %q schema has no testing object", id, verifier.Role)
			continue
		}
		cases := node.Properties["cases"]
		if cases == nil || cases.Items == nil || cases.Items.Properties["id"] == nil {
			t.Errorf("%s: role %q has no testing.cases[].id node — the pinned-case enum "+
				"cannot be applied and D5 silently does nothing", id, verifier.Role)
			continue
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no shipped pinned_case_validation verifier was checked; this contract is unenforced")
	}
}

// findRole resolves a role name within dev-swarm.
//
// SCOPED TO dev-swarm ON PURPOSE, for the reason producerRoleFor already
// records: several shipped swarms define a role called "tester", so ranging
// over the swarm map would pick an arbitrary one and make this guard pass or
// fail on Go's map iteration order rather than on the configuration. The bench
// runs `--swarm dev-swarm`; if that changes, change it here too.
func findRole(swarms map[string]*registry.Swarm, name string) *registry.SwarmRole {
	sw := swarms["dev-swarm"]
	if sw == nil {
		return nil
	}
	for i := range sw.Roles {
		if sw.Roles[i].Name == name {
			return &sw.Roles[i]
		}
	}
	return nil
}
