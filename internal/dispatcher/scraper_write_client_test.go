package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// fakeScraperMCPExec is the narrow MCP execute seam under test: it records the
// (project, tool, args) it was called with and returns canned (result, err) so
// no real MCP/scraper is needed.
type fakeScraperMCPExec struct {
	gotProject string
	gotTool    string
	gotArgs    string
	calls      int
	ret        string
	retErr     error
}

func (f *fakeScraperMCPExec) Execute(_ context.Context, projectID, tool, args string) (string, error) {
	f.calls++
	f.gotProject = projectID
	f.gotTool = tool
	f.gotArgs = args
	return f.ret, f.retErr
}

// testSubmitSecret is the daemon↔scraper web_submit capability secret (shared
// C1 contract) the client under test attaches as daemon_auth on every call.
const testSubmitSecret = "sekret-cap-xyz"

// ---------- Preview ----------

func TestMCPScraperWriteClient_PreviewMarshalsAndParses(t *testing.T) {
	fake := &fakeScraperMCPExec{
		ret: `{
			"submission_id": "scraper-uuid",
			"screenshot": "data:image/png;base64,AAAA",
			"field_table": [
				{"name":"full_name","label":"Full name","value":"Ada Lovelace","provenance":"agent-bound","bound":true},
				{"name":"csrf","label":"","value":"tok-123","provenance":"volatile","bound":false},
				{"name":"source","label":"","value":"web","provenance":"hidden","bound":false}
			],
			"volatile_fields": ["csrf"],
			"block_reason": "none"
		}`,
	}
	c := NewMCPScraperWriteClient(fake, testSubmitSecret)

	res, err := c.Preview(context.Background(), PreviewReq{
		URL:       "https://example.com/form",
		Fields:    []WebField{{Selector: "#name", Value: "Ada Lovelace"}},
		ProjectID: "snake",
		Profile:   "default",
	})
	if err != nil {
		t.Fatalf("Preview returned error: %v", err)
	}

	// Routing: correct project + qualified tool name.
	if fake.gotProject != "snake" {
		t.Errorf("project = %q, want snake", fake.gotProject)
	}
	if fake.gotTool != scraperWebSubmitTool {
		t.Errorf("tool = %q, want %q", fake.gotTool, scraperWebSubmitTool)
	}

	// Args: mode=preview + url + fields + project_id.
	var args map[string]any
	if err := json.Unmarshal([]byte(fake.gotArgs), &args); err != nil {
		t.Fatalf("args not valid JSON: %v\n%s", err, fake.gotArgs)
	}
	if args["mode"] != "preview" {
		t.Errorf("args.mode = %v, want preview", args["mode"])
	}
	if args["url"] != "https://example.com/form" {
		t.Errorf("args.url = %v", args["url"])
	}
	if args["project_id"] != "snake" {
		t.Errorf("args.project_id = %v, want snake", args["project_id"])
	}
	if args["profile"] != "default" {
		t.Errorf("args.profile = %v, want default", args["profile"])
	}
	// C1: the daemon-only capability secret rides on the preview call.
	if args["daemon_auth"] != testSubmitSecret {
		t.Errorf("args.daemon_auth = %v, want %q", args["daemon_auth"], testSubmitSecret)
	}
	fields, ok := args["fields"].([]any)
	if !ok || len(fields) != 1 {
		t.Fatalf("args.fields = %v, want 1 binding", args["fields"])
	}
	f0 := fields[0].(map[string]any)
	if f0["selector"] != "#name" || f0["value"] != "Ada Lovelace" {
		t.Errorf("args.fields[0] = %v", f0)
	}

	// Parsed result: volatile, screenshot, field_table verbatim, no block.
	if len(res.Volatile) != 1 || res.Volatile[0] != "csrf" {
		t.Errorf("Volatile = %v, want [csrf]", res.Volatile)
	}
	if res.ScreenshotRef != "data:image/png;base64,AAAA" {
		t.Errorf("ScreenshotRef = %q", res.ScreenshotRef)
	}
	if res.BlockReason != "" {
		t.Errorf("BlockReason = %q, want empty (none normalised)", res.BlockReason)
	}
	if !strings.Contains(string(res.FieldTable), "full_name") || !strings.Contains(string(res.FieldTable), "csrf") {
		t.Errorf("FieldTable not passed through: %s", res.FieldTable)
	}

	// Derived payload: non-volatile name→value only (csrf excluded).
	var payload map[string]string
	if err := json.Unmarshal(res.Payload, &payload); err != nil {
		t.Fatalf("Payload not valid JSON: %v (%s)", err, res.Payload)
	}
	if payload["full_name"] != "Ada Lovelace" || payload["source"] != "web" {
		t.Errorf("Payload = %v, want non-volatile fields", payload)
	}
	if _, present := payload["csrf"]; present {
		t.Errorf("Payload must exclude volatile csrf, got %v", payload)
	}

	// Derived selector bindings = the caller's fields.
	var sel []WebField
	if err := json.Unmarshal(res.SelectorBindings, &sel); err != nil {
		t.Fatalf("SelectorBindings not valid JSON: %v (%s)", err, res.SelectorBindings)
	}
	if len(sel) != 1 || sel[0].Selector != "#name" || sel[0].Value != "Ada Lovelace" {
		t.Errorf("SelectorBindings = %v, want the input bindings", sel)
	}
}

