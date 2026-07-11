package projectwizard

import (
	"sort"
	"strings"

	"vornik.io/vornik/internal/rolelibrary"
)

// BuildComposerGrounding assembles the tier-3 "parts bin" grounding
// block (design §5.3's three curated inventories, minus the model
// catalog — that one is already rendered by BuildGrounding's
// buildModelsSection) appended to the system prompt ONLY when the
// composer is available for this turn (Wizard.composerTier3Available).
// Concise by design — this grounds the model in what's legal, it isn't
// a novel prompt.
func BuildComposerGrounding(lib map[string]*rolelibrary.RoleArchetype, systemHandlerNames []string) string {
	var b strings.Builder
	b.WriteString("\n\nComposer grounding (tier-3 free-form synthesis):\n\n")
	b.WriteString(buildRoleLibrarySection(lib))
	b.WriteString("\n")
	b.WriteString(buildStepVocabularySection(systemHandlerNames))
	b.WriteString("\n")
	b.WriteString(buildComposerInstructionSection())
	return b.String()
}

// buildRoleLibrarySection renders every archetype's archetypeId,
// displayName, description, and tool allowlist so a composer-enabled
// turn can select valid archetypeIds and knows each one's capability
// ceiling (design §5.3: "composed roles may subset it, never exceed
// it" — the guardrail pass enforces this; the prompt just says so).
// Archetypes render in ArchetypeID order for a stable prompt across
// turns — map iteration order is otherwise randomized per process.
func buildRoleLibrarySection(lib map[string]*rolelibrary.RoleArchetype) string {
	var b strings.Builder
	b.WriteString("Role library — every composed role MUST select one of these archetypeIds; " +
		"its tools must be a SUBSET of the archetype's allowlist below, never a superset:\n")
	if len(lib) == 0 {
		b.WriteString("- (none available)\n")
		return b.String()
	}
	ids := make([]string, 0, len(lib))
	for id := range lib {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		a := lib[id]
		if a == nil {
			continue
		}
		b.WriteString("- `")
		b.WriteString(a.ArchetypeID)
		b.WriteString("` (")
		b.WriteString(a.DisplayName)
		b.WriteString(")")
		if a.Description != "" {
			b.WriteString(": ")
			b.WriteString(a.Description)
		}
		b.WriteString(" — tools: ")
		if len(a.Tools) == 0 {
			b.WriteString("(none)")
		} else {
			b.WriteString(strings.Join(a.Tools, ", "))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// buildStepVocabularySection renders the workflow step kinds a
// composed tier-3 workflow may use (design §5.3): `agent` (runs a
// swarm role), `gate`, `approval`, and the daemon's registered
// `system` step handlers, named exactly — `call_project` is excluded
// in v1. systemHandlerNames is nil-safe: an empty/nil slice still
// documents the four step kinds, just without a concrete handler list
// (a daemon that hasn't wired Wizard.SystemHandlerNames).
func buildStepVocabularySection(systemHandlerNames []string) string {
	var b strings.Builder
	b.WriteString("Workflow step vocabulary — every step is one of:\n")
	b.WriteString("- `agent`: runs one of the swarm's composed roles.\n")
	b.WriteString("- `gate`: deterministic branch/merge control, no LLM call.\n")
	b.WriteString("- `approval`: pauses for a human decision before the next step runs.\n")
	b.WriteString("- `system`: an existing daemon capability handler, named EXACTLY as listed below — never invent one:\n")
	if len(systemHandlerNames) == 0 {
		b.WriteString("  (no system handlers registered on this daemon)\n")
	} else {
		names := append([]string(nil), systemHandlerNames...)
		sort.Strings(names)
		for _, n := range names {
			b.WriteString("  - `")
			b.WriteString(n)
			b.WriteString("`\n")
		}
	}
	b.WriteString("`call_project` is not available to the composer in v1.\n")
	return b.String()
}

// buildComposerInstructionSection is the concise "when to return
// tier:3" instruction (design §5.1/§5.3): free-form synthesis is for a
// from-scratch automation no template covers, not the default path.
func buildComposerInstructionSection() string {
	return "Composer mode: return \"tier\":1 (proposal) or \"tier\":2 (composition) whenever a base " +
		"template + params/addons expresses the operator's intent — prefer that. Return \"tier\":3 " +
		"with a \"bundle\" (project + swarm + 1-2 workflows + a plan) ONLY for a from-scratch " +
		"automation no template covers. In a bundle, select archetypeIds from the role library above, " +
		"keep each role's tools within its archetype's allowlist, and compose workflow steps only from " +
		"the step vocabulary above.\n"
}
