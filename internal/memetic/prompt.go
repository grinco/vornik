package memetic

import (
	"encoding/json"
	"fmt"
	"strings"

	"vornik.io/vornik/internal/workflowtelemetry"
)

// defaultSystemPrompt is the architect's role. Holds three
// invariants in tension:
//   - Aggressive enough to find structural problems the operator
//     hasn't noticed.
//   - Conservative enough that it CITES specific runs as evidence
//     rather than philosophising.
//   - Operator-bound — never promises to apply, only proposes.
//
// Tone is deliberately constrained ("propose", "consider", "based
// on these N runs") because the proposal lands as a YAML diff in
// a UI that the operator reviews. Aspirational language tempts
// approvals; specific language gates them.
const defaultSystemPrompt = `You are vornik's workflow architect. Your job: read execution telemetry for one workflow over a recent window, and PROPOSE a structural YAML edit if (and only if) the evidence supports it.

You MUST follow these rules:

1. EVIDENCE-REQUIRED. Cite at least 3 specific execution IDs in evidence_run_ids. Each ID must be present in the rollup's per-step error data, judge verdicts, or step outcomes you're reasoning about. Made-up IDs are an automatic rejection.

2. STRUCTURAL ONLY. You may propose: adding a step, removing a step, adding an on_success / on_failure transition, tuning retry / timeout / max_iters, reordering transitions. You may NOT change prompts, change roles, or invent new step types. Tag the change in the "kind" field (schema below) with the single best-matching class.

3. CONFIDENCE HONEST. Self-rate 0.0 - 1.0 based on how directly the evidence supports the change. If the evidence is weak, return confidence < 0.6 and the operator never sees it. Padding confidence to clear the gate is a hard violation.

4. PROPOSE OR PASS. If no structural change is supported by the evidence, emit confidence: 0.0 and an empty motivation. The system filters those out before the operator sees them.

5. EMIT ONLY JSON. The complete response is one JSON object matching the schema below. No prose, no markdown, no code fences. Any extra text causes a parse failure.

Output schema:
{
  "workflow_id":       "<unchanged from input>",
  "proposed_yaml":     "<full new WORKFLOW.md content, frontmatter + body>",
  "motivation":        "<2-4 sentences citing specific failure classes and run IDs>",
  "evidence_run_ids":  ["exec_...", "exec_...", "exec_..."],
  "kind":              "<one of: add_step | remove_step | change_transition | change_timeout | change_retry_policy | reorder_steps>",
  "confidence":        0.0
}

The "kind" classifies your edit so operators can filter proposals by
type. Pick the single closest match from the list above. If the edit
genuinely doesn't fit any (or you're passing), omit it — it defaults to
"unspecified". Do NOT invent a kind outside the list.

Your reasoning surfaces in the motivation field, not as commentary above the JSON.`

