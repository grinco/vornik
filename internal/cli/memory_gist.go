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

// `vornikctl memory gist <projectID>` — operator-facing read of
// the periodic LLM-free per-project term-frequency summary.
// Mirrors `webhook events`'s shape (project + json flag), kept
// terse because the operator path is "did the loop run, what
// does it think this project is about?"

type gistTermDTO struct {
	Term  string `json:"term"`
	Count int    `json:"count"`
}

type gistResponseDTO struct {
	ProjectID     string        `json:"project_id"`
	ChunksScanned int           `json:"chunks_scanned"`
	Terms         []gistTermDTO `json:"terms"`
	GeneratedAt   string        `json:"generated_at"`
	DurationMs    int           `json:"duration_ms"`
}

var (
	memoryGistJSONFlag bool
)

var memoryGistCmd = &cobra.Command{
	Use:   "gist <projectID>",
	Short: "Show the periodic LLM-free per-project term-frequency gist",
	Long: `Print the latest gist (top-N ranked terms by raw frequency over the
project's chunk corpus) produced by the consolidate worker. The worker
runs every ~10 minutes by default; a 404 means the loop hasn't fired
for this project yet.`,
	Args: cobra.ExactArgs(1),
	RunE: runMemoryGist,
}

func init() {
	memoryGistCmd.Flags().BoolVar(&memoryGistJSONFlag, "json", false, "Emit JSON instead of a table")
	memoryCmd.AddCommand(memoryGistCmd)
}

func runMemoryGist(cmd *cobra.Command, args []string) error {
	client := ClientFromEnv()
	resp, err := client.Get(fmt.Sprintf("/api/v1/projects/%s/gist", args[0]))
	if err != nil {
		return fmt.Errorf("gist: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		fmt.Fprintf(os.Stderr, "No gist for project %q yet — the consolidate loop runs every ~10 minutes.\n", args[0])
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		return ParseAPIError(resp)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	var out gistResponseDTO
	if err := json.Unmarshal(body, &out); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	if memoryGistJSONFlag {
		return json.NewEncoder(os.Stdout).Encode(out)
	}
	fmt.Printf("Gist for project %q\n", out.ProjectID)
	fmt.Printf("  chunks scanned: %d\n", out.ChunksScanned)
	fmt.Printf("  generated at:   %s\n", out.GeneratedAt)
	fmt.Printf("  duration:       %d ms\n\n", out.DurationMs)
	if len(out.Terms) == 0 {
		fmt.Println("  no terms — project has no chunks yet")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "  TERM\tCOUNT")
	for _, t := range out.Terms {
		_, _ = fmt.Fprintf(w, "  %s\t%d\n", t.Term, t.Count)
	}
	return w.Flush()
}
