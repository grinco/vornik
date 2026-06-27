package cli

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

var (
	instinctListDomain  string
	instinctListScope   string
	instinctListProject string
	instinctListStatus  string
	instinctListMinConf float64
	instinctListLimit   int
	instinctListJSON    bool

	instinctListCmd = &cobra.Command{
		Use:   "list",
		Short: "List instincts (filterable by domain, scope, project, status, confidence)",
		Long: `Show instincts from the running daemon, highest confidence first.

Filters compose with AND. Default limit 100; max 1000.

  vornikctl instinct list --domain recovery --status active
  vornikctl instinct list --project assistant --min-confidence 0.6`,
		RunE: runInstinctList,
	}

	instinctShowJSON   bool
	instinctRetireJSON bool

	instinctShowCmd = &cobra.Command{
		Use:   "show <id>",
		Short: "Show a single instinct by id",
		Args:  cobra.ExactArgs(1),
		RunE:  runInstinctShow,
	}

	instinctRetireCmd = &cobra.Command{
		Use:   "retire <id>",
		Short: "Retire an instinct (advisory — it stays for audit)",
		Long: `Flip an instinct to status=retired. Advisory only: the row stays
for audit and nothing about agent behaviour changes — it is simply
removed from the advisory surfaces. Idempotent.`,
		Args: cobra.ExactArgs(1),
		RunE: runInstinctRetire,
	}
)

func init() {
	instinctListCmd.Flags().StringVar(&instinctListDomain, "domain", "", "Filter by domain (recovery|cost|quality|retrieval|workflow)")
	instinctListCmd.Flags().StringVar(&instinctListScope, "scope", "", "Filter by scope (project|global)")
	instinctListCmd.Flags().StringVar(&instinctListProject, "project", "", "Filter by project ID")
	instinctListCmd.Flags().StringVar(&instinctListStatus, "status", "", "Filter by status (candidate|active|promoted|retired)")
	instinctListCmd.Flags().Float64Var(&instinctListMinConf, "min-confidence", 0, "Only instincts with confidence >= this (0-1)")
	instinctListCmd.Flags().IntVarP(&instinctListLimit, "limit", "n", 100, "Maximum rows to return (1-1000)")
	instinctListCmd.Flags().BoolVar(&instinctListJSON, "json", false, "Output JSON instead of table")

	instinctShowCmd.Flags().BoolVar(&instinctShowJSON, "json", false, "Output JSON instead of human-readable")
	instinctRetireCmd.Flags().BoolVar(&instinctRetireJSON, "json", false, "Output JSON instead of human-readable")

	instinctCmd.AddCommand(instinctListCmd)
	instinctCmd.AddCommand(instinctShowCmd)
	instinctCmd.AddCommand(instinctRetireCmd)
}

// instinctListQuery builds the query string shared by `list` and
// `export`. Exported helper keeps the filter-flag → query mapping in
// one place.
func instinctListQuery(domain, scope, project, status string, minConf float64, limit int) string {
	q := url.Values{}
	if domain != "" {
		q.Set("domain", domain)
	}
	if scope != "" {
		q.Set("scope", scope)
	}
	if project != "" {
		q.Set("project", project)
	}
	if status != "" {
		q.Set("status", status)
	}
	if minConf > 0 {
		q.Set("min_confidence", strconv.FormatFloat(minConf, 'f', -1, 64))
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	path := "/api/v1/instincts"
	if enc := q.Encode(); enc != "" {
		path += "?" + enc
	}
	return path
}

// fetchInstincts performs the GET and decodes the list response.
// Shared by `list` and `export`.
func fetchInstincts(path string) ([]instinctEntry, error) {
	client := ClientFromEnv()
	resp, err := client.Get(path)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		return nil, ParseAPIError(resp)
	}
	var out instinctListResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode failed: %w", err)
	}
	return out.Instincts, nil
}

func runInstinctList(_ *cobra.Command, _ []string) error {
	path := instinctListQuery(instinctListDomain, instinctListScope, instinctListProject,
		instinctListStatus, instinctListMinConf, instinctListLimit)
	rows, err := fetchInstincts(path)
	if err != nil {
		return fmt.Errorf("instinct list: %w", err)
	}
	if instinctListJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(instinctListResponse{Instincts: rows})
	}
	if len(rows) == 0 {
		fmt.Println("No instincts match the filter.")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer func() { _ = tw.Flush() }()
	if _, err := fmt.Fprintln(tw, "ID\tDOMAIN\tSCOPE\tSTATUS\tCONF\t+/-\tACTION"); err != nil {
		return err
	}
	for _, e := range rows {
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%.2f\t%d/%d\t%s\n",
			truncate(e.ID, 24),
			e.Domain,
			e.Scope,
			e.Status,
			e.Confidence,
			e.SupportCount, e.ContradictCount,
			truncate(e.Action, 48)); err != nil {
			return err
		}
	}
	return nil
}

func runInstinctShow(_ *cobra.Command, args []string) error {
	id := args[0]
	client := ClientFromEnv()
	resp, err := client.Get("/api/v1/instincts/" + url.PathEscape(id))
	if err != nil {
		return fmt.Errorf("instinct show: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		return ParseAPIError(resp)
	}
	var out instinctShowResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("instinct show: decode failed: %w", err)
	}
	if instinctShowJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out.Instinct)
	}
	printInstinctRow(out.Instinct)
	return nil
}

func runInstinctRetire(_ *cobra.Command, args []string) error {
	id := args[0]
	client := ClientFromEnv()
	resp, err := client.Post("/api/v1/instincts/"+url.PathEscape(id)+"/retire", nil)
	if err != nil {
		return fmt.Errorf("instinct retire: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		return ParseAPIError(resp)
	}
	var out map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("instinct retire: decode failed: %w", err)
	}
	if instinctRetireJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	fmt.Printf("Retired %s. New status: %s\n", out["id"], out["status"])
	return nil
}

func printInstinctRow(e instinctEntry) {
	fmt.Printf("ID:               %s\n", e.ID)
	fmt.Printf("Domain:           %s\n", e.Domain)
	fmt.Printf("Scope:            %s\n", e.Scope)
	if e.ProjectID != "" {
		fmt.Printf("Project:          %s\n", e.ProjectID)
	}
	fmt.Printf("Status:           %s\n", e.Status)
	fmt.Printf("Confidence:       %.4f\n", e.Confidence)
	fmt.Printf("Support / Contra: %d / %d\n", e.SupportCount, e.ContradictCount)
	fmt.Printf("Source:           %s\n", e.Source)
	if e.DistillModel != "" {
		fmt.Printf("Distill model:    %s\n", e.DistillModel)
	}
	fmt.Printf("Trigger key:      %s\n", e.TriggerKey)
	if e.Trigger != "" {
		fmt.Printf("Trigger:          %s\n", e.Trigger)
	}
	fmt.Printf("Action:           %s\n", e.Action)
	fmt.Printf("Created:          %s\n", e.CreatedAt)
	fmt.Printf("Updated:          %s\n", e.UpdatedAt)
	fmt.Printf("Last seen:        %s\n", e.LastSeenAt)
}
