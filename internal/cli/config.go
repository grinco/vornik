package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

// Config management CLI. Three commands:
//   - config show    — dump effective daemon config (secrets redacted)
//   - config reload  — trigger manual reload (POST /api/v1/config/reload)
//   - config reload-status — last reload/timestamp + validation errors
//
// Together they close the "operator has to SSH in and SIGHUP" loop
// that used to be the only way to poke live state.

var (
	configCmd = &cobra.Command{
		Use:   "config",
		Short: "Inspect and control daemon configuration",
	}
	configShowCmd = &cobra.Command{
		Use:   "show",
		Short: "Dump effective daemon config (secrets redacted)",
		RunE:  runConfigShow,
	}
	configReloadCmd = &cobra.Command{
		Use:   "reload",
		Short: "Trigger a configuration reload",
		Long: `Trigger a manual reload of the daemon config and registry. Equivalent
to sending SIGHUP to the vornik process. Useful when the file watcher
doesn't pick up a change (network filesystem, edit-in-place with
overwrite semantics, etc).`,
		RunE: runConfigReload,
	}
	configReloadStatusCmd = &cobra.Command{
		Use:   "reload-status",
		Short: "Show last reload outcome and any validation errors",
		RunE:  runConfigReloadStatus,
	}

	configReloadForce bool
	configJSON        bool
)

func init() {
	configShowCmd.Flags().BoolVar(&configJSON, "json", true, "JSON output (the only supported shape — default)")
	configReloadCmd.Flags().BoolVar(&configReloadForce, "force", false, "Reload even when validation errors are present")
	configReloadStatusCmd.Flags().BoolVar(&configJSON, "json", false, "JSON output instead of the human summary")

	configCmd.AddCommand(configShowCmd, configReloadCmd, configReloadStatusCmd)
	rootCmd.AddCommand(configCmd)
}

func runConfigShow(cmd *cobra.Command, args []string) error {
	raw, err := fetchJSON("/api/v1/config")
	if err != nil {
		return err
	}
	return prettyPrintJSON(raw)
}

func runConfigReload(cmd *cobra.Command, args []string) error {
	client := ClientFromEnv()
	body := map[string]bool{"force": configReloadForce}
	resp, err := client.Post("/api/v1/config/reload", body)
	if err != nil {
		return fmt.Errorf("reload: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		// Surface whatever the server said, preserving the body so
		// validation errors are visible.
		return &APIError{StatusCode: resp.StatusCode, Message: string(raw)}
	}

	var parsed struct {
		Success   bool   `json:"success"`
		Message   string `json:"message"`
		Timestamp string `json:"timestamp"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		// Print whatever we got if it's not the expected shape.
		return prettyPrintJSON(raw)
	}
	if parsed.Success {
		fmt.Printf("reload ok at %s\n", parsed.Timestamp)
	} else {
		fmt.Printf("reload failed at %s: %s\n", parsed.Timestamp, parsed.Message)
	}
	return nil
}

func runConfigReloadStatus(cmd *cobra.Command, args []string) error {
	raw, err := fetchJSON("/api/v1/config/reload-status")
	if err != nil {
		return err
	}
	if configJSON {
		return prettyPrintJSON(raw)
	}
	var parsed struct {
		LastReload        string   `json:"last_reload"`
		LastAttempt       string   `json:"last_attempt"`
		Errors            []string `json:"errors"`
		HasErrors         bool     `json:"has_errors"`
		PendingActivation bool     `json:"pending_activation"`
		Blocked           bool     `json:"blocked"`
		BlockedReason     string   `json:"blocked_reason"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}
	tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintf(tw, "Last reload:\t%s\n", formatTimeOrDash(parsed.LastReload))
	_, _ = fmt.Fprintf(tw, "Last attempt:\t%s\n", formatTimeOrDash(parsed.LastAttempt))
	_, _ = fmt.Fprintf(tw, "Pending activation:\t%v\n", parsed.PendingActivation)
	_, _ = fmt.Fprintf(tw, "Blocked:\t%v\n", parsed.Blocked)
	if parsed.BlockedReason != "" {
		_, _ = fmt.Fprintf(tw, "Blocked reason:\t%s\n", parsed.BlockedReason)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if parsed.HasErrors && len(parsed.Errors) > 0 {
		fmt.Println("\nValidation errors:")
		for _, e := range parsed.Errors {
			fmt.Println("  -", e)
		}
	}
	return nil
}

// formatTimeOrDash prints an RFC3339 timestamp as "<t> (<relative>)" or
// a "—" placeholder when the timestamp is empty. Keeps the reload-status
// view readable at a glance.
func formatTimeOrDash(s string) string {
	if s == "" {
		return "—"
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return fmt.Sprintf("%s (%s ago)", s, time.Since(t).Round(time.Second))
	}
	return s
}
