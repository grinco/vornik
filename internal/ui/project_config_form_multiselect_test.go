package ui

import (
	"bytes"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"vornik.io/vornik/internal/registry"
)

// TestPopulateFromProject_SplitsAllowedToolsByBuiltin — when the
// project's allowedTools mixes canonical built-ins with operator-
// curated custom names, the form data must split them so the UI
// can render the checkbox grid + custom textarea independently.
// Without this split, ticking a checkbox loses any non-builtin
// entry the operator typed.
func TestPopulateFromProject_SplitsAllowedToolsByBuiltin(t *testing.T) {
	proj := &registry.Project{
		Permissions: registry.ProjectPermissions{
			AllowedTools: []string{
				"file_read",              // suggested agent tool
				"my_custom_tool",         // not in suggestions
				"run_shell",              // suggested agent tool
				"mcp_broker_place_order", // MCP
			},
		},
	}
	var data ProjectConfigFormData
	populateFormFromProject(&data, proj)

	// Builtin checkboxes must reflect which canonical tools are set.
	checked := map[string]bool{}
	for _, b := range data.BuiltinTools {
		checked[b.Name] = b.Allowed
	}
	if !checked["file_read"] || !checked["run_shell"] {
		t.Errorf("built-ins not marked Allowed: %+v", data.BuiltinTools)
	}
	if checked["git_diff"] {
		t.Errorf("untouched built-in marked Allowed: %+v", data.BuiltinTools)
	}

	// Custom textarea must keep ONLY the non-builtins.
	customs := strings.Split(strings.TrimSpace(data.CustomAllowedTools), "\n")
	customSet := map[string]bool{}
	for _, c := range customs {
		customSet[c] = true
	}
	if !customSet["my_custom_tool"] || !customSet["mcp_broker_place_order"] {
		t.Errorf("custom textarea missing non-builtins: %q", data.CustomAllowedTools)
	}
	if customSet["file_read"] {
		t.Errorf("custom textarea contaminated with built-in: %q", data.CustomAllowedTools)
	}
}

// TestApplyFormPostValues_CombinesBuiltinsAndCustoms — the
// form-parse path must merge ticked builtins + textarea customs
// back into a single allowedTools list. Otherwise saving the form
// would silently drop everything the operator picked.
func TestApplyFormPostValues_CombinesBuiltinsAndCustoms(t *testing.T) {
	form := url.Values{}
	// Two builtins ticked, two customs typed.
	form.Add("permissions_allowedTools_builtin", "file_read")
	form.Add("permissions_allowedTools_builtin", "run_shell")
	form.Set("permissions_allowedTools_custom", "my_custom_tool\nmcp_broker_place_order")

	r, _ := http.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if err := r.ParseForm(); err != nil {
		t.Fatalf("parse form: %v", err)
	}

	var data ProjectConfigFormData
	overlayFormValuesOntoData(&data, r)

	// Final combined list (what the YAML patcher consumes) must
	// have both builtins and the custom entries.
	combined := data.PermissionsAllowedTools
	for _, want := range []string{"file_read", "run_shell", "my_custom_tool", "mcp_broker_place_order"} {
		if !strings.Contains(combined, want) {
			t.Errorf("PermissionsAllowedTools missing %q. got: %q", want, combined)
		}
	}

	// Builtin checkbox state must round-trip back into the form
	// data so a validation-failure re-render keeps the ticks.
	checked := map[string]bool{}
	for _, b := range data.BuiltinTools {
		checked[b.Name] = b.Allowed
	}
	if !checked["file_read"] || !checked["run_shell"] {
		t.Errorf("ticked checkboxes not round-tripped: %+v", data.BuiltinTools)
	}
	if checked["git_diff"] {
		t.Errorf("unticked checkbox marked Allowed: %+v", data.BuiltinTools)
	}

	// Custom textarea round-trips verbatim.
	if !strings.Contains(data.CustomAllowedTools, "my_custom_tool") {
		t.Errorf("CustomAllowedTools dropped its content: %q", data.CustomAllowedTools)
	}
}

// TestApplyFormPostValues_NoBuiltinsTicked — when no boxes are
// checked and the custom textarea is empty, the final list is
// empty too (preserves the "inherit defaults" semantics).
func TestApplyFormPostValues_NoBuiltinsTicked(t *testing.T) {
	form := url.Values{}
	form.Set("permissions_allowedTools_custom", "")
	r, _ := http.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if err := r.ParseForm(); err != nil {
		t.Fatalf("parse form: %v", err)
	}
	var data ProjectConfigFormData
	overlayFormValuesOntoData(&data, r)
	if strings.TrimSpace(data.PermissionsAllowedTools) != "" {
		t.Errorf("expected empty allowedTools when nothing ticked; got %q", data.PermissionsAllowedTools)
	}
}

// TestProjectConfigForm_RendersBuiltinCheckboxGrid — the form
// template must render a checkbox for every common agent runtime
// tool so operators don't have to remember the spellings.
func TestProjectConfigForm_RendersBuiltinCheckboxGrid(t *testing.T) {
	s := NewServer()
	builtins := commonAllowedToolSuggestions
	opts := make([]BuiltinToolOption, len(builtins))
	for i, n := range builtins {
		opts[i] = BuiltinToolOption{Name: n, Allowed: n == "file_read"}
	}
	data := ProjectConfigFormData{
		Title:           "Project Config",
		CurrentPage:     "projects",
		ProjectID:       "demo",
		WorkflowOptions: []string{"adaptive"},
		AutonomyModes:   []string{"llm"},
		TimezoneOptions: []string{"UTC"},
		BuiltinTools:    opts,
	}
	var buf bytes.Buffer
	if err := s.templates.ExecuteTemplate(&buf, "project_config_form.html", data); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()

	for _, name := range builtins {
		if !strings.Contains(body, `value="`+name+`"`) {
			t.Errorf("checkbox missing for built-in %q", name)
		}
	}
	if strings.Contains(body, `value="create_task"`) {
		t.Errorf("dispatcher chat tool create_task should not be rendered as an agent allowedTools option")
	}
	// The ticked one must include `checked`.
	idx := strings.Index(body, `value="file_read"`)
	if idx < 0 {
		t.Errorf("rendered grid missing the file_read entry entirely")
	} else {
		end := idx + 120
		if end > len(body) {
			end = len(body)
		}
		if !strings.Contains(body[idx:end], "checked") {
			t.Errorf("rendered file_read checkbox is not checked")
		}
	}
}

// TestProjectConfigForm_RendersTaskTypeChips — the chip row above
// the Allowed Task Types textarea shipped suggestions so operators
// don't start from a blank slate.
func TestProjectConfigForm_RendersTaskTypeChips(t *testing.T) {
	s := NewServer()
	data := ProjectConfigFormData{
		Title:               "Project Config",
		CurrentPage:         "projects",
		ProjectID:           "demo",
		WorkflowOptions:     []string{"adaptive"},
		AutonomyModes:       []string{"llm"},
		TimezoneOptions:     []string{"UTC"},
		TaskTypeSuggestions: []string{"research", "trading"},
	}
	var buf bytes.Buffer
	if err := s.templates.ExecuteTemplate(&buf, "project_config_form.html", data); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := buf.String()
	if !strings.Contains(body, `data-task-type="research"`) {
		t.Errorf("chip missing for 'research'")
	}
	if !strings.Contains(body, `data-task-type="trading"`) {
		t.Errorf("chip missing for 'trading'")
	}
}
