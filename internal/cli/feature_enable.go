package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/spf13/cobra"
)

// enablePlanDTO mirrors api.enablePlanDTO for JSON decoding.
type enablePlanDTO struct {
	FeatureID string          `json:"feature_id"`
	Changes   []gateChangeDTO `json:"changes"`
	Apply     string          `json:"apply"`
}

// gateChangeDTO mirrors api.gateChangeDTO.
type gateChangeDTO struct {
	Key  string `json:"key"`
	From any    `json:"from"`
	To   any    `json:"to"`
}

// enableResultDTO mirrors api.enableResultDTO.
type enableResultDTO struct {
	FeatureID string `json:"feature_id"`
	OK        bool   `json:"ok"`
	Detail    string `json:"detail,omitempty"`
}

var featureEnableApply bool

var featureEnableCmd = &cobra.Command{
	Use:   "enable <id>",
	Short: "Propose (or apply) enabling a feature via the feature doctor",
	Long: `Compute the gate changes required to enable a feature and optionally apply them.

Without --apply (dry-run): prints the proposed gate changes and apply
mechanism. No config is mutated.

With --apply: sends the changes to the daemon, which:
  1. Backs up config.yaml
  2. Writes all gate changes (comment-preserving)
  3. Triggers a config reload
  4. Runs the feature's verify check
  5. Rolls back to the backup on any failure

This command requires admin credentials (VORNIK_API_KEY must be an admin key
when auth is enabled).`,
	Args: cobra.ExactArgs(1),
	// SilenceErrors/SilenceUsage: the non-zero exit on verify failure is not
	// a usage mistake; suppress cobra's "Error: " prefix.
	SilenceErrors: true,
	SilenceUsage:  true,
	RunE:          runFeatureEnable,
}

func init() {
	featureEnableCmd.Flags().BoolVar(&featureEnableApply, "apply", false,
		"Apply the gate changes (default: dry-run only)")
	doctorFeatureCmd.AddCommand(featureEnableCmd)
}

func runFeatureEnable(cmd *cobra.Command, args []string) error {
	id := args[0]
	client := ClientFromEnv()

	body := map[string]bool{"apply": featureEnableApply}
	resp, err := client.Post("/api/v1/doctor/features/"+id+"/enable", body)
	if err != nil {
		return fmt.Errorf("failed to connect to vornik: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return ParseAPIError(resp)
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response: %w", err)
	}

	if !featureEnableApply {
		// Dry-run: decode and render the plan.
		var plan enablePlanDTO
		if err := json.Unmarshal(raw, &plan); err != nil {
			return fmt.Errorf("failed to parse plan: %w", err)
		}
		fmt.Print(renderEnablePlan(plan))
		return nil
	}

	// Apply: decode and render the result.
	var result enableResultDTO
	if err := json.Unmarshal(raw, &result); err != nil {
		return fmt.Errorf("failed to parse result: %w", err)
	}
	fmt.Print(renderEnableResult(result))
	if !result.OK {
		return &featureExitError{code: 1}
	}
	return nil
}

// renderEnablePlan formats a dry-run plan for human consumption.
// Pure function; returned string ends with a newline.
func renderEnablePlan(plan enablePlanDTO) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Feature %q — dry-run enable plan\n", plan.FeatureID)
	fmt.Fprintf(&sb, "Apply mechanism: %s\n", plan.Apply)
	if len(plan.Changes) == 0 {
		sb.WriteString("  (no gate changes needed — all gates already at target values)\n")
	} else {
		sb.WriteString("Gate changes:\n")
		for _, ch := range plan.Changes {
			fmt.Fprintf(&sb, "  %-50s  %v  →  %v\n", ch.Key, ch.From, ch.To)
		}
	}
	if plan.Apply == "restart_required" {
		sb.WriteString("\nRun with --apply to write changes to config.yaml.\n")
		sb.WriteString("A daemon restart will be required to activate the feature (config.yaml is not hot-reloaded).\n")
	} else {
		sb.WriteString("\nRun with --apply to write changes to config.yaml and reload the daemon.\n")
	}
	return sb.String()
}

// renderEnableResult formats an apply result for human consumption.
func renderEnableResult(result enableResultDTO) string {
	var sb strings.Builder
	if result.OK {
		// Check for restart-required detail to show the correct message.
		if strings.Contains(result.Detail, "restart vornik to apply") {
			fmt.Fprintf(&sb, "OK  Feature %q: config written.\n", result.FeatureID)
		} else {
			fmt.Fprintf(&sb, "OK  Feature %q enabled and verified.\n", result.FeatureID)
		}
	} else {
		fmt.Fprintf(&sb, "!   Feature %q enable attempted but verify failed.\n", result.FeatureID)
	}
	if result.Detail != "" {
		fmt.Fprintf(&sb, "    %s\n", result.Detail)
	}
	return sb.String()
}
