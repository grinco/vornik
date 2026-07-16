package rolelibrary

import (
	"fmt"
	"sort"
	"strings"
	"text/template"

	"vornik.io/vornik/internal/agenttools"
)

// Finding severities. Errors mean the archetype is broken and MUST be
// fixed before it can ground a composition (fail loudly, per §5.3). A
// Flag is not a failure: it is a loud security callout — an archetype
// whose tool allowlist is broad enough that a reviewer should look at
// it, because the `tools` list is the outer boundary of every future
// composed automation's capabilities (design §5.3 / §6).
const (
	SeverityError = "error"
	SeverityFlag  = "flag"
)

// Finding is one result of the role-library doctor check for one
// archetype. Severity is SeverityError (validation failure) or
// SeverityFlag (broad-allowlist security callout).
type Finding struct {
	ArchetypeID string
	Severity    string
	Message     string
}

func (f Finding) String() string {
	id := f.ArchetypeID
	if id == "" {
		id = "<unknown>"
	}
	return fmt.Sprintf("[%s] %s: %s", f.Severity, id, f.Message)
}

// broadToolThreshold is the allowlist size above which an archetype is
// flagged for review. Curated archetypes sit comfortably below this;
// crossing it is the signal that someone widened the parts bin.
const broadToolThreshold = 8

// validModelTiers is the closed set of modelTier values.
var validModelTiers = map[string]bool{
	ModelTierTrivial:  true,
	ModelTierStandard: true,
	ModelTierComplex:  true,
}

// CheckLibrary validates every archetype and returns all findings,
// sorted (errors before flags, then by archetype id). An empty result
// means the library is clean. systemHandlers is the set of system-step
// handler names (executor.SystemHandlerRegistry.Names()) that a tool
// entry may also legitimately name; pass nil when unavailable (only
// built-in + MCP names then validate).
//
// Per-archetype rules (design §5.3):
//   - archetypeId non-empty (else findings can't be attributed).
//   - every tools entry is a known built-in (agenttools), a system
//     handler, or an `mcp__…` reference (dynamic, accepted — the
//     compose path checks the server exists).
//   - requiredOutputKeys non-empty, and each key a non-empty string.
//   - modelTier ∈ {trivial,standard,complex}.
//   - runtime.maxTokens > 0; cpu/memory non-empty.
//   - the prompt body parses as a Go text/template whose ONLY splice
//     points are the declared promptParams (an undeclared {{.x}} is an
//     error — the composer would splice a value it never gathered).
//
// Additionally, a broad tools list (a wildcard entry, run_shell, or
// more than broadToolThreshold entries) yields a SeverityFlag finding
// — not a failure, a loud "review this" marker.
func CheckLibrary(archetypes []*RoleArchetype, systemHandlers []string) []Finding {
	handlerSet := make(map[string]bool, len(systemHandlers))
	for _, h := range systemHandlers {
		handlerSet[h] = true
	}

	var findings []Finding
	// Duplicate archetypeId detection — the composer selects by ID, so two
	// archetypes sharing an ID make the selection ambiguous (review-20260716-7e65).
	idFiles := map[string][]string{}
	for _, a := range archetypes {
		if a == nil {
			continue
		}
		if id := strings.TrimSpace(a.ArchetypeID); id != "" {
			idFiles[id] = append(idFiles[id], a.SourceFile)
		}
		findings = append(findings, checkArchetype(a, handlerSet)...)
	}
	for id, files := range idFiles {
		if len(files) > 1 {
			findings = append(findings, Finding{
				ArchetypeID: id,
				Severity:    SeverityError,
				Message:     fmt.Sprintf("duplicate archetypeId %q defined in %d files: %s", id, len(files), strings.Join(files, ", ")),
			})
		}
	}

	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].Severity != findings[j].Severity {
			// errors first
			return findings[i].Severity == SeverityError
		}
		if findings[i].ArchetypeID != findings[j].ArchetypeID {
			return findings[i].ArchetypeID < findings[j].ArchetypeID
		}
		return findings[i].Message < findings[j].Message
	})
	return findings
}

