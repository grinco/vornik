package mcp

import (
	"strings"
	"testing"
)

// Guard for the Workspace design §10.2: a third-party MCP server's tool set is
// NOT ours, and a release can add a destructive tool at any time. A static
// `allowed_tools` list in our YAML cannot defend against that on its own — the
// list is ours, the tool set is theirs — so classification happens at connect
// time against what the server actually advertises.
//
// Design: https://docs.vornik.io §10.2

// The classifier is a READ-VERB ALLOWLIST, not a mutating-verb denylist. An
// unrecognised verb is treated as MUTATING, because a denylist fails open on
// exactly the tool nobody anticipated — which is the one that matters.
func TestToolIsMutating_UnknownVerbsAreTreatedAsMutating(t *testing.T) {
	for _, name := range []string{
		"gmail_delete", "drive_trash", "calendar_move_event",
		"sheets_update_range", "docs_insert_text", "gmail_send",
		"frobnicate_widget", // deliberately meaningless: unknown ⇒ mutating
		"",                  // degenerate ⇒ mutating
	} {
		t.Run(name, func(t *testing.T) {
			if !(Tool{Name: name}).IsMutating() {
				t.Errorf("%q must be classified mutating — an unrecognised verb is exactly "+
					"the case a denylist would miss", name)
			}
		})
	}
}

func TestToolIsMutating_KnownReadVerbsAreNotMutating(t *testing.T) {
	for _, name := range []string{
		"drive_search", "drive_read_file", "calendar_list_events",
		"calendar_get_schedule", "gmail_list_messages", "sheets_get_values",
		"docs_export_markdown", "people_find_contact", "drive_describe_file",
		"docs_view", "sheets_count_rows", "gmail_query_threads",
	} {
		t.Run(name, func(t *testing.T) {
			if (Tool{Name: name}).IsMutating() {
				t.Errorf("%q is a read tool and should not be classified mutating", name)
			}
		})
	}
}

// Server-supplied annotations are AUTHORITATIVE where present — the server knows
// its own semantics better than our verb table does, in both directions.
func TestToolIsMutating_AnnotationsWinOverTheVerbHeuristic(t *testing.T) {
	// A read-looking name the server declares destructive.
	readOnly := false
	destructive := true
	tricky := Tool{
		Name:        "drive_get_and_purge",
		Annotations: &ToolAnnotations{ReadOnlyHint: &readOnly, DestructiveHint: &destructive},
	}
	if !tricky.IsMutating() {
		t.Error("a server declaring destructiveHint must be believed over the name")
	}

	// A mutating-looking name the server declares read-only.
	ro := true
	safe := Tool{
		Name:        "calendar_update_view_preference",
		Annotations: &ToolAnnotations{ReadOnlyHint: &ro},
	}
	if safe.IsMutating() {
		t.Error("a server declaring readOnlyHint must be believed over the name")
	}
}

// --- the registration gate ---

// The dangerous configuration: no allowlist means "expose everything", and
// everything now includes a destructive tool. Refuse, because the operator's
// config said expose-all under different circumstances.
func TestGateAdvertisedTools_NoAllowlistPlusMutatingToolIsRefused(t *testing.T) {
	err := gateAdvertisedTools("google-workspace", true, nil, []Tool{
		{Name: "drive_search"},
		{Name: "gmail_delete"},
	})
	if err == nil {
		t.Fatal("an empty allowed_tools with a mutating tool advertised must refuse registration")
	}
	if !strings.Contains(err.Error(), "gmail_delete") {
		t.Errorf("the refusal must name the offending tool, got %v", err)
	}
	if !strings.Contains(err.Error(), "allowed_tools") {
		t.Errorf("the refusal must tell the operator how to resolve it, got %v", err)
	}
}

// With no allowlist and nothing mutating, expose-all is fine — this must not
// become a blanket requirement to declare tools.
func TestGateAdvertisedTools_NoAllowlistAllReadOnlyIsFine(t *testing.T) {
	if err := gateAdvertisedTools("cal", true, nil, []Tool{
		{Name: "calendar_list_events"}, {Name: "calendar_get_schedule"},
	}); err != nil {
		t.Fatalf("read-only tools with no allowlist must be permitted: %v", err)
	}
}

// An explicit allowlist already filters, so a newly-advertised mutating tool
// does NOT reach an agent. Registration proceeds — taking the whole server down
// because the upstream gained a tool would be a self-inflicted outage on a
// schedule Google controls — but the operator is told (asserted in the caller
// test below via the returned notice).
func TestGateAdvertisedTools_AllowlistFiltersAndReportsUndeclaredMutatingTools(t *testing.T) {
	err := gateAdvertisedTools("google-workspace", true,
		map[string]struct{}{"drive_search": {}, "calendar_get_schedule": {}},
		[]Tool{
			{Name: "drive_search"},
			{Name: "calendar_get_schedule"},
			{Name: "gmail_delete"}, // newly advertised upstream, NOT declared
		})
	if err != nil {
		t.Fatalf("an allowlist filters the undeclared tool, so registration must proceed: %v", err)
	}
}