func TestMCPScraperWriteClient_PreviewBlockReason(t *testing.T) {
	fake := &fakeScraperMCPExec{
		ret: `{"screenshot":"data:image/png;base64,ZZ","field_table":[],"volatile_fields":[],"block_reason":"captcha","block_detail":"hcaptcha challenge"}`,
	}
	c := NewMCPScraperWriteClient(fake, testSubmitSecret)
	res, err := c.Preview(context.Background(), PreviewReq{
		URL:       "https://example.com/form",
		Fields:    []WebField{{Selector: "#n", Value: "x"}},
		ProjectID: "snake",
	})
	if err != nil {
		t.Fatalf("Preview error: %v", err)
	}
	if res.BlockReason != "captcha" {
		t.Errorf("BlockReason = %q, want captcha", res.BlockReason)
	}
}

func TestMCPScraperWriteClient_PreviewUnboundFoldsIntoBlock(t *testing.T) {
	// An unbindable field must fail closed. With no gate, unbound_fields is
	// surfaced via BlockReason so the daemon refuses.
	fake := &fakeScraperMCPExec{
		ret: `{"field_table":[{"name":"a","value":"1","provenance":"page-default","bound":false}],"volatile_fields":[],"block_reason":"none","unbound_fields":["selector=#missing"]}`,
	}
	c := NewMCPScraperWriteClient(fake, testSubmitSecret)
	res, err := c.Preview(context.Background(), PreviewReq{
		URL:       "https://example.com/form",
		Fields:    []WebField{{Selector: "#missing", Value: "x"}},
		ProjectID: "snake",
	})
	if err != nil {
		t.Fatalf("Preview error: %v", err)
	}
	if !strings.Contains(res.BlockReason, "unbindable") || !strings.Contains(res.BlockReason, "#missing") {
		t.Errorf("BlockReason = %q, want it to name the unbindable binding", res.BlockReason)
	}
}

func TestMCPScraperWriteClient_PreviewProjectFromContext(t *testing.T) {
	fake := &fakeScraperMCPExec{ret: `{"field_table":[],"volatile_fields":[]}`}
	c := NewMCPScraperWriteClient(fake, testSubmitSecret)
	ctx := WithScraperWriteProject(context.Background(), "ctxproj")
	if _, err := c.Preview(ctx, PreviewReq{
		URL:    "https://example.com/form",
		Fields: []WebField{{Selector: "#n", Value: "x"}},
		// ProjectID intentionally empty → falls back to context.
	}); err != nil {
		t.Fatalf("Preview error: %v", err)
	}
	if fake.gotProject != "ctxproj" {
		t.Errorf("project = %q, want ctxproj (context fallback)", fake.gotProject)
	}
}

// ---------- Submit ----------

