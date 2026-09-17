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
	// Dedup, first pass: an exact same-named skill in this project (any
	// scope). Cheap, and it catches the trivial case without an embedding.
	//
	// It catches ONLY that. This comment used to claim it "avoids
	// near-duplicate drafts", which is precisely what a case-insensitive
	// equality cannot do: `prague-restaurant-feed-refresh` and
	// `prague-restaurants-feed-refresh` differ by one character, and the
	// second was proposed, approved, and never fired once while the first
	// accumulated 23 firings.
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
	// Dedup, second pass: the SEMANTIC gate (LLD §12.2). It has existed since
	// the knowledge-skill store shipped and was wired into exactly one
	// entrypoint — the companion `skill_propose` MCP tool — while this path
	// produced 105 of the reference deployment's 128 skills, 69 of which never
	// fired. A gate on one door out of several is the shape that let the
	// config assistant's mayAutoApply through on a different surface.
	//
	// SILENT SKIP, not a block-and-ask: this path is unattended, so there is
	// nobody to answer "supersedes or confirm_distinct?". Skipping matches what
	// the exact-name check above already does; the log line and the metric are
	// what make the skip rate visible, so "the gate is working" stays
	// distinguishable from "the distiller stopped proposing".
	if e.dupeChecker != nil {
		matches, derr := e.dupeChecker.NearDuplicateSkills(ctx, skill)
		switch {
		case derr != nil:
			// An embedder outage must not silence the distiller. §12.2 makes
			// the same choice on the MCP path; inheriting it beats inventing a
			// stricter rule here.
			e.logger.Warn().Err(derr).Str("name", skill.Name).
				Msg("skill distiller: near-duplicate check failed; proposing anyway")
		case len(matches) > 0:
			e.logger.Info().
				Str("name", skill.Name).
				Str("nearest", matches[0].Name).
				Float64("score", matches[0].Score).
				Str("reason", matches[0].Reason).
				Int("matches", len(matches)).
				Str("task_id", task.ID).
				Msg("skill distiller: draft skipped as a near-duplicate of an existing skill")
			if e.metrics != nil {
				e.metrics.RecordSkillDistillSkipped(matches[0].Reason)
			}
			return
		}
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

// SkillDupeMatch is one near-duplicate the gate found. Declared here rather
// than reused from internal/api because api imports this package — the
// dependency only runs one way, so the container adapts across the seam.
type SkillDupeMatch struct {
	Name   string
	Score  float64
	Reason string
}

// SkillDupeChecker scores a proposed skill against the existing catalogue.
// Optional: a nil checker leaves the exact-name dedup above as the only gate,
// which is the pre-2026-09-16 behaviour.
type SkillDupeChecker interface {
	NearDuplicateSkills(ctx context.Context, candidate *persistence.Skill) ([]SkillDupeMatch, error)
}
