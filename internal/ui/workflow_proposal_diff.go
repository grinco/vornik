package ui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/workflowtelemetry"
)

// WorkflowSourceUI is the optional capability the proposal detail
// page uses to load the CURRENT on-disk WORKFLOW.md so it can render
// a before/after diff against the proposed YAML (§8.5 operator review
// UI: diff panel). Satisfied by the service layer's fsWorkflowSource
// adapter. When unwired, the diff panel degrades to showing the
// proposed YAML only (the pre-§8.5 behaviour).
type WorkflowSourceUI interface {
	Load(ctx context.Context, workflowID string) ([]byte, error)
}

// WorkflowRollupSource is the optional capability the proposal detail
// page uses to fetch the workflow's CURRENT telemetry rollup so the
// predicted-impact panel can show the real cost / failure-rate
// baseline the proposal targets (Slice 3 "Predicted impact" — see
// https://docs.vornik.io §Slice-3).
// Satisfied by *workflowtelemetry.Service via a service-layer adapter.
// When unwired (or on error / no rows), the panel degrades to the
// heuristic predictedImpactSummary one-liner unchanged.
//
// HONESTY NOTE: this surfaces the workflow's *current* profile, not a
// forward forecast. The architect cannot compute a numeric expected
// cost/failure delta from a YAML diff, so the panel never invents one;
// it shows the baseline the proposal aims to improve and leaves the
// intended direction to the kind/confidence heuristic.
type WorkflowRollupSource interface {
	ForWorkflow(ctx context.Context, workflowID string, since time.Time) (*workflowtelemetry.Rollup, error)
}

// diffOp is the classification of one line in a unified diff.
type diffOp string

const (
	diffContext diffOp = "context" // unchanged line, present in both
	diffAdd     diffOp = "add"     // present only in the proposed YAML
	diffRemove  diffOp = "remove"  // present only in the current YAML
)

// DiffLine is one rendered row in the proposal diff panel. The
// template colours by Op (add=emerald, remove=rose, context=muted).
type DiffLine struct {
	Op   diffOp
	Text string
}

// computeWorkflowDiff produces a line-level unified diff of current →
// proposed using the classic LCS longest-common-subsequence
// algorithm. WORKFLOW.md files are small (tens to low-hundreds of
// lines), so the O(n*m) table is cheap and the result is a faithful
// minimal diff rather than a naive line-by-line zip.
//
// Empty current (no on-disk source wired / new workflow) yields an
// all-add diff; identical inputs yield all-context.
func computeWorkflowDiff(current, proposed string) []DiffLine {
	a := splitLinesForDiff(current)
	b := splitLinesForDiff(proposed)

	// LCS length table. lcs[i][j] = LCS length of a[i:] and b[j:].
	lcs := make([][]int, len(a)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}

	var out []DiffLine
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			out = append(out, DiffLine{Op: diffContext, Text: a[i]})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			out = append(out, DiffLine{Op: diffRemove, Text: a[i]})
			i++
		default:
			out = append(out, DiffLine{Op: diffAdd, Text: b[j]})
			j++
		}
	}
	for ; i < len(a); i++ {
		out = append(out, DiffLine{Op: diffRemove, Text: a[i]})
	}
	for ; j < len(b); j++ {
		out = append(out, DiffLine{Op: diffAdd, Text: b[j]})
	}
	return out
}

// splitLinesForDiff splits on newline, dropping a single trailing
// empty line so a file with/without a final newline diffs the same.
func splitLinesForDiff(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	return lines
}

// predictedImpactSummary builds the operator-facing one-liner for
// the §8.5 predicted-impact panel from data already on the proposal
// row (confidence + evidence count) plus the computed diff size. It
// deliberately makes NO telemetry claim it can't back — a richer
// cost/failure-rate-delta panel needs the workflow rollup plumbed
// into the UI handler and is tracked as a follow-on.
func predictedImpactSummary(p *persistence.WorkflowProposal, added, removed int, hasDiff bool) string {
	if p == nil {
		return ""
	}
	var b strings.Builder
	kind := p.Kind
	if kind == "" {
		kind = persistence.WorkflowProposalKindUnspecified
	}
	fmt.Fprintf(&b, "%s change", strings.ReplaceAll(string(kind), "_", " "))
	if hasDiff {
		fmt.Fprintf(&b, " · +%d/−%d lines", added, removed)
	}
	fmt.Fprintf(&b, " · architect confidence %.0f%%", p.Confidence*100)
	fmt.Fprintf(&b, " · backed by %d evidence run(s)", len(p.EvidenceRunIDs))
	return b.String()
}

