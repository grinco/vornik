package forge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"vornik.io/vornik/internal/executor"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/untrusted"
)

// FetchCIHandler implements the "forge.fetch_ci" system step: read what CI
// concluded for the commit under review and pass it to the next step.
//
// Design: https://docs.vornik.io §6.
//
// THIS IS THE ONE PLACE ARTIFACT CONTENT REACHES A PROMPT. Everything upstream
// carries a reference; the excerpt is read here and wrapped here, so there is
// one wrapping site rather than one per consumer. A deposit workflow starts
// with this step for exactly that reason.
type FetchCIHandler struct {
	outcomes persistence.ForgeCIOutcomeRepository
}

// NewFetchCIHandler wires the handler.
func NewFetchCIHandler(outcomes persistence.ForgeCIOutcomeRepository) *FetchCIHandler {
	return &FetchCIHandler{outcomes: outcomes}
}

// Name implements executor.SystemHandler.
func (h *FetchCIHandler) Name() string { return "forge.fetch_ci" }

// Execute implements executor.SystemHandler.
//
// The step is OPTIONAL in a workflow: a review that does not list it behaves
// exactly as it did before this feature, which is how "no behaviour change for
// existing flows" is satisfied by construction rather than by testing for it
// afterwards.
func (h *FetchCIHandler) Execute(ctx context.Context, in executor.SystemStepInput) (executor.SystemStepResult, error) {
	const name = "forge.fetch_ci"
	if h == nil || h.outcomes == nil {
		return executor.SystemStepResult{}, errors.New(name + ": handler is missing required dependencies (outcome store)")
	}
	job, err := forgeJobFromTask(in.Task, name)
	if err != nil {
		return executor.SystemStepResult{}, err
	}

	head := strings.TrimSpace(job.HeadSHA)
	if head == "" {
		// A review whose job carries no head cannot ask about a commit. Say so
		// rather than returning an empty CI section, which would read as "CI
		// passed" (design §6).
		return ciResult("", nil, "this review carries no head commit, so no CI outcome could be looked up"), nil
	}

	rows, err := h.outcomes.ListByHeadSHA(ctx, in.Task.ProjectID, job.Repo, head)
	if err != nil {
		return executor.SystemStepResult{}, fmt.Errorf("%s: read ci outcomes: %w", name, err)
	}
	if len(rows) == 0 {
		// "No CI has completed for this commit" and "CI passed" are different
		// facts, and a blank section reads as the second.
		return ciResult(head, nil, "no CI run has completed for this commit"), nil
	}
	return ciResult(head, rows, ""), nil
}

// ciResult renders the outcomes into the step result.
func ciResult(head string, rows []*persistence.ForgeCIOutcome, note string) executor.SystemStepResult {
	message := renderCIOutcomes(rows, note)
	payload := map[string]any{
		"message":  message,
		"head_sha": head,
		"runs":     len(rows),
	}
	// A structured summary alongside the prose, for a consumer that would
	// rather branch than read. Conclusions only — never the excerpt, which
	// exists in the message inside its wrapper and nowhere else.
	summaries := make([]map[string]any, 0, len(rows))
	failed := 0
	for _, r := range rows {
		if r.Failed() {
			failed++
		}
		summaries = append(summaries, map[string]any{
			"run_id":        r.RunID,
			"workflow":      r.WorkflowPath,
			"conclusion":    r.Conclusion,
			"has_content":   r.ArtifactExcerpt != "",
			"content_trunc": r.ArtifactTruncated,
		})
	}
	payload["outcomes"] = summaries
	payload["failed"] = failed

	out, _ := json.Marshal(payload)
	return executor.SystemStepResult{Result: out}
}

// renderCIOutcomes writes the human-readable CI section.
//
// The CONCLUSIONS render plainly: they are Forge's own structured record, read
// from its own database, and wrapping them would dilute the untrusted marker
// that has to mean "expect hostile text".
//
// The ARTIFACT EXCERPT is wrapped, always. Anyone who can push a branch can
// make CI print anything, so it is data rather than instructions and it says so.
func renderCIOutcomes(rows []*persistence.ForgeCIOutcome, note string) string {
	var b strings.Builder
	if note != "" {
		b.WriteString("CI: ")
		b.WriteString(note)
		b.WriteString(".\n")
		return b.String()
	}

	fmt.Fprintf(&b, "CI outcomes for this commit (%d run(s)):\n", len(rows))
	for _, r := range rows {
		fmt.Fprintf(&b, "\n- %s — %s", displayWorkflow(r), r.Conclusion)
		if len(r.Jobs) > 0 {
			b.WriteString("\n  jobs:")
			for _, j := range r.Jobs {
				fmt.Fprintf(&b, " %s=%s", j.Name, j.Conclusion)
			}
		}
		if r.ArtifactExcerpt == "" {
			continue
		}
		if r.ArtifactTruncated {
			// Stated before the content, so a reader who stops early still
			// knows the plan they are looking at is incomplete.
			fmt.Fprintf(&b, "\n  output (TRUNCATED at %d bytes — this is not the whole file):", r.ArtifactBytes)
		} else {
			b.WriteString("\n  output:")
		}
		b.WriteString("\n")
		b.WriteString(untrusted.WrapLabeled("ci_artifact", r.ArtifactExcerpt))
	}
	b.WriteString("\n")
	return b.String()
}

// displayWorkflow names a run: its workflow path when known, else its display
// name, else the run id. Never empty — an unnamed line in a list of outcomes
// reads as a rendering bug.
func displayWorkflow(r *persistence.ForgeCIOutcome) string {
	if r.WorkflowPath != "" {
		return r.WorkflowPath
	}
	if r.WorkflowName != "" {
		return r.WorkflowName
	}
	return fmt.Sprintf("run %d", r.RunID)
}