func TestMCPScraperWriteClient_SubmitMarshalsAndParses(t *testing.T) {
	fake := &fakeScraperMCPExec{
		ret: `{"status":"submitted","confirmation_text":"Thank you, reference 42","confirmation_screenshot":"data:image/png;base64,QQ","sent_body":{"full_name":"Ada Lovelace","csrf":"tok-999"},"divergence_reason":""}`,
	}
	c := NewMCPScraperWriteClient(fake, testSubmitSecret)
	ctx := WithScraperWriteProject(context.Background(), "snake")

	res, err := c.Submit(ctx, SubmitReq{
		SubmissionID:     "ww_abc",
		URL:              "https://example.com/form",
		Payload:          []byte(`{"full_name":"Ada Lovelace"}`),
		SelectorBindings: []byte(`[{"selector":"#name","value":"Ada Lovelace"}]`),
		Volatile:         []string{"csrf"},
		WritesMode:       "on",
		WriteAllowlist:   []string{"example.com"},
	})
	if err != nil {
		t.Fatalf("Submit error: %v", err)
	}

	if fake.gotProject != "snake" {
		t.Errorf("project = %q, want snake", fake.gotProject)
	}
	if fake.gotTool != scraperWebSubmitTool {
		t.Errorf("tool = %q", fake.gotTool)
	}

	var args map[string]any
	if err := json.Unmarshal([]byte(fake.gotArgs), &args); err != nil {
		t.Fatalf("args not valid JSON: %v\n%s", err, fake.gotArgs)
	}
	if args["mode"] != "submit" {
		t.Errorf("args.mode = %v, want submit", args["mode"])
	}
	if args["submission_id"] != "ww_abc" {
		t.Errorf("args.submission_id = %v", args["submission_id"])
	}
	if args["url"] != "https://example.com/form" {
		t.Errorf("args.url = %v", args["url"])
	}
	if args["writes_mode"] != "on" {
		t.Errorf("args.writes_mode = %v, want on", args["writes_mode"])
	}
	if args["project_id"] != "snake" {
		t.Errorf("args.project_id = %v", args["project_id"])
	}
	// C1: the daemon-only capability secret rides on the submit call too.
	if args["daemon_auth"] != testSubmitSecret {
		t.Errorf("args.daemon_auth = %v, want %q", args["daemon_auth"], testSubmitSecret)
	}
	if al, ok := args["write_allowlist"].([]any); !ok || len(al) != 1 || al[0] != "example.com" {
		t.Errorf("args.write_allowlist = %v, want [example.com]", args["write_allowlist"])
	}
	if vol, ok := args["volatile"].([]any); !ok || len(vol) != 1 || vol[0] != "csrf" {
		t.Errorf("args.volatile = %v, want [csrf]", args["volatile"])
	}
	// payload forwarded as an object.
	payload, ok := args["payload"].(map[string]any)
	if !ok || payload["full_name"] != "Ada Lovelace" {
		t.Errorf("args.payload = %v", args["payload"])
	}
	// selector_bindings forwarded as an array.
	sel, ok := args["selector_bindings"].([]any)
	if !ok || len(sel) != 1 {
		t.Fatalf("args.selector_bindings = %v", args["selector_bindings"])
	}
	if s0 := sel[0].(map[string]any); s0["selector"] != "#name" || s0["value"] != "Ada Lovelace" {
		t.Errorf("args.selector_bindings[0] = %v", sel[0])
	}

	// Parsed result.
	if res.Status != "submitted" {
		t.Errorf("Status = %q, want submitted", res.Status)
	}
	if res.Confirmation != "Thank you, reference 42" {
		t.Errorf("Confirmation = %q", res.Confirmation)
	}
	if res.DivergenceReason != "" {
		t.Errorf("DivergenceReason = %q, want empty", res.DivergenceReason)
	}
	var sent map[string]string
	if err := json.Unmarshal(res.SentBody, &sent); err != nil {
		t.Fatalf("SentBody not valid JSON: %v (%s)", err, res.SentBody)
	}
	if sent["full_name"] != "Ada Lovelace" {
		t.Errorf("SentBody = %v", sent)
	}
}

func TestMCPScraperWriteClient_SubmitDivergence(t *testing.T) {
	fake := &fakeScraperMCPExec{
		ret: `{"status":"failed","divergence_reason":"field 'amount' diverged from the approved value"}`,
	}
	c := NewMCPScraperWriteClient(fake, testSubmitSecret)
	ctx := WithScraperWriteProject(context.Background(), "snake")
	res, err := c.Submit(ctx, SubmitReq{SubmissionID: "ww_x", URL: "https://example.com", WritesMode: "on"})
	if err != nil {
		t.Fatalf("Submit error: %v", err)
	}
	if res.Status != "failed" {
		t.Errorf("Status = %q, want failed", res.Status)
	}
	if !strings.Contains(res.DivergenceReason, "diverged") {
		t.Errorf("DivergenceReason = %q", res.DivergenceReason)
	}
}

