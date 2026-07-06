package templates

import (
	"fmt"
	"regexp"
	"strings"
)

// Recognised optionsFrom sources (spec §1a). The manifest loader
// refuses anything else so a typo fails at daemon startup, not at
// first form render.
const (
	OptionsSourceMCPRegistry = "mcp_registry"
	OptionsSourceModels      = "models"
)

// KnownOptionsSource reports whether s is a recognised dynamic
// option source.
func KnownOptionsSource(s string) bool {
	return s == OptionsSourceMCPRegistry || s == OptionsSourceModels
}

// OptionsResolver resolves an optionsFrom source name to the live
// option set. Task 2 wires real implementations; nil means dynamic
// sources are not resolvable in this context (offline CLI) and
// optionsFrom values pass through as free strings.
type OptionsResolver interface {
	ResolveOptions(source string) ([]string, error)
}

// NeedsMultiValue reports whether this manifest declares any
// parameter that the legacy scalar path cannot express: list,
// multiselect, or a dynamic option source. Callers branch on this
// so scalar-only manifests keep the byte-identical legacy path
// (spec back-compat contract item 1).
func (m Manifest) NeedsMultiValue() bool {
	for _, p := range m.Parameters {
		switch strings.ToLower(p.Type) {
		case "list", "multiselect":
			return true
		}
		if p.OptionsFrom != "" {
			return true
		}
	}
	return false
}

// ValidateParamsMulti is the multi-value sibling of ValidateParams.
// Scalar parameters take the LAST supplied value (matches HTML-form
// and repeated-flag override intuition); list/multiselect keep every
// non-empty trimmed value in order. Returns a text/template context:
// string for scalars, []string for list types.
func ValidateParamsMulti(m Manifest, params map[string][]string, resolver OptionsResolver) (map[string]any, error) {
	out := make(map[string]any, len(m.Parameters))
	declared := make(map[string]struct{}, len(m.Parameters))
	for _, p := range m.Parameters {
		declared[p.Name] = struct{}{}
		values := trimNonEmpty(params[p.Name])
		switch strings.ToLower(p.Type) {
		case "list", "multiselect":
			if len(values) == 0 {
				if p.Required {
					return nil, &ValidationError{Field: p.Name, Message: "required (at least one value)"}
				}
				out[p.Name] = []string{}
				continue
			}
			if err := checkListValues(p, values, resolver); err != nil {
				return nil, err
			}
			out[p.Name] = values
		default:
			raw := ""
			if len(values) > 0 {
				raw = values[len(values)-1]
			}
			cleaned, err := validateScalar(p, raw, resolver)
			if err != nil {
				return nil, err
			}
			out[p.Name] = cleaned
		}
	}
	for key := range params {
		if _, ok := declared[key]; !ok {
			return nil, &ValidationError{Field: key, Message: "unknown parameter for this template"}
		}
	}
	return out, nil
}

// validateScalar mirrors ValidateParams' per-parameter logic for one
// value, plus optionsFrom resolution (Task 2). Kept separate from
// ValidateParams so the legacy scalar path stays physically
// untouched (spec back-compat contract).
func validateScalar(p Parameter, raw string, resolver OptionsResolver) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		if p.Required && p.Default == "" {
			return "", &ValidationError{Field: p.Name, Message: "required"}
		}
		raw = p.Default
	}
	switch strings.ToLower(p.Type) {
	case "enum":
		opts, err := effectiveOptions(p, resolver)
		if err != nil {
			return "", err
		}
		if opts == nil { // dynamic source, no resolver: free string
			return raw, nil
		}
		for _, opt := range opts {
			if raw == opt {
				return raw, nil
			}
		}
		return "", staleOptionError(p, raw, opts)
	case "bool":
		lower := strings.ToLower(raw)
		if lower != "true" && lower != "false" && lower != "" {
			return "", &ValidationError{Field: p.Name, Message: "must be 'true' or 'false'"}
		}
		return raw, nil
	default: // string
		if p.Pattern != "" {
			re, rerr := regexp.Compile("^(?:" + p.Pattern + ")$")
			if rerr != nil {
				return "", &ValidationError{Field: p.Name, Message: fmt.Sprintf("manifest pattern is not a valid regex: %v", rerr)}
			}
			if !re.MatchString(raw) {
				return "", &ValidationError{Field: p.Name, Message: fmt.Sprintf("must match pattern %s", p.Pattern)}
			}
		}
		return raw, nil
	}
}

