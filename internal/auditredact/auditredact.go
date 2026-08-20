// Package auditredact redacts secrets out of tool-audit entries at the
// repository seam, so no writer has to remember to do it.
//
// WHY A DECORATOR AND NOT A SCAN AT EACH WRITER. There are three writers into
// tool_audit_log — the realtime POST handler (api.IngestToolAudit), the
// post-step batch persist (executor artifacts), and the companion tool roll-up
// — and until 2026-08-20 only the batch one scanned. Worse, the repository
// INSERT is `ON CONFLICT (id) DO NOTHING` and both agent-side writers share the
// agent-supplied audit_id, so the unredacted realtime row landed FIRST and won:
// the redacted row was silently discarded. Credentials sat in the table at
// rest while secret_redaction_audit dutifully recorded findings, which made the
// control look healthy in exactly the place an operator would check it.
//
// Patching the handler would have made three sites that each have to remember,
// right after being bitten by the second one forgetting. Decorating the
// interface makes the redaction structural: every writer goes through Log, so
// every writer is covered, including ones not written yet.
//
// Design of record: https://docs.vornik.io
package auditredact

import (
	"context"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/secrets"
)

// Repo wraps a persistence.ToolAuditRepository and cleans entries on the way in.
type Repo struct {
	inner        persistence.ToolAuditRepository
	detector     secrets.Detector
	actions      map[string]secrets.Action
	audit        persistence.SecretRedactionAuditRepository
	trustedTools []string
	logger       *zerolog.Logger
}

// New wraps inner. A nil detector makes the decorator a pass-through, so CE
// paths and tests that never wire secret scanning keep their audit trail rather
// than losing it.
//
// trustedTools carries the operator's secrets.trusted_output_tools prefixes:
// for those tools the OUTPUT is a daemon-proxied response the agent cannot
// forge, so its HEURISTIC findings are dropped (a PageDrop viewing password is
// the motivating case). Strong prefix-anchored patterns are never dropped, and
// the agent-supplied INPUT is never exempt — otherwise the exemption becomes a
// labelled exfil channel.
func New(
	inner persistence.ToolAuditRepository,
	detector secrets.Detector,
	actions map[string]secrets.Action,
	audit persistence.SecretRedactionAuditRepository,
	trustedTools []string,
	logger *zerolog.Logger,
) *Repo {
	return &Repo{
		inner:        inner,
		detector:     detector,
		actions:      actions,
		audit:        audit,
		trustedTools: trustedTools,
		logger:       logger,
	}
}

// Log scans the entry and stores the cleaned copy.
//
// The caller's struct is never mutated: the batch path reuses its parsed
// entries for other bookkeeping, and a redaction visible there would be a
// surprise at a distance.
func (r *Repo) Log(ctx context.Context, entry *persistence.ToolAuditEntry) error {
	if r.detector == nil || entry == nil {
		return r.inner.Log(ctx, entry)
	}

	inputFindings := r.detector.Scan([]byte(entry.ToolInput))
	outputFindings := r.detector.Scan([]byte(entry.ToolOutput))
	if len(outputFindings) > 0 && r.isTrustedOutputTool(entry.ToolName) {
		outputFindings = secrets.DropHeuristic(outputFindings)
	}
	if len(inputFindings) == 0 && len(outputFindings) == 0 {
		return r.inner.Log(ctx, entry)
	}

	combined := make([]secrets.Finding, 0, len(inputFindings)+len(outputFindings))
	combined = append(combined, inputFindings...)
	combined = append(combined, outputFindings...)
	counts := secrets.CountByType(combined)

	action := secrets.ResolveAction(secrets.CheckpointToolAudit, r.actions)
	r.record(ctx, entry, counts)

	switch action {
	case secrets.ActionDetect:
		// Deliberate operator choice: keep the raw row for audit fidelity. The
		// findings are still recorded above, so choosing detect costs the
		// redaction, not the signal.
		r.log(entry, action, counts, "secrets: tool audit scanned — detect-only, row stored intact")
		return r.inner.Log(ctx, entry)
	default:
		// Redact, and Block degrades to it: refusing to persist the row would
		// lose more signal than the redaction does. Same degradation the
		// executor's scanner applied before this seam existed.
		r.log(entry, action, counts, "secrets: tool audit scanned — redacting before persist")
		// Shallow copy is sufficient ONLY while every field this redacts is a
		// value type: ToolInput and ToolOutput are strings, so replacing them
		// aliases nothing back to the caller. If ToolAuditEntry ever gains a
		// slice or map field carrying scannable content, this needs a deep
		// copy — a shallow one would leave the caller holding the same backing
		// array and the redaction would be visible at a distance, or absent
		// from the stored row depending on write order.
		clean := *entry
		clean.ToolInput = string(secrets.Redact([]byte(entry.ToolInput), inputFindings))
		clean.ToolOutput = string(secrets.Redact([]byte(entry.ToolOutput), outputFindings))
		return r.inner.Log(ctx, &clean)
	}
}

// List delegates unchanged.
func (r *Repo) List(ctx context.Context, filter persistence.ToolAuditFilter) ([]*persistence.ToolAuditEntry, error) {
	return r.inner.List(ctx, filter)
}

// CountByTool delegates unchanged.
func (r *Repo) CountByTool(ctx context.Context, executionID string) (map[string]int64, error) {
	return r.inner.CountByTool(ctx, executionID)
}

// ToolLatencyP95ByProjectTool delegates unchanged.
func (r *Repo) ToolLatencyP95ByProjectTool(ctx context.Context, since time.Time) ([]persistence.ToolLatencyStat, error) {
	return r.inner.ToolLatencyP95ByProjectTool(ctx, since)
}

func (r *Repo) isTrustedOutputTool(tool string) bool {
	for _, prefix := range r.trustedTools {
		if secrets.ToolNameMatchesPrefix(tool, prefix) {
			return true
		}
	}
	return false
}

// record writes one secret_redaction_audit row per finding type. Best-effort:
// the audit row must never block the tool-audit write, which is itself already
// a best-effort side channel.
func (r *Repo) record(ctx context.Context, entry *persistence.ToolAuditEntry, counts map[string]int) {
	if r.audit == nil || len(counts) == 0 {
		return
	}
	events := make([]persistence.SecretRedactionEvent, 0, len(counts))
	for findingType, n := range counts {
		if n <= 0 {
			continue
		}
		events = append(events, persistence.SecretRedactionEvent{
			ProjectID:   entry.ProjectID,
			TaskID:      entry.TaskID,
			ExecutionID: entry.ExecutionID,
			Checkpoint:  secrets.CheckpointToolAudit,
			FindingType: findingType,
			Count:       n,
			Source:      "live",
		})
	}
	if len(events) == 0 {
		return
	}
	if err := r.audit.Record(ctx, events); err != nil && r.logger != nil {
		r.logger.Warn().Err(err).Msg("secrets: recording tool-audit redaction events failed")
	}
}

func (r *Repo) log(entry *persistence.ToolAuditEntry, action secrets.Action, counts map[string]int, msg string) {
	if r.logger == nil {
		return
	}
	r.logger.Warn().
		Str("task_id", entry.TaskID).
		Str("execution_id", entry.ExecutionID).
		Str("tool", entry.ToolName).
		Str("checkpoint", secrets.CheckpointToolAudit).
		Str("action", string(action)).
		Interface("by_type", counts).
		Msg(msg)
}
