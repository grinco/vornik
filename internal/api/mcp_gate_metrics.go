package api

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// mcpGapReason names WHY the MCP role gate could not resolve an allowlist.
//
// The gate fails open on every one of these (Finding B2), deliberately, so that
// dev and sqlite deployments keep working and the project gate carries the
// boundary. What was missing is that they were indistinguishable from each
// other AND from a healthy resolve: roleToolAllowlist returned a bare nil for
// all of them, so a deployment running every MCP call unrestricted looked
// exactly like one whose roles all resolve.
//
// Enumerated rather than free-form because the distinctions are the point. "No
// dependencies wired" is a dev deployment behaving as designed; "role not found"
// is a misconfigured swarm in production; "role declares none" is an operator
// decision. One aggregate count would answer none of those questions, which is
// the same collapse the reasons exist to undo.
type mcpGapReason string

const (
	// mcpGapNone means the allowlist resolved and the gate applied it.
	mcpGapNone mcpGapReason = ""
	// mcpGapNoTaskID — the caller carried no task identity: an operator CLI,
	// the UI, or a call with neither task-bound key nor X-Task-ID.
	mcpGapNoTaskID mcpGapReason = "no_task_id"
	// mcpGapDepsUnwired — no execution repository or no project registry. The
	// expected state for CE/dev, and a serious one in production.
	mcpGapDepsUnwired mcpGapReason = "deps_unwired"
	// mcpGapNoExecution — the task has no execution, or it has no current step.
	mcpGapNoExecution mcpGapReason = "no_execution"
	// mcpGapNoWorkflow — the project or its workflow could not be loaded.
	mcpGapNoWorkflow mcpGapReason = "no_workflow"
	// mcpGapNoStepRole — the current step is unknown to the workflow, or names
	// no role.
	mcpGapNoStepRole mcpGapReason = "no_step_role"
	// mcpGapNoSwarm — the project's swarm could not be loaded.
	mcpGapNoSwarm mcpGapReason = "no_swarm"
	// mcpGapRoleNotFound — the step's role is not in the swarm. A misconfigured
	// deployment, and the reason most worth alerting on.
	mcpGapRoleNotFound mcpGapReason = "role_not_found"
	// mcpGapRoleDeclaresNone — the role resolved and declared no allowedTools,
	// which the daemon reads as unrestricted. An operator decision rather than a
	// failure, and counted separately for exactly that reason.
	mcpGapRoleDeclaresNone mcpGapReason = "role_declares_none"
)

// allMCPGapReasons is the enumeration, for tests and for anyone wiring an alert.
func allMCPGapReasons() []mcpGapReason {
	return []mcpGapReason{
		mcpGapNoTaskID, mcpGapDepsUnwired, mcpGapNoExecution, mcpGapNoWorkflow,
		mcpGapNoStepRole, mcpGapNoSwarm, mcpGapRoleNotFound, mcpGapRoleDeclaresNone,
	}
}

// MCPGateMetrics makes the MCP gate's fail-open states countable.
//
// The backlog asked for the gaps to be "at least logged, so a deployment
// silently running unrestricted is visible rather than merely permitted"
// (2026-08-13). A counter rather than only a log line because the question is
// how OFTEN, on which path, and for which reason — a log answers none of those
// without someone already suspecting the answer.
//
// It is also the sizing instrument for the open decision above it: whether the
// container should enforce MCP itself needs to know which gaps real traffic
// actually hits, rather than reasoning about which ones exist.
type MCPGateMetrics struct {
	// UnrestrictedTotal counts MCP resolutions that applied NO role allowlist,
	// by {path, reason}. path = call (the /mcp/call gate) | advertise (the
	// catalogue filter, where a gap costs a wide prompt rather than a wide
	// grant).
	UnrestrictedTotal *prometheus.CounterVec
}

// NewMCPGateMetrics registers the fail-open counter. Same shared-registry +
// nil-defaults contract as the other api metrics constructors.
func NewMCPGateMetrics(registerer prometheus.Registerer) *MCPGateMetrics {
	if registerer == nil {
		registerer = prometheus.DefaultRegisterer
	}
	return &MCPGateMetrics{
		UnrestrictedTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "vornik",
				Name:      "mcp_gate_unrestricted_total",
				Help: "MCP role-gate resolutions that applied NO allowlist, by {path, reason}. " +
					"This is a fail-open census, NOT a refusal count: every one of these calls was " +
					"PERMITTED, by design (the project gate remains in force). A rising number means " +
					"roles are not resolving, not that something was blocked. path = call|advertise; " +
					"reason = no_task_id|deps_unwired|no_execution|no_workflow|no_step_role|no_swarm|" +
					"role_not_found|role_declares_none.",
			},
			[]string{"path", "reason"},
		),
	}
}

// recordMCPGap counts one fail-open resolution. No-op when metrics are unwired
// (CE and test paths) or when the allowlist resolved normally.
func (s *Server) recordMCPGap(path string, reason mcpGapReason) {
	if s == nil || s.mcpGateMetrics == nil || reason == mcpGapNone {
		return
	}
	s.mcpGateMetrics.UnrestrictedTotal.WithLabelValues(path, string(reason)).Inc()
}
