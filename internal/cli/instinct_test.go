package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// sampleWireInstincts returns a couple of wire entries (mirroring
// api.InstinctJSON) for the list/export tests.
func sampleWireInstincts() []instinctEntry {
	return []instinctEntry{
		{
			ID: "ins_1", Scope: "project", ProjectID: "alpha", Domain: "recovery",
			TriggerKey: "tk_a", Trigger: `{"role":"coder","error_class":"Timeout"}`,
			Action: "retry resolved it", Confidence: 0.82, SupportCount: 5, ContradictCount: 1,
			Source: "observer", Status: "active",
		},
		{
			ID: "ins_2", Scope: "global", Domain: "workflow",
			TriggerKey: "tk_b", Action: "split the verify step",
			Confidence: 0.91, SupportCount: 9, Source: "observer", Status: "promoted",
		},
	}
}

func instinctHTTPStub(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	t.Setenv("VORNIK_API_URL", srv.URL)
}

func TestInstinctList_TableOutput(t *testing.T) {
	instinctHTTPStub(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/v1/instincts") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("domain"); got != "recovery" {
			t.Errorf("domain query = %q, want recovery", got)
		}
		_ = json.NewEncoder(w).Encode(instinctListResponse{Instincts: sampleWireInstincts()})
	})
	instinctListDomain = "recovery"
	instinctListJSON = false
	defer func() { instinctListDomain = ""; instinctListLimit = 100 }()
	instinctListLimit = 100

	out, err := captureStdoutFn(t, func() error { return runInstinctList(nil, nil) })
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(out, "ins_1") || !strings.Contains(out, "retry resolved it") {
		t.Errorf("table missing rows; got %q", out)
	}
}

func TestInstinctList_EmptyHint(t *testing.T) {
	instinctHTTPStub(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(instinctListResponse{Instincts: []instinctEntry{}})
	})
	instinctListDomain = ""
	instinctListJSON = false
	instinctListLimit = 100
	out, err := captureStdoutFn(t, func() error { return runInstinctList(nil, nil) })
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(strings.ToLower(out), "no instincts") {
		t.Errorf("expected empty hint; got %q", out)
	}
}

func TestInstinctShow_HumanOutput(t *testing.T) {
	instinctHTTPStub(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/instincts/ins_1" {
			t.Errorf("path = %q, want /api/v1/instincts/ins_1", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(instinctShowResponse{Instinct: sampleWireInstincts()[0]})
	})
	instinctShowJSON = false
	out, err := captureStdoutFn(t, func() error { return runInstinctShow(nil, []string{"ins_1"}) })
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	if !strings.Contains(out, "ins_1") || !strings.Contains(out, "Trigger:") {
		t.Errorf("show output incomplete; got %q", out)
	}
}

func TestInstinctRetire_PostsAndReports(t *testing.T) {
	var method, path string
	instinctHTTPStub(t, func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "ins_1", "status": "retired"})
	})
	instinctRetireJSON = false
	out, err := captureStdoutFn(t, func() error { return runInstinctRetire(nil, []string{"ins_1"}) })
	if err != nil {
		t.Fatalf("retire: %v", err)
	}
	if method != http.MethodPost {
		t.Errorf("method = %q, want POST", method)
	}
	if path != "/api/v1/instincts/ins_1/retire" {
		t.Errorf("path = %q", path)
	}
	if !strings.Contains(out, "retired") {
		t.Errorf("retire output = %q", out)
	}
}

func TestInstinctRetire_ServerError(t *testing.T) {
	instinctHTTPStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "NOT_FOUND", "message": "instinct not found"}})
	})
	err := runInstinctRetire(nil, []string{"ghost"})
	if err == nil {
		t.Fatal("expected error on 404")
	}
}

