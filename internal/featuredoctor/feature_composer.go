package featuredoctor

import (
	"context"
	"fmt"
	"strings"

	"vornik.io/vornik/internal/rolelibrary"
	"vornik.io/vornik/internal/version"
)

// composerFeature registers the NL Automation Composer (wizard tier-3
// free-form synthesis) with the feature-doctor. Gated off by default
// until Phase 3's soak completes (design §9). Prereqs mirror the
// design's rollout section: a working chat provider, at least one
// role-library entry that validates, and wizard v2 (the Composition
// structured-build path) present in the running binary — the composer
// builds directly on both.
func composerFeature() Feature {
	return Feature{
		ID:      "composer",
		Title:   "NL Automation Composer",
		Summary: "Free-form tier-3 synthesis for the conversational project wizard: describe an automation in plain language and get a validated, guardrailed project + swarm + workflow bundle.",
		LLDRef:  "https://docs.vornik.io",
		DocRef:  "docs/public/features/composer.md",
		Edition: version.EditionEnterprise,
		Apply:   RestartRequired,
		Gates:   []Gate{{Key: "composer.enabled", EnableTo: true}},
		// Verify answers "is the enabled composer actually working?" — without it,
		// ComputeStatus (status.go) can never return StatusOK and an enabled,
		// healthy composer reports a permanent false [degraded] (2026-07-16). The
		// composer's load-bearing runtime dependency is a loadable role library to
		// ground tier-3 synthesis; re-confirm it (the prereq screens at config
		// time, this confirms the enabled feature can still ground a composition).
		// Scope note (review-20260717-21f4 #1): Verify covers ONLY the role library
		// on purpose — ComputeStatus (status.go) already re-evaluates every Prereq
		// (allPrereqOK && verify.OK), so the chat-provider config prereq still gates
		// [degraded]; and a chat provider that is configured-but-DOWN at runtime is
		// surfaced by the chat model-health circuit breaker, not this doctor Verify.
		Verify: func(_ context.Context, d Deps) PrereqResult {
			r := checkRoleLibraryPrereq(d)
			if !r.OK {
				return r
			}
			return PrereqResult{OK: true, Detail: "composer ready — " + r.Detail}
		},
		Prereqs: []Prereq{
			{
				Name: "chat provider configured",
				Check: func(_ context.Context, d Deps) PrereqResult {
					if !chatProviderConfigured(d) {
						return PrereqResult{OK: false, Fixable: false,
							Detail:      "no chat provider configured",
							Remediation: "set chat.provider (+ chat.endpoint/chat.model for the single-provider HTTP path, or the router.* sub-providers) before enabling the composer — every tier-3 turn is an LLM call"}
					}
					return PrereqResult{OK: true, Detail: "chat provider configured"}
				},
			},
			{
				Name: "role library has at least one valid archetype",
				Check: func(_ context.Context, d Deps) PrereqResult {
					return checkRoleLibraryPrereq(d)
				},
			},
			{
				Name: "wizard v2 present",
				Check: func(_ context.Context, _ Deps) PrereqResult {
					// Wizard v2 (the structured Composition build —
					// template + params + addons) is compiled into
					// every enterprise binary unconditionally; the
					// composer's tier-3 path is a third tier of the
					// SAME internal/projectwizard engine (design §5.1),
					// not a separately-toggled feature, so this prereq
					// simply confirms the binary is the one that ships
					// it (always true once this feature-doctor entry
					// itself exists to be diagnosed — a version this
					// old wouldn't have the composer.enabled gate to
					// flip in the first place).
					return PrereqResult{OK: true, Detail: "wizard v2 (Composition build) present in this binary"}
				},
			},
		},
	}
}

// chatProviderConfigured mirrors config.ChatConfig.ProviderConfigured
// via the generic dotted-path ConfigReader (featuredoctor can't import
// internal/config's concrete ChatConfig type without creating an
// import cycle risk with the rest of the feature set's Deps-only
// contract, so the family-aware logic is re-derived here against the
// same three leaf keys).
func chatProviderConfigured(d Deps) bool {
	if d.Config == nil {
		return false
	}
	provider, _ := d.Config.GateValue("chat.provider")
	p, _ := provider.(string)
	switch strings.TrimSpace(p) {
	case "router", "claude-cli", "codex-cli":
		return true
	default:
		endpoint, _ := d.Config.GateValue("chat.endpoint")
		model, _ := d.Config.GateValue("chat.model")
		e, _ := endpoint.(string)
		m, _ := model.(string)
		return strings.TrimSpace(e) != "" && strings.TrimSpace(m) != ""
	}
}

// checkRoleLibraryPrereq loads the role library from Deps.RoleLibraryDir
// and reports OK when at least one archetype has zero
// rolelibrary.SeverityError findings (a role-library that's entirely
// broken cannot ground any tier-3 composition — see design §5.3).
// systemHandlers is passed as nil here (a config-time prereq, not the
// live daemon's registered handler set); a role's system-handler-only
// tool reference is thus screened purely against agenttools/mcp__ at
// this stage — the stricter check with the live handler set is the
// dedicated role-library doctor report (internal/api/doctor_role_library.go).
func checkRoleLibraryPrereq(d Deps) PrereqResult {
	if d.RoleLibraryDir == "" {
		return PrereqResult{OK: false, Fixable: false, Detail: "configs directory not wired"}
	}
	archetypes, err := rolelibrary.Load(d.RoleLibraryDir)
	if err != nil {
		return PrereqResult{OK: false, Detail: "role-library load failed: " + err.Error()}
	}
	if len(archetypes) == 0 {
		// Last-hop drift message (design §5, 2026-07-16): this prereq reads the
		// DEPLOYED tree, so an empty role-library is almost always a deploy gap
		// (repo has configs/role-library/*.md, deployed tree doesn't) — the exact
		// 2026-07-16 incident. Name the subtree + manifest class + the check so the
		// failure points straight at the fix instead of "seed some files".
		return PrereqResult{OK: false, Fixable: false,
			Detail:      fmt.Sprintf("no role-library archetypes in the deployed tree (%s/role-library)", d.RoleLibraryDir),
			Remediation: "role-library is likely not deployed: run `make config-diff` (it's a CONFIG_DEPLOYABLE_DIRS subtree) and sync configs/role-library/*.md into the deployed tree, then reload — before enabling the composer"}
	}
	findings := rolelibrary.CheckLibrary(archetypes, nil)
	erroredIDs := map[string]bool{}
	for _, f := range findings {
		if f.Severity == rolelibrary.SeverityError {
			erroredIDs[f.ArchetypeID] = true
		}
	}
	passing := 0
	for _, a := range archetypes {
		if a != nil && !erroredIDs[a.ArchetypeID] {
			passing++
		}
	}
	if passing == 0 {
		return PrereqResult{OK: false,
			Detail:      fmt.Sprintf("all %d role-library archetypes have validation errors", len(archetypes)),
			Remediation: "fix the role-library doctor findings (internal/api/doctor_role_library.go) before enabling the composer"}
	}
	return PrereqResult{OK: true, Detail: fmt.Sprintf("%d/%d archetypes valid", passing, len(archetypes))}
}
