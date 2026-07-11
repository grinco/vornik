package fixitdoctor

import (
	"encoding/json"

	"vornik.io/vornik/internal/chat"
)

// EnvelopeResponseFormat returns the json_schema response_format
// directive that constrains the LLM to the FixItEnvelope shape,
// mirroring projectwizard's envelopeResponseFormat (wizard.go:506).
// The actions[].kind enum is built from AllowedActionKinds(edition)
// so a Community build's LLM call is never even told config_apply
// exists — the edition gate applies at the prompt-contract level, not
// just at server-side validation time.
func EnvelopeResponseFormat(edition string) *chat.ResponseFormat {
	kinds := AllowedActionKinds(edition)
	kindEnum := make([]string, 0, len(kinds))
	for _, k := range kinds {
		kindEnum = append(kindEnum, string(k))
	}

	schema := map[string]any{
		"type":     "object",
		"required": []string{"message", "resolved"},
		"properties": map[string]any{
			"message": map[string]any{
				"type":        "string",
				"description": "Operator-facing assistant message.",
			},
			"resolved": map[string]any{
				"type":        "boolean",
				"description": "True when the assistant believes the underlying failure is now fixed.",
			},
			"actions": map[string]any{
				"type":        "array",
				"description": "Proposed remediation actions, if any.",
				"items": map[string]any{
					"type":     "object",
					"required": []string{"kind", "label"},
					"properties": map[string]any{
						"kind": map[string]any{
							"type": "string",
							"enum": kindEnum,
						},
						"label": map[string]any{
							"type":        "string",
							"description": "Short operator-facing label for this action.",
						},
						"params": map[string]any{
							"type":                 "object",
							"additionalProperties": map[string]any{"type": "string"},
						},
					},
				},
			},
		},
	}
	schemaBytes, _ := json.Marshal(schema)
	return &chat.ResponseFormat{
		Type: "json_schema",
		JSONSchema: &chat.ResponseJSONSchema{
			Name:        "FixItEnvelope",
			Description: "Structured output for one Fix-It Doctor repair-chat turn.",
			Schema:      schemaBytes,
		},
	}
}
