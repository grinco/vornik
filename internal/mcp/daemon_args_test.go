package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// scraperWebFetchSchema is the real advertised schema from services/scraper as
// it stood on 2026-08-05, trimmed to the fields that matter here.
const scraperWebFetchSchema = `{
  "type":"object",
  "required":["url","project_id","allowed_hosts"],
  "properties":{
    "url":{"type":"string","format":"uri"},
    "project_id":{"type":"string","description":"vornik project ID"},
    "allowed_hosts":{"type":"array","items":{"type":"string"}},
    "text_only":{"type":"boolean"}
  }
}`

// TestHideDaemonSuppliedArgs_RemovesProjectIDFromTheAdvertisedSchema — the
// model was being asked, as a REQUIRED field, for a value it has no way to
// know. On 2026-08-05 a plain "review this PR" request came back as
// `path: ["project_id"] Required` and the assistant read it as its own mistake.
func TestHideDaemonSuppliedArgs_RemovesProjectIDFromTheAdvertisedSchema(t *testing.T) {
	out, ok := hideDaemonSuppliedArgs(json.RawMessage(scraperWebFetchSchema))
	require.True(t, ok, "the schema mentions project_id, so it must be rewritten")

	var doc map[string]any
	require.NoError(t, json.Unmarshal(out, &doc))

	props, _ := doc["properties"].(map[string]any)
	require.NotContains(t, props, "project_id", "the model must not be offered the field at all")
	require.Contains(t, props, "url", "unrelated properties must survive")
	require.Contains(t, props, "allowed_hosts")

	req, _ := doc["required"].([]any)
	require.NotContains(t, req, "project_id")
	require.Contains(t, req, "url", "the genuine requirements must survive")
	require.Contains(t, req, "allowed_hosts")
}

// A schema that never mentions the field is returned byte-identical, so the
// common case does not pay a re-marshal (and cannot reorder a server's keys).
func TestHideDaemonSuppliedArgs_UntouchedSchemaIsReturnedVerbatim(t *testing.T) {
	in := json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)
	out, ok := hideDaemonSuppliedArgs(in)
	require.False(t, ok)
	require.JSONEq(t, string(in), string(out))
}

// Dropping the last required entry must remove the key, not leave `required: []`
// — some validators reject an empty required array.
func TestHideDaemonSuppliedArgs_EmptiedRequiredIsDropped(t *testing.T) {
	out, ok := hideDaemonSuppliedArgs(json.RawMessage(
		`{"type":"object","required":["project_id"],"properties":{"project_id":{"type":"string"}}}`))
	require.True(t, ok)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(out, &doc))
	require.NotContains(t, doc, "required")
}

// A non-object schema must pass through rather than be mangled.
func TestHideDaemonSuppliedArgs_NonObjectSchemaIsLeftAlone(t *testing.T) {
	in := json.RawMessage(`"not-a-schema"`)
	out, ok := hideDaemonSuppliedArgs(in)
	require.False(t, ok)
	require.Equal(t, string(in), string(out))
}

// TestInjectDaemonSuppliedArgs_OverwritesAModelSuppliedProjectID is the
// security half. project_id keys context isolation, concurrency accounting and
// the per-project browser profile, so a model that names ANOTHER project would
// be charged against that project's limits and could reach its profile. The
// daemon knows the real caller; the model's claim is never believed.
func TestInjectDaemonSuppliedArgs_OverwritesAModelSuppliedProjectID(t *testing.T) {
	out := injectDaemonSuppliedArgs(`{"url":"https://x.example","project_id":"some-other-project"}`, "easeit-companion")
	var args map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &args))
	require.Equal(t, "easeit-companion", args["project_id"], "the model must not choose its own identity")
	require.Equal(t, "https://x.example", args["url"], "the caller's own arguments survive")
}

func TestInjectDaemonSuppliedArgs_AddsToEmptyAndAbsentArgs(t *testing.T) {
	for _, in := range []string{``, `{}`} {
		var args map[string]any
		require.NoError(t, json.Unmarshal([]byte(injectDaemonSuppliedArgs(in, "proj")), &args))
		require.Equal(t, "proj", args["project_id"], "input %q", in)
	}
}

// A non-object argument payload is passed through untouched: rewriting a shape
// we do not understand would turn the server's clear validation error into a
// confusing one.
func TestInjectDaemonSuppliedArgs_NonObjectPayloadIsUntouched(t *testing.T) {
	require.Equal(t, `[1,2,3]`, injectDaemonSuppliedArgs(`[1,2,3]`, "proj"))
	require.Equal(t, `not json`, injectDaemonSuppliedArgs(`not json`, "proj"))
}

// An empty projectID means there is no identity to assert, so nothing is added.
func TestInjectDaemonSuppliedArgs_NoProjectMeansNoInjection(t *testing.T) {
	require.Equal(t, `{"url":"x"}`, injectDaemonSuppliedArgs(`{"url":"x"}`, ""))
}

// End to end through the manager: the tab the model sees has no project_id, and
// the call the server receives does.
func TestManager_ProjectIDIsHiddenFromTheModelAndSuppliedToTheServer(t *testing.T) {
	var seen string
	srv := managerCovSSEClient(t, "scraper", func(args string) map[string]any {
		seen = args
		return map[string]any{"content": []map[string]any{{"type": "text", "text": "ok"}}}
	})
	// Advertise the real schema so Tools() has something to strip.
	srv.tools = []Tool{{Name: "web_fetch", InputSchema: json.RawMessage(scraperWebFetchSchema)}}

	mgr := NewManager(zerolog.Nop())
	mgr.clients["easeit-companion"] = map[string]*Client{"scraper": srv}

	tools := mgr.Tools("easeit-companion")
	require.Len(t, tools, 1)
	require.NotContains(t, string(tools[0].Function.Parameters), "project_id",
		"the advertised tool must not ask the model for the daemon's own identity")

	_, err := mgr.Execute(context.Background(), "easeit-companion",
		"mcp__scraper__web_fetch", `{"url":"https://github.com/x"}`)
	require.NoError(t, err)
	require.JSONEq(t, `{"url":"https://github.com/x","project_id":"easeit-companion"}`, seen,
		"the server must receive the identity the daemon knows")
}
