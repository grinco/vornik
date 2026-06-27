package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// modelEntryCLI mirrors the JSON shape returned by GET /api/v1/models.
// Kept private to the CLI package so the daemon owns the wire shape;
// adding a field on the daemon side surfaces here automatically when
// the JSON decoder skips unknown fields by default.
type modelEntryCLI struct {
	ID                  string  `json:"id"`
	Provider            string  `json:"provider"`
	Source              string  `json:"source"`
	OwnedBy             string  `json:"owned_by"`
	Created             int64   `json:"created"`
	Priced              bool    `json:"priced"`
	InputUSDPerMillion  float64 `json:"input_usd_per_m"`
	OutputUSDPerMillion float64 `json:"output_usd_per_m"`
	ReasoningMultiplier float64 `json:"reasoning_multiplier"`
}

type modelsResponseCLI struct {
	Models      []modelEntryCLI   `json:"models"`
	Errors      map[string]string `json:"errors"`
	PricingPath string            `json:"pricing_path"`
}

var modelsListJSON bool
var modelsListProvider string
var modelsListUnpricedOnly bool

var modelsCmd = &cobra.Command{
	Use:   "models",
	Short: "Discover and inspect chat-provider models",
	Long: `List models served by each enabled chat sub-provider.

The daemon walks every enabled chat sub-provider that supports
discovery (HTTP gateway, Vertex, claude/codex subscriptions, claude/codex
CLIs) and aggregates the results. Each row is crosswalked against
configs/pricing.yaml so you can see at a glance which models have
explicit cost entries — those without will accrue spend at the
configured 'default' rate (or zero) until pricing.yaml is updated.

Sources:
  live   — fetched from the provider's /v1/models endpoint
  static — curated list (Claude / Codex OAuth surfaces don't expose
           a public model list, so the daemon ships a hardcoded one)`,
}

var modelsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List discoverable models across all chat sub-providers",
	RunE:  runModelsList,
}

func init() {
	modelsListCmd.Flags().BoolVar(&modelsListJSON, "json", false, "Output in JSON format")
	modelsListCmd.Flags().StringVar(&modelsListProvider, "provider", "", "Filter to one sub-provider (e.g. vertex, http, claude-subscription)")
	modelsListCmd.Flags().BoolVar(&modelsListUnpricedOnly, "unpriced", false, "Only show models without a pricing.yaml entry")
	modelsCmd.AddCommand(modelsListCmd)
	rootCmd.AddCommand(modelsCmd)
}

func runModelsList(cmd *cobra.Command, args []string) error {
	client := ClientFromEnv()

	resp, err := client.Get("/api/v1/models")
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

	var report modelsResponseCLI
	if err := json.Unmarshal(body, &report); err != nil {
		return fmt.Errorf("failed to parse response: %w", err)
	}

	// Filter in place — the daemon returns the full list and the CLI
	// owns how to narrow it. Filtering server-side would multiply the
	// number of query-params for an endpoint that's barely a few KB.
	filtered := report.Models[:0]
	for _, m := range report.Models {
		if modelsListProvider != "" && m.Provider != modelsListProvider {
			continue
		}
		if modelsListUnpricedOnly && m.Priced {
			continue
		}
		filtered = append(filtered, m)
	}
	report.Models = filtered

	if modelsListJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}

	if len(report.Models) == 0 {
		fmt.Println("(no models matched the filter)")
		return nil
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "PROVIDER\tMODEL\tSOURCE\tPRICED\tINPUT $/1M\tOUTPUT $/1M\tOWNED BY")
	for _, m := range report.Models {
		priced := "no"
		input := "-"
		output := "-"
		if m.Priced {
			priced = "yes"
			input = fmt.Sprintf("%.4f", m.InputUSDPerMillion)
			out := m.OutputUSDPerMillion
			if m.ReasoningMultiplier > 1 {
				// Show the effective output rate including the
				// reasoning multiplier so cost comparisons are
				// honest — the executor charges this rate too.
				out = m.OutputUSDPerMillion * m.ReasoningMultiplier
				output = fmt.Sprintf("%.4f (×%.1f)", out, m.ReasoningMultiplier)
			} else {
				output = fmt.Sprintf("%.4f", out)
			}
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			m.Provider, m.ID, m.Source, priced, input, output, m.OwnedBy)
	}
	_ = tw.Flush()

	if len(report.Errors) > 0 {
		fmt.Println()
		fmt.Println("Sub-provider errors (other providers' results above are unaffected):")
		for name, msg := range report.Errors {
			fmt.Printf("  %-22s %s\n", name, msg)
		}
	}

	if report.PricingPath != "" {
		fmt.Println()
		fmt.Printf("Pricing source: %s\n", report.PricingPath)
	} else {
		fmt.Println()
		fmt.Println("Pricing source: (none — daemon has no pricing.yaml configured)")
	}

	return nil
}