// TestInstinctExportImport_RoundTrip is the core round-trip assertion:
// export → frontmatter → import → wire shape must preserve the
// instincts, with trigger_json surviving the trigger-map detour.
func TestInstinctExportImport_RoundTrip(t *testing.T) {
	rows := sampleWireInstincts()

	doc, err := instinctsToFrontmatter(rows)
	if err != nil {
		t.Fatalf("to frontmatter: %v", err)
	}
	if doc.Version != instinctFrontmatterVersion {
		t.Errorf("version = %d, want %d", doc.Version, instinctFrontmatterVersion)
	}
	// The trigger_json must have decoded into a nested map.
	if doc.Instincts[0].Trigger["role"] != "coder" {
		t.Errorf("trigger.role = %v, want coder", doc.Instincts[0].Trigger["role"])
	}

	// Marshal → parse → validate → back to wire.
	raw, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	parsed, err := parseInstinctFrontmatter(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := validateInstinctFrontmatter(parsed); err != nil {
		t.Fatalf("validate: %v", err)
	}
	back := frontmatterToInstincts(parsed)

	if len(back) != len(rows) {
		t.Fatalf("round-trip count = %d, want %d", len(back), len(rows))
	}
	// The trigger string must re-encode to a semantically-equal JSON
	// object (key order is not guaranteed, so compare decoded maps).
	var origTrig, backTrig map[string]any
	_ = json.Unmarshal([]byte(rows[0].Trigger), &origTrig)
	_ = json.Unmarshal([]byte(back[0].Trigger), &backTrig)
	if !reflect.DeepEqual(origTrig, backTrig) {
		t.Errorf("trigger round-trip mismatch: %v vs %v", origTrig, backTrig)
	}
	// Non-trigger fields must be byte-identical.
	for i := range rows {
		if back[i].ID != rows[i].ID || back[i].Domain != rows[i].Domain ||
			back[i].Scope != rows[i].Scope || back[i].Action != rows[i].Action ||
			back[i].Status != rows[i].Status || back[i].Confidence != rows[i].Confidence {
			t.Errorf("instinct[%d] mismatch:\n got %+v\nwant %+v", i, back[i], rows[i])
		}
	}
}

func TestValidateInstinctFrontmatter_Rejects(t *testing.T) {
	cases := []struct {
		name string
		doc  instinctFrontmatter
	}{
		{"empty", instinctFrontmatter{Version: 1}},
		{"bad domain", instinctFrontmatter{Version: 1, Instincts: []instinctFrontmatterEntry{{Domain: "bogus", Scope: "global", Action: "x"}}}},
		{"bad scope", instinctFrontmatter{Version: 1, Instincts: []instinctFrontmatterEntry{{Domain: "recovery", Scope: "nowhere", Action: "x"}}}},
		{"missing action", instinctFrontmatter{Version: 1, Instincts: []instinctFrontmatterEntry{{Domain: "recovery", Scope: "global"}}}},
		{"project without id", instinctFrontmatter{Version: 1, Instincts: []instinctFrontmatterEntry{{Domain: "recovery", Scope: "project", Action: "x"}}}},
		{"global with id", instinctFrontmatter{Version: 1, Instincts: []instinctFrontmatterEntry{{Domain: "recovery", Scope: "global", ProjectID: "alpha", Action: "x"}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateInstinctFrontmatter(tc.doc); err == nil {
				t.Errorf("%s: expected validation error", tc.name)
			}
		})
	}
}

func TestParseInstinctFrontmatter_VersionHandling(t *testing.T) {
	// Missing version defaults to current; unsupported version errors.
	if _, err := parseInstinctFrontmatter([]byte("instincts:\n  - domain: recovery\n    scope: global\n    action: x\n")); err != nil {
		t.Errorf("missing version should default, got %v", err)
	}
	if _, err := parseInstinctFrontmatter([]byte("version: 99\ninstincts: []\n")); err == nil {
		t.Error("version 99 should be unsupported")
	}
}

func TestInstinctImport_EndToEnd(t *testing.T) {
	rows := sampleWireInstincts()
	doc, _ := instinctsToFrontmatter(rows)
	raw, _ := yaml.Marshal(doc)
	dir := t.TempDir()
	path := filepath.Join(dir, "instincts.yaml")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	cmd := &cobra.Command{}
	var buf strings.Builder
	cmd.SetOut(&buf)
	instinctImportJSON = false
	if err := runInstinctImport(cmd, []string{path}); err != nil {
		t.Fatalf("import: %v", err)
	}
	if !strings.Contains(buf.String(), "Validated 2 instinct") {
		t.Errorf("import output = %q", buf.String())
	}
}

func TestInstinctImport_BadFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.yaml")
	_ = os.WriteFile(path, []byte("version: 1\ninstincts:\n  - domain: nope\n    scope: global\n    action: x\n"), 0o644)
	cmd := &cobra.Command{}
	if err := runInstinctImport(cmd, []string{path}); err == nil {
		t.Error("expected import to reject bad domain")
	}
}

func TestInstinctList_JSONOutput(t *testing.T) {
	instinctHTTPStub(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(instinctListResponse{Instincts: sampleWireInstincts()})
	})
	instinctListDomain, instinctListScope, instinctListProject, instinctListStatus = "", "", "", ""
	instinctListMinConf = 0
	instinctListLimit = 100
	instinctListJSON = true
	defer func() { instinctListJSON = false }()
	out, err := captureStdoutFn(t, func() error { return runInstinctList(nil, nil) })
	if err != nil {
		t.Fatalf("list json: %v", err)
	}
	var resp instinctListResponse
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("json output not decodable: %v; got %q", err, out)
	}
	if len(resp.Instincts) != 2 {
		t.Errorf("json list count = %d, want 2", len(resp.Instincts))
	}
}

func TestInstinctShow_JSONOutput(t *testing.T) {
	instinctHTTPStub(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(instinctShowResponse{Instinct: sampleWireInstincts()[0]})
	})
	instinctShowJSON = true
	defer func() { instinctShowJSON = false }()
	out, err := captureStdoutFn(t, func() error { return runInstinctShow(nil, []string{"ins_1"}) })
	if err != nil {
		t.Fatalf("show json: %v", err)
	}
	var e instinctEntry
	if err := json.Unmarshal([]byte(out), &e); err != nil {
		t.Fatalf("show json undecodable: %v", err)
	}
	if e.ID != "ins_1" {
		t.Errorf("json id = %q", e.ID)
	}
}

