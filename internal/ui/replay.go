package ui

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"vornik.io/vornik/internal/replay"
)

// ReplayData is the template payload for /executions/<id>/replay.
// Mirrors replay.Timeline at the field level so the template can
// reference {{.Timeline.Steps}} etc. without an extra layer of
// indirection. Title + CurrentPage land on the base layout.
type ReplayData struct {
	Title       string
	CurrentPage string
	Timeline    *replay.Timeline
}

// ExecutionReplay renders the failure forensics replay page. The
// route lives at /executions/<id>/replay and is dispatched from
// executionRouter when the path ends in "/replay".
//
// 404 when the execution doesn't exist. 503 when the required
// repos aren't wired (older deployments missing one of the seven
// data sources land here — operator sees a clear message).
func (s *Server) ExecutionReplay(w http.ResponseWriter, r *http.Request, execID string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if execID == "" {
		http.Error(w, "execution id required", http.StatusBadRequest)
		return
	}
	builder, err := s.replayBuilder()
	if err != nil {
		s.logger.Warn().Err(err).Msg("replay builder unavailable")
		http.Error(w, "replay not available on this deployment: "+err.Error(),
			http.StatusServiceUnavailable)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	tl, err := builder.Build(ctx, execID)
	if err != nil {
		if errors.Is(err, replay.ErrExecutionNotFound) {
			http.NotFound(w, r)
			return
		}
		s.logger.Warn().Err(err).Str("execution_id", execID).Msg("replay build failed")
		http.Error(w, "replay build failed", http.StatusInternalServerError)
		return
	}

	data := ReplayData{
		Title:       "Replay — " + execID,
		CurrentPage: "executions",
		Timeline:    tl,
	}
	s.render(w, "replay.html", data)
}

// replayBuilder constructs a replay.Builder from the repos already
// on the Server. Returns an error when any required repo is
// missing — the handler surfaces this as 503 with a clear
// message rather than panicking.
//
// PostMortem repo is optional; missing it just means the
// diagnosis card on the page renders empty.
func (s *Server) replayBuilder() (*replay.Builder, error) {
	missing := []string{}
	check := func(name string, ok bool) {
		if !ok {
			missing = append(missing, name)
		}
	}
	check("execution", s.execRepo != nil)
	check("task", s.taskRepo != nil)
	check("outcome", s.outcomeRepo != nil)
	check("llm_usage", s.llmUsageRepo != nil)
	check("tool_audit", s.auditRepo != nil)
	check("artifact", s.artifactRepo != nil)
	check("task_message", s.taskMessageRepo != nil)
	if len(missing) > 0 {
		return nil, errors.New("missing repos: " + strings.Join(missing, ", "))
	}
	return &replay.Builder{
		Executions:  s.execRepo,
		Tasks:       s.taskRepo,
		Outcomes:    s.outcomeRepo,
		LLMUsage:    s.llmUsageRepo,
		ToolAudit:   s.auditRepo,
		Artifacts:   s.artifactRepo,
		Messages:    s.taskMessageRepo,
		PostMortems: s.postMortemRepo, // optional
		// Inter-project orchestration Phase C — multi-hop
		// replay tree dependencies. All optional + nil-safe in
		// the Builder; deployments without the feature wired
		// keep the legacy single-project timeline.
		CrossProjectCalls: s.crossProjectCallRepo,
		ProjectSpawns:     s.projectSpawnRepo,
		TaskChildren:      s.taskRepo, // GetChildren method satisfies the narrow interface
		ExecutionByTask:   s.execRepo, // GetByTaskID method satisfies the narrow interface
	}, nil
}
