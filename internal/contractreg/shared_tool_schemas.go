package contractreg

import (
	"encoding/json"
	"fmt"
	"sort"
)

// VocabularyView is one tool surface's definitions keyed by tool name — the
// value is the OpenAI function definition ({"type":"function","function":{…}})
// or the bare function object; both shapes are accepted.
//
// Two surfaces share names today: the container's declaration
// (internal/agenttools) and the dispatcher's chat tools
// (internal/dispatcher.DispatcherTools). They are separate implementations
// behind separate gates, and their descriptions legitimately differ — the chat
// memory_search explains memory_forget, which the container does not have. What
// must NOT differ is the shape a model learns: a parameter's type and whether
// it is required. A model that used one surface carries those expectations to
// the other (backlog, 2026-09-03). This check is the agreement contract for the
// names both surfaces claim; the surfaces are fed in by the caller so this
// package stays a leaf.
type VocabularyView struct {
	Surface string
	Schemas map[string]json.RawMessage
}

type functionShape struct {
	Name        string
	Description string
	Properties  map[string]string // property name → JSON type ("" when untyped)
	Required    map[string]bool
}

// CheckSharedToolSchemas reports, for every name present in BOTH views, a
// parameter typed differently on the two surfaces, a parameter required on one
// surface and optional on the other while present on both, and a definition
// with no description. A name on only one surface is not a finding — the
// surfaces are allowed to differ in what they offer, not in what a shared
// name means.
func CheckSharedToolSchemas(a, b VocabularyView) []Finding {
	const check = "shared-tool-schema"
	var out []Finding
	names := make([]string, 0)
	for n := range a.Schemas {
		if _, ok := b.Schemas[n]; ok {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		fa, errA := parseFunctionShape(a.Schemas[name])
		fb, errB := parseFunctionShape(b.Schemas[name])
		src := []string{a.Surface, b.Surface}
		if errA != nil || errB != nil {
			out = append(out, Finding{Check: check, Name: name, Sources: src,
				Detail: fmt.Sprintf("definition does not parse as an OpenAI function (%s: %v; %s: %v)", a.Surface, errA, b.Surface, errB)})
			continue
		}
		if fa.Description == "" || fb.Description == "" {
			out = append(out, Finding{Check: check, Name: name, Sources: src,
				Detail: "a shared tool must carry a description on both surfaces — an empty one makes the model guess from the other surface's wording"})
		}
		props := make([]string, 0)
		for p := range fa.Properties {
			if _, ok := fb.Properties[p]; ok {
				props = append(props, p)
			}
		}
		sort.Strings(props)
		for _, p := range props {
			if ta, tb := fa.Properties[p], fb.Properties[p]; ta != tb {
				out = append(out, Finding{Check: check, Name: name, Sources: src,
					Detail: fmt.Sprintf("parameter %q is %q on %s and %q on %s — a model that learned one surface will send the wrong type to the other", p, ta, a.Surface, tb, b.Surface)})
			}
			if ra, rb := fa.Required[p], fb.Required[p]; ra != rb {
				out = append(out, Finding{Check: check, Name: name, Sources: src,
					Detail: fmt.Sprintf("parameter %q is required=%t on %s and required=%t on %s — the stricter surface will refuse calls the other taught", p, ra, a.Surface, rb, b.Surface)})
			}
		}
	}
	return out
}

// SharedToolNames returns the names both views define, sorted. Consumers pin
// this set so a new overlap is a deliberate change rather than a coincidence
// of naming.
func SharedToolNames(a, b VocabularyView) []string {
	var names []string
	for n := range a.Schemas {
		if _, ok := b.Schemas[n]; ok {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names
}

func parseFunctionShape(raw json.RawMessage) (*functionShape, error) {
	var outer struct {
		Type     string          `json:"type"`
		Function json.RawMessage `json:"function"`
	}
	if err := json.Unmarshal(raw, &outer); err != nil {
		return nil, err
	}
	fnRaw := raw
	if len(outer.Function) > 0 {
		fnRaw = outer.Function
	}
	var fn struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Parameters  struct {
			Properties map[string]struct {
				Type string `json:"type"`
			} `json:"properties"`
			Required []string `json:"required"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal(fnRaw, &fn); err != nil {
		return nil, err
	}
	if fn.Name == "" {
		return nil, fmt.Errorf("function has no name")
	}
	shape := &functionShape{Name: fn.Name, Description: fn.Description,
		Properties: map[string]string{}, Required: map[string]bool{}}
	for p, v := range fn.Parameters.Properties {
		shape.Properties[p] = v.Type
	}
	for _, r := range fn.Parameters.Required {
		shape.Required[r] = true
	}
	return shape, nil
}
