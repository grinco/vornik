package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/spf13/cobra"
)

// `vornikctl execution rate` — the operator's way to say that was better or
// that was worse about what a run produced (LLD
// 2026-09-04-execution-ratings-design, phase 1).
//
// The verdict informs an operator deciding whether to approve or retire an
// automated change. It never gates a run: nothing in the executor, the instinct
// layer or the skill store reads a rating.

var (
	executionRateReason string
	executionRateClear  bool
)

type executionRatingWire struct {
	ExecutionID string `json:"execution_id"`
	RaterID     string `json:"rater_id"`
	Verdict     string `json:"verdict"`
	Reason      string `json:"reason,omitempty"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

var executionRateCmd = &cobra.Command{
	Use:   "rate <execution-id> [up|down]",
	Short: "Record your verdict on what an execution produced",
	Long: "Record, change or withdraw YOUR rating of an execution.\n\n" +
		"Re-rating replaces your previous verdict — an operator's first reaction\n" +
		"to a digest is not their considered one. A second operator's rating is a\n" +
		"separate row, never an overwrite.\n\n" +
		"The rater is resolved from your credentials, so the deployment needs\n" +
		"per-operator API keys; one shared service key has no identity to record.",
	Args: cobra.RangeArgs(1, 2),
	RunE: func(_ *cobra.Command, args []string) error {
		verdict := ""
		if len(args) > 1 {
			verdict = args[1]
		}
		return runExecutionRate(args[0], verdict, executionRateReason, executionRateClear)
	},
}

// runExecutionRate posts, or withdraws, the caller's rating.
//
// Both refusals below happen BEFORE the request: a typo should cost a message,
// not a round trip and a 400 that has to be read back.
func runExecutionRate(executionID, verdict, reason string, withdraw bool) error {
	if withdraw && verdict != "" {
		return fmt.Errorf("--clear withdraws your rating and a verdict records one; "+
			"pass one or the other, not both (got --clear with %q)", verdict)
	}
	client := ClientFromEnv()
	path := "/api/v1/executions/" + executionID + "/rating"

	if withdraw {
		resp, err := client.Delete(path)
		if err != nil {
			return fmt.Errorf("withdraw rating: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
			return ratingAPIError(resp)
		}
		fmt.Printf("Withdrew your rating of %s.\n", executionID)
		return nil
	}

	// Closed here as well as at the handler and in the table's CHECK. There is
	// no scale on purpose: the difference between three and four stars is not
	// a thing anyone can define.
	if verdict != "up" && verdict != "down" {
		return fmt.Errorf("verdict must be \"up\" or \"down\" (got %q); "+
			"use --clear to withdraw a rating instead", verdict)
	}
	if len(reason) > 500 {
		return fmt.Errorf("reason is %d characters; the limit is 500 and it is refused "+
			"rather than truncated, so shorten it rather than have it cut", len(reason))
	}

	resp, err := client.Post(path, map[string]any{"verdict": verdict, "reason": reason})
	if err != nil {
		return fmt.Errorf("rate execution: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ratingAPIError(resp)
	}

	var out executionRatingWire
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	fmt.Printf("Rated %s %s (as %s).\n", out.ExecutionID, out.Verdict, out.RaterID)
	if out.CreatedAt != out.UpdatedAt {
		fmt.Printf("Replaced your earlier verdict; first rated %s.\n", out.CreatedAt)
	}
	return nil
}

// ratingAPIError turns the daemon's refusal into one an operator can act on.
//
// The 401 gets its own text because it is a DEPLOYMENT SHAPE problem, not a
// mistake in the command: an API fronted by one shared service key has no
// per-caller identity to record, and the fix is per-operator keys rather than
// anything the caller can retype. Relaying "401 Unauthorized" would send them
// looking for a bad token instead.
func ratingAPIError(resp *http.Response) error {
	if resp.StatusCode == http.StatusUnauthorized {
		return errors.New("the daemon could not resolve who you are, so it refused to " +
			"record an anonymous rating. This needs a per-operator API key: a deployment " +
			"sharing one service key across users has no identity to attribute a rating to")
	}
	if resp.StatusCode == http.StatusServiceUnavailable {
		return errors.New("execution ratings are not wired on this deployment")
	}
	return ParseAPIError(resp)
}

func init() {
	executionRateCmd.Flags().StringVar(&executionRateReason, "reason", "",
		"Optional one-line reason (max 500 chars). A down-vote never requires one.")
	executionRateCmd.Flags().BoolVar(&executionRateClear, "clear", false,
		"Withdraw your rating of this execution")
	executionCmd.AddCommand(executionRateCmd)
}