// WorkflowBaselineImpact is the telemetry-backed "Current baseline"
// block of the predicted-impact panel. Every field describes the
// workflow's CURRENT profile over the rollup window — NOT a forecast.
// The template labels it "Current baseline (last 30d)" and renders the
// directional hint (from DirectionHint) separately, clearly flagged as
// a heuristic rather than a computed delta.
//
// see https://docs.vornik.io §Slice-3
type WorkflowBaselineImpact struct {
	// WindowDays is the lookback the rollup covers (e.g. 30).
	WindowDays int
	// HasRuns is false when RunCount is 0 over the window — the
	// template then says "no runs in window" instead of showing a
	// failure rate / avg cost computed from a zero denominator.
	HasRuns  bool
	RunCount int
	// FailureRatePct is FailureCount/RunCount as a percentage. 0 when
	// HasRuns is false (never divides by zero).
	FailureRatePct float64
	FailureCount   int
	// AvgCostUSD is the rollup's average cost per run.
	AvgCostUSD float64
	// TopFailureClasses is the rollup's top error classes (already
	// sorted desc, capped at 10 upstream); the template shows the
	// first few so the operator can see what the proposal targets.
	TopFailureClasses []workflowtelemetry.FailureClassCount
	// DirectionHint is a one-line, clearly-labelled heuristic ("aims
	// to reduce failures", etc.) derived from the proposal kind +
	// confidence. It is NOT a forecast and the template says so.
	DirectionHint string
}

// buildWorkflowBaseline turns a telemetry rollup into the
// WorkflowBaselineImpact view for the predicted-impact panel. Returns
// nil when the rollup is nil so the caller falls back to the heuristic
// summary. RunCount 0 yields HasRuns=false and a zeroed rate (no
// divide-by-zero).
//
// see https://docs.vornik.io §Slice-3
func buildWorkflowBaseline(p *persistence.WorkflowProposal, r *workflowtelemetry.Rollup, windowDays int) *WorkflowBaselineImpact {
	if r == nil {
		return nil
	}
	out := &WorkflowBaselineImpact{
		WindowDays:        windowDays,
		RunCount:          r.RunCount,
		FailureCount:      r.FailureCount,
		AvgCostUSD:        r.AvgCostUSD,
		TopFailureClasses: r.TopFailureClasses,
		DirectionHint:     baselineDirectionHint(p),
	}
	if r.RunCount > 0 {
		out.HasRuns = true
		out.FailureRatePct = float64(r.FailureCount) / float64(r.RunCount) * 100
	}
	return out
}

// baselineDirectionHint produces the explicitly-heuristic directional
// note shown beneath the baseline. It reads the proposal kind +
// confidence already on the row — the same signals the architect
// emits — and NEVER claims a numeric delta the system can't compute
// from a YAML diff. The template labels this "heuristic, not a
// forecast".
func baselineDirectionHint(p *persistence.WorkflowProposal) string {
	if p == nil {
		return ""
	}
	kind := p.Kind
	if kind == "" {
		kind = persistence.WorkflowProposalKindUnspecified
	}
	var intent string
	switch kind {
	case persistence.WorkflowProposalKindAddStep,
		persistence.WorkflowProposalKindChangeRetryPolicy,
		persistence.WorkflowProposalKindChangeTransition,
		persistence.WorkflowProposalKindChangeRoleAssignment:
		intent = "aims to reduce failures against this baseline"
	case persistence.WorkflowProposalKindRemoveStep,
		persistence.WorkflowProposalKindReorderSteps:
		intent = "aims to cut cost / latency against this baseline"
	case persistence.WorkflowProposalKindChangeTimeout:
		intent = "aims to trade off latency vs. failures against this baseline"
	default:
		intent = "intended to improve on this baseline"
	}
	return fmt.Sprintf("%s (architect confidence %.0f%%)", intent, p.Confidence*100)
}

// diffStats counts the add/remove lines for the panel summary
// ("+N −M"). Context lines aren't counted.
func diffStats(lines []DiffLine) (added, removed int) {
	for _, l := range lines {
		switch l.Op {
		case diffAdd:
			added++
		case diffRemove:
			removed++
		}
	}
	return added, removed
}
