package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"

	"github.com/spf13/cobra"
)

// `vornikctl knowledge rollup` — did this skill make things worse?
// (LLD 2026-09-07-execution-ratings-rollup-design.md, phase 2.)
//
// Advisory. The rollup informs an operator deciding whether to retire a skill;
// nothing reads it back into behaviour.

var knowledgeRollupWindowHours int

type rollupArmWire struct {
	Coverage   float64 `json:"coverage"`
	RatedN     int     `json:"rated_n"`
	UpN        int     `json:"up_n"`
	ContestedN int     `json:"contested_n"`
}

type rollupContextWire struct {
	ProjectID  string        `json:"project_id"`
	WorkflowID string        `json:"workflow_id"`
	Verdict    string        `json:"verdict"`
	Lift       float64       `json:"lift"`
	Treatment  rollupArmWire `json:"treatment"`
	Baseline   rollupArmWire `json:"baseline"`
}

type rollupWire struct {
	SkillID     string              `json:"skill_id"`
	WindowHours int                 `json:"window_hours"`
	Summary     string              `json:"summary"`
	Contexts    []rollupContextWire `json:"contexts"`
}

var knowledgeRollupCmd = &cobra.Command{
	Use:   "rollup <skill-id>",
	Short: "Show whether a knowledge skill's outputs are rated worse than comparable ones",
	Long: `Compare how operators rated executions this skill was injected into
against comparable executions in the same project and workflow that it was not.

Reported per (project, workflow), never pooled: a digest and a code review are
not comparable outputs. Every figure is printed beside its baseline and the
coverage of both arms — a rating exists only where someone chose to write one,
so a difference between two differently-watched arms measures who was watching.

Advisory only. Nothing retires a skill on this.`,
	Args: cobra.ExactArgs(1),
	RunE: func(_ *cobra.Command, args []string) error {
		return runKnowledgeRollup(os.Stdout, args[0], knowledgeRollupWindowHours)
	},
}

// runKnowledgeRollup fetches and renders the rollup.
//
// Takes an io.Writer rather than printing to stdout so the rendering — which is
// the part that must not drop the counterfactual — is assertable directly.
func runKnowledgeRollup(out io.Writer, skillID string, windowHours int) error {
	path := "/api/v1/skills/" + skillID + "/rating-rollup"
	if windowHours > 0 {
		path += "?window_hours=" + strconv.Itoa(windowHours)
	}
	client := ClientFromEnv()
	resp, err := client.Get(path)
	if err != nil {
		return fmt.Errorf("fetch rollup: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ratingAPIError(resp)
	}

	var wire rollupWire
	if err := json.NewDecoder(resp.Body).Decode(&wire); err != nil {
		return fmt.Errorf("decode: %w", err)
	}

	_, _ = fmt.Fprintf(out, "skill %s over the last %dh: %s\n",
		wire.SkillID, wire.WindowHours, wire.Summary)

	if len(wire.Contexts) == 0 {
		// An empty table reads as "nothing wrong". Say which nothing it is.
		_, _ = fmt.Fprintf(out, "  not injected into any execution in this window, so there is "+
			"nothing to compare — this is a statement about USE, not about quality.\n")
		return nil
	}

	for _, c := range wire.Contexts {
		_, _ = fmt.Fprintf(out, "\n  %s / %s — %s\n", c.ProjectID, c.WorkflowID, c.Verdict)
		// The arms always print together. A lift figure alone is the vanity
		// metric this whole arc exists to avoid, and a renderer that shows one
		// arm is the easiest place to reintroduce it.
		_, _ = fmt.Fprintf(out, "    with this skill:    %d/%d rated up   (coverage %.0f%%%s)\n",
			c.Treatment.UpN, c.Treatment.RatedN, c.Treatment.Coverage*100,
			contestedNote(c.Treatment.ContestedN))
		_, _ = fmt.Fprintf(out, "    without it:         %d/%d rated up   (coverage %.0f%%%s)\n",
			c.Baseline.UpN, c.Baseline.RatedN, c.Baseline.Coverage*100,
			contestedNote(c.Baseline.ContestedN))
		// The difference is printed ONLY for verdicts that earned it. On
		// not_comparable and unknown the number exists and does not mean what
		// it looks like, and the review of this design made the point exactly:
		// "operators will ignore the badge and quote the number". Refusing to
		// print it is the same move the verdict itself makes.
		if c.Verdict == "low_lift" || c.Verdict == "helping" {
			_, _ = fmt.Fprintf(out, "    difference:         %+.0f percentage points\n", c.Lift*100)
		}
		if note := verdictNote(c.Verdict); note != "" {
			_, _ = fmt.Fprintf(out, "    %s\n", note)
		}
	}
	return nil
}

// contestedNote surfaces excluded executions rather than letting them vanish.
func contestedNote(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf(", %d contested and excluded", n)
}

// verdictNote says what a verdict means in the terms an operator acts on.
// `helping` gets the weakest wording it can: with this signal the ceiling is
// silence, so it can only ever mean nothing wrong was detected.
func verdictNote(verdict string) string {
	switch verdict {
	case "not_comparable":
		return "the two groups were watched at very different rates, so this " +
			"comparison would measure attention rather than quality"
	case "unknown":
		return "too few ratings in this context to say anything"
	case "low_lift":
		return "rated materially worse than comparable work — worth a look"
	case "helping":
		return "no sign it is hurting (this cannot show a benefit, only the absence of harm)"
	}
	return ""
}

func init() {
	knowledgeRollupCmd.Flags().IntVar(&knowledgeRollupWindowHours, "window-hours", 0,
		"Measurement window in hours (0 = the daemon's default of one week)")
	knowledgeCmd.AddCommand(knowledgeRollupCmd)
}
