package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

// Regression tests against the REAL catalogue advertised by Google's Workspace
// MCP server (`workspace-server/dist/index.js`, v0.0.8, clone 089927e), probed
// on 2026-07-29. Everything here was pinned from live `tools/list` output, not
// from the design doc — the design's illustrative names were underscore-lowercase
// (`drive_search`, `calendar_list_events`) and the server's real names are
// namespace-plus-camelCase (`drive_search`, `calendar_listEvents`), optionally
// dot-separated when launched with `--use-dot-names`.
//
// Two defects in the classifier were found this way, both of which would have
// mattered:
//
//  1. camelCase was not split, so `docs_getSuggestions` and `drive_getComments` —
//     ordinary read tools — classified as MUTATING. Fail-safe, but it makes the
//     operator declare read tools as though they were dangerous, which trains
//     them to ignore the warning.
//
//  2. Fixing (1) would have promoted `drive_downloadFile` to READ-ONLY, because
//     `download` is in the read-verb allowlist. That tool takes an absolute
//     `localPath` and writes to it as the daemon user. See the host-filesystem
//     tests below — this is why the two changes had to land together.
//
// Design: https://docs.vornik.io §10.2

// The read tools the server advertises WITHOUT any annotation. These are exactly
// the ones the name heuristic must get right on its own, so they are the whole
// reason camelCase splitting is required.
func TestWorkspaceCatalogue_UnannotatedReadToolsAreNotMutating(t *testing.T) {
	for _, name := range []string{
		"docs_getSuggestions", // retrieves suggested edits — read
		"drive_getComments",   // retrieves comments — read
		// The same names under `--use-dot-names`, which start.js passes.
		"docs.getSuggestions",
		"drive.getComments",
	} {
		t.Run(name, func(t *testing.T) {
			if (Tool{Name: name}).IsMutating() {
				t.Errorf("%q is a read tool the server does not annotate; the name heuristic "+
					"must recognise it, or the operator is asked to declare reads as mutating", name)
			}
		})
	}
}

// The mutating half of the real catalogue. Pinned because a separator or
// camelCase change to the splitter could silently reclassify any of these, and
// each one is a live write against the operator's Workspace.
func TestWorkspaceCatalogue_RealMutatingToolsAreClassifiedMutating(t *testing.T) {
	for _, name := range []string{
		"auth_clear", "auth_refreshToken",
		"docs_create", "docs_writeText", "docs_replaceText", "docs_formatText",
		"drive_createFolder", "drive_moveFile", "drive_trashFile", "drive_renameFile",
		"calendar_createEvent", "calendar_updateEvent", "calendar_respondToEvent",
		"calendar_deleteEvent",
		"chat_sendMessage", "chat_sendDm", "chat_setUpSpace",
		"gmail_modify", "gmail_batchModify", "gmail_modifyThread",
		"gmail_send", "gmail_createDraft", "gmail_sendDraft", "gmail_createLabel",
		// Dot-separated variants.
		"gmail.send", "calendar.deleteEvent", "drive.trashFile",
	} {
		t.Run(name, func(t *testing.T) {
			if !(Tool{Name: name}).IsMutating() {
				t.Errorf("%q writes to the operator's Workspace and must classify mutating", name)
			}
		})
	}
}

// The read half, by name alone (the server also annotates most of these, but the
// heuristic must stand on its own in case a release drops the annotations).
func TestWorkspaceCatalogue_RealReadToolsAreNotMutating(t *testing.T) {
	for _, name := range []string{
		"drive_findFolder", "drive_search",
		"docs_getText",
		"slides_getText", "slides_getMetadata", "slides_getSpeakerNotes",
		"sheets_getText", "sheets_getRange", "sheets_getMetadata",
		"calendar_list", "calendar_listEvents", "calendar_getEvent",
		"calendar_findFreeTime",
		"chat_listSpaces", "chat_findSpaceByName", "chat_getMessages",
		"chat_findDmByEmail", "chat_listThreads",
		"gmail_search", "gmail_get", "gmail_listLabels",
		"time_getCurrentDate", "time_getCurrentTime", "time_getTimeZone",
		"people_getUserProfile", "people_getMe", "people_getUserRelations",
		"calendar.listEvents", "drive.search",
	} {
		t.Run(name, func(t *testing.T) {
			if (Tool{Name: name}).IsMutating() {
				t.Errorf("%q is a read tool and must not classify mutating", name)
			}
		})
	}
}

