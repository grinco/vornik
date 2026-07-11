package projectwizard

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v5"
	"vornik.io/vornik/internal/rolelibrary"
)

// compileTier3Schema compiles the package's tier-3 response_format
// schema so tests can validate documents against the same schema the
// LLM is constrained to.
func compileTier3Schema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	c := jsonschema.NewCompiler()
	if err := c.AddResource("tier3.json", bytes.NewReader(tier3EnvelopeSchemaJSON())); err != nil {
		t.Fatalf("add schema resource: %v", err)
	}
	s, err := c.Compile("tier3.json")
	if err != nil {
		t.Fatalf("compile tier-3 schema: %v", err)
	}
	return s
}

// TestTier3SchemaMatchesAppendixA asserts the schema constant carries
// the invariants the design's Appendix A pins: required top-level
// fields, tier enum, and the workflow minItems 1 / maxItems 2 cap plus
// the plan's required steps + cost_band.
//
// v2 (whole-branch review C1 fix) additionally pins that the schema is
// a true SUPERSET of the plain tier-1/2 envelope: `proposal` and
// `composition` must be present alongside `bundle`, so ONE
// response_format can express any tier a composer-enabled turn picks
// (see composerResponseFormat / Wizard.responseFormatForTurn).
func TestTier3SchemaMatchesAppendixA(t *testing.T) {
	schema := tier3EnvelopeSchema
	req := toStringSet(schema["required"])
	for _, want := range []string{"message", "tier", "ready_to_commit"} {
		if !req[want] {
			t.Errorf("top-level required missing %q", want)
		}
	}
	props := schema["properties"].(map[string]any)
	tier := props["tier"].(map[string]any)
	if tier["type"] != "integer" {
		t.Errorf("tier type = %v, want integer", tier["type"])
	}
	if got := tier["enum"].([]any); len(got) != 3 {
		t.Errorf("tier enum = %v, want [1 2 3]", got)
	}
	bundle := props["bundle"].(map[string]any)
	breq := toStringSet(bundle["required"])
	for _, want := range []string{"project", "swarm", "workflows", "plan"} {
		if !breq[want] {
			t.Errorf("bundle required missing %q", want)
		}
	}
	bprops := bundle["properties"].(map[string]any)
	wf := bprops["workflows"].(map[string]any)
	if wf["minItems"] != 1 {
		t.Errorf("workflows minItems = %v, want 1", wf["minItems"])
	}
	if wf["maxItems"] != 2 {
		t.Errorf("workflows maxItems = %v, want 2", wf["maxItems"])
	}
	plan := bprops["plan"].(map[string]any)
	preq := toStringSet(plan["required"])
	if !preq["steps"] || !preq["cost_band"] {
		t.Errorf("plan required = %v, want steps + cost_band", plan["required"])
	}
	if ComposerSchemaVersion == "" {
		t.Error("ComposerSchemaVersion must be set")
	}
	if ComposerSchemaVersion != "v2" {
		t.Errorf("ComposerSchemaVersion = %q, want the v2 superset bump (whole-branch review C1 fix)", ComposerSchemaVersion)
	}
	proposal, ok := props["proposal"].(map[string]any)
	if !ok {
		t.Fatal("expected a top-level \"proposal\" sub-schema (tier-1 superset field)")
	}
	pprops, _ := proposal["properties"].(map[string]any)
	if _, ok := pprops["raw"]; !ok {
		t.Errorf("proposal.properties missing \"raw\", got %v", pprops)
	}
	composition, ok := props["composition"].(map[string]any)
	if !ok {
		t.Fatal("expected a top-level \"composition\" sub-schema (tier-2 superset field)")
	}
	cprops, _ := composition["properties"].(map[string]any)
	for _, want := range []string{"template", "params", "addons"} {
		if _, ok := cprops[want]; !ok {
			t.Errorf("composition.properties missing %q, got %v", want, cprops)
		}
	}
}

// TestComposerResponseFormat_CarriesTheSupersetSchema pins
// composerResponseFormat's json_schema body against the same
// tier3EnvelopeSchema this test file pins above — the seam
// Wizard.responseFormatForTurn hands to the chat client on a
// composer-enabled turn.
func TestComposerResponseFormat_CarriesTheSupersetSchema(t *testing.T) {
	rf := composerResponseFormat()
	if rf.Type != "json_schema" || rf.JSONSchema == nil {
		t.Fatalf("expected a json_schema response_format, got %+v", rf)
	}
	if string(rf.JSONSchema.Schema) != string(tier3EnvelopeSchemaJSON()) {
		t.Error("composerResponseFormat must carry the package's tier3EnvelopeSchema verbatim")
	}
}

func toStringSet(v any) map[string]bool {
	out := map[string]bool{}
	if list, ok := v.([]any); ok {
		for _, e := range list {
			if s, ok := e.(string); ok {
				out[s] = true
			}
		}
	}
	return out
}

