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

// MCP debug CLI — read-only list of a project's MCP tools and a
// one-shot invocation path (no dispatcher loop, no LLM cost) for
// validating that the scraper / gmail / github / whatever backend is
// actually wired up. Until this landed, you had to stand up a full
// agent container to test a single MCP round-trip.

var (
	mcpCmd = &cobra.Command{
		Use:   "mcp",
		Short: "Inspect and call MCP tools wired to a project",
	}
	mcpToolsCmd = &cobra.Command{
		Use:   "tools",
		Short: "List tools a project's MCP servers advertise",
		RunE:  runMCPTools,
	}
	mcpServersCmd = &cobra.Command{
		Use:   "servers",
		Short: "List the daemon-level MCP server inventory",
		Long: `List every MCP server declared at the daemon level (top-level
mcp.servers block) with its reachability state and the tool catalog
each advertises. Listing a server here does NOT grant any project
access to its tools — that still requires editing the project's own
mcp.servers list.`,
		RunE: runMCPServers,
	}
	mcpCallCmd = &cobra.Command{
		Use:   "call",
		Short: "Invoke one MCP tool directly (debug path; skips the LLM)",
		Long: `Invoke one MCP tool by its qualified name (mcp__{server}__{tool}) and
print the result. Arguments are a JSON object supplied via --args. Use
this to prove a scraper / gmail / github connection is live without
spinning up an agent container and paying for an LLM call.

Examples:
  vornikctl mcp tools -p janka
  vornikctl mcp servers
  vornikctl mcp call -p janka --tool mcp__scraper__web_fetch \
      --args '{"url":"https://example.com","project_id":"janka","allowed_hosts":["*"],"text_only":true,"max_bytes":2000}'`,
		RunE: runMCPCall,
	}

	mcpProject string
	mcpTool    string
	mcpArgs    string
	mcpJSON    bool
)

func init() {
	mcpToolsCmd.Flags().StringVarP(&mcpProject, "project", "p", "", "Project ID (required)")
	mcpToolsCmd.Flags().BoolVar(&mcpJSON, "json", false, "JSON output instead of the table")
	_ = mcpToolsCmd.MarkFlagRequired("project")

	mcpCallCmd.Flags().StringVarP(&mcpProject, "project", "p", "", "Project ID (required)")
	mcpCallCmd.Flags().StringVar(&mcpTool, "tool", "", "Qualified tool name: mcp__{server}__{tool} (required)")
	mcpCallCmd.Flags().StringVar(&mcpArgs, "args", "{}", "Arguments as a JSON object (default \"{}\")")
	mcpCallCmd.Flags().BoolVar(&mcpJSON, "json", false, "JSON output instead of the plain-text response body")
	_ = mcpCallCmd.MarkFlagRequired("project")
	_ = mcpCallCmd.MarkFlagRequired("tool")

	mcpServersCmd.Flags().BoolVar(&mcpJSON, "json", false, "JSON output instead of the table")

	mcpCmd.AddCommand(mcpToolsCmd, mcpCallCmd, mcpServersCmd)
	rootCmd.AddCommand(mcpCmd)
}

type mcpToolDescriptor struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type mcpToolsResp struct {
	Tools []mcpToolDescriptor `json:"tools"`
}

func runMCPTools(cmd *cobra.Command, args []string) error {
	raw, err := fetchJSON(fmt.Sprintf("/api/v1/projects/%s/mcp/tools", mcpProject))
	if err != nil {
		return err
	}
	if mcpJSON {
		return prettyPrintJSON(raw)
	}
	var resp mcpToolsResp
	if err := json.Unmarshal(raw, &resp); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}
	tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "TOOL\tDESCRIPTION")
	for _, t := range resp.Tools {
		desc := t.Function.Description
		if len(desc) > 80 {
			desc = desc[:77] + "..."
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\n", t.Function.Name, desc)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Printf("\nTotal: %d\n", len(resp.Tools))
	return nil
}

// mcpServersDescriptor is one row in the /api/v1/mcp/servers
// response. Field shape mirrors the API handler (see
// internal/api/mcp_servers_handler.go). Kept local to the CLI so
// the cli package doesn't import internal/api.
type mcpServersDescriptor struct {
	Name      string `json:"name"`
	Transport string `json:"transport"`
	URL       string `json:"url,omitempty"`
	Command   string `json:"command,omitempty"`
	Reachable bool   `json:"reachable"`
	Tools     []struct {
		Name        string `json:"name"`
		Description string `json:"description,omitempty"`
	} `json:"tools"`
	Error         string `json:"error,omitempty"`
	LastCheckedAt string `json:"last_checked_at"`
}

type mcpServersResp struct {
	Servers []mcpServersDescriptor `json:"servers"`
}

func runMCPServers(cmd *cobra.Command, args []string) error {
	raw, err := fetchJSON("/api/v1/mcp/servers")
	if err != nil {
		return err
	}
	if mcpJSON {
		return prettyPrintJSON(raw)
	}
	var resp mcpServersResp
	if err := json.Unmarshal(raw, &resp); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}
	tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "SERVER\tTRANSPORT\tSTATUS\tTOOLS\tENDPOINT")
	for _, s := range resp.Servers {
		endpoint := s.URL
		if endpoint == "" {
			endpoint = s.Command
		}
		status := "reachable"
		if !s.Reachable {
			status = "unreachable"
			if s.Error != "" {
				// Cap the error so it doesn't blow up the table layout
				// — operators with --json get the full string.
				e := s.Error
				if len(e) > 50 {
					e = e[:47] + "..."
				}
				status = "unreachable: " + e
			}
		}
		toolCount := "—"
		if s.Reachable {
			toolCount = fmt.Sprintf("%d", len(s.Tools))
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			s.Name, s.Transport, status, toolCount, endpoint)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Printf("\nTotal: %d server(s) configured at daemon scope\n", len(resp.Servers))
	return nil
}

func runMCPCall(cmd *cobra.Command, args []string) error {
	// Validate args JSON up front so bad input fails fast instead of
	// getting bounced back from the MCP server with an opaque error.
	if !json.Valid([]byte(mcpArgs)) {
		return fmt.Errorf("--args is not valid JSON")
	}
	body := map[string]any{
		"name":      mcpTool,
		"arguments": json.RawMessage(mcpArgs),
	}
	client := ClientFromEnv()
	resp, err := client.Post(fmt.Sprintf("/api/v1/projects/%s/mcp/tools/call", mcpProject), body)
	if err != nil {
		return fmt.Errorf("mcp call: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return &APIError{StatusCode: resp.StatusCode, Message: string(raw)}
	}
	if mcpJSON {
		return prettyPrintJSON(raw)
	}
	// The server shape is { "text": "..." } for a successful invocation;
	// fall back to the raw body if the shape is different so operators
	// still see something useful.
	var parsed struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parsed); err == nil && parsed.Text != "" {
		fmt.Print(parsed.Text)
		if len(parsed.Text) > 0 && parsed.Text[len(parsed.Text)-1] != '\n' {
			fmt.Println()
		}
		return nil
	}
	return prettyPrintJSON(raw)
}