// renderUserPrompt formats the rollup + current YAML into the
// user-message payload the architect reads. Sections are titled
// so the model can find them deterministically; the rollup goes
// last because the architect's reasoning typically reads the
// YAML first, then asks "where does the failure live?"
//
// Splitting prompt rendering out of architect.go keeps the
// templating logic test-isolated and avoids dragging the chat
// package into prompt tests.
func renderUserPrompt(workflowID string, currentYAML []byte, rollup *workflowtelemetry.Rollup, candidateEvidenceRunIDs []string, priors, recovery []prior) (string, error) {
	rollupJSON, err := json.MarshalIndent(rollup, "", "  ")
	if err != nil {
		return "", fmt.Errorf("renderUserPrompt: marshal rollup: %w", err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Workflow ID: %s\n\n", workflowID)
	b.WriteString("=== Current WORKFLOW.md ===\n")
	b.Write(currentYAML)
	if len(currentYAML) > 0 && currentYAML[len(currentYAML)-1] != '\n' {
		b.WriteByte('\n')
	}
	b.WriteString("\n=== Telemetry rollup (last window) ===\n")
	b.Write(rollupJSON)
	if len(candidateEvidenceRunIDs) > 0 {
		evidenceJSON, err := json.MarshalIndent(candidateEvidenceRunIDs, "", "  ")
		if err != nil {
			return "", fmt.Errorf("renderUserPrompt: marshal candidate evidence: %w", err)
		}
		b.WriteString("\n\n=== Candidate evidence run IDs from detector ===\n")
		b.Write(evidenceJSON)
		b.WriteString("\nUse these IDs when they support the proposed structural change. They are still validated against the workflow before persistence.")
	}
	renderPriorsSection(&b, priors)
	renderRecoverySection(&b, recovery)
	b.WriteString("\n\nReturn one JSON object per the schema. No prose, no fences.")
	return b.String(), nil
}

// renderRecoverySection appends the recovery-domain priors block: the
// observer-mined recovery actions that have already resolved the
// failure classes this rollup is showing. Framed as context the
// architect must weigh — a failure class that reliably self-resolves
// on retry argues for encoding that recovery structurally (retry /
// timeout tuning) or passing, not for a larger rewrite. No-op when
// recovery is empty, keeping no-failure / gate-off prompts
// byte-for-byte unchanged.
func renderRecoverySection(b *strings.Builder, recovery []prior) {
	if len(recovery) == 0 {
		return
	}
	var lines []string
	for _, pr := range recovery {
		if pr.inst == nil {
			continue
		}
		action := strings.TrimSpace(pr.inst.Action)
		if action == "" {
			continue
		}
		lines = append(lines, fmt.Sprintf("- %s (confidence %.2f, observed %dx)", action, pr.inst.Confidence, pr.inst.SupportCount))
	}
	if len(lines) == 0 {
		return
	}
	b.WriteString("\n\n=== Observed recovery patterns for the failing classes (advisory) ===\n")
	b.WriteString("These recovery actions have already resolved this workflow's observed failure classes in production:\n")
	b.WriteString(strings.Join(lines, "\n"))
	b.WriteString("\nWeigh them when proposing: if a failure class already self-resolves (e.g. on retry), prefer encoding that recovery structurally (retry policy / timeout tuning) over a larger change — or PASS (confidence 0.0) if no structural change is warranted.")
}

// renderPriorsSection appends the learned-instinct priors block to the
// architect prompt. Positive (support) priors are framed as patterns
// that previously correlated with success; negative ('architect-reject')
// priors are framed as changes the operator has already declined so the
// architect stops re-proposing them. No-op when priors is empty (gate
// off / not wired), keeping gate-off prompts byte-for-byte unchanged.
func renderPriorsSection(b *strings.Builder, priors []prior) {
	if len(priors) == 0 {
		return
	}
	var pos, neg []string
	for _, pr := range priors {
		if pr.inst == nil {
			continue
		}
		action := strings.TrimSpace(pr.inst.Action)
		if action == "" {
			continue
		}
		line := fmt.Sprintf("- %s (confidence %.2f)", action, pr.inst.Confidence)
		if pr.negative {
			neg = append(neg, line)
		} else {
			pos = append(pos, line)
		}
	}
	if len(pos) == 0 && len(neg) == 0 {
		return
	}
	b.WriteString("\n\n=== Learned priors for this workflow (advisory) ===\n")
	if len(pos) > 0 {
		b.WriteString("Patterns that have correlated with success — weigh them as supporting evidence:\n")
		b.WriteString(strings.Join(pos, "\n"))
		b.WriteString("\n")
	}
	if len(neg) > 0 {
		b.WriteString("Changes the operator has previously DECLINED — do NOT re-propose these:\n")
		b.WriteString(strings.Join(neg, "\n"))
		b.WriteString("\n")
	}
	b.WriteString("These priors are advisory. They do not expand what you may edit (structural steps / terminals / transitions only) and every proposal is still reviewed by the operator.")
}