func checkListValues(p Parameter, values []string, resolver OptionsResolver) error {
	if strings.EqualFold(p.Type, "multiselect") {
		opts, err := effectiveOptions(p, resolver)
		if err != nil {
			return err
		}
		if opts != nil {
			for _, v := range values {
				if !containsString(opts, v) {
					return staleOptionError(p, v, opts)
				}
			}
		}
		return nil
	}
	// list: Pattern applies per element.
	if p.Pattern == "" {
		return nil
	}
	re, rerr := regexp.Compile("^(?:" + p.Pattern + ")$")
	if rerr != nil {
		return &ValidationError{Field: p.Name, Message: fmt.Sprintf("manifest pattern is not a valid regex: %v", rerr)}
	}
	for _, v := range values {
		if !re.MatchString(v) {
			return &ValidationError{Field: p.Name, Message: fmt.Sprintf("value %q must match pattern %s", v, p.Pattern)}
		}
	}
	return nil
}

// effectiveOptions returns the closed option set for an enum or
// multiselect: static Options when declared, else the resolved
// dynamic source. Returns (nil, nil) when the parameter is dynamic
// but no resolver is available — callers treat that as "free
// string" (documented offline-CLI behaviour, spec §1a).
func effectiveOptions(p Parameter, resolver OptionsResolver) ([]string, error) {
	if len(p.Options) > 0 {
		return p.Options, nil
	}
	if p.OptionsFrom == "" {
		if strings.EqualFold(p.Type, "enum") {
			return nil, &ValidationError{Field: p.Name, Message: "enum parameter has no options declared in manifest"}
		}
		return nil, &ValidationError{Field: p.Name, Message: "multiselect parameter has no options declared in manifest"}
	}
	if resolver == nil {
		return nil, nil
	}
	opts, err := resolver.ResolveOptions(p.OptionsFrom)
	if err != nil {
		return nil, &ValidationError{Field: p.Name, Message: fmt.Sprintf("could not resolve options from %s: %v", p.OptionsFrom, err)}
	}
	return opts, nil
}

// staleOptionError is the spec-mandated targeted message for a
// value that is no longer (or never was) in the option set.
func staleOptionError(p Parameter, value string, opts []string) error {
	source := p.OptionsFrom
	if source == "" {
		return &ValidationError{Field: p.Name, Message: fmt.Sprintf("must be one of %v", opts)}
	}
	return &ValidationError{Field: p.Name, Message: fmt.Sprintf(
		"selected value %q is no longer available from %s — refresh the form to see current options", value, source)}
}

func trimNonEmpty(in []string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

func containsString(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// MaterialiseFilesMulti is the multi-value sibling of
// MaterialiseFiles. Identical contract (validate-first, no
// filesystem writes, target path safety) with a map[string]any
// render context so templates can {{range}} list parameters.
func (c *Catalog) MaterialiseFilesMulti(m Manifest, params map[string][]string, resolver OptionsResolver) (map[string]string, error) {
	cleaned, err := ValidateParamsMulti(m, params, resolver)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(m.Files))
	for _, fm := range m.Files {
		target, terr := renderInlineAny(fm.Target, cleaned)
		if terr != nil {
			return nil, fmt.Errorf("render target %q: %w", fm.Target, terr)
		}
		if err := validateRelativeTarget(target); err != nil {
			return nil, fmt.Errorf("rendered target %q refused: %w", target, err)
		}
		body, berr := c.renderSourceAny(m.Slug, fm.Source, cleaned)
		if berr != nil {
			return nil, berr
		}
		out[target] = body
	}
	return out, nil
}
