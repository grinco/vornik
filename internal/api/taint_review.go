package api

import (
	"context"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/taintlineage"
)

// Taint-lineage write gate (taint-lineage-tracking-design.md §4.4). Mirrors the
// agent_writes resolve-then-decide shape: resolveTaintReview resolves the
// task-lineage taint rollup under the effective enforcement mode ONCE up front;
// the query_api handler then refuses (enforce + requiresReview) or flags
// (advisory). The forge surface parks instead (executor.parkForTaintReview) —
// query_api is refuse-only in v1 (D5), no park/latch.

// taintResolution is the per-write taint decision + its audit fields.
type taintResolution struct {
	mode           taintlineage.Mode
	tainted        bool
	requiresReview bool // High present, OR (enforce) Unknown, OR (enforce) walk incomplete
	walkComplete   bool
	park           bool // enforce && park formula (used by the forge surface; query_api treats park as refuse)
	sourceSetHash  string
	sourceCount    int
}

// effectiveTaintMode resolves the effective mode for a project: a non-empty
// project override wins; otherwise the daemon default. Never LLM-settable.
func (s *Server) effectiveTaintMode(projectID string) taintlineage.Mode {
	override := ""
	if s.projectRegistry != nil && projectID != "" {
		if p := s.projectRegistry.GetProject(projectID); p != nil {
			override = p.TaintLineage.Mode
		}
	}
	return taintlineage.EffectiveMode(override, s.taintDefaultMode)
}

// resolveTaintReview computes the taint decision for a task-originated write.
// off → returns early with no query (parity with agent_writes off; cheap). In
// advisory/enforce it walks the request-root lineage once, batch-queries the
// tainted steps (I7), rolls them up, reads the D7 latch, and applies the
// canonical D8 formula (in taintlineage.Decide). A repo error fails CLOSED under
// enforce (D6 — requiresReview+park), non-blocking under advisory.
func (s *Server) resolveTaintReview(ctx context.Context, projectID, taskID string) taintResolution {
	mode := s.effectiveTaintMode(projectID)
	if mode == taintlineage.ModeOff || s.stepOutcomeRepo == nil || s.taskRepo == nil || taskID == "" {
		return taintResolution{mode: taintlineage.ModeOff, walkComplete: true}
	}

	lineageIDs, outcome, err := persistence.ResolveLineageWithCompleteness(
		ctx, s.taskRepo, taskID, persistence.MaxRequestRootWalkDepth)
	if err != nil {
		return s.taintFailClosed(mode)
	}
	walkComplete := outcome == persistence.WalkOutcomeCleanRoot
	if len(lineageIDs) == 0 {
		lineageIDs = []string{taskID}
	}

	rows, err := s.stepOutcomeRepo.TaintedStepsForTasks(ctx, lineageIDs)
	if err != nil {
		return s.taintFailClosed(mode)
	}

	var own, ancestor []taintlineage.StepTaint
	for _, r := range rows {
		st := taintlineage.StepTaintFromBlob(r.UntrustedSources, r.RequiresReview)
		if r.TaskID == taskID {
			own = append(own, st)
		} else {
			ancestor = append(ancestor, st)
		}
	}
	roll := taintlineage.Rollup(own, ancestor, walkComplete)

	dec := taintlineage.Decide(mode, roll, s.taintLatchHashes(ctx, taskID))
	return taintResolution{
		mode:           mode,
		tainted:        dec.Tainted,
		requiresReview: dec.RequiresReview,
		walkComplete:   dec.WalkComplete,
		park:           dec.Park,
		sourceSetHash:  dec.SourceSetHash,
		sourceCount:    dec.SourceCount,
	}
}

// taintFailClosed is the enforce fail-closed result for a resolution error
// (D6): treat as requires-review + park. advisory/off never blocks.
func (s *Server) taintFailClosed(mode taintlineage.Mode) taintResolution {
	if mode == taintlineage.ModeEnforce {
		return taintResolution{mode: mode, tainted: true, requiresReview: true, walkComplete: false, park: true}
	}
	return taintResolution{mode: mode, walkComplete: false}
}

