package executor

import (
	"context"
	"fmt"
	"strings"

	"vornik.io/vornik/internal/persistence"
)

// Knowledge-skill injection v2 — progressive disclosure (LLD
// 2026-07-12-skill-progressive-disclosure-design).
//
// v1 injected the FULL BODIES of the 5 most-recently-updated matching
// skills into every step's system prompt: no task relevance (a 9.6KB
// person-dossier skill rode along on world-news feed refreshes), cap
// starvation past 5 skills, and `fired` telemetry that meant
// "injected", not "used".
//
// v2 injects a compact INDEX (name + when-to-use, one line per skill)
// of ALL eligible skills and instructs the agent to pull full bodies
// on demand via the skill_fetch tool (GET
// /projects/{id}/skills/fetch). The agent self-selects with full task
// context; `fired` + the (execution, skill) association are recorded
// at fetch time by the API handler, so the learning loop's
// promote/decay worker finally sees honest use.
//
// Scope note (unchanged from v1): skills are selected by project +
// role + maturity, project-WIDE across repo scopes. Per-task
// repo-scope resolution is still a follow-up; with index-only
// injection its absence costs ~a line of prompt, not kilobytes.

// skillIndexLimit caps the index so a runaway catalogue can't blow the
// system-prompt budget — at ~120 bytes/line this bounds the block to
// ~6KB. Truncation is logged so the operator knows to retire skills.
const skillIndexLimit = 50

// SkillIndexEntry is one line of the injected skill index.
type SkillIndexEntry struct {
	Name        string
	Description string
}

// resolveSkillIndex loads the approved skills relevant to role for the
// project and returns their index entries (no bodies). No usage
// telemetry here — `fired` is stamped when the agent actually fetches
// the body. Best-effort: a nil store or any query error degrades to no
// index (the agent still works, it just doesn't see learned skills).
func (e *Executor) resolveSkillIndex(ctx context.Context, projectID, role string) []SkillIndexEntry {
	if e.skillRepo == nil || projectID == "" {
		return nil
	}
	skills, err := e.skillRepo.List(ctx, projectID, persistence.SkillListFilter{
		Maturities: []string{persistence.SkillMaturityActive, persistence.SkillMaturityTrusted},
		Role:       role,
		// Index the project's own skills PLUS any operator-wide global
		// skill (LLD 2026-07-07-cross-project-global-skills-design), so a
		// skill authored once reaches every project's roles.
		IncludeGlobal: true,
		Limit:         skillIndexLimit + 1, // +1 to detect truncation
	})
	if err != nil || len(skills) == 0 {
		return nil
	}
	if len(skills) > skillIndexLimit {
		skills = skills[:skillIndexLimit]
		e.logger.Warn().
			Str("project_id", projectID).
			Str("role", role).
			Int("limit", skillIndexLimit).
			Msg("skill index truncated — consider retiring stale skills")
	}
	out := make([]SkillIndexEntry, 0, len(skills))
	seen := make(map[string]bool, len(skills))
	for _, s := range skills {
		// Dedup by skill id: a global skill whose home IS this project
		// matches the project_id OR is_global widening as a single row,
		// but guard anyway.
		if seen[s.ID] {
			continue
		}
		seen[s.ID] = true
		out = append(out, SkillIndexEntry{Name: s.Name, Description: s.Description})
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

// renderSkillIndexBlock formats the skill index as a system-prompt
// directive section. One line per skill; bodies are fetched on demand.
// Returns "" for no skills so callers can append unconditionally.
func renderSkillIndexBlock(skills []SkillIndexEntry) string {
	if len(skills) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## LEARNED SKILLS\n\n")
	b.WriteString("Operator-approved procedures for this project. This is an INDEX — ")
	b.WriteString("before doing work that one of these covers, call the `skill_fetch` ")
	b.WriteString("tool with the skill's name to get its full instructions, then follow them. ")
	b.WriteString("Skip skills irrelevant to the current task.\n\n")
	for _, s := range skills {
		desc := strings.TrimSpace(s.Description)
		if len(desc) > 200 {
			desc = desc[:200] + "…"
		}
		if desc == "" {
			fmt.Fprintf(&b, "- `%s`\n", s.Name)
			continue
		}
		fmt.Fprintf(&b, "- `%s` — %s\n", s.Name, desc)
	}
	return strings.TrimRight(b.String(), "\n")
}

// composeSystemPromptWithSkillIndex appends the learned-skills index
// block to a system prompt. No-op when there are no skills.
func composeSystemPromptWithSkillIndex(systemPrompt string, skills []SkillIndexEntry) string {
	block := renderSkillIndexBlock(skills)
	if block == "" {
		return systemPrompt
	}
	if systemPrompt == "" {
		return block
	}
	return systemPrompt + "\n\n" + block
}
