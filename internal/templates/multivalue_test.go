package templates

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func multiManifest() Manifest {
	return Manifest{
		Slug:        "multi",
		DisplayName: "Multi",
		Parameters: []Parameter{
			{Name: "projectId", Type: "string", Label: "ID", Required: true},
			{Name: "feeds", Type: "list", Label: "Feeds", Required: true,
				Pattern: `https://.+`},
			{Name: "servers", Type: "multiselect", Label: "Servers",
				Options: []string{"slack", "github"}},
			{Name: "cadence", Type: "enum", Label: "Cadence",
				Options: []string{"6h", "24h"}, Default: "24h"},
		},
		Files: []FileMap{{Source: "p.tmpl", Target: "projects/{{.projectId}}.yaml"}},
	}
}

func TestNeedsMultiValue(t *testing.T) {
	if !multiManifest().NeedsMultiValue() {
		t.Fatal("manifest with list param must need multi-value path")
	}
	scalar := Manifest{Parameters: []Parameter{{Name: "a", Type: "string"}}}
	if scalar.NeedsMultiValue() {
		t.Fatal("scalar-only manifest must not need multi-value path")
	}
}

func TestValidateParamsMulti_ListCollectsAllValues(t *testing.T) {
	got, err := ValidateParamsMulti(multiManifest(), map[string][]string{
		"projectId": {"px"},
		"feeds":     {"https://a.example", " https://b.example "},
		"servers":   {"slack"},
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	feeds, ok := got["feeds"].([]string)
	if !ok || len(feeds) != 2 || feeds[1] != "https://b.example" {
		t.Fatalf("feeds not collected/trimmed: %#v", got["feeds"])
	}
	if got["cadence"] != "24h" {
		t.Fatalf("scalar default not filled: %#v", got["cadence"])
	}
	if got["projectId"] != "px" {
		t.Fatalf("scalar stored as %T %#v, want string", got["projectId"], got["projectId"])
	}
}

func TestValidateParamsMulti_RequiredListRefusesEmpty(t *testing.T) {
	_, err := ValidateParamsMulti(multiManifest(), map[string][]string{
		"projectId": {"px"},
		"feeds":     {"", "   "},
	}, nil)
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Field != "feeds" {
		t.Fatalf("want ValidationError on feeds, got %v", err)
	}
}

func TestValidateParamsMulti_ListPatternPerElement(t *testing.T) {
	_, err := ValidateParamsMulti(multiManifest(), map[string][]string{
		"projectId": {"px"},
		"feeds":     {"https://ok.example", "ftp://bad.example"},
	}, nil)
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Field != "feeds" {
		t.Fatalf("want per-element pattern failure on feeds, got %v", err)
	}
}

func TestValidateParamsMulti_MultiselectRefusesUnknownOption(t *testing.T) {
	_, err := ValidateParamsMulti(multiManifest(), map[string][]string{
		"projectId": {"px"},
		"feeds":     {"https://a.example"},
		"servers":   {"slack", "jira"},
	}, nil)
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Field != "servers" {
		t.Fatalf("want ValidationError on servers, got %v", err)
	}
}

func TestValidateParamsMulti_ScalarTakesLastValue(t *testing.T) {
	got, err := ValidateParamsMulti(multiManifest(), map[string][]string{
		"projectId": {"first", "second"},
		"feeds":     {"https://a.example"},
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["projectId"] != "second" {
		t.Fatalf("scalar should take last value, got %#v", got["projectId"])
	}
}

func TestValidateParamsMulti_UnknownParamRefused(t *testing.T) {
	_, err := ValidateParamsMulti(multiManifest(), map[string][]string{
		"projectId": {"px"},
		"feeds":     {"https://a.example"},
		"evil":      {"x"},
	}, nil)
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Field != "evil" {
		t.Fatalf("want unknown-param refusal, got %v", err)
	}
}

func TestMaterialiseFilesMulti_RangeRendersList(t *testing.T) {
	dir := t.TempDir()
	slugDir := filepath.Join(dir, "multi")
	if err := os.MkdirAll(slugDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "id: {{.projectId}}\nfeeds:\n{{range .feeds}}  - {{.}}\n{{end}}"
	if err := os.WriteFile(filepath.Join(slugDir, "p.tmpl"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	manifestYAML := `displayName: "Multi"
parameters:
  - name: projectId
    type: string
    label: ID
    required: true
  - name: feeds
    type: list
    label: Feeds
    required: true
files:
  - source: p.tmpl
    target: "projects/{{.projectId}}.yaml"
`
	if err := os.WriteFile(filepath.Join(slugDir, "template.yaml"), []byte(manifestYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	cat, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	m, _ := cat.Get("multi")
	rendered, err := cat.MaterialiseFilesMulti(m, map[string][]string{
		"projectId": {"px"},
		"feeds":     {"https://a.example", "https://b.example"},
	}, nil)
	if err != nil {
		t.Fatalf("materialise: %v", err)
	}
	got := rendered["projects/px.yaml"]
	if !strings.Contains(got, "- https://a.example") || !strings.Contains(got, "- https://b.example") {
		t.Fatalf("list not rendered via range:\n%s", got)
	}
}

func TestValidateManifest_ListTypeAccepted_DefaultRefused(t *testing.T) {
	m := multiManifest()
	if err := validateManifest(m); err != nil {
		t.Fatalf("list/multiselect types must be accepted: %v", err)
	}
	m.Parameters[1].Default = "https://a.example"
	if err := validateManifest(m); err == nil {
		t.Fatal("Default on a list param must be refused")
	}
}

// Regression (spec back-compat contract item 1): for scalar-only
// manifests the multi path must produce byte-identical output to
// the legacy path — the shipped templates are the fixtures.
func TestMultiPathByteIdenticalForShippedTemplates(t *testing.T) {
	cat, err := Load("../../configs/project-templates")
	if err != nil {
		t.Fatalf("load shipped catalog: %v", err)
	}
	samples := map[string]map[string]string{
		"news-feed":          {"projectId": "bytecheck", "displayName": "Byte Check", "topic": "t", "interval": "4h", "llmModel": ""},
		"personal-assistant": {"projectId": "bytecheck", "displayName": "Byte Check"},
		"companion":          {"projectId": "bytecheck", "displayName": "Byte Check"},
	}
	for slug, params := range samples {
		m, ok := cat.Get(slug)
		if !ok {
			t.Fatalf("shipped template %q missing", slug)
		}
		if m.NeedsMultiValue() {
			t.Fatalf("shipped template %q must stay scalar-only in this phase", slug)
		}
		legacy, lerr := cat.MaterialiseFiles(m, params)
		if lerr != nil {
			t.Fatalf("%s legacy materialise: %v", slug, lerr)
		}
		multi := map[string][]string{}
		for k, v := range params {
			multi[k] = []string{v}
		}
		viaMulti, merr := cat.MaterialiseFilesMulti(m, multi, nil)
		if merr != nil {
			t.Fatalf("%s multi materialise: %v", slug, merr)
		}
		if len(legacy) != len(viaMulti) {
			t.Fatalf("%s: file-set size differs: %d vs %d", slug, len(legacy), len(viaMulti))
		}
		for target, body := range legacy {
			if viaMulti[target] != body {
				t.Fatalf("%s: %s differs between legacy and multi path", slug, target)
			}
		}
	}
}
