package assetschema

import (
	"strings"
	"testing"
)

func bindSchema() AssetSchema {
	return AssetSchema{
		Asset: "project",
		Sections: []Section{{
			Title: "T",
			Fields: []Field{
				{Path: "projectId", Kind: KindString, Required: true, ReadOnly: true},
				{Path: "maxConcurrentTasks", Kind: KindInt},
				{Path: "budget.daily_hard_usd", Kind: KindFloat},
				{Path: "autonomy.enabled", Kind: KindBool},
				{Path: "autonomy.mode", Kind: KindEnum, Enum: []string{"llm", "cron", "backlog"}},
				{Path: "autonomy.pollInterval", Kind: KindDuration},
				{Path: "acceptCallsFrom", Kind: KindStringList},
			},
		}},
	}
}

func TestBindForm_TypedConversion(t *testing.T) {
	form := map[string]string{
		"projectId":             "should-be-ignored", // ReadOnly → skipped
		"maxConcurrentTasks":    "4",
		"budget.daily_hard_usd": "25.5",
		"autonomy.enabled":      "on",
		"autonomy.mode":         "cron",
		"autonomy.pollInterval": "5m",
		"acceptCallsFrom":       "a\nb, c\n\n",
	}
	vals, errs := BindForm(bindSchema(), func(n string) string { return form[n] })
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %+v", errs)
	}
	got := map[string]any{}
	for _, v := range vals {
		got[v.Path] = v.Value
		if v.Path == "projectId" {
			t.Error("ReadOnly projectId must not be bound")
		}
	}
	if got["maxConcurrentTasks"] != 4 {
		t.Errorf("int convert: %v (%T)", got["maxConcurrentTasks"], got["maxConcurrentTasks"])
	}
	if got["budget.daily_hard_usd"] != 25.5 {
		t.Errorf("float convert: %v", got["budget.daily_hard_usd"])
	}
	if got["autonomy.enabled"] != true {
		t.Errorf("bool convert: %v", got["autonomy.enabled"])
	}
	if got["autonomy.mode"] != "cron" {
		t.Errorf("enum convert: %v", got["autonomy.mode"])
	}
	list, ok := got["acceptCallsFrom"].([]string)
	if !ok || len(list) != 3 || list[0] != "a" || list[2] != "c" {
		t.Errorf("stringlist convert: %v", got["acceptCallsFrom"])
	}
}

// TestBindForm_NormalizesLineEndings is the regression guard for the
// config-drift-via-CRLF incident: HTML <textarea> values arrive CRLF-encoded
// per the HTTP spec, and the schema save path wrote those bytes verbatim into
// the deployed YAML/markdown, re-triggering config-drift-check on every UI
// edit. BindForm is the single seam every schema save (project/swarm/workflow,
// top-level + collection) routes through, so it must normalise CRLF (and bare
// CR) to LF before the value is validated, converted, and ultimately persisted.
func TestBindForm_NormalizesLineEndings(t *testing.T) {
	schema := AssetSchema{
		Asset: "swarm",
		Sections: []Section{{
			Title: "T",
			Fields: []Field{
				{Path: "systemPrompt", Kind: KindString, Backing: BackingBody},
				{Path: "crOnly", Kind: KindString},
			},
		}},
	}
	form := map[string]string{
		"systemPrompt": "line one\r\nline two\r\nline three",
		"crOnly":       "alpha\rbeta",
	}
	vals, errs := BindForm(schema, func(n string) string { return form[n] })
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %+v", errs)
	}
	got := map[string]string{}
	for _, v := range vals {
		s, _ := v.Value.(string)
		got[v.Path] = s
	}
	wantBody := "line one\nline two\nline three"
	if got["systemPrompt"] != wantBody {
		t.Errorf("CRLF not normalised: %q want %q", got["systemPrompt"], wantBody)
	}
	if strings.ContainsRune(got["systemPrompt"], '\r') {
		t.Errorf("bound value still contains CR: %q", got["systemPrompt"])
	}
	wantCR := "alpha\nbeta"
	if got["crOnly"] != wantCR {
		t.Errorf("bare CR not normalised: %q want %q", got["crOnly"], wantCR)
	}
}

func TestBindForm_ValidationErrorsBlock(t *testing.T) {
	form := map[string]string{
		"maxConcurrentTasks":    "notanint",
		"autonomy.mode":         "bogus",
		"autonomy.pollInterval": "5 hours",
	}
	vals, errs := BindForm(bindSchema(), func(n string) string { return form[n] })
	if len(errs) != 3 {
		t.Fatalf("want 3 field errors, got %d: %+v", len(errs), errs)
	}
	// The bad fields must NOT appear in the bound values.
	for _, v := range vals {
		switch v.Path {
		case "maxConcurrentTasks", "autonomy.mode", "autonomy.pollInterval":
			t.Errorf("invalid field %q should not be bound", v.Path)
		}
	}
}

func TestBindForm_OptionalBlankNotProvided(t *testing.T) {
	vals, errs := BindForm(bindSchema(), func(string) string { return "" })
	if len(errs) != 0 {
		t.Fatalf("blank optional fields must not error: %+v", errs)
	}
	for _, v := range vals {
		if v.Provided {
			t.Errorf("blank field %q must be Provided=false", v.Path)
		}
	}
}

func TestCurrentValues_Prefill(t *testing.T) {
	doc := map[string]any{
		"projectId":          "ibkr",
		"maxConcurrentTasks": 4,
		"budget":             map[string]any{"daily_hard_usd": 25.5},
		"autonomy":           map[string]any{"enabled": true, "mode": "cron"},
		"acceptCallsFrom":    []any{"a", "b"},
	}
	cur := CurrentValues(bindSchema(), doc)
	if cur["projectId"] != "ibkr" {
		t.Errorf("projectId prefill: %q", cur["projectId"])
	}
	if cur["maxConcurrentTasks"] != "4" {
		t.Errorf("int prefill: %q", cur["maxConcurrentTasks"])
	}
	if cur["budget.daily_hard_usd"] != "25.5" {
		t.Errorf("nested float prefill: %q", cur["budget.daily_hard_usd"])
	}
	if cur["autonomy.enabled"] != "true" {
		t.Errorf("nested bool prefill: %q", cur["autonomy.enabled"])
	}
	if cur["acceptCallsFrom"] != "a\nb" {
		t.Errorf("stringlist prefill: %q", cur["acceptCallsFrom"])
	}
	// A path absent from the doc yields "".
	if cur["autonomy.pollInterval"] != "" {
		t.Errorf("absent path should prefill empty, got %q", cur["autonomy.pollInterval"])
	}
}