// A mutating tool the operator DECLARED is permitted with no allowlist debate —
// drafting is a wanted capability in the personal tier.
func TestGateAdvertisedTools_DeclaredMutatingToolIsPermitted(t *testing.T) {
	if err := gateAdvertisedTools("gmail", true,
		map[string]struct{}{"gmail_create_draft": {}},
		[]Tool{{Name: "gmail_create_draft"}}); err != nil {
		t.Fatalf("an explicitly declared mutating tool must be permitted: %v", err)
	}
}

// UndeclaredMutating is what the caller logs and the doctor surfaces; it must
// report every offender, not just the first, or a second one hides behind the
// first fix.
func TestUndeclaredMutatingTools_ReportsAllOffenders(t *testing.T) {
	got := undeclaredMutatingTools(
		map[string]struct{}{"drive_search": {}},
		[]Tool{
			{Name: "drive_search"},
			{Name: "gmail_delete"},
			{Name: "drive_trash"},
		})
	if len(got) != 2 {
		t.Fatalf("expected both offenders, got %v", got)
	}
	joined := strings.Join(got, ",")
	for _, want := range []string{"gmail_delete", "drive_trash"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q from %v", want, got)
		}
	}
}

// THE REGRESSION THAT MATTERS. The gate must be opt-in: the reference deployment
// runs four expose-all servers with legitimately mutating tool sets (a page
// publisher, a home-automation bridge, a scraper with submit actions). A global
// rule would have refused to register integrations that work today — a
// self-inflicted outage dressed as a security control, caught only because a
// pre-existing test panicked.
func TestGateAdvertisedTools_OptInOnly_ExposeAllServersKeepWorking(t *testing.T) {
	exposeAllWithMutatingTools := []Tool{
		{Name: "pagedrop_publish_doc"},
		{Name: "pagedrop_delete"},
		{Name: "homeassistant_turn_on"},
		{Name: "web_submit"},
	}
	if err := gateAdvertisedTools("pagedrop", false, nil, exposeAllWithMutatingTools); err != nil {
		t.Fatalf("a server that has NOT opted in must register unchanged, got %v", err)
	}
	// And the same server, opted in, refuses — so the flag is what decides.
	if err := gateAdvertisedTools("pagedrop", true, nil, exposeAllWithMutatingTools); err == nil {
		t.Fatal("with require_declared_tools set, the same server must refuse")
	}
}

// A server that sets BOTH hints has contradicted itself. The safe reading of a
// contradiction is the destructive one, so DestructiveHint outranks ReadOnlyHint.
// Pinned because the priority is invisible in the type and a refactor could
// silently invert it.
func TestToolIsMutating_DestructiveHintOutranksReadOnlyHint(t *testing.T) {
	yes, no := true, false
	contradictory := Tool{
		Name:        "drive_get_thing",
		Annotations: &ToolAnnotations{ReadOnlyHint: &yes, DestructiveHint: &yes},
	}
	if !contradictory.IsMutating() {
		t.Error("destructiveHint must win when a server sets both — a contradiction reads destructive")
	}
	// Sanity: readOnlyHint alone still wins over a mutating-looking name.
	readOnly := Tool{Name: "drive_delete_thing", Annotations: &ToolAnnotations{ReadOnlyHint: &yes}}
	if readOnly.IsMutating() {
		t.Error("readOnlyHint alone must win over the name")
	}
	// And destructiveHint:false does not make a mutating name safe — absence of
	// destructiveness is not an assertion of read-only.
	notDestructive := Tool{Name: "drive_delete_thing", Annotations: &ToolAnnotations{DestructiveHint: &no}}
	if !notDestructive.IsMutating() {
		t.Error("destructiveHint:false must not promote a mutating-looking name to read-only")
	}
}

// The override trail is what makes a third-party reclassification visible. Without
// it, a server could quietly declare a destructive-looking tool read-only and
// nothing would record the claim.
func TestAnnotationOverrides_RecordsBothDirections(t *testing.T) {
	yes := true
	got := AnnotationOverrides([]Tool{
		{Name: "drive_search"}, // no annotation, no override
		{Name: "drive_delete_thing", Annotations: &ToolAnnotations{ReadOnlyHint: &yes}},
		{Name: "drive_get_thing", Annotations: &ToolAnnotations{DestructiveHint: &yes}},
	})
	if len(got) != 2 {
		t.Fatalf("both overrides should be recorded, got %v", got)
	}
	joined := strings.Join(got, " | ")
	if !strings.Contains(joined, "drive_delete_thing") || !strings.Contains(joined, "read-only") {
		t.Errorf("the read-only override should be recorded with its direction: %v", got)
	}
	if !strings.Contains(joined, "drive_get_thing") || !strings.Contains(joined, "destructive") {
		t.Errorf("the destructive override should be recorded with its direction: %v", got)
	}
}

// A tool whose annotation AGREES with the name is not an override — otherwise the
// log fills with noise and the real reclassifications hide in it.
func TestAnnotationOverrides_AgreementIsNotAnOverride(t *testing.T) {
	yes := true
	ro := true
	got := AnnotationOverrides([]Tool{
		{Name: "drive_delete_thing", Annotations: &ToolAnnotations{DestructiveHint: &yes}},
		{Name: "drive_search", Annotations: &ToolAnnotations{ReadOnlyHint: &ro}},
	})
	if len(got) != 0 {
		t.Errorf("annotations agreeing with the name are not overrides, got %v", got)
	}
}
