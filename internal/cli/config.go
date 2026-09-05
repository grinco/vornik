package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
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
		Long: `Print the configuration the running daemon resolved, secrets redacted.

The dump follows a hot reload. With --provenance every key is printed with
where its value came from — file, env, placeholder, alias, default, derived,
secret_file, or unset (the zero value; nothing set it, which is how a key
written to a tree the daemon never reads looks). An origin of env_invalid
names a variable that was set and did not parse.`,
		RunE: runConfigShow,
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
	configProvenance  bool
	configTrees       bool
)

func init() {
	configShowCmd.Flags().BoolVar(&configJSON, "json", true, "JSON output (default; --json=false with --provenance prints a table)")
	configShowCmd.Flags().BoolVar(&configProvenance, "provenance", false, "Print every key with the origin and source of its value")
	configShowCmd.Flags().BoolVar(&configTrees, "trees", false, "Add the registry trees: which file supplied each project, swarm, workflow and role, and the files the loader refused")
	configReloadCmd.Flags().BoolVar(&configReloadForce, "force", false, "Reload even when validation errors are present")
	configReloadStatusCmd.Flags().BoolVar(&configJSON, "json", false, "JSON output instead of the human summary")

	configCmd.AddCommand(configShowCmd, configReloadCmd, configReloadStatusCmd)
	rootCmd.AddCommand(configCmd)
}

func runConfigShow(cmd *cobra.Command, args []string) error {
	if !configProvenance && !configTrees {
		raw, err := fetchJSON("/api/v1/config")
		if err != nil {
			return err
		}
		return prettyPrintJSON(raw)
	}
	raw, err := fetchJSON(fmt.Sprintf("/api/v1/config?provenance=%t&trees=%t", configProvenance, configTrees))
	if err != nil {
		return err
	}
	if configJSON {
		return prettyPrintJSON(raw)
	}
	var view configProvenanceView
	if err := json.Unmarshal(raw, &view); err != nil {
		return fmt.Errorf("decode provenance: %w", err)
	}
	return renderConfigProvenance(os.Stdout, &view)
}

type configProvenanceView struct {
	ConfigPath string `json:"config_path"`
	LoadedAt   string `json:"loaded_at"`
	Values     map[string]struct {
		Value  any    `json:"value"`
		Origin string `json:"origin"`
		Source string `json:"source"`
	} `json:"values"`
	Trees *struct {
		Layers  []string `json:"layers"`
		Sources []struct {
			Kind, ID, Path, Layer string
			ShadowedBy            string `json:"shadowed_by"`
		} `json:"sources"`
		Rejected []struct {
			Kind, Path, Layer, Error string
		} `json:"rejected"`
	} `json:"trees"`
}

// renderConfigProvenance prints one line per key: key, value, origin, source.
// Sorted by key so a diff between two dumps reads.
func renderConfigProvenance(out io.Writer, view *configProvenanceView) error {
	if view.ConfigPath != "" {
		_, _ = fmt.Fprintf(out, "config: %s  (loaded %s)\n", view.ConfigPath, view.LoadedAt)
	}
	if len(view.Values) > 0 {
		keys := make([]string, 0, len(view.Values))
		for k := range view.Values {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		tw := tabwriter.NewWriter(out, 2, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "KEY\tVALUE\tORIGIN\tSOURCE")
		for _, k := range keys {
			e := view.Values[k]
			val := fmt.Sprintf("%v", e.Value)
			if e.Value == nil {
				val = ""
			}
			if len(val) > 60 {
				val = val[:57] + "..."
			}
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", k, val, e.Origin, e.Source)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	if view.Trees != nil {
		_, _ = fmt.Fprintf(out, "\ntrees: %d layer(s): %s\n", len(view.Trees.Layers), strings.Join(view.Trees.Layers, " < "))
		tw := tabwriter.NewWriter(out, 2, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "KIND\tID\tFILE\tLAYER\tSHADOWED BY")
		for _, s := range view.Trees.Sources {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", s.Kind, s.ID, s.Path, s.Layer, s.ShadowedBy)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
		if len(view.Trees.Rejected) > 0 {
			_, _ = fmt.Fprintf(out, "\nrejected by the loader (%d) — written, and NOT in effect:\n", len(view.Trees.Rejected))
			for _, r := range view.Trees.Rejected {
				_, _ = fmt.Fprintf(out, "  %s %s (%s): %s\n", r.Kind, r.Path, r.Layer, r.Error)
			}
		}
	}
	return nil
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
