package assetschema

import "testing"

// The generic renderer branches on a field's kind to pick an input
// widget. Because Kind is a custom string type, html/template's `eq`
// can't compare it to a string literal, so the schema exposes typed
// predicates the template calls instead. These tests pin them.

func TestFieldInputPredicates(t *testing.T) {
	cases := []struct {
		kind       Kind
		isEnum     bool
		isBool     bool
		isList     bool
		isNumeric  bool
		isTextarea bool
		inputType  string
	}{
		{kind: KindString, inputType: "text"},
		{kind: KindInt, isNumeric: true, inputType: "number"},
		{kind: KindFloat, isNumeric: true, inputType: "number"},
		{kind: KindBool, isBool: true, inputType: "text"},
		{kind: KindEnum, isEnum: true, inputType: "text"},
		{kind: KindDuration, inputType: "text"},
		{kind: KindStringList, isList: true, isTextarea: true, inputType: "text"},
	}
	for _, c := range cases {
		f := Field{Kind: c.kind}
		if got := f.IsEnum(); got != c.isEnum {
			t.Errorf("kind %q IsEnum = %v, want %v", c.kind, got, c.isEnum)
		}
		if got := f.IsBool(); got != c.isBool {
			t.Errorf("kind %q IsBool = %v, want %v", c.kind, got, c.isBool)
		}
		if got := f.IsStringList(); got != c.isList {
			t.Errorf("kind %q IsStringList = %v, want %v", c.kind, got, c.isList)
		}
		if got := f.IsNumeric(); got != c.isNumeric {
			t.Errorf("kind %q IsNumeric = %v, want %v", c.kind, got, c.isNumeric)
		}
		if got := f.IsTextarea(); got != c.isTextarea {
			t.Errorf("kind %q IsTextarea = %v, want %v", c.kind, got, c.isTextarea)
		}
		if got := f.InputType(); got != c.inputType {
			t.Errorf("kind %q InputType = %q, want %q", c.kind, got, c.inputType)
		}
	}
}

// HTMLName is the form field name the renderer emits and the binder
// reads back. It must equal the dotted Path verbatim — BindForm keys
// its lookups on Path, so any transformation here would break the
// render→bind round-trip.
func TestFieldHTMLName(t *testing.T) {
	f := Field{Path: "budget.daily_soft_usd"}
	if got := f.HTMLName(); got != "budget.daily_soft_usd" {
		t.Errorf("HTMLName = %q, want the dotted path", got)
	}
}