func TestMCPScraperWriteClient_SubmitNoProjectErrors(t *testing.T) {
	fake := &fakeScraperMCPExec{ret: `{"status":"submitted"}`}
	c := NewMCPScraperWriteClient(fake, testSubmitSecret)
	// No WithScraperWriteProject → must fail closed without calling the tool.
	_, err := c.Submit(context.Background(), SubmitReq{SubmissionID: "ww_x", URL: "https://example.com"})
	if err == nil {
		t.Fatal("Submit without a project in context should error")
	}
	if fake.calls != 0 {
		t.Errorf("tool must not be called when project is missing (calls=%d)", fake.calls)
	}
}

func TestMCPScraperWriteClient_SubmitOmitsNullPayload(t *testing.T) {
	// The daemon's nonNilJSON stores "null" for an empty JSONB column; the
	// client must omit it (not send a literal null the scraper's zod rejects).
	fake := &fakeScraperMCPExec{ret: `{"status":"submitted"}`}
	c := NewMCPScraperWriteClient(fake, testSubmitSecret)
	ctx := WithScraperWriteProject(context.Background(), "snake")
	if _, err := c.Submit(ctx, SubmitReq{
		SubmissionID:     "ww_x",
		URL:              "https://example.com",
		Payload:          []byte("null"),
		SelectorBindings: []byte(""),
		WritesMode:       "on",
	}); err != nil {
		t.Fatalf("Submit error: %v", err)
	}
	if strings.Contains(fake.gotArgs, `"payload"`) {
		t.Errorf("null payload should be omitted, got args %s", fake.gotArgs)
	}
	if strings.Contains(fake.gotArgs, `"selector_bindings"`) {
		t.Errorf("empty selector_bindings should be omitted, got args %s", fake.gotArgs)
	}
}

// ---------- error mapping ----------

func TestMCPScraperWriteClient_ToolErrorBecomesGoError(t *testing.T) {
	// *mcp.Manager returns a tool-level (isError) result as an "MCP error: …"
	// string with a nil Go error; the client must convert it to a Go error.
	fake := &fakeScraperMCPExec{ret: "MCP error: web_submit mode=submit requires submission_id"}
	c := NewMCPScraperWriteClient(fake, testSubmitSecret)
	ctx := WithScraperWriteProject(context.Background(), "snake")

	if _, err := c.Submit(ctx, SubmitReq{SubmissionID: "ww_x", URL: "https://example.com", WritesMode: "on"}); err == nil {
		t.Fatal("Submit should surface an MCP tool error as a Go error")
	} else if !strings.Contains(err.Error(), "requires submission_id") {
		t.Errorf("error = %v, want the tool message preserved", err)
	}

	if _, err := c.Preview(context.Background(), PreviewReq{
		URL: "https://example.com/form", Fields: []WebField{{Selector: "#n", Value: "x"}}, ProjectID: "snake",
	}); err == nil {
		t.Fatal("Preview should surface an MCP tool error as a Go error")
	}
}

func TestMCPScraperWriteClient_TransportErrorBecomesGoError(t *testing.T) {
	fake := &fakeScraperMCPExec{retErr: errors.New(`MCP server "scraper" not connected for project "snake"`)}
	c := NewMCPScraperWriteClient(fake, testSubmitSecret)
	_, err := c.Preview(context.Background(), PreviewReq{
		URL: "https://example.com/form", Fields: []WebField{{Selector: "#n", Value: "x"}}, ProjectID: "snake",
	})
	if err == nil {
		t.Fatal("Preview should surface a transport error as a Go error")
	}
	if !strings.Contains(err.Error(), "not connected") {
		t.Errorf("error = %v, want the transport cause wrapped", err)
	}
}

func TestMCPScraperWriteClient_EmptyResultErrors(t *testing.T) {
	fake := &fakeScraperMCPExec{ret: "   "}
	c := NewMCPScraperWriteClient(fake, testSubmitSecret)
	_, err := c.Preview(context.Background(), PreviewReq{
		URL: "https://example.com/form", Fields: []WebField{{Selector: "#n", Value: "x"}}, ProjectID: "snake",
	})
	if err == nil {
		t.Fatal("an empty MCP result should be an error (fail-closed)")
	}
}
