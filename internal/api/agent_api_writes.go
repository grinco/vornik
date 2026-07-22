package api

import (
	"context"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"vornik.io/vornik/internal/persistence"
)

// Agent-write policy (LLD 2026-07-22-agent-query-api-write-policy-design.md).
// gateway.agent_writes is a daemon-wide tri-state governing whether a query_api
// write issued from WITHIN a task (the agent surface) is permitted. It is
// AND-gated with each provider's writes_enabled + the gateway route allowlist,
// so this only widens WHICH task origins may write, never which providers.
const (
	agentWritesOff  = "off"
	agentWritesUser = "user"
	agentWritesAll  = "all"
)

// Audit-layer walk_outcome values that are NOT in-repo walk results
// (persistence.WalkOutcome carries clean_root|missing_parent|cycle|
// depth_exhausted). "not_walked" = off mode performed no walk; "error" = a repo
// failure aborted the walk. Kept here, not in persistence, so the resolver's
// enum stays limited to actual walk classifications.
const (
	walkOutcomeNotWalked  = "not_walked"
	walkOutcomeError      = "error"
	creationSourceUnknown = "unknown"
)

// writeResolution is the per-request agent-write decision + its audit trail,
// resolved ONCE up front (review I-R2 resolve-then-decide): the gate closure
// reads .permit; the audit row + metric read the rest. For off/all the permit
// is a constant, but §6 still needs root_task_id/walk_outcome/creation_source
// for correlation — data only a walk produces — so all performs the walk for
// AUDIT ONLY (non-blocking) while off performs none.
type writeResolution struct {
	mode           string // resolved gateway.agent_writes mode (off|user|all)
	permit         bool   // did the agent-write POLICY permit this write
	walked         bool   // was an ancestor walk performed (false only for off)
	rootTaskID     string // resolved request-root id, when the walk was clean
	walkOutcome    string // clean_root|missing_parent|cycle|depth_exhausted|error|not_walked
	creationSource string // resolved root creation_source, or "unknown"
}

// resolveAgentWrite computes the write decision for a task-originated write
// under the daemon's gateway.agent_writes mode. Called only for write methods
// (reads never reach the gate, so never pay for a walk). Fail-closed: an
// unrecognised mode (should be impossible — validated at load) refuses.
//
//   - off  → refuse, no walk (not_walked / nil root / unknown source).
//   - all  → permit ALWAYS; walk only to label the audit row (non-blocking, an
//     incomplete/errored walk never flips the permit).
//   - user → walk gates: permit iff the walk reached a genuine ParentTaskID==nil
//     root (clean_root) whose creation_source == USER. Any incomplete/ambiguous
//     lineage (missing parent, cycle, depth-exhaustion, repo error) refuses.
func (s *Server) resolveAgentWrite(ctx context.Context, taskID string) writeResolution {
	mode := s.agentWritesMode
	if mode == "" {
		mode = agentWritesOff
	}
	switch mode {
	case agentWritesAll:
		res := s.walkOrigin(ctx, taskID)
		res.mode = mode
		res.permit = true // non-blocking: all trusts any origin, walk is audit-only
		return res
	case agentWritesUser:
		res := s.walkOrigin(ctx, taskID)
		res.mode = mode
		res.permit = res.walkOutcome == string(persistence.WalkOutcomeCleanRoot) &&
			res.creationSource == string(persistence.TaskCreationSourceUser)
		return res
	default: // off, and any never-reached invalid value → fail closed, no walk
		return writeResolution{
			mode:           agentWritesOff,
			permit:         false,
			walked:         false,
			walkOutcome:    walkOutcomeNotWalked,
			creationSource: creationSourceUnknown,
		}
	}
}

// walkOrigin resolves the calling task's request-root via the
// completeness-returning resolver and packs the outcome into a writeResolution
// (permit unset — the caller sets it per mode). A repo/lookup failure yields
// walk_outcome=error with nil root + unknown source (fail-closed for user;
// non-blocking for all). Only clean_root populates root_task_id + the real
// creation_source; every incomplete outcome leaves them nil/unknown so an
// ambiguous lineage can never masquerade as a resolved USER root.
func (s *Server) walkOrigin(ctx context.Context, taskID string) writeResolution {
	res := writeResolution{walked: true, walkOutcome: walkOutcomeError, creationSource: creationSourceUnknown}
	if s.taskRepo == nil || taskID == "" {
		return res
	}
	task, err := s.taskRepo.Get(ctx, taskID)
	if err != nil || task == nil {
		return res
	}
	roots, outcomes, err := persistence.ResolveRequestRootsWithCompleteness(
		ctx, s.taskRepo, []*persistence.Task{task}, persistence.MaxRequestRootWalkDepth)
	if err != nil {
		return res
	}
	oc := outcomes[task.ID]
	res.walkOutcome = string(oc)
	if oc == persistence.WalkOutcomeCleanRoot {
		if root := roots[task.ID]; root != nil {
			res.rootTaskID = root.ID
			res.creationSource = string(root.CreationSource)
		}
	}
	return res
}

// AgentAPIWriteMetrics is the runtime observability for the agent-write policy
// (review I4): a counter so an operator can see the broad `all` capability (or
// `user` grants) actually being exercised, not just read a load-time warning.
type AgentAPIWriteMetrics struct {
	// WritesTotal counts task-originated query_api WRITE attempts (reads
	// excluded) by {mode, creation_source, outcome}. outcome = permitted when
	// the write actually went through the full gate, refused otherwise.
	WritesTotal *prometheus.CounterVec
}

// NewAgentAPIWriteMetrics registers the agent-write counter. Same
// shared-registry + nil-defaults contract as the other api metrics
// constructors.
func NewAgentAPIWriteMetrics(registerer prometheus.Registerer) *AgentAPIWriteMetrics {
	if registerer == nil {
		registerer = prometheus.DefaultRegisterer
	}
	return &AgentAPIWriteMetrics{
		WritesTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "vornik",
				Name:      "agent_api_writes_total",
				Help:      "Task-originated query_api write attempts by {mode, creation_source, outcome}. mode = gateway.agent_writes (off|user|all); creation_source = resolved request-root origin (or 'unknown' under off / an incomplete walk); outcome = permitted|refused (whether the write cleared the full gate).",
			},
			[]string{"mode", "creation_source", "outcome"},
		),
	}
}

// record increments the write counter for one resolved write attempt. permitted
// reflects the FINAL gate result (agent_writes AND writes_enabled AND route),
// so an `all`-permitted write that writes_enabled then blocks is honestly
// counted refused. No-op when metrics are unwired.
func (s *Server) recordAgentWrite(res writeResolution, permitted bool) {
	if s.agentWriteMetrics == nil {
		return
	}
	outcome := "refused"
	if permitted {
		outcome = "permitted"
	}
	src := res.creationSource
	if src == "" {
		src = creationSourceUnknown
	}
	s.agentWriteMetrics.WritesTotal.WithLabelValues(res.mode, src, outcome).Inc()
}
