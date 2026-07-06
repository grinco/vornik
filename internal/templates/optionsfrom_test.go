package templates

import (
	"errors"
	"strings"
	"testing"
)

type fakeResolver struct {
	bySource map[string][]string
	err      error
}

func (f *fakeResolver) ResolveOptions(source string) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.bySource[source], nil
}

func dynManifest() Manifest {
	return Manifest{
		Slug: "dyn", DisplayName: "Dyn",
		Parameters: []Parameter{
			{Name: "projectId", Type: "string", Label: "ID", Required: true},
			{Name: "mcpServers", Type: "multiselect", Label: "Servers",
				OptionsFrom: OptionsSourceMCPRegistry},
			{Name: "model", Type: "enum", Label: "Model",
				OptionsFrom: OptionsSourceModels, Required: true},
		},
		Files: []FileMap{{Source: "p.tmpl", Target: "projects/{{.projectId}}.yaml"}},
	}
}

func TestOptionsFrom_ResolvedAndAccepted(t *testing.T) {
	r := &fakeResolver{bySource: map[string][]string{
		OptionsSourceMCPRegistry: {"slack", "github"},
		OptionsSourceModels:      {"m1"},
	}}
	got, err := ValidateParamsMulti(dynManifest(), map[string][]string{
		"projectId":  {"px"},
		"mcpServers": {"slack"},
		"model":      {"m1"},
	}, r)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if got["model"] != "m1" {
		t.Fatalf("model: %#v", got["model"])
	}
}

func TestOptionsFrom_StaleValueTargetedError(t *testing.T) {
	r := &fakeResolver{bySource: map[string][]string{
		OptionsSourceMCPRegistry: {"slack"},
		OptionsSourceModels:      {"m1"},
	}}
	_, err := ValidateParamsMulti(dynManifest(), map[string][]string{
		"projectId":  {"px"},
		"mcpServers": {"jira"},
		"model":      {"m1"},
	}, r)
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Field != "mcpServers" {
		t.Fatalf("want targeted ValidationError on mcpServers, got %v", err)
	}
	if !strings.Contains(ve.Message, `"jira"`) ||
		!strings.Contains(ve.Message, "no longer available from mcp_registry") ||
		!strings.Contains(ve.Message, "refresh the form") {
		t.Fatalf("message not targeted per spec: %q", ve.Message)
	}
}

func TestOptionsFrom_ResolverErrorSurfaces(t *testing.T) {
	r := &fakeResolver{err: errors.New("daemon unreachable")}
	_, err := ValidateParamsMulti(dynManifest(), map[string][]string{
		"projectId": {"px"}, "model": {"m1"},
	}, r)
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("want ValidationError, got %v", err)
	}
	if !strings.Contains(ve.Message, "could not resolve options") {
		t.Fatalf("message: %q", ve.Message)
	}
}

func TestOptionsFrom_NilResolverFreeString(t *testing.T) {
	got, err := ValidateParamsMulti(dynManifest(), map[string][]string{
		"projectId": {"px"},
		"model":     {"anything-goes"},
	}, nil)
	if err != nil {
		t.Fatalf("nil resolver must pass values through: %v", err)
	}
	if got["model"] != "anything-goes" {
		t.Fatalf("model: %#v", got["model"])
	}
}

func TestOptionsFrom_NilResolverMultiselectFreeValues(t *testing.T) {
	got, err := ValidateParamsMulti(dynManifest(), map[string][]string{
		"projectId":  {"px"},
		"mcpServers": {"arbitrary", "values", "accepted"},
		"model":      {"m1"},
	}, nil)
	if err != nil {
		t.Fatalf("nil resolver with multiselect must pass values through: %v", err)
	}
	servers := got["mcpServers"].([]string)
	if len(servers) != 3 || servers[0] != "arbitrary" || servers[1] != "values" || servers[2] != "accepted" {
		t.Fatalf("mcpServers: %#v", servers)
	}
}

