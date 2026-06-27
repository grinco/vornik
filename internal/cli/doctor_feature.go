package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// featureStatusDTO mirrors api.featureStatusDTO for JSON decoding.
type featureStatusDTO struct {
	ID      string            `json:"id"`
	Title   string            `json:"title"`
	Summary string            `json:"summary"`
	Status  string            `json:"status"`
	GatesOn bool              `json:"gates_on"`
	Prereqs []prereqResultDTO `json:"prereqs"`
	Verify  *prereqResultDTO  `json:"verify,omitempty"`
}

// prereqResultDTO mirrors api.prereqResultDTO.
type prereqResultDTO struct {
	Name        string `json:"name"`
	OK          bool   `json:"ok"`
	Fixable     bool   `json:"fixable"`
	Detail      string `json:"detail,omitempty"`
	Remediation string `json:"remediation,omitempty"`
}

var doctorFeatureJSONFlag bool

var doctorFeatureCmd = &cobra.Command{
	Use:   "feature [id]",
	Short: "Show feature-doctor diagnoses (all features, or one by id)",
	Long: `Query the daemon's feature-doctor surface and render a status table.

Without an id argument, all registered features are listed with their
current status (ok/ready/blocked/degraded/unknown).

With an id argument, full prereq detail (including remediation hints for
unmet prereqs) is shown for that feature.

Exit code 1 when any feature is blocked or degraded.`,
	// SilenceErrors prevents cobra from printing "Error: " when RunE
	// returns a featureExitError (whose Error() is ""). SilenceUsage
	// suppresses the usage printout on the same path — the blocked/
	// degraded exit-1 path is not a usage mistake.
	SilenceErrors: true,
	SilenceUsage:  true,
	RunE:          runDoctorFeature,
}

func init() {
	doctorFeatureCmd.Flags().BoolVar(&doctorFeatureJSONFlag, "json", false, "Output in JSON format")
	doctorCmd.AddCommand(doctorFeatureCmd)
}

func runDoctorFeature(cmd *cobra.Command, args []string) error {
	client := ClientFromEnv()

	if len(args) > 0 {
		return runDoctorFeatureSingle(client, args[0])
	}
	return runDoctorFeatureList(client)
}

func runDoctorFeatureList(client *Client) error {
	resp, err := client.Get("/api/v1/doctor/features")
	if err != nil {
		return fmt.Errorf("failed to connect to vornik: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return ParseAPIError(resp)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response: %w", err)
	}

	var features []featureStatusDTO
	if err := json.Unmarshal(body, &features); err != nil {
		return fmt.Errorf("failed to parse response: %w", err)
	}

	if doctorFeatureJSONFlag {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(features)
	}

	fmt.Println(renderFeatureTable(features))

	// Exit code 1 if any feature is blocked or degraded.
	for _, f := range features {
		switch strings.ToLower(f.Status) {
		case "blocked", "degraded":
			return &featureExitError{code: 1}
		}
	}
	return nil
}

func runDoctorFeatureSingle(client *Client, id string) error {
	resp, err := client.Get("/api/v1/doctor/features/" + id)
	if err != nil {
		return fmt.Errorf("failed to connect to vornik: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return ParseAPIError(resp)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response: %w", err)
	}

	var f featureStatusDTO
	if err := json.Unmarshal(body, &f); err != nil {
		return fmt.Errorf("failed to parse response: %w", err)
	}

	if doctorFeatureJSONFlag {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(f)
	}

	fmt.Print(renderFeatureDetail(f))

	switch strings.ToLower(f.Status) {
	case "blocked", "degraded":
		return &featureExitError{code: 1}
	}
	return nil
}

// renderFeatureTable renders an aligned status table for a slice of features.
// Pure function — no I/O. Returns the formatted string.
func renderFeatureTable(features []featureStatusDTO) string {
	if len(features) == 0 {
		return "(no features registered)\n"
	}
	var sb strings.Builder
	// Header.
	fmt.Fprintf(&sb, "%-12s  %-36s  %s\n", "STATUS", "FEATURE", "SUMMARY")
	sb.WriteString(strings.Repeat("-", 80) + "\n")
	for _, f := range features {
		icon := featureStatusIcon(f.Status)
		// Truncate summary to 40 chars for the table.
		summary := f.Summary
		if len(summary) > 40 {
			summary = summary[:37] + "..."
		}
		fmt.Fprintf(&sb, "%-12s  %-36s  %s\n", icon+" "+f.Status, f.Title, summary)
	}
	return sb.String()
}

// renderFeatureDetail renders full detail for one feature including
// per-prereq remediation hints.
func renderFeatureDetail(f featureStatusDTO) string {
	var sb strings.Builder
	icon := featureStatusIcon(f.Status)
	fmt.Fprintf(&sb, "%s  %s  [%s]\n", icon, f.Title, f.Status)
	sb.WriteString("    " + f.Summary + "\n")
	if len(f.Prereqs) > 0 {
		sb.WriteString("\nPrereqs:\n")
		for _, p := range f.Prereqs {
			mark := "OK "
			if !p.OK {
				mark = "!  "
			}
			line := fmt.Sprintf("  %s  %s", mark, p.Name)
			if p.Detail != "" {
				line += " — " + p.Detail
			}
			sb.WriteString(line + "\n")
			if !p.OK && p.Remediation != "" {
				sb.WriteString("       Remediation: " + p.Remediation + "\n")
			}
		}
	}
	if f.Verify != nil {
		sb.WriteString("\nVerify:\n")
		mark := "OK "
		if !f.Verify.OK {
			mark = "!  "
		}
		fmt.Fprintf(&sb, "  %s  %s\n", mark, f.Verify.Detail)
		if !f.Verify.OK && f.Verify.Remediation != "" {
			sb.WriteString("       Remediation: " + f.Verify.Remediation + "\n")
		}
	}
	return sb.String()
}

// featureStatusIcon returns a short status indicator for rendering.
func featureStatusIcon(status string) string {
	switch strings.ToLower(status) {
	case "ok":
		return "OK "
	case "ready":
		return "RDY"
	case "blocked":
		return "BLK"
	case "degraded":
		return "DGR"
	case "disabled":
		return "OFF"
	default:
		return "?  "
	}
}

// featureExitError carries a non-zero exit code. RunE returning a non-nil
// error causes cobra to print the error message; we want a silent exit so
// we implement Error() as empty — cobra still sets the exit code from the
// RunE return value when SilenceErrors is true on the parent, otherwise
// the message is omitted via the empty string.
type featureExitError struct{ code int }

func (e *featureExitError) Error() string { return "" }
