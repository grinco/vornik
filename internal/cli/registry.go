package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// Registry-introspection CLI surface. Reads the daemon's in-memory
// registry via GET /api/v1/{projects,swarms,workflows}. Without these
// the only way to see what the daemon is serving was tailing journald
// or reading YAML on disk.

var (
	projectCmd = &cobra.Command{
		Use:   "project",
		Short: "Inspect projects loaded by the daemon",
	}
	projectListCmd = &cobra.Command{
		Use:   "list",
		Short: "List projects the daemon is serving",
		RunE:  runProjectList,
	}
	projectShowCmd = &cobra.Command{
		Use:   "show <projectId>",
		Short: "Show one project's full config",
		Args:  cobra.ExactArgs(1),
		RunE:  runProjectShow,
	}

	swarmCmd = &cobra.Command{
		Use:   "swarm",
		Short: "Inspect swarm definitions",
	}
	swarmListCmd = &cobra.Command{
		Use:   "list",
		Short: "List swarms",
		RunE:  runSwarmList,
	}
	swarmShowCmd = &cobra.Command{
		Use:   "show <swarmId>",
		Short: "Show one swarm's full definition (roles, prompts, permissions)",
		Args:  cobra.ExactArgs(1),
		RunE:  runSwarmShow,
	}

	workflowCmd = &cobra.Command{
		Use:   "workflow",
		Short: "Inspect workflow definitions",
	}
	workflowListCmd = &cobra.Command{
		Use:   "list",
		Short: "List workflows",
		RunE:  runWorkflowList,
	}
	workflowShowCmd = &cobra.Command{
		Use:   "show <workflowId>",
		Short: "Show one workflow's full definition",
		Args:  cobra.ExactArgs(1),
		RunE:  runWorkflowShow,
	}

	registryJSON bool
)

func init() {
	for _, c := range []*cobra.Command{
		projectListCmd, projectShowCmd,
		swarmListCmd, swarmShowCmd,
		workflowListCmd, workflowShowCmd,
	} {
		c.Flags().BoolVar(&registryJSON, "json", false, "Output in JSON format")
	}
	projectCmd.AddCommand(projectListCmd, projectShowCmd)
	swarmCmd.AddCommand(swarmListCmd, swarmShowCmd)
	workflowCmd.AddCommand(workflowListCmd, workflowShowCmd)
	rootCmd.AddCommand(projectCmd, swarmCmd, workflowCmd)
}

// --- project --------------------------------------------------------------

type projectSummary struct {
	ProjectID         string `json:"projectId"`
	DisplayName       string `json:"displayName"`
	SwarmID           string `json:"swarmId"`
	DefaultWorkflowID string `json:"defaultWorkflowId"`
	AutonomyEnabled   bool   `json:"autonomyEnabled"`
}

type projectListResp struct {
	Projects []projectSummary `json:"projects"`
	Total    int              `json:"total"`
}

func runProjectList(cmd *cobra.Command, args []string) error {
	raw, err := fetchJSON("/api/v1/projects")
	if err != nil {
		return err
	}
	if registryJSON {
		return passthroughJSON(raw)
	}
	var wrap projectListResp
	if err := json.Unmarshal(raw, &wrap); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}
	sort.Slice(wrap.Projects, func(i, j int) bool { return wrap.Projects[i].ProjectID < wrap.Projects[j].ProjectID })
	tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "PROJECT ID\tDISPLAY NAME\tSWARM\tWORKFLOW\tAUTONOMY")
	for _, p := range wrap.Projects {
		autonomy := "off"
		if p.AutonomyEnabled {
			autonomy = "on"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", p.ProjectID, p.DisplayName, p.SwarmID, p.DefaultWorkflowID, autonomy)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Printf("\nTotal: %d\n", wrap.Total)
	return nil
}

func runProjectShow(cmd *cobra.Command, args []string) error {
	projectID := args[0]
	raw, err := fetchJSON("/api/v1/projects/" + projectID + "/config")
	if err != nil {
		return err
	}
	// Always pretty-print — show is the "full detail" command, so the
	// tabular fallback would just throw away information.
	return prettyPrintJSON(raw)
}

// --- swarm -----------------------------------------------------------------

type swarmSummary struct {
	SwarmID     string   `json:"swarmId"`
	DisplayName string   `json:"displayName"`
	LeadRole    string   `json:"leadRole"`
	Roles       []string `json:"roles"`
}

type swarmListResp struct {
	Swarms []swarmSummary `json:"swarms"`
	Total  int            `json:"total"`
}

func runSwarmList(cmd *cobra.Command, args []string) error {
	raw, err := fetchJSON("/api/v1/swarms")
	if err != nil {
		return err
	}
	if registryJSON {
		return passthroughJSON(raw)
	}
	var wrap swarmListResp
	if err := json.Unmarshal(raw, &wrap); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}
	sort.Slice(wrap.Swarms, func(i, j int) bool { return wrap.Swarms[i].SwarmID < wrap.Swarms[j].SwarmID })
	tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "SWARM ID\tDISPLAY NAME\tLEAD\tROLES")
	for _, sw := range wrap.Swarms {
		rolesLabel := fmt.Sprintf("%d", len(sw.Roles))
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", sw.SwarmID, sw.DisplayName, sw.LeadRole, rolesLabel)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Printf("\nTotal: %d\n", wrap.Total)
	return nil
}