// --- host filesystem access ---

// localPathSchema is the real shape of the four download/export tools: a plain
// string property naming an absolute path on OUR host.
func localPathSchema(prop string) json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{` +
		`"fileId":{"type":"string"},` +
		`"` + prop + `":{"type":"string","description":"The absolute local file path."}},` +
		`"required":["fileId","` + prop + `"]}`)
}

// THE FINDING THAT MATTERS. `drive_downloadFile` begins with `download`, which is
// in the read-verb allowlist, and the server supplies NO annotation for it. Once
// camelCase splitting landed, the name alone reads as a safe export — but the tool
// writes caller-chosen bytes to a caller-chosen absolute path, as the OS user
// running the daemon. On the reference host that user owns
// ~/.config/vornik/secrets/admin-key.txt (0600) and the database password in
// config.yaml, so exposing this tool is arbitrary file write as the daemon.
//
// A tool that takes a host filesystem path is therefore mutating regardless of
// its verb: what it mutates is our machine, not the remote service.
func TestWorkspaceCatalogue_HostFilesystemWritersAreMutatingDespiteAReadVerb(t *testing.T) {
	for _, tc := range []struct{ name, prop string }{
		{"drive_downloadFile", "localPath"},       // `download` is a read verb
		{"gmail_downloadAttachment", "localPath"}, // ditto
		{"slides_getImages", "localPath"},         // `get` is a read verb
		{"slides_getSlideThumbnail", "localPath"}, // ditto
	} {
		t.Run(tc.name, func(t *testing.T) {
			tool := Tool{Name: tc.name, InputSchema: localPathSchema(tc.prop)}
			if !tool.IsMutating() {
				t.Errorf("%q takes a host filesystem path (%s) and writes to it as the daemon "+
					"user; it must classify mutating even though its verb reads as a fetch",
					tc.name, tc.prop)
			}
		})
	}
}

// gmail_createDraft's host path is NESTED, inside attachments[].filePath, and it
// is a host READ — the mirror hazard: it can attach ~/.config/vornik/secrets to a
// draft. The walker must therefore descend into array items, not just scan
// top-level properties.
func TestWorkspaceCatalogue_NestedHostPathIsDetected(t *testing.T) {
	createDraft := Tool{
		Name: "gmail_createDraft",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"to":{"anyOf":[{"type":"string"},{"type":"array","items":{"type":"string"}}]},
			"subject":{"type":"string"},
			"attachments":{"type":"array","items":{"type":"object","properties":{
				"filePath":{"type":"string","description":"Absolute local filesystem path."},
				"filename":{"type":"string"}},"required":["filePath"]}}},
			"required":["to","subject"]}`),
	}
	got := HostFilesystemTools([]Tool{createDraft})
	if len(got) != 1 || !strings.Contains(got[0], "filePath") {
		t.Fatalf("a host path nested in attachments[].filePath must be found, got %v", got)
	}
}

// A third party may not declare a host-filesystem write safe. readOnlyHint is a
// claim about the REMOTE service; it carries no authority over our disk, so the
// host-path rule outranks it. None of the five tools annotates itself today —
// this pins the precedence before one does.
func TestHostFilesystemAccess_OutranksReadOnlyHint(t *testing.T) {
	ro := true
	tool := Tool{
		Name:        "drive_downloadFile",
		Annotations: &ToolAnnotations{ReadOnlyHint: &ro},
		InputSchema: localPathSchema("localPath"),
	}
	if !tool.IsMutating() {
		t.Error("a server's readOnlyHint must not be able to declare a host filesystem write safe")
	}
}