func checkArchetype(a *RoleArchetype, handlerSet map[string]bool) []Finding {
	id := a.ArchetypeID
	add := func(sev, format string, args ...any) Finding {
		return Finding{ArchetypeID: id, Severity: sev, Message: fmt.Sprintf(format, args...)}
	}
	var out []Finding

	if strings.TrimSpace(id) == "" {
		out = append(out, Finding{ArchetypeID: a.SourceFile, Severity: SeverityError, Message: "archetypeId is empty"})
	}

	// Prompt body is the role's system prompt — an empty/whitespace body
	// composes a role with no instructions (review-20260716-7e65).
	if strings.TrimSpace(a.Prompt) == "" {
		out = append(out, add(SeverityError, "prompt body is empty (the Markdown body IS the role's system prompt)"))
	}

	// Tool allowlist membership.
	for _, tool := range a.Tools {
		switch {
		case agenttools.IsBuiltin(tool):
		case handlerSet[tool]:
		case agenttools.IsMCPTool(tool):
		default:
			out = append(out, add(SeverityError, "tool %q is not a known built-in tool, system handler, or mcp__ reference", tool))
		}
	}

	// requiredOutputKeys.
	if len(a.RequiredOutputKeys) == 0 {
		out = append(out, add(SeverityError, "requiredOutputKeys is empty (a role with no required output contributes nothing verifiable)"))
	}
	for _, k := range a.RequiredOutputKeys {
		if strings.TrimSpace(k) == "" {
			out = append(out, add(SeverityError, "requiredOutputKeys contains an empty string"))
		}
	}

	// modelTier.
	if !validModelTiers[a.ModelTier] {
		out = append(out, add(SeverityError, "modelTier %q is not one of trivial|standard|complex", a.ModelTier))
	}

	// runtime.
	if a.Runtime.MaxTokens <= 0 {
		out = append(out, add(SeverityError, "runtime.maxTokens must be > 0 (got %d)", a.Runtime.MaxTokens))
	}
	if strings.TrimSpace(a.Runtime.CPU) == "" {
		out = append(out, add(SeverityError, "runtime.cpu is empty"))
	}
	if strings.TrimSpace(a.Runtime.Memory) == "" {
		out = append(out, add(SeverityError, "runtime.memory is empty"))
	}

	// Prompt template: parses, and uses only declared promptParams.
	out = append(out, checkPromptTemplate(a)...)

	// Broad-allowlist security flag (loud, not fatal).
	if msg := broadToolsReason(a.Tools); msg != "" {
		out = append(out, add(SeverityFlag, "%s — allowlist expansion is security-review-worthy (§5.3)", msg))
	}

	return out
}

// checkPromptTemplate parses the prompt as a Go text/template and
// verifies every `{{.field}}` splice point is a declared promptParam.
func checkPromptTemplate(a *RoleArchetype) []Finding {
	id := a.ArchetypeID
	tmpl, err := template.New(id).Option("missingkey=error").Parse(a.Prompt)
	if err != nil {
		return []Finding{{ArchetypeID: id, Severity: SeverityError, Message: fmt.Sprintf("prompt body does not parse as a template: %v", err)}}
	}
	declared := make(map[string]bool, len(a.PromptParams))
	for _, p := range a.PromptParams {
		declared[p] = true
	}
	var out []Finding
	for _, name := range templateFields(tmpl) {
		if !declared[name] {
			out = append(out, Finding{
				ArchetypeID: id,
				Severity:    SeverityError,
				Message:     fmt.Sprintf("prompt uses splice point {{.%s}} but %q is not in promptParams", name, name),
			})
		}
	}
	return out
}

func broadToolsReason(tools []string) string {
	for _, t := range tools {
		if t == "*" || strings.HasSuffix(t, "__*") || strings.HasSuffix(t, "*") {
			return fmt.Sprintf("tools list contains a wildcard entry %q", t)
		}
	}
	for _, t := range tools {
		if t == "run_shell" {
			return "tools list grants run_shell (arbitrary command execution)"
		}
	}
	if len(tools) > broadToolThreshold {
		return fmt.Sprintf("tools list is large (%d entries; review threshold %d)", len(tools), broadToolThreshold)
	}
	return ""
}
