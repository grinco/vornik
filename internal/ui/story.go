package ui

import (
	"context"
	"net/http"
	"sort"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/auth"
)

// StoryLineRow is one narration line projected for the story-view
// template (task 2.2, narrated-execution-design.md §5.6). Seq
// mirrors persistence.ExecutionNarration.Seq — the per-execution
// monotonic counter the narrator's dual-path merge (seeded +
// live) orders by. Degraded flags a deterministic-fallback line
// (LLM unavailable/timed out/budget spent) so the template can
// render a subtle "simplified" hint instead of the LLM-authored
// prose.
type StoryLineRow struct {
	Seq      int64
	Kind     string
	Text     string
	Degraded bool
}

// storyLines loads the persisted narration for an execution, ordered
// by seq ascending (the story in emission order) — the server-side
// seed for the story panel on both the live task page and the
// completed task-detail page. Best-effort, mirroring liveStepOutcomes:
// a nil repo, empty executionID, or a read error yields no seed rows
// rather than failing the page — the panel just renders its empty
// state and (on the live page) still fills in from the WebSocket.
//
// This is READ-ONLY against the 2.1 narrator's store; it never
// writes execution_narration rows.
func (s *Server) storyLines(ctx context.Context, executionID string) []StoryLineRow {
	if s.narrationRepo == nil || executionID == "" {
		return nil
	}
	rows, err := s.narrationRepo.ListByExecution(ctx, executionID)
	if err != nil {
		s.logger.Warn().Err(err).Str("execution_id", executionID).
			Msg("story: failed to load narration lines for story-panel seed")
		return nil
	}
	if len(rows) == 0 {
		return nil
	}
	// The repo contract already promises seq-ascending order, but sort
	// defensively — the story's ordering invariant (never render a
	// higher-seq line before a lower one) is load-bearing enough that
	// a future repo implementation change shouldn't silently violate it.
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Seq < rows[j].Seq })
	out := make([]StoryLineRow, 0, len(rows))
	for _, r := range rows {
		if r == nil {
			continue
		}
		out = append(out, StoryLineRow{
			Seq:      r.Seq,
			Kind:     r.Kind,
			Text:     r.Text,
			Degraded: r.Degraded,
		})
	}
	return out
}

// isStoryDefaultViewer reports whether the requester is a project-
// scoped, non-admin session — the audience the story view defaults
// open for (narrated-execution-design.md §5.6). Empty role (auth
// disabled, or no session backend configured) is treated the same as
// admin: the technical view stays open by default, matching today's
// behaviour for those deployments.
func isStoryDefaultViewer(r *http.Request) bool {
	return api.SessionRoleFromContext(r.Context()) == auth.RoleUser
}
