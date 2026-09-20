package executor

import (
	"encoding/json"

	"vornik.io/vornik/internal/quality"
	"vornik.io/vornik/internal/registry"
)

// pinnedCaseIDPath is where the verifier's per-case id lives inside a
// pinned_case_validation tester's output schema, mirroring what
// decodeVerifierCases reads out of the emitted result.
var pinnedCaseIDPath = []string{"testing", "cases", "id"}

// pinCaseIDEnum returns a COPY of the verifier role's output schema with the
// producer's pinned case ids installed as an enum on testing.cases[].id, plus
// the number of ids pinned. The input schema is never mutated: it belongs to
// the swarm role and is shared by every execution of it, so pinning in place
// would leak one task's ids into the next task's tester.
//
// Why the schema and not the prompt (D5). The 2026.9.4 arm measured the
// verifier reporting ids it was not given while demonstrably holding the
// correct list — it read the file containing them on every scored task. This
// design already established the lever that binds, in §12.11.7: the emitted
// schema "is what actually binds at tool-call decoding time — naming a field
// in a plausibility `require:` constrains nothing during generation". A prompt
// instruction is in the same category as a plausibility rule: advisory at
// generation time. An enum is not. With the pinned ids on the id field, a
// conforming decoder cannot emit `case_1` when the enum holds `s1_case_1`,
// so renumbering and inventing become unrepresentable rather than forbidden.
//
// Precedent, measured in-house: 2026.8.7 moved `cases` and
// `pinned_cases_validated` from prose obligation into schema `required` and
// took tester plausibility violations from 37/50 to 0/6 on the same
// deployment. This is the same move one level down.
//
// KNOWN LIMIT, stated because the control must not overclaim: an enum binds
// only where the provider enforces the emitted schema. effectiveResponseFormat
// asks for "json_schema" whenever a role declares an output schema, but a
// provider that does not honour it degrades to a json_object nudge and the
// enum becomes advisory again — exactly what the prompt already was. Whether
// this deployment's provider enforces it is unmeasured; the next arm settles
// it, and a non-zero ExtraCaseCount after this lands is the signal that it
// does not.
func pinCaseIDEnum(schema *registry.OutputSchema, producerResult json.RawMessage) (*registry.OutputSchema, int) {
	if schema == nil {
		return nil, 0
	}
	ids := decodePinnedIDs(producerResult)
	if len(ids) == 0 {
		// No readable pinned ids: leave the schema exactly as declared. An
		// empty enum would admit NOTHING and fail every tester report, which
		// is a worse failure than the one being fixed.
		return schema, 0
	}

	out := schema.Clone()
	node := out
	for i, seg := range pinnedCaseIDPath {
		if node == nil {
			return schema, 0
		}
		next := node.Properties[seg]
		if next == nil {
			return schema, 0
		}
		// An array node carries its element shape on Items; the id lives on
		// the element, not on the array.
		if i < len(pinnedCaseIDPath)-1 && next.Type == "array" {
			next = next.Items
		}
		node = next
	}
	if node == nil {
		return schema, 0
	}
	enum := make([]any, len(ids))
	for i, id := range ids {
		enum[i] = id
	}
	node.Enum = enum
	return out, len(ids)
}

// decodePinnedIDs reads the producer's pinned case ids from its result.json at
// quality.PinnedProducerFieldPath — the same path the scorer's denominator
// comes from, which is the whole point: the verifier is constrained to exactly
// the set it will be scored against.
func decodePinnedIDs(producerResult json.RawMessage) []string {
	if len(producerResult) == 0 {
		return nil
	}
	var doc any
	if err := json.Unmarshal(producerResult, &doc); err != nil {
		return nil
	}
	v := resolveDotted(doc, quality.PinnedProducerFieldPath)
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	ids := make([]string, 0, len(list))
	for _, item := range list {
		s, ok := item.(string)
		if !ok || s == "" {
			return nil
		}
		ids = append(ids, s)
	}
	return ids
}

// applyPinnedCaseEnum specialises the verifier step's output schema for THIS
// execution, installing the producer's pinned case ids as an enum on
// testing.cases[].id and refreshing the two copies the provider is handed
// (opts.ResponseSchema and opts.ResultEmissionTool). It is a no-op unless the
// workflow declares pinned_case_validation AND this step is its verifier.
//
// Called after applyRoleSchemaOpts, which builds those copies from the role's
// shared declaration; this narrows them without touching the declaration.
// Returns the number of ids installed, 0 when the pin did not apply — the
// caller records that as PinnedCaseEnumInstalledTotal, which is what tells a
// later reader whether a violation indicts the provider or the daemon's own
// pin. See D6.4.
func applyPinnedCaseEnum(
	opts *agentInputOpts,
	plan *executionPlan,
	currentStepID string,
	role *registry.SwarmRole,
	state *executionState,
) int {
	if opts == nil || role == nil || role.OutputSchema == nil || state == nil {
		return 0
	}
	if plan == nil || plan.workflow == nil || plan.workflow.QualityScoring == nil {
		return 0
	}
	policy := plan.workflow.QualityScoring
	if policy.Kind != quality.ScoreKindPinnedCaseValidation || policy.VerifierStep != currentStepID {
		return 0
	}
	pinned, n := pinCaseIDEnum(role.OutputSchema, state.StepResults[policy.ProducerStep])
	if n == 0 {
		// Producer evidence unreadable or absent. Leave the declared schema in
		// place: the scorer will record the missing-producer diagnostic, and a
		// tester constrained to an empty set would fail for the wrong reason.
		return 0
	}
	opts.ResponseSchema = pinned.ToJSONSchema()
	opts.ResultEmissionTool = pinned.ToToolSpec(role.Name)
	// D6.1a: RETAIN the clone. Generating the two provider copies from it and
	// then dropping it left the dialect tree unreachable, so a receipt-time
	// check reaching for it would find the role's shared declaration — which
	// deliberately never carries these ids — and enforce the wrong contract.
	opts.EffectiveSchema = pinned
	return n
}
