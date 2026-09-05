package dispatcher

import (
	"encoding/json"
	"reflect"
	"testing"

	"vornik.io/vornik/internal/agenttools"
	"vornik.io/vornik/internal/contractreg"
)

// The dispatcher's chat tools and the container's agent tools are two
// vocabularies with four names in common. They are separate implementations
// behind separate gates and their descriptions differ on purpose; what a
// model carries from one surface to the other is the PARAMETER SHAPE, so
// that is what is held equal (agent-tool declaration design §12, 2026-09-05).
// This runs as a test rather than inside cmd/lint-lld-contracts because the
// lint binary must not import the dispatcher; contractreg stays a leaf and
// receives both surfaces as data.
func vocabularyViews(t *testing.T) (container, dispatcher contractreg.VocabularyView) {
	t.Helper()
	container = contractreg.VocabularyView{Surface: "container (internal/agenttools)", Schemas: map[string]json.RawMessage{}}
	for _, tool := range agenttools.Tools {
		container.Schemas[tool.Name] = tool.Schema
	}
	dispatcher = contractreg.VocabularyView{Surface: "dispatcher (RegisteredDispatcherTools)", Schemas: map[string]json.RawMessage{}}
	for _, tool := range RegisteredDispatcherTools() {
		raw, err := json.Marshal(tool)
		if err != nil {
			t.Fatalf("marshal %s: %v", tool.Function.Name, err)
		}
		dispatcher.Schemas[tool.Function.Name] = raw
	}
	return container, dispatcher
}

// TestSharedToolNames_SetIsExact pins the overlap. A fifth shared name is a
// deliberate decision — it means a model will meet the same name on both
// surfaces — not something that arrives by naming coincidence.
func TestSharedToolNames_SetIsExact(t *testing.T) {
	container, dispatcher := vocabularyViews(t)
	want := []string{"list_apis", "memory_search", "query_api", "tool_search"}
	if got := contractreg.SharedToolNames(container, dispatcher); !reflect.DeepEqual(got, want) {
		t.Fatalf("shared tool names = %v, want %v — a new overlap must be recorded in the agent-tool declaration design §12", got, want)
	}
}

// TestSharedToolNames_ParameterShapesAgree — every parameter both surfaces
// declare has the same JSON type and the same required-ness. The
// dispatcher's memory_search has project_id/from_date/to_date the container
// lacks; that is allowed, a one-sided parameter is not a contradiction.
func TestSharedToolNames_ParameterShapesAgree(t *testing.T) {
	container, dispatcher := vocabularyViews(t)
	for _, f := range contractreg.CheckSharedToolSchemas(container, dispatcher) {
		t.Errorf("%s: %s", f.Name, f.Detail)
	}
}