func runSwarmShow(cmd *cobra.Command, args []string) error {
	raw, err := fetchJSON("/api/v1/swarms/" + args[0])
	if err != nil {
		return err
	}
	return prettyPrintJSON(raw)
}

// --- workflow --------------------------------------------------------------

type workflowSummary struct {
	WorkflowID  string   `json:"workflowId"`
	DisplayName string   `json:"displayName"`
	Steps       []string `json:"steps"`
}

type workflowListResp struct {
	Workflows []workflowSummary `json:"workflows"`
	Total     int               `json:"total"`
}

func runWorkflowList(cmd *cobra.Command, args []string) error {
	raw, err := fetchJSON("/api/v1/workflows")
	if err != nil {
		return err
	}
	if registryJSON {
		return passthroughJSON(raw)
	}
	var wrap workflowListResp
	if err := json.Unmarshal(raw, &wrap); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}
	sort.Slice(wrap.Workflows, func(i, j int) bool { return wrap.Workflows[i].WorkflowID < wrap.Workflows[j].WorkflowID })
	tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "WORKFLOW ID\tDISPLAY NAME\tSTEPS")
	for _, wf := range wrap.Workflows {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%d\n", wf.WorkflowID, wf.DisplayName, len(wf.Steps))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Printf("\nTotal: %d\n", wrap.Total)
	return nil
}

func runWorkflowShow(cmd *cobra.Command, args []string) error {
	raw, err := fetchJSON("/api/v1/workflows/" + args[0])
	if err != nil {
		return err
	}
	return prettyPrintJSON(raw)
}

// --- helpers ---------------------------------------------------------------

// fetchJSON wraps the common GET → parse-error → return-bytes dance.
// Using []byte rather than streaming the decoder keeps every caller's
// error reporting consistent (the shared ParseAPIError sees the full
// body) and lets --json mode just reprint the server response verbatim.
func fetchJSON(path string) ([]byte, error) {
	client := ClientFromEnv()
	resp, err := client.Get(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, ParseAPIError(resp)
	}
	return io.ReadAll(resp.Body)
}

// postJSON POSTs body (JSON-encoded) to path, returns the response
// body when the server returned 2xx, or a parsed APIError otherwise.
// Mirrors fetchJSON's shape so callers compose the two trivially.
func postJSON(path string, body interface{}) ([]byte, error) {
	client := ClientFromEnv()
	resp, err := client.Post(path, body)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, ParseAPIError(resp)
	}
	return io.ReadAll(resp.Body)
}

func passthroughJSON(raw []byte) error {
	// Re-encode with indentation so --json output is always operator-readable
	// even when the server emits compact bytes.
	return prettyPrintJSON(raw)
}

func prettyPrintJSON(raw []byte) error {
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		// Not JSON — fall back to raw bytes rather than an error; the
		// server might have returned plain text for some reason and the
		// operator still wants to see it.
		_, werr := os.Stdout.Write(raw)
		if werr == nil {
			_, _ = os.Stdout.Write([]byte{'\n'})
		}
		return werr
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(generic)
}
