package executor

import (
	"testing"

	"vornik.io/vornik/internal/registry"
)

func TestRecoveryOverrideClearsTheEffectiveSchema(t *testing.T) {
	// Test 59 — the trap D6.1b names. The recovery override runs AFTER
	// applyRoleSchemaOpts has already installed the role's dialect tree, and
	// it replaces the emitted schema with a hand-built map. Leaving the tree
	// behind would enforce the role's enums against output produced under a
	// completely different contract.
	role := &registry.SwarmRole{
		Name: "lead",
		OutputSchema: &registry.OutputSchema{
			Type: "object",
			Properties: map[string]*registry.OutputSchema{
				"decision": {Type: "string", Enum: []any{"approve", "reject"}},
			},
		},
	}
	opts := &agentInputOpts{}
	applyRoleSchemaOpts(opts, role)
	if opts.EffectiveSchema == nil {
		t.Fatal("precondition failed: the role's tree should be installed before the override")
	}

	applyRecoverySchemaOverride(opts)

	if opts.EffectiveSchema != nil {
		t.Fatal("the role's dialect tree survived the recovery override; a lead answering the " +
			"recovery schema would be failed for violating the role's enum instead")
	}
	if opts.ResponseSchema == nil {
		t.Fatal("the recovery response schema was not installed")
	}
	if opts.ResultEmissionTool != nil {
		t.Fatal("a recovery hop must carry no result-emission tool")
	}

	// And the consequence at the seam: a recovery reply that would violate the
	// ROLE's enum is not an enum failure, while requiredOutputKeys still runs.
	e := &Executor{}
	reply := []byte(`{"outcome":"checkpoint","decision":"defer"}`)
	if msg := e.checkOutputContract(reply, "lead", nil, opts.EffectiveSchema); msg != "" {
		t.Fatalf("recovery reply failed the enum check: %q", msg)
	}
	if msg := e.checkOutputContract(reply, "lead", []string{"summary"}, opts.EffectiveSchema); msg == "" {
		t.Fatal("requiredOutputKeys stopped running on a recovery hop; nil EffectiveSchema must " +
			"suppress the enum sub-check ONLY")
	}
}