// taintLatchHashes reads the recorded taint_latch source-set hashes for a task
// (D7). Best-effort: a repo error yields no latches (fail-closed — an
// unreadable latch never suppresses a park). Nil repo ⇒ no latches.
func (s *Server) taintLatchHashes(ctx context.Context, taskID string) []string {
	if s.taskMessageRepo == nil || taskID == "" {
		return nil
	}
	msgs, err := s.taskMessageRepo.List(ctx, persistence.TaskMessageFilter{
		TaskID:       taskID,
		MessageKinds: []string{persistence.TaskMessageKindSystem},
	})
	if err != nil {
		return nil
	}
	var out []string
	for _, m := range msgs {
		if h, ok := taintlineage.ParseLatchHash(m.Metadata); ok {
			out = append(out, h)
		}
	}
	return out
}

// IsTaintReviewCheckpoint reports whether a checkpoint message's metadata is an
// untrusted-review decision (decision.kind == "untrusted_review"), so the answer
// handlers (API + UI) branch onto the admin-class allow/cancel path. Sibling of
// IsBudgetCheckpoint. Exported so the ui package shares one definition.
func IsTaintReviewCheckpoint(meta []byte) bool {
	return taintlineage.IsTaintReviewCheckpointMeta(meta)
}

// recordTaintReviewLatch writes the D7 latch marker for a reviewed source set:
// a system task_message carrying the source-set hash the admin operator was
// shown. resolveTaintReview reads it back via taintLatchHashes on the re-run.
// Best-effort — a latch write failure is logged; the resume still proceeds (the
// re-run simply re-parks, the safe direction).
func (s *Server) recordTaintReviewLatch(ctx context.Context, taskID, executionID, sourceSetHash string) {
	if s.taskMessageRepo == nil || taskID == "" || sourceSetHash == "" {
		return
	}
	marker := &persistence.TaskMessage{
		TaskID:      taskID,
		AuthorKind:  persistence.TaskMessageAuthorSystem,
		MessageKind: persistence.TaskMessageKindSystem,
		Content:     "untrusted-content review cleared for the reviewed source set",
		Metadata:    taintlineage.LatchMarkerMetadata(sourceSetHash),
	}
	if executionID != "" {
		marker.ExecutionID = &executionID
	}
	if err := s.taskMessageRepo.Insert(ctx, marker); err != nil {
		s.logger.Warn().Err(err).Str("taskId", taskID).Msg("taint: failed to record review latch")
	}
}

// TaintWriteMetrics counts tainted-write outcomes (§8):
// vornik_taint_writes_total{mode,write_surface,outcome}. outcome ∈
// {permitted, flagged, parked, refused}.
type TaintWriteMetrics struct {
	WritesTotal *prometheus.CounterVec
}

// NewTaintWriteMetrics registers the tainted-write counter. Same shared-registry
// + nil-defaults contract as the other api metrics constructors.
func NewTaintWriteMetrics(registerer prometheus.Registerer) *TaintWriteMetrics {
	if registerer == nil {
		registerer = prometheus.DefaultRegisterer
	}
	return &TaintWriteMetrics{
		WritesTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "vornik",
				Name:      "taint_writes_total",
				Help:      "Autonomous write attempts gated by taint-lineage, by {mode, write_surface, outcome}. mode = effective enforcement mode; write_surface = query_api|forge; outcome = permitted (allowed, untainted) | flagged (advisory tainted, allowed) | parked (forge enforce park) | refused (query_api enforce refusal).",
			},
			[]string{"mode", "write_surface", "outcome"},
		),
	}
}

// recordTaintWrite increments the tainted-write counter for one resolved
// query_api write. No-op when metrics are unwired. The api server only gates the
// query_api surface, so write_surface is fixed here; the forge surface records
// its own outcomes through the service container's recordForgeTaintWrite, which
// targets the SAME vornik_taint_writes_total counter (single registration).
func (s *Server) recordTaintWrite(mode taintlineage.Mode, outcome string) {
	if s.taintWriteMetrics == nil || s.taintWriteMetrics.WritesTotal == nil {
		return
	}
	s.taintWriteMetrics.WritesTotal.WithLabelValues(string(mode), "query_api", outcome).Inc()
}