func TestInstinctRetire_JSONOutput(t *testing.T) {
	instinctHTTPStub(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "ins_1", "status": "retired"})
	})
	instinctRetireJSON = true
	defer func() { instinctRetireJSON = false }()
	out, err := captureStdoutFn(t, func() error { return runInstinctRetire(nil, []string{"ins_1"}) })
	if err != nil {
		t.Fatalf("retire json: %v", err)
	}
	if !strings.Contains(out, `"status": "retired"`) {
		t.Errorf("retire json = %q", out)
	}
}

func TestInstinctListQuery_AllFilters(t *testing.T) {
	q := instinctListQuery("recovery", "global", "alpha", "active", 0.6, 25)
	for _, want := range []string{"domain=recovery", "scope=global", "project=alpha", "status=active", "min_confidence=0.6", "limit=25"} {
		if !strings.Contains(q, want) {
			t.Errorf("query %q missing %q", q, want)
		}
	}
	// No filters → bare path.
	if got := instinctListQuery("", "", "", "", 0, 0); got != "/api/v1/instincts" {
		t.Errorf("empty query = %q, want bare path", got)
	}
}

func TestInstinctExport_WritesFileAndStdout(t *testing.T) {
	instinctHTTPStub(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(instinctListResponse{Instincts: sampleWireInstincts()})
	})
	instinctExportDomain, instinctExportScope, instinctExportProject, instinctExportStatus = "", "", "", ""
	instinctExportMinConf = 0
	instinctExportLimit = 1000

	// To a file.
	dir := t.TempDir()
	path := filepath.Join(dir, "out.yaml")
	instinctExportOutput = path
	cmd := &cobra.Command{}
	var errBuf strings.Builder
	cmd.SetErr(&errBuf)
	if err := runInstinctExport(cmd, nil); err != nil {
		t.Fatalf("export to file: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read export: %v", err)
	}
	var doc instinctFrontmatter
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("export file not valid YAML: %v", err)
	}
	if len(doc.Instincts) != 2 {
		t.Errorf("export wrote %d instincts, want 2", len(doc.Instincts))
	}

	// To stdout.
	instinctExportOutput = ""
	var outBuf strings.Builder
	cmd2 := &cobra.Command{}
	cmd2.SetOut(&outBuf)
	if err := runInstinctExport(cmd2, nil); err != nil {
		t.Fatalf("export to stdout: %v", err)
	}
	if !strings.Contains(outBuf.String(), "instincts:") {
		t.Errorf("stdout export missing instincts list; got %q", outBuf.String())
	}
}

func TestInstinctImport_JSONOutput(t *testing.T) {
	rows := sampleWireInstincts()
	doc, _ := instinctsToFrontmatter(rows)
	raw, _ := yaml.Marshal(doc)
	dir := t.TempDir()
	path := filepath.Join(dir, "in.yaml")
	_ = os.WriteFile(path, raw, 0o644)

	cmd := &cobra.Command{}
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&strings.Builder{})
	instinctImportJSON = true
	defer func() { instinctImportJSON = false }()
	if err := runInstinctImport(cmd, []string{path}); err != nil {
		t.Fatalf("import json: %v", err)
	}
	var resp instinctListResponse
	if err := json.Unmarshal([]byte(out.String()), &resp); err != nil {
		t.Fatalf("import json undecodable: %v; got %q", err, out.String())
	}
	if len(resp.Instincts) != 2 {
		t.Errorf("import json count = %d, want 2", len(resp.Instincts))
	}
}

func TestInstinctImport_MissingFile(t *testing.T) {
	cmd := &cobra.Command{}
	if err := runInstinctImport(cmd, []string{"/no/such/file.yaml"}); err == nil {
		t.Error("expected error on missing file")
	}
}

func TestMarshalTriggerMap(t *testing.T) {
	if got := marshalTriggerMap(nil); got != "" {
		t.Errorf("nil map = %q, want empty", got)
	}
	got := marshalTriggerMap(map[string]any{"role": "coder"})
	if got != `{"role":"coder"}` {
		t.Errorf("marshal = %q", got)
	}
}

// TestInstinctCommand_Wiring confirms the subcommands are registered
// under the parent in init().
func TestInstinctCommand_Wiring(t *testing.T) {
	want := map[string]bool{"list": false, "show": false, "retire": false, "export": false, "import": false}
	for _, c := range instinctCmd.Commands() {
		name := strings.Fields(c.Use)[0]
		if _, ok := want[name]; ok {
			want[name] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("subcommand %q not registered under instinct", name)
		}
	}
	// And the parent is registered under root.
	var onRoot bool
	for _, c := range rootCmd.Commands() {
		if c == instinctCmd {
			onRoot = true
		}
	}
	if !onRoot {
		t.Error("instinct command not registered on rootCmd")
	}
}
