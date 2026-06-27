package assetschema

import "testing"

func sampleSchema() AssetSchema {
	return AssetSchema{
		Asset: "project",
		Sections: []Section{
			{
				Title: "Core",
				Fields: []Field{
					{Path: "projectId", Label: "Project ID", Kind: KindString, Required: true},
					{Path: "defaultPriority", Label: "Default priority", Kind: KindInt},
				},
			},
			{
				Title:    "Budget",
				Advanced: true,
				Fields: []Field{
					{Path: "budget.daily_cap_usd", Label: "Daily cap", Kind: KindFloat},
					{Path: "budget.on_exceed", Label: "On exceed", Kind: KindEnum, Enum: []string{"block", "warn", "downscale"}},
					{Path: "budget.reserve_on_admit", Label: "Reserve on admit", Kind: KindBool},
				},
			},
		},
	}
}

func TestFieldsAndLookup(t *testing.T) {
	s := sampleSchema()
	if got := len(s.Fields()); got != 5 {
		t.Fatalf("Fields() = %d, want 5", got)
	}
	if _, ok := s.FieldByPath("budget.on_exceed"); !ok {
		t.Error("FieldByPath(budget.on_exceed) should resolve")
	}
	if _, ok := s.FieldByPath("nope"); ok {
		t.Error("FieldByPath(nope) should not resolve")
	}
	paths := s.Paths()
	if len(paths) != 5 || paths[0] != "projectId" {
		t.Errorf("Paths() unexpected: %v", paths)
	}
}

func TestValidateValue(t *testing.T) {
	cases := []struct {
		name  string
		field Field
		raw   string
		ok    bool
	}{
		{"required-empty", Field{Path: "projectId", Kind: KindString, Required: true}, "", false},
		{"required-present", Field{Path: "projectId", Kind: KindString, Required: true}, "p1", true},
		{"optional-empty-ok", Field{Path: "x", Kind: KindInt}, "", true},
		{"int-ok", Field{Path: "x", Kind: KindInt}, "42", true},
		{"int-bad", Field{Path: "x", Kind: KindInt}, "4.5", false},
		{"float-ok", Field{Path: "x", Kind: KindFloat}, "25.0", true},
		{"float-bad", Field{Path: "x", Kind: KindFloat}, "abc", false},
		{"bool-true", Field{Path: "x", Kind: KindBool}, "true", true},
		{"bool-on", Field{Path: "x", Kind: KindBool}, "on", true},
		{"bool-bad", Field{Path: "x", Kind: KindBool}, "maybe", false},
		{"duration-ok", Field{Path: "x", Kind: KindDuration}, "5m", true},
		{"duration-bad", Field{Path: "x", Kind: KindDuration}, "5 minutes", false},
		{"enum-ok", Field{Path: "x", Kind: KindEnum, Enum: []string{"block", "warn"}}, "warn", true},
		{"enum-bad", Field{Path: "x", Kind: KindEnum, Enum: []string{"block", "warn"}}, "blok", false},
		{"string-anything", Field{Path: "x", Kind: KindString}, "whatever", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.field.ValidateValue(c.raw)
			if c.ok && err != nil {
				t.Errorf("want ok, got error: %v", err)
			}
			if !c.ok && err == nil {
				t.Error("want error, got nil")
			}
		})
	}
}

func TestBackingDefaultsFrontmatter(t *testing.T) {
	f := Field{Path: "x", Kind: KindString}
	if f.IsBody() {
		t.Error("default backing must be frontmatter, not body")
	}
	b := Field{Path: "systemPrompt", Kind: KindString, Backing: BackingBody}
	if !b.IsBody() {
		t.Error("explicit body backing must report IsBody()=true")
	}
}
