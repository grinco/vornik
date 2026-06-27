package assetschema

// Render helpers — typed predicates the generic html/template renderer
// uses to pick an input widget per field. Kind is a custom string type,
// so html/template's `eq` can't compare it to a string literal; the
// template calls these instead (e.g. {{if .IsEnum}}). They are the only
// kind-dispatch the renderer needs: enum→select, bool→true/false select,
// stringlist→textarea, int/float→number input, everything else→text.

// IsEnum reports whether the field renders as a dropdown.
func (f Field) IsEnum() bool { return f.Kind == KindEnum }

// IsBool reports whether the field renders as a true/false select.
func (f Field) IsBool() bool { return f.Kind == KindBool }

// IsStringList reports whether the field holds a list of scalars.
func (f Field) IsStringList() bool { return f.Kind == KindStringList }

// IsNumeric reports whether the field takes a numeric input.
func (f Field) IsNumeric() bool { return f.Kind == KindInt || f.Kind == KindFloat }

// IsTextarea reports whether the field renders as a multi-line textarea:
// string-lists (one item per line), explicit Multiline prose, or any
// body-backed field (a role's systemPrompt).
func (f Field) IsTextarea() bool {
	return f.Kind == KindStringList || f.Multiline || f.IsBody()
}

// InputType is the HTML <input type="…"> attribute for the field's
// scalar inputs. Numeric kinds get "number"; everything that uses a
// plain <input> gets "text". (Enum/bool render as <select>, stringlist
// as <textarea>; for those the value is cosmetic.)
func (f Field) InputType() string {
	if f.IsNumeric() {
		return "number"
	}
	return "text"
}

// HTMLName is the form field name the renderer emits and BindForm reads
// back — the dotted Path verbatim. HTML name attributes may contain
// dots, and BindForm keys its lookups on Path, so the two must agree.
func (f Field) HTMLName() string { return f.Path }