func TestValidateManifest_OptionsFromRules(t *testing.T) {
	m := dynManifest()
	if err := validateManifest(m); err != nil {
		t.Fatalf("valid optionsFrom manifest refused: %v", err)
	}
	bad := dynManifest()
	bad.Parameters[1].OptionsFrom = "phases-of-the-moon"
	if err := validateManifest(bad); err == nil {
		t.Fatal("unknown optionsFrom source must be refused at load")
	}
	both := dynManifest()
	both.Parameters[1].Options = []string{"x"}
	if err := validateManifest(both); err == nil {
		t.Fatal("options + optionsFrom together must be refused")
	}
	wrongType := dynManifest()
	wrongType.Parameters[0].OptionsFrom = OptionsSourceModels // string param
	if err := validateManifest(wrongType); err == nil {
		t.Fatal("optionsFrom on a string param must be refused")
	}
}

func TestNeedsMultiValue_EnumWithOptionsFrom(t *testing.T) {
	m := Manifest{
		Slug: "enum-dyn", DisplayName: "EnumDyn",
		Parameters: []Parameter{
			{Name: "model", Type: "enum", Label: "Model", OptionsFrom: OptionsSourceModels},
		},
		Files: []FileMap{{Source: "p.tmpl", Target: "out.yaml"}},
	}
	if !m.NeedsMultiValue() {
		t.Fatal("manifest with enum + optionsFrom must report NeedsMultiValue=true")
	}
}

// TestLoad_RejectsOptionsFromViolations pins the new optionsFrom
// manifest rules at the Load() level — the actual daemon-startup
// path — not just via direct validateManifest calls. A bad
// template.yaml on disk must fail Load so operators see the error
// at startup (Task 1 review, Important finding 1).
func TestLoad_RejectsOptionsFromViolations(t *testing.T) {
	cases := []struct {
		name     string
		manifest string
		wantErr  string
	}{
		{
			name: "options and optionsFrom both set",
			manifest: `
displayName: "BothSet"
parameters:
  - {name: model, type: enum, label: "Model", optionsFrom: models, options: ["x"]}
files:
  - {source: p.tmpl, target: "projects/x.yaml"}
`,
			wantErr: "options and optionsFrom are mutually exclusive",
		},
		{
			name: "unknown optionsFrom source",
			manifest: `
displayName: "UnknownSource"
parameters:
  - {name: servers, type: multiselect, label: "Servers", optionsFrom: phases-of-the-moon}
files:
  - {source: p.tmpl, target: "projects/x.yaml"}
`,
			wantErr: "unknown optionsFrom source",
		},
		{
			name: "optionsFrom on string param",
			manifest: `
displayName: "WrongType"
parameters:
  - {name: projectId, type: string, label: "ID", optionsFrom: models}
files:
  - {source: p.tmpl, target: "projects/x.yaml"}
`,
			wantErr: "only valid on enum or multiselect",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeManifest(t, dir, "offender", tc.manifest, "p.tmpl", "x\n")
			_, err := Load(dir)
			if err == nil {
				t.Fatalf("Load must refuse manifest with %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}

// And the valid shape must still Load — a dynamic-source manifest
// on disk is accepted end to end.
func TestLoad_AcceptsValidOptionsFromManifest(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "dyn-ok", `
displayName: "DynOK"
parameters:
  - {name: mcpServers, type: multiselect, label: "Servers", optionsFrom: mcp_registry}
  - {name: model, type: enum, label: "Model", optionsFrom: models, required: true}
files:
  - {source: p.tmpl, target: "projects/x.yaml"}
`, "p.tmpl", "x\n")
	c, err := Load(dir)
	if err != nil {
		t.Fatalf("valid optionsFrom manifest must load: %v", err)
	}
	m, ok := c.Get("dyn-ok")
	if !ok {
		t.Fatal("loaded manifest not found by slug")
	}
	if m.Parameters[0].OptionsFrom != OptionsSourceMCPRegistry ||
		m.Parameters[1].OptionsFrom != OptionsSourceModels {
		t.Fatalf("optionsFrom not parsed from YAML: %#v", m.Parameters)
	}
}

func TestValidateManifest_ListRefusesStaticOptions(t *testing.T) {
	m := Manifest{
		Slug: "list-opts", DisplayName: "ListOpts",
		Parameters: []Parameter{
			{Name: "items", Type: "list", Label: "Items", Options: []string{"a", "b"}},
		},
		Files: []FileMap{{Source: "p.tmpl", Target: "out.yaml"}},
	}
	err := validateManifest(m)
	if err == nil {
		t.Fatal("list type with static options must be refused")
	}
	if !strings.Contains(err.Error(), "list type does not take options") {
		t.Fatalf("message: %q", err)
	}
}
