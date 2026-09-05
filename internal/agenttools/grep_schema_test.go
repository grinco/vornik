package agenttools

import (
	"encoding/json"
	"testing"
)

// September 2026 backlog: callers must see the same positive head_limit
// constraint the helper enforces, rather than being invited to cause a panic.
func TestGrepSchemaPositiveLimit(t *testing.T) {
	for _, tool := range Tools {
		if tool.Name != "grep" {
			continue
		}
		var schema struct {
			Function struct {
				Parameters struct {
					Properties map[string]struct {
						Type    string `json:"type"`
						Minimum int    `json:"minimum"`
					} `json:"properties"`
				} `json:"parameters"`
			} `json:"function"`
		}
		if err := json.Unmarshal(tool.Schema, &schema); err != nil {
			t.Fatal(err)
		}
		limit := schema.Function.Parameters.Properties["head_limit"]
		if limit.Type != "integer" || limit.Minimum != 1 {
			t.Fatalf("head_limit contract: %+v", limit)
		}
		return
	}
	t.Fatal("grep declaration missing")
}
