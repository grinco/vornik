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
	if len(got) != len(RegisteredDispatcherTools()) {
		t.Errorf("inventory size = %d, registered tools = %d",
			len(got), len(RegisteredDispatcherTools()))
	}
	gotNames := map[string]bool{}
	for _, r := range got {
		gotNames[r.Name] = true
	}
	for _, tl := range RegisteredDispatcherTools() {
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
		"send_email":              "EmailSender",
		"memory_search":           "MemorySearcher",
		"memory_correct":          "MemoryCorrector",
		"set_reminder":            "ReminderRepository",
		"cancel_reminder":         "ReminderRepository",
		"update_reminder":         "ReminderRepository",
		"update_operator_profile": "OperatorProfileRepository",
		"compose_automation":      "ComposerBridge",
		"list_apis":               "APIClient",
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

// TestInventoryTools_ReminderAndProfileReposToggle pins the admin
// inventory rows for dispatcher write tools whose descriptors are
// always registered but whose calls require repository wiring.
func TestInventoryTools_ReminderAndProfileReposToggle(t *testing.T) {
	a := NewAgent(nil, nil, nil, nil, nil,
		WithReminderRepository(&stubReminderRepo{}),
		WithOperatorProfileRepository(&stubOpProfileRepo{}),
	)

	rows := a.InventoryTools()
	byName := map[string]ToolInfo{}
	for _, r := range rows {
		byName[r.Name] = r
	}
	for _, tool := range []string{"set_reminder", "cancel_reminder", "update_reminder"} {
		row, ok := byName[tool]
		if !ok {
			t.Fatalf("tool %q missing from inventory", tool)
		}
		if row.BackingService != "ReminderRepository" {
			t.Errorf("%s BackingService = %q, want ReminderRepository", tool, row.BackingService)
		}
		if !row.Available {
			t.Errorf("%s Available = false after WithReminderRepository", tool)
		}
	}
	row, ok := byName["update_operator_profile"]
	if !ok {
		t.Fatal("update_operator_profile missing from inventory")
	}
	if row.BackingService != "OperatorProfileRepository" {
		t.Errorf("update_operator_profile BackingService = %q, want OperatorProfileRepository", row.BackingService)
	}
	if !row.Available {
		t.Error("update_operator_profile Available = false after WithOperatorProfileRepository")
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

// TestInventoryTools_ComposerBridgeRequiresBothBridgeAndEnabled pins
// the task-1.4 double gate: wiring a bridge alone (composerEnabled
// still false, the soak default) must NOT flip compose_automation
// Available — both the bridge AND composer.enabled are required.
// Setting enabled=true after that flips it.
func TestInventoryTools_ComposerBridgeRequiresBothBridgeAndEnabled(t *testing.T) {
	a := &Agent{}
	a.SetComposerBridge(stubComposerBridgeForInventory{}, false)

	rows := a.InventoryTools()
	for _, r := range rows {
		if r.Name == "compose_automation" {
			if r.Available {
				t.Error("compose_automation Available = true with composer.enabled=false (bridge wired, soak default)")
			}
			break
		}
	}

	a.SetComposerBridge(stubComposerBridgeForInventory{}, true)
	rows = a.InventoryTools()
	for _, r := range rows {
		if r.Name == "compose_automation" {
			if !r.Available {
				t.Error("compose_automation Available = false after SetComposerBridge(bridge, true)")
			}
			return
		}
	}
	t.Fatal("compose_automation not in inventory")
}

// TestInventoryTools_ListAPIsRequiresProviderLister pins the
// stricter list_apis gate (design §5.5): unlike query_api, which is
// available as soon as any apiClient is wired, list_apis must ALSO
// have that client satisfy the optional ProviderLister capability —
// a Call-only fake must NOT flip it Available, but a fake that also
// implements ListProviders must.
func TestInventoryTools_ListAPIsRequiresProviderLister(t *testing.T) {
	a := NewAgent(nil, nil, nil, nil, nil, WithAPIClient(&fakeAPIClient{}))
	rows := a.InventoryTools()
	byName := map[string]ToolInfo{}
	for _, r := range rows {
		byName[r.Name] = r
	}

	queryRow, ok := byName["query_api"]
	if !ok || !queryRow.Available {
		t.Fatalf("query_api should be Available with a Call-only client, got %+v (ok=%v)", queryRow, ok)
	}
	listRow, ok := byName["list_apis"]
	if !ok {
		t.Fatal("list_apis missing from inventory")
	}
	if listRow.Available {
		t.Error("list_apis Available = true with a Call-only client that does NOT implement ProviderLister")
	}

	b := NewAgent(nil, nil, nil, nil, nil, WithAPIClient(&fakeListerClient{}))
	var listRow2 ToolInfo
	var found bool
	for _, r := range b.InventoryTools() {
		if r.Name == "list_apis" {
			listRow2, found = r, true
		}
	}
	if !found {
		t.Fatal("list_apis missing from inventory (lister client)")
	}
	if !listRow2.Available {
		t.Error("list_apis Available = false with a client that DOES implement ProviderLister")
	}
}

// stubComposerBridgeForInventory satisfies dispatcher.ComposerBridge
// without doing any work — InventoryTools only checks for
// nil/non-nil + the composerEnabled flag, not behaviour.
type stubComposerBridgeForInventory struct{}

func (stubComposerBridgeForInventory) ComposeTurn(context.Context, string, string, string) (string, string, bool, error) {
	return "", "", true, nil
}

// stubEmailSenderForInventory satisfies the EmailSender interface
// without doing any work — InventoryTools only checks for
// nil/non-nil, not behavior.
type stubEmailSenderForInventory struct{}

func (stubEmailSenderForInventory) SendEmail(_ context.Context, _ string, _ EmailSendRequest) (string, error) {
	return "", nil
}

// TestInventoryTools_BackingIsDeclaredForExactlyTheRegisteredTools — the
// inventory is a view over DispatcherTools() plus one backing declaration per
// tool, and the two sets must be identical in both directions. Before
// 2026-09-05 a tool added to DispatcherTools() without a backing entry
// silently rendered as Available=false ("a third accounting of the same
// names", backlog 2026-09-03); now it is this failure, by name.
func TestInventoryTools_BackingIsDeclaredForExactlyTheRegisteredTools(t *testing.T) {
	a := &Agent{}
	backing := a.inventoryBacking()
	registered := map[string]bool{}
	for _, tl := range RegisteredDispatcherTools() {
		registered[tl.Function.Name] = true
		if _, ok := backing[tl.Function.Name]; !ok {
			t.Errorf("tool %q is registered in RegisteredDispatcherTools() but has no backing declaration in inventoryBacking()", tl.Function.Name)
		}
	}
	for name := range backing {
		if !registered[name] {
			t.Errorf("inventoryBacking() declares %q, which RegisteredDispatcherTools() does not register — a stale entry", name)
		}
	}
}
