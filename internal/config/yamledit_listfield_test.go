package config

import (
	"strings"
	"testing"
)

// SetYAMLListItemField is the actionizer's workhorse (LLD 2026-07-11
// actionable-proposals §4.2): update ONE field on ONE list item matched by a
// field value, preserving every other field, comment, and the list order.

const listFieldFixture = `# swarm roles
roles:
  # the planner
  - name: planner
    model: "old-model" # inline comment
    modelFallback: "fb"
  - name: coder
    model: "coder-model"
`

func TestSetYAMLListItemField_UpdatesMatchedItemOnly(t *testing.T) {
	out, err := SetYAMLListItemField([]byte(listFieldFixture), "roles", "name", "planner", "model", "new-model")
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, "new-model") {
		t.Fatal("matched item's field not updated")
	}
	if !strings.Contains(s, "coder-model") {
		t.Fatal("unmatched item must be untouched")
	}
	if !strings.Contains(s, `modelFallback: "fb"`) {
		t.Fatal("sibling fields on the matched item must be preserved")
	}
	if !strings.Contains(s, "# swarm roles") || !strings.Contains(s, "# the planner") || !strings.Contains(s, "# inline comment") {
		t.Fatal("comments must be preserved")
	}
	if strings.Contains(s, "old-model") {
		t.Fatal("old value must be gone")
	}
}

func TestSetYAMLListItemField_NestedListKey(t *testing.T) {
	in := []byte("mcp:\n  servers:\n    - name: scraper\n      timeout_seconds: 30\n    - name: other\n      timeout_seconds: 10\n")
	out, err := SetYAMLListItemField(in, "mcp.servers", "name", "scraper", "timeout_seconds", 120)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, "timeout_seconds: 120") {
		t.Fatal("timeout not updated")
	}
	if !strings.Contains(s, "timeout_seconds: 10") {
		t.Fatal("other server must be untouched")
	}
}

func TestSetYAMLListItemField_CreatesFieldWhenAbsent(t *testing.T) {
	in := []byte("mcp:\n  servers:\n    - name: scraper\n      transport: sse\n")
	out, err := SetYAMLListItemField(in, "mcp.servers", "name", "scraper", "timeout_seconds", 90)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, "timeout_seconds: 90") {
		t.Fatal("absent field must be appended to the matched item")
	}
	if !strings.Contains(s, "transport: sse") {
		t.Fatal("existing fields must be preserved")
	}
}

func TestSetYAMLListItemField_Errors(t *testing.T) {
	base := []byte(listFieldFixture)
	// Missing list key.
	if _, err := SetYAMLListItemField(base, "steps", "id", "x", "timeout", "5m"); err == nil {
		t.Fatal("missing list must error")
	}
	// No matching item.
	if _, err := SetYAMLListItemField(base, "roles", "name", "ghost", "model", "m"); err == nil {
		t.Fatal("missing item must error")
	}
	// Path segment exists but is a scalar, not a sequence.
	if _, err := SetYAMLListItemField([]byte("roles: nope\n"), "roles", "name", "planner", "model", "m"); err == nil {
		t.Fatal("scalar at list key must error")
	}
	// Unsupported value type.
	if _, err := SetYAMLListItemField(base, "roles", "name", "planner", "model", 3.14); err == nil {
		t.Fatal("unsupported value type must error")
	}
}

// Workflow-file shape: steps keyed by `id`, timeout as quoted duration.
func TestSetYAMLListItemField_WorkflowStepTimeout(t *testing.T) {
	in := []byte("steps:\n  - id: implement\n    role: coder\n    timeout: \"10m\"\n  - id: review\n    role: reviewer\n    timeout: \"15m\"\n")
	out, err := SetYAMLListItemField(in, "steps", "id", "implement", "timeout", "24m")
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, "timeout: \"24m\"") && !strings.Contains(s, "timeout: 24m") {
		t.Fatalf("step timeout not updated: %s", s)
	}
	if !strings.Contains(s, `timeout: "15m"`) {
		t.Fatal("other step must be untouched")
	}
}