// Remote-only reads must not be swept up. `sheets_getRange` takes an A1 `range`
// and `drive_search` a `query`; neither touches our disk, and misclassifying them
// would make the host-path rule useless noise.
func TestHostFilesystemTools_DoesNotFlagRemoteOnlyReads(t *testing.T) {
	remote := []Tool{
		{Name: "sheets_getRange", InputSchema: json.RawMessage(
			`{"type":"object","properties":{"spreadsheetId":{"type":"string"},"range":{"type":"string"}}}`)},
		{Name: "drive_search", InputSchema: json.RawMessage(
			`{"type":"object","properties":{"query":{"type":"string"},"pageSize":{"type":"number"}}}`)},
		{Name: "calendar_listEvents", InputSchema: json.RawMessage(
			`{"type":"object","properties":{"calendarId":{"type":"string"}}}`)},
	}
	if got := HostFilesystemTools(remote); len(got) != 0 {
		t.Errorf("remote-only reads must not be flagged as touching the host filesystem: %v", got)
	}
	for _, tool := range remote {
		if tool.IsMutating() {
			t.Errorf("%q must remain read-only", tool.Name)
		}
	}
}

// Malformed or absent schemas must not panic, and must not be read as "no host
// access" with confidence — an uninspectable schema is one we cannot vet, so the
// tool falls through to the name heuristic rather than being trusted.
func TestHostFilesystemTools_ToleratesUnusableSchemas(t *testing.T) {
	for _, tc := range []struct {
		name   string
		schema json.RawMessage
	}{
		{"nil schema", nil},
		{"empty schema", json.RawMessage(``)},
		{"not an object", json.RawMessage(`"nope"`)},
		{"malformed json", json.RawMessage(`{"properties":`)},
		{"null properties", json.RawMessage(`{"properties":null}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tool := Tool{Name: "drive_search", InputSchema: tc.schema}
			if got := HostFilesystemTools([]Tool{tool}); len(got) != 0 {
				t.Errorf("an unusable schema declares no host path, got %v", got)
			}
			if tool.IsMutating() {
				t.Error("an unusable schema must fall through to the name heuristic")
			}
		})
	}
}

// Deeply nested schemas must not be walked forever. A hostile or generated schema
// can nest arbitrarily; the walker bounds depth and simply stops.
func TestHostFilesystemTools_BoundsRecursionDepth(t *testing.T) {
	deep := `{"type":"object","properties":{"a":` +
		strings.Repeat(`{"type":"object","properties":{"a":`, 200) +
		`{"type":"string"}` + strings.Repeat(`}}`, 200) + `}}`
	// Must return rather than hang or overflow the stack, and must not invent a
	// finding: there is no host path anywhere in that nest.
	if got := HostFilesystemTools([]Tool{{Name: "x_get", InputSchema: json.RawMessage(deep)}}); len(got) != 0 {
		t.Errorf("no host path exists in the nested schema, got %v", got)
	}

	// And a host path buried BELOW the depth bound is deliberately not found —
	// the bound is a real limit, not a claim of exhaustiveness. Pinned so the
	// tradeoff is visible if someone raises or removes maxSchemaDepth.
	buried := `{"type":"object","properties":{"a":` +
		strings.Repeat(`{"type":"object","properties":{"a":`, maxSchemaDepth+2) +
		`{"type":"object","properties":{"localPath":{"type":"string"}}}` +
		strings.Repeat(`}}`, maxSchemaDepth+2) + `}}`
	if got := HostFilesystemTools([]Tool{{Name: "x_get", InputSchema: json.RawMessage(buried)}}); len(got) != 0 {
		t.Errorf("a path below the depth bound is out of scope by design, got %v", got)
	}
	// Within the bound it IS found, which is what makes the bound the only gap.
	shallow := `{"type":"object","properties":{"a":{"type":"object","properties":{` +
		`"localPath":{"type":"string"}}}}}`
	if got := HostFilesystemTools([]Tool{{Name: "x_get", InputSchema: json.RawMessage(shallow)}}); len(got) != 1 {
		t.Errorf("a nested path within the bound must be found, got %v", got)
	}
}

// --- what the operator is told ---

// The five host-touching tools are the ones an operator most needs to see named
// at startup, because nothing in their names says "reaches your disk".
func TestHostFilesystemTools_ReportsEveryOffenderSorted(t *testing.T) {
	got := HostFilesystemTools([]Tool{
		{Name: "slides_getImages", InputSchema: localPathSchema("localPath")},
		{Name: "drive_search", InputSchema: json.RawMessage(`{"properties":{"query":{"type":"string"}}}`)},
		{Name: "drive_downloadFile", InputSchema: localPathSchema("localPath")},
	})
	if len(got) != 2 {
		t.Fatalf("expected both host-touching tools, got %v", got)
	}
	if !strings.HasPrefix(got[0], "drive_downloadFile") {
		t.Errorf("output must be sorted for stable diagnostics, got %v", got)
	}
	for _, want := range []string{"drive_downloadFile", "slides_getImages", "localPath"} {
		if !strings.Contains(strings.Join(got, " | "), want) {
			t.Errorf("missing %q from %v", want, got)
		}
	}
}

// The whole catalogue, exposed with no allowlist, must refuse under
// require_declared_tools — this server advertises gmail_send, calendar_deleteEvent
// and drive_trashFile, so expose-all is never the right configuration for it.
func TestWorkspaceCatalogue_ExposeAllIsRefusedForThisServer(t *testing.T) {
	err := gateAdvertisedTools("google-workspace", true, nil, []Tool{
		{Name: "calendar_listEvents"},
		{Name: "drive_search"},
		{Name: "gmail_send"},
		{Name: "drive_trashFile"},
	})
	if err == nil {
		t.Fatal("the Workspace catalogue contains destructive tools; expose-all must refuse")
	}
	for _, want := range []string{"gmail_send", "drive_trashFile", "allowed_tools"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should name %q, got %v", want, err)
		}
	}
}

// The Phase 0 allowlist that ships in vornik.yaml.example must actually be
// registrable, and must contain nothing mutating. This is the config the runbook
// tells the operator to paste, so a defect here is a defect in the runbook.
func TestWorkspaceCatalogue_Phase0AllowlistIsReadOnlyAndRegistrable(t *testing.T) {
	phase0 := []string{
		"calendar_list", "calendar_listEvents", "calendar_getEvent", "calendar_findFreeTime",
		"drive_search", "drive_findFolder", "drive_getComments",
		"docs_getText", "docs_getSuggestions",
		"sheets_getText", "sheets_getRange", "sheets_getMetadata",
		"slides_getText", "slides_getMetadata", "slides_getSpeakerNotes",
		"time_getCurrentDate", "time_getCurrentTime", "time_getTimeZone",
		"people_getMe",
	}
	tools := make([]Tool, 0, len(phase0))
	for _, n := range phase0 {
		tools = append(tools, Tool{Name: n})
		if (Tool{Name: n}).IsMutating() {
			t.Errorf("%q is in the Phase 0 read-only allowlist but classifies mutating", n)
		}
	}
	if err := gateAdvertisedTools("google-workspace", true, allowedToolSet(phase0), tools); err != nil {
		t.Fatalf("the shipped Phase 0 allowlist must register cleanly: %v", err)
	}
	// And none of the five host-filesystem tools may be in it.
	for _, banned := range []string{
		"drive_downloadFile", "gmail_downloadAttachment",
		"slides_getImages", "slides_getSlideThumbnail", "gmail_createDraft",
	} {
		for _, n := range phase0 {
			if n == banned {
				t.Errorf("%q reaches the host filesystem and must not be in the Phase 0 allowlist", banned)
			}
		}
	}
}

// The distinction the WARN turns on: a host-path tool that is ADVERTISED is worth
// a note, one that is inside allowed_tools is reachable by an agent and worth
// alarm. allowedSubset is what separates them.
func TestAllowedSubset_SeparatesAdvertisedFromReachable(t *testing.T) {
	tools := []Tool{
		{Name: "drive_search"},
		{Name: "drive_downloadFile", InputSchema: localPathSchema("localPath")},
	}
	// Expose-all: everything is reachable, including the host writer.
	if got := HostFilesystemTools(allowedSubset(nil, tools)); len(got) != 1 {
		t.Errorf("with no allowlist the host writer is reachable, got %v", got)
	}
	// A read-only allowlist: advertised but not reachable.
	readOnly := allowedToolSet([]string{"drive_search"})
	if got := HostFilesystemTools(allowedSubset(readOnly, tools)); len(got) != 0 {
		t.Errorf("an allowlist excluding it means no agent can reach it, got %v", got)
	}
}
