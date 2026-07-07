package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
)

func skillBodyHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// Server-side skill distiller — E1 of the learning loop (LLD 2026-07-07-
// knowledge-skill-learning-loop-design §E1). After a Vornik task
// completes successfully, a bounded LLM pass looks at the task's goal +
// result and, if it finds durable reusable know-how, proposes a DRAFT
// skill (a human still approves before it fires). This covers work
// Vornik itself executes; companion-session know-how is captured
// client-side (E2) because Vornik can't see those transcripts.
//
// Cost control: per-project rate limit + the model returns {skip:true}
// for trivial/one-off tasks, so most autonomous ticks distil nothing.

const (
	skillDistillMaxPerWindow = 3
	skillDistillWindow       = time.Hour
	skillDistillBodyCap      = 65536
)

// distillCandidate is the model's structured verdict.
type distillCandidate struct {
	Skip        bool     `json:"skip"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Body        string   `json:"body"`
	Domain      string   `json:"domain"`
	Roles       []string `json:"roles"`
}

const skillDistillSystemPrompt = `You review a COMPLETED task and decide whether it produced DURABLE, REUSABLE, project-relevant know-how worth saving as a "skill" (an instructional procedure a future agent should follow).

Return STRICT JSON only, no prose. Either:
  {"skip": true}
when the task is trivial, one-off, or produced nothing reusable (most autonomous/feed tasks are skip), OR:
  {"skip": false, "name": "kebab-case-slug", "description": "one line: WHEN to apply this", "body": "# Title\nActionable Markdown steps / checks / anti-patterns", "domain": "software|networking|...", "roles": []}
Set "roles" to swarm role names the skill applies to, or [] for any role. Never include secrets. Prefer skip unless the procedure is genuinely worth reusing.`

// skillDistillLimiter is an in-memory per-project rate limiter.
type skillDistillLimiter struct {
	mu     sync.Mutex
	starts map[string][]time.Time
}

func (l *skillDistillLimiter) allow(project string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.starts == nil {
		l.starts = make(map[string][]time.Time)
	}
	cutoff := now.Add(-skillDistillWindow)
	kept := l.starts[project][:0]
	for _, t := range l.starts[project] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= skillDistillMaxPerWindow {
		l.starts[project] = kept
		return false
	}
	l.starts[project] = append(kept, now)
	return true
}

// maybeDistillSkill runs the distiller for a successfully-completed task.
// Best-effort and nil-safe: any miss (no LLM wired, rate-limited, model
// says skip, parse error, duplicate) simply proposes nothing. Intended
// to run async so it never delays task completion.
func (e *Executor) maybeDistillSkill(ctx context.Context, task *persistence.Task, result string) {
	if e.distillerLLM == nil || e.skillRepo == nil || task == nil {
		return
	}
	if !e.distillLimiter.allow(task.ProjectID, time.Now().UTC()) {
		return
	}
	goal := strings.TrimSpace(taskGoal(task))
	if goal == "" && strings.TrimSpace(result) == "" {
		return
	}
	user := "TASK GOAL:\n" + truncateForDistill(goal, 4000) + "\n\nTASK RESULT:\n" + truncateForDistill(result, 6000)
	resp, err := e.distillerLLM.Complete(ctx, []chat.Message{
		{Role: "system", Content: skillDistillSystemPrompt},
		{Role: "user", Content: user},
	})
	if err != nil || resp == nil || len(resp.Choices) == 0 {
		return
	}
	cand, ok := parseDistillCandidate(resp.Choices[0].Message.Content)
	if !ok || cand.Skip {
		return
	}
	cand.Name = strings.TrimSpace(cand.Name)
	cand.Description = strings.TrimSpace(cand.Description)
	if cand.Name == "" || cand.Description == "" || strings.TrimSpace(cand.Body) == "" {
		return
	}
	if len(cand.Body) > skillDistillBodyCap {
		cand.Body = cand.Body[:skillDistillBodyCap]
	}
	// Dedup: skip if a same-named skill already exists in the project
	// (any scope). Cheap List + name compare avoids near-duplicate drafts.
	existing, _ := e.skillRepo.List(ctx, task.ProjectID, persistence.SkillListFilter{})
	for _, s := range existing {
		if strings.EqualFold(s.Name, cand.Name) {
			return
		}
	}
	sum := skillBodyHash(cand.Body)
	skill := &persistence.Skill{
		ID:           persistence.GenerateID("skill"),
		ProjectID:    task.ProjectID,
		Name:         cand.Name,
		Description:  cand.Description,
		Body:         cand.Body,
		BodySHA256:   sum,
		Domain:       strings.TrimSpace(cand.Domain),
		Roles:        cand.Roles,
		Maturity:     persistence.SkillMaturityDraft,
		Version:      1,
		OriginClient: "vornik-distiller",
		OriginTask:   task.ID,
	}
	if err := e.skillRepo.Create(ctx, skill); err != nil {
		return // conflict / error → propose nothing
	}
	e.logger.Info().Str("skill_id", skill.ID).Str("name", skill.Name).
		Str("task_id", task.ID).Msg("skill distiller: proposed draft from completed task")
}

func parseDistillCandidate(raw string) (distillCandidate, bool) {
	s := strings.TrimSpace(raw)
	// Tolerate a ```json fence.
	if i := strings.Index(s, "{"); i >= 0 {
		if j := strings.LastIndex(s, "}"); j >= i {
			s = s[i : j+1]
		}
	}
	var c distillCandidate
	if err := json.Unmarshal([]byte(s), &c); err != nil {
		return distillCandidate{}, false
	}
	return c, true
}

// taskGoal best-effort extracts the human goal/prompt from a task's
// JSON payload (the field name varies across creation paths).
func taskGoal(task *persistence.Task) string {
	if len(task.Payload) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(task.Payload, &m); err != nil {
		return ""
	}
	for _, k := range []string{"prompt", "goal", "description", "task", "input"} {
		if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func truncateForDistill(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