// TestGoldenEnvelopesParseAndShapeCheck parses each recorded canonical
// tier-3 envelope via parseEnvelope and validates it against the
// tier-3 schema — guarding the envelope schema against drift (design
// §8 golden-envelope tests). The staged-registry VALIDATION of these
// bundles is task 1.1b; here it is parse + schema-shape only.
func TestGoldenEnvelopesParseAndShapeCheck(t *testing.T) {
	schema := compileTier3Schema(t)
	dir := filepath.Join("testdata", "golden-envelopes")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read golden dir: %v", err)
	}
	seen := 0
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".json" {
			continue
		}
		seen++
		t.Run(e.Name(), func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}

			// 1. Schema shape-check (raw document vs the response_format schema).
			var doc any
			if err := json.Unmarshal(raw, &doc); err != nil {
				t.Fatalf("golden is not valid JSON: %v", err)
			}
			if err := schema.Validate(doc); err != nil {
				t.Fatalf("golden violates tier-3 schema: %v", err)
			}

			// 2. parseEnvelope round-trips it into the typed Envelope.
			env, err := parseEnvelope(string(raw))
			if err != nil {
				t.Fatalf("parseEnvelope: %v", err)
			}
			if env.Tier != 3 {
				t.Errorf("Tier = %d, want 3", env.Tier)
			}
			if env.Bundle == nil {
				t.Fatal("Bundle is nil")
			}
			n := len(env.Bundle.Workflows)
			if n < 1 || n > 2 {
				t.Errorf("workflows = %d, want 1..2", n)
			}
			if len(env.Bundle.Plan.Steps) == 0 {
				t.Error("plan.steps is empty")
			}
			if env.Bundle.Plan.CostBand == "" {
				t.Error("plan.cost_band is empty")
			}
			if env.Bundle.Project == nil || env.Bundle.Swarm == nil {
				t.Error("bundle project/swarm must be present")
			}
		})
	}
	if seen != 4 {
		t.Fatalf("expected 4 golden envelopes, found %d", seen)
	}
}

// TestParseEnvelopeTier3Bundle covers the parse path directly with an
// inline tier-3 envelope (fenced) to pin that parseEnvelope populates
// Tier + Bundle from the extended struct.
func TestParseEnvelopeTier3Bundle(t *testing.T) {
	raw := "```json\n" + `{
      "message": "ok",
      "tier": 3,
      "ready_to_commit": false,
      "bundle": {
        "project": {"projectId": "p"},
        "swarm": {"swarmId": "s"},
        "workflows": [{"workflowId": "w"}],
        "plan": {"steps": ["do a thing"], "cost_band": "~$0.10", "approvals_bypassed": ["email send"]}
      }
    }` + "\n```"
	env, err := parseEnvelope(raw)
	if err != nil {
		t.Fatalf("parseEnvelope: %v", err)
	}
	if env.Tier != 3 || env.Bundle == nil {
		t.Fatalf("tier/bundle not parsed: tier=%d bundle=%v", env.Tier, env.Bundle)
	}
	if got := env.Bundle.Plan.ApprovalsBypassed; len(got) != 1 || got[0] != "email send" {
		t.Errorf("ApprovalsBypassed = %v", got)
	}
	if env.Bundle.Project["projectId"] != "p" {
		t.Errorf("project map not parsed: %v", env.Bundle.Project)
	}
}

// TestNonTier3EnvelopeUnaffected pins that a legacy (tier-omitted)
// envelope still parses with Tier 0 and nil Bundle — the extension is
// backward-compatible.
func TestNonTier3EnvelopeUnaffected(t *testing.T) {
	env, err := parseEnvelope(`{"message":"hi","ready_to_commit":false}`)
	if err != nil {
		t.Fatalf("parseEnvelope: %v", err)
	}
	if env.Tier != 0 || env.Bundle != nil {
		t.Errorf("legacy envelope: tier=%d bundle=%v, want 0/nil", env.Tier, env.Bundle)
	}
}

// TestGoldenEnvelopes_FullStagedValidationAndGuardrails runs each of
// the 4 canonical golden tier-3 envelopes through the FULL 1.1b
// engine pipeline: materialize (archetype expansion) → guardrail pass
// (fill + violation detection) → render → staged registry validation.
// This is the "4 golden bundles pass full validation+guardrails" item
// of design §8 — the shape-check-only coverage lives in
// TestGoldenEnvelopesParseAndShapeCheck above; this test exercises the
// server-side enforcement the LLM output must survive before
// ready_to_commit can ever be true.
func TestGoldenEnvelopes_FullStagedValidationAndGuardrails(t *testing.T) {
	archetypes, err := rolelibrary.Load("../../configs")
	if err != nil {
		t.Fatalf("load role library: %v", err)
	}
	if len(archetypes) == 0 {
		t.Fatal("expected the shipped role-library archetypes to load")
	}
	archMap := make(map[string]*rolelibrary.RoleArchetype, len(archetypes))
	for _, a := range archetypes {
		archMap[a.ArchetypeID] = a
	}

	dir := filepath.Join("testdata", "golden-envelopes")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read golden dir: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".json" {
			continue
		}
		t.Run(e.Name(), func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			env, err := parseEnvelope(string(raw))
			if err != nil {
				t.Fatalf("parseEnvelope: %v", err)
			}
			if _, shapeErrs := shapeCheckBundle(env.Bundle); len(shapeErrs) != 0 {
				t.Fatalf("shape check failed: %v", shapeErrs)
			}
			mb, toolViolations, err := materializeBundle(env.Bundle, archMap)
			if err != nil {
				t.Fatalf("materializeBundle: %v", err)
			}
			gr := applyGuardrails(mb, env.Bundle.Plan, toolViolations, testBudgetDefaults(), "")
			if len(gr.Violations) != 0 {
				t.Fatalf("unexpected guardrail violations for a golden bundle: %v", gr.Violations)
			}
			files, err := renderMaterializedBundle(mb)
			if err != nil {
				t.Fatalf("renderMaterializedBundle: %v", err)
			}
			res, err := stageBundleForValidation("", files)
			if err != nil {
				t.Fatalf("stageBundleForValidation: %v", err)
			}
			if !res.OK {
				t.Fatalf("staged validation failed for a golden bundle: %v", res.Errors)
			}
		})
	}
}
