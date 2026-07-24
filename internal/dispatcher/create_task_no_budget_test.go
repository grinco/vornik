package dispatcher

import (
	"strings"
	"testing"
)

// TestCreateTaskTool_HasNoBudgetParam enforces the per-task cost governor's
// authority invariant (LLD 2026-07-24 §3.1): the create_task dispatcher tool
// exposes NO budget parameter — a per-task budget is operator/admin-only and
// must NEVER be LLM-settable. If someone adds a "budget" property to the tool
// schema, this test fails loudly.
func TestCreateTaskTool_HasNoBudgetParam(t *testing.T) {
	for _, tool := range DispatcherTools() {
		if tool.Function.Name != "create_task" {
			continue
		}
		if strings.Contains(strings.ToLower(string(tool.Function.Parameters)), "budget") {
			t.Fatalf("create_task tool must not expose a budget parameter (authority §3.1); schema=%s", tool.Function.Parameters)
		}
		return
	}
	t.Fatal("create_task tool not found in DispatcherTools()")
}
