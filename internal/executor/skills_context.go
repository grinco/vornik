package executor

import (
	"context"
	"fmt"
	"strings"

	"vornik.io/vornik/internal/persistence"
)

// Knowledge-skill injection (LLD 2026-07-07-knowledge-skill-store-design).
//
// At task start the executor pre-loads the project's APPROVED
// (active/trusted) knowledge skills relevant to the step's role and
// folds them into the role's system prompt as trusted directive
// guidance — the same pre-load pattern as CanonicalContext, so the
// agent doesn't burn tool calls rediscovering procedures it should
// already know.
//
// Scope note (slice A+): skills are selected by project + role +
// maturity, project-WIDE across repo scopes. Per-task repo-scope
// resolution isn't wired yet (the Task carries no repo_scope), so
// scope-precise injection — matching the task's repo the way
// skill_search does — is a follow-up. Store/search scope isolation is
// unaffected; only injection breadth is project-wide for now.

// skillInjectionLimit caps how many skills are injected so a large
// store can't blow the system-prompt budget. Tunable later.
const skillInjectionLimit = 5

// SkillBlock is one injected skill, rendered into the role prompt.
type SkillBlock struct {
	Name        string
	Description string
	Body        string
}

// resolveSkills loads the approved skills relevant to role for the
// project, stamps a "fired" signal on each, and records the
// (execution, skill) association so a successful task can later credit
// "worked". Best-effort: a nil store or any query error degrades to no
// skills (the agent still works, it just doesn't get learned skills).
func (e *Executor) resolveSkills(ctx context.Context, projectID, role, executionID string) []SkillBlock {
	if e.skillRepo == nil || projectID == "" {
		return nil
	}
	skills, err := e.skillRepo.List(ctx, projectID, persistence.SkillListFilter{
		Maturities: []string{persistence.SkillMaturityActive, persistence.SkillMaturityTrusted},
		Role:       role,
		Limit:      skillInjectionLimit,
	})
	if err != nil || len(skills) == 0 {
		return nil
	}
	out := make([]SkillBlock, 0, len(skills))
	for _, s := range skills {
		// Usage telemetry (learning-loop §D.1). Best-effort, nil-safe;
		// a failure here must never block injection.
		_ = e.skillRepo.RecordFeedback(ctx, s.ID, persistence.SkillSignalFired)
		if e.execSkillRepo != nil && executionID != "" {
			_ = e.execSkillRepo.Record(ctx, executionID, s.ID)
		}
		out = append(out, SkillBlock{Name: s.Name, Description: s.Description, Body: s.Body})
	}
	return out
}

// creditSkillsWorked stamps a "worked" maturity signal on every skill
// injected into any execution of a task that completed successfully
// (learning-loop §D.1). Best-effort; nil-safe. Deduped so a skill
// injected into several of the task's step-executions is credited once.
func (e *Executor) creditSkillsWorked(ctx context.Context, taskID string) {
	if e.execSkillRepo == nil || e.skillRepo == nil || e.execRepo == nil || taskID == "" {
		return
	}
	execs, err := e.execRepo.List(ctx, persistence.ExecutionFilter{TaskID: &taskID})
	if err != nil {
		return
	}
	credited := make(map[string]bool)
	for _, ex := range execs {
		ids, lerr := e.execSkillRepo.ListByExecution(ctx, ex.ID)
		if lerr != nil {
			continue
		}
		for _, id := range ids {
			if credited[id] {
				continue
			}
			credited[id] = true
			_ = e.skillRepo.RecordFeedback(ctx, id, persistence.SkillSignalWorked)
		}
	}
}

// renderSkillsBlock formats injected skills as a system-prompt
// directive section. Returns "" for no skills so callers can append
// unconditionally.
func renderSkillsBlock(skills []SkillBlock) string {
	if len(skills) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## LEARNED SKILLS\n\n")
	b.WriteString("Operator-approved procedures for this project. Apply them when relevant.\n")
	for _, s := range skills {
		fmt.Fprintf(&b, "\n### %s\n", s.Name)
		if s.Description != "" {
			fmt.Fprintf(&b, "_%s_\n\n", s.Description)
		}
		b.WriteString(strings.TrimSpace(s.Body))
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// composeSystemPromptWithSkills appends the learned-skills directive
// block to a system prompt. No-op when there are no skills.
func composeSystemPromptWithSkills(systemPrompt string, skills []SkillBlock) string {
	block := renderSkillsBlock(skills)
	if block == "" {
		return systemPrompt
	}
	if systemPrompt == "" {
		return block
	}
	return systemPrompt + "\n\n" + block
}
