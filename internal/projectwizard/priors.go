package projectwizard

import (
	"strings"

	"vornik.io/vornik/internal/templates"
)

// TemplatePrior is a compressed gallery summary the wizard
// embeds in its system prompt so the LLM can suggest the closest
// matching template. One per slug in
// `configs/project-templates/`.
type TemplatePrior struct {
	Slug        string
	DisplayName string
	Description string // first line of the manifest description
	Domain      string
}

// BuildPriors compresses a templates.Catalog into the prior list.
// Manifests with empty descriptions are still emitted (the LLM
// gets the slug + display name only — better than nothing for a
// thin template), so the gallery's structure remains the
// authority on what's available.
func BuildPriors(cat *templates.Catalog) []TemplatePrior {
	if cat == nil {
		return nil
	}
	manifests := cat.List()
	out := make([]TemplatePrior, 0, len(manifests))
	for _, m := range manifests {
		desc := m.Description
		// Compress multi-line descriptions to the first non-empty
		// line; the LLM doesn't need the full paragraph + the
		// system prompt budget is bounded.
		for _, line := range strings.Split(desc, "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed != "" {
				desc = trimmed
				break
			}
		}
		out = append(out, TemplatePrior{
			Slug:        m.Slug,
			DisplayName: m.DisplayName,
			Description: desc,
			Domain:      m.Domain,
		})
	}
	return out
}

// RenderPriors formats the prior list as a compact markdown block
// suitable for splicing into the wizard's system prompt. Returns
// an empty string when priors is empty so the prompt section can
// be cleanly omitted.
func RenderPriors(priors []TemplatePrior) string {
	if len(priors) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Available project templates:\n")
	for _, p := range priors {
		b.WriteString("- `")
		b.WriteString(p.Slug)
		b.WriteString("` — ")
		b.WriteString(p.DisplayName)
		if p.Description != "" {
			b.WriteString(": ")
			b.WriteString(p.Description)
		}
		b.WriteString("\n")
	}
	return b.String()
}
