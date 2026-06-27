// Tests for Agent.InventoryTools — the operator-facing reflection
// of which dispatcher tools are currently usable. The admin UI
// renders this directly so a stalled SMTP boot, missing memory
// wiring, etc., are diagnosable at a glance instead of "the bot
// says it can't do X."
package dispatcher

import (
	"context"
	"testing"
)

// TestInventoryTools_NilAgent — defensive: nil receiver must
// return nil, not panic. The handler calls this through an adapter
// that could in theory hand it a nil agent during early boot.
func TestInventoryTools_NilAgent(t *testing.T) {
	var a *Agent
	if got := a.InventoryTools(); got != nil {
		t.Errorf("nil agent: got %+v, want nil", got)
	}
}

// TestInventoryTools_ListsEveryRegisteredTool — every entry in
// DispatcherTools() must appear in the inventory output so the
// admin UI can't "forget" a tool. Pin by count + name set.
func TestInventoryTools_ListsEveryRegisteredTool(t *testing.T) {
	a := &Agent{}
	got := a.InventoryTools()
	if len(got) != len(DispatcherTools()) {
		t.Errorf("inventory size = %d, registered tools = %d",
			len(got), len(DispatcherTools()))
	}
	gotNames := map[string]bool{}
	for _, r := range got {
		gotNames[r.Name] = true
	}
	for _, tl := range DispatcherTools() {
		if !gotNames[tl.Function.Name] {
			t.Errorf("inventory missing tool %q", tl.Function.Name)
		}
	}
}

// TestInventoryTools_AvailabilityReflectsWiring — every tool with
// an explicit backing-service dependency must report Available=false
// when that dependency is nil. The send_email tool is the canonical
// case: without EmailSender, Available must be false even though
// the tool is "registered."
func TestInventoryTools_AvailabilityReflectsWiring(t *testing.T) {
	bareAgent := &Agent{}
	rows := bareAgent.InventoryTools()
	byName := map[string]ToolInfo{}
	for _, r := range rows {
		byName[r.Name] = r
	}

	mustNotAvailable := map[string]string{
		"send_email":     "EmailSender",
		"memory_search":  "MemorySearcher",
		"memory_correct": "MemoryCorrector",
	}
	for tool, dep := range mustNotAvailable {
		row, ok := byName[tool]
		if !ok {
			t.Errorf("tool %q missing from inventory", tool)
			continue
		}
		if row.Available {
			t.Errorf("tool %q reports Available=true with %s nil", tool, dep)
		}
		if row.BackingService != dep {
			t.Errorf("tool %q BackingService = %q, want %q", tool, row.BackingService, dep)
		}
	}
}

// TestInventoryTools_EmailSenderToggles — wiring an EmailSender
// flips send_email to Available=true. Pin the toggle so a future
// refactor that changes how emailSender is stored on Agent can't
// silently break the admin UI's signal.
func TestInventoryTools_EmailSenderToggles(t *testing.T) {
	a := &Agent{}
	a.SetEmailSender(stubEmailSenderForInventory{})

	for _, r := range a.InventoryTools() {
		if r.Name == "send_email" {
			if !r.Available {
				t.Errorf("send_email Available = false after SetEmailSender")
			}
			return
		}
	}
	t.Fatal("send_email not in inventory")
}

// TestInventoryTools_AlwaysAvailableTools — some tools have no
// runtime wiring (always usable as long as the dispatcher itself
// runs). switch_project is the canonical always-on tool: it
// flips the active project on the session, no external service
// to fail. Pin so a future refactor doesn't accidentally gate it.
// (tool_search is injected lazily by the deferred-loading code
// path — not in DispatcherTools() by default — so it's NOT in
// the inventory; expected behavior.)
func TestInventoryTools_AlwaysAvailableTools(t *testing.T) {
	a := &Agent{}
	got := map[string]bool{}
	for _, r := range a.InventoryTools() {
		if r.Name == "switch_project" {
			got[r.Name] = r.Available
		}
	}
	if !got["switch_project"] {
		t.Error("switch_project Available = false, want true on bare Agent")
	}
}

// stubEmailSenderForInventory satisfies the EmailSender interface
// without doing any work — InventoryTools only checks for
// nil/non-nil, not behavior.
type stubEmailSenderForInventory struct{}

func (stubEmailSenderForInventory) SendEmail(_ context.Context, _ string, _ EmailSendRequest) (string, error) {
	return "", nil
}
