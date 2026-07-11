package projectwizard

import (
	"encoding/json"

	"vornik.io/vornik/internal/chat"
)

// ComposerSchemaVersion versions the tier-3 envelope response_format
// schema. Bump it whenever the schema below changes shape; the
// golden-envelope tests (composer_golden_test.go) pin the schema
// against drift, and this version is the human-facing marker for
// which schema a recorded golden was captured against.
//
// v2 (whole-branch review C1 fix): the schema is now a true SUPERSET
// of the v1/v2 wizard envelope — it additionally carries `proposal`
// (tier 1) and `composition` (tier 2), copied verbatim from
// envelopeResponseFormat() — so ONE response_format can be handed to
// the model for an entire composer-enabled session regardless of
// which tier it ultimately picks per turn (see Wizard.Converse /
// composerResponseFormat). Before v2, a composer-enabled turn could
// only legally express tier 3; the model had no schema-legal way to
// answer with a plain proposal or composition, which starved every
// non-tier-3 turn of a valid shape.
const ComposerSchemaVersion = "v2"

// tier3EnvelopeSchema is the JSON-schema handed to the chat router's
// response_format for a composer-enabled wizard turn (any of tier
// 1/2/3). It is the authoritative implementation of the schema
// sketched in the design's Appendix A — base WizardEnvelope fields
// plus `tier` and the `bundle` (ComposedBundle + ComposedPlan) shape —
// extended (v2) with the `proposal`/`composition` sub-schemas so the
// same contract covers every tier. The workflow invariant + v1 cap
// (1 <= workflows <= 2) is encoded here at the earliest layer
// (minItems/maxItems); the server-side shape check remains the
// enforcement of record because json_schema enforcement is
// best-effort on some providers (design Appendix A note).
//
// Kept as a Go literal (rather than a raw string) so it is
// syntactically checked at compile time and marshals deterministically
// for the golden tests; tier3EnvelopeSchemaJSON returns the marshalled
// bytes for handing to chat.ResponseJSONSchema.
var tier3EnvelopeSchema = map[string]any{
	"type":     "object",
	"required": []any{"message", "tier", "ready_to_commit"},
	"properties": map[string]any{
		"message":         map[string]any{"type": "string"},
		"tier":            map[string]any{"type": "integer", "enum": []any{1, 2, 3}},
		"ready_to_commit": map[string]any{"type": "boolean"},
		"open_questions": map[string]any{
			"type":  "array",
			"items": map[string]any{"type": "string"},
		},
		"suggested_template": map[string]any{"type": "string"},
		// proposal / composition (v2): identical shape to
		// envelopeResponseFormat()'s tier-1/tier-2 fields — duplicated
		// rather than shared so this file stays the single, compile-
		// time-checked source of truth for the composer contract
		// without wizard.go and composer_schema.go importing each
		// other's schema fragments.
		"proposal": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"raw": map[string]any{
					"type":        "object",
					"description": "Proposed project YAML as a generic map.",
				},
			},
		},
		"composition": map[string]any{
			"type":        "object",
			"description": "Structured build: template, params, and addons.",
			"properties": map[string]any{
				"template": map[string]any{
					"type":        "string",
					"description": "Base template slug.",
				},
				"params": map[string]any{
					"type":                 "object",
					"additionalProperties": true,
					"description":          "Template parameters (each value is a string or array of strings).",
				},
				"addons": map[string]any{
					"type":        "array",
					"description": "Ordered list of typed composition mutations.",
					"items": map[string]any{
						"type":                 "object",
						"required":             []any{"type"},
						"additionalProperties": true,
						"properties": map[string]any{
							"type": map[string]any{
								"type":        "string",
								"description": "Addon type selector (e.g., schedule, mcp_server).",
							},
						},
					},
				},
			},
		},
		"bundle": map[string]any{
			"type":     "object",
			"required": []any{"project", "swarm", "workflows", "plan"},
			"properties": map[string]any{
				"project": map[string]any{"type": "object"},
				"swarm":   map[string]any{"type": "object"},
				"workflows": map[string]any{
					"type":     "array",
					"items":    map[string]any{"type": "object"},
					"minItems": 1,
					"maxItems": 2,
				},
				"plan": map[string]any{
					"type":     "object",
					"required": []any{"steps", "cost_band"},
					"properties": map[string]any{
						"steps": map[string]any{
							"type":     "array",
							"items":    map[string]any{"type": "string"},
							"minItems": 1,
						},
						"schedule":           map[string]any{"type": "string"},
						"cost_band":          map[string]any{"type": "string"},
						"approvals":          map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						"approvals_bypassed": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					},
				},
			},
		},
	},
}

// tier3EnvelopeSchemaJSON marshals the tier-3 schema to JSON bytes for
// chat.ResponseJSONSchema.Schema.
func tier3EnvelopeSchemaJSON() []byte {
	b, _ := json.Marshal(tier3EnvelopeSchema)
	return b
}

// composerResponseFormat returns the json_schema response_format
// directive for a composer-enabled turn (whole-branch review C1 fix):
// the tier3EnvelopeSchema superset, so the model may legally answer
// with tier 1 (proposal), tier 2 (composition), or tier 3 (bundle) in
// one contract. Wizard.Converse selects this INSTEAD of
// envelopeResponseFormat() only when composerTier3Available() is true
// (composer.max_tier >= 3 AND a non-empty role library) — otherwise
// the plain tier-1/2 envelope schema is unchanged, byte for byte.
func composerResponseFormat() *chat.ResponseFormat {
	return &chat.ResponseFormat{
		Type: "json_schema",
		JSONSchema: &chat.ResponseJSONSchema{
			Name:        "ComposerEnvelope",
			Description: "Structured output for one composer-enabled wizard turn (tier 1, 2, or 3).",
			Schema:      tier3EnvelopeSchemaJSON(),
		},
	}
}
