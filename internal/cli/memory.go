package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/storage"
)

var (
	memoryReassignFrom   string
	memoryReassignTo     string
	memoryReassignDryRun bool
)

var memoryCmd = &cobra.Command{
	Use:   "memory",
	Short: "Operate on project memory chunks",
	Long:  "Administrative commands for per-project RAG memory (project_memory_chunks).",
}

var memoryReassignCmd = &cobra.Command{
	Use:   "reassign",
	Short: "Move memory chunks from one project to another",
	Long: `Move all project_memory_chunks rows from --from to --to in a single
transaction, handling the (project_id, content_hash) unique constraint by
dropping source rows whose hashes already exist at the destination.

Example:
  vornikctl memory reassign --from old-project --to new-project
  vornikctl memory reassign --from old-project --to new-project --dry-run

Use --dry-run first to see counts without making changes.`,
	RunE: runMemoryReassign,
}

var (
	memorySearchProject string
	memorySearchQuery   string
	memorySearchLimit   int
	memorySearchJSON    bool

	memoryStatsProject string
	memoryStatsJSON    bool
)

var memorySearchCmd = &cobra.Command{
	Use:   "search",
	Short: "Search project memory (RAG index)",
	Long: `Query a project's hybrid RAG index and print the top matches. Wraps
GET /api/v1/projects/{project}/memory/search. Useful for verifying what
the researcher role is actually retrieving before you pin a behaviour
down in prompts.`,
	RunE: runMemorySearch,
}

var memoryStatsCmd = &cobra.Command{
	Use:   "stats",
	Short: "Show per-project RAG chunk counts and embedding coverage",
	Long: `Report total chunks, embedded chunks, and embed-queue depth for every
project (or one, with --project). Embedding coverage = embedded/total;
100% means the embed worker has caught up. A non-zero queue depth with
100% coverage means the worker is about to re-embed something (e.g.
after a model change).`,
	RunE: runMemoryStats,
}

func init() {
	memoryReassignCmd.Flags().StringVar(&memoryReassignFrom, "from", "", "source project ID (required)")
	memoryReassignCmd.Flags().StringVar(&memoryReassignTo, "to", "", "destination project ID (required)")
	memoryReassignCmd.Flags().BoolVar(&memoryReassignDryRun, "dry-run", false, "Report what would change without writing")
	_ = memoryReassignCmd.MarkFlagRequired("from")
	_ = memoryReassignCmd.MarkFlagRequired("to")

	memorySearchCmd.Flags().StringVarP(&memorySearchProject, "project", "p", "", "Project ID (required)")
	memorySearchCmd.Flags().StringVarP(&memorySearchQuery, "query", "q", "", "Search query (required)")
	memorySearchCmd.Flags().IntVarP(&memorySearchLimit, "limit", "n", 10, "Max results (1-50)")
	memorySearchCmd.Flags().BoolVar(&memorySearchJSON, "json", false, "JSON output instead of the short-form table")
	_ = memorySearchCmd.MarkFlagRequired("project")
	_ = memorySearchCmd.MarkFlagRequired("query")

	memoryStatsCmd.Flags().StringVarP(&memoryStatsProject, "project", "p", "", "Show only this project (default: all)")
	memoryStatsCmd.Flags().BoolVar(&memoryStatsJSON, "json", false, "JSON output")

	memoryCmd.AddCommand(memoryReassignCmd, memorySearchCmd, memoryStatsCmd)
	rootCmd.AddCommand(memoryCmd)
}

func runMemorySearch(cmd *cobra.Command, args []string) error {
	q := url.Values{}
	q.Set("q", memorySearchQuery)
	if memorySearchLimit > 0 {
		q.Set("limit", fmt.Sprintf("%d", memorySearchLimit))
	}
	path := fmt.Sprintf("/api/v1/projects/%s/memory/search?%s", memorySearchProject, q.Encode())
	raw, err := fetchJSON(path)
	if err != nil {
		return err
	}
	if memorySearchJSON {
		return prettyPrintJSON(raw)
	}
	var parsed struct {
		Results []struct {
			ChunkID    string  `json:"chunk_id"`
			ProjectID  string  `json:"project_id"`
			TaskID     string  `json:"task_id"`
			SourceName string  `json:"source_name"`
			Content    string  `json:"content"`
			Score      float64 `json:"score"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}
	if len(parsed.Results) == 0 {
		fmt.Println("(no results)")
		return nil
	}
	for i, r := range parsed.Results {
		snippet := r.Content
		if len(snippet) > 240 {
			snippet = snippet[:237] + "..."
		}
		fmt.Printf("%d. score=%.4f source=%s task=%s\n   %s\n\n", i+1, r.Score, r.SourceName, r.TaskID, snippet)
	}
	return nil
}

func runMemoryStats(cmd *cobra.Command, args []string) error {
	raw, err := fetchJSON("/api/v1/memory/stats")
	if err != nil {
		return err
	}
	var parsed struct {
		Projects []struct {
			ProjectID      string `json:"projectId"`
			ChunksTotal    int64  `json:"chunksTotal"`
			ChunksEmbedded int64  `json:"chunksEmbedded"`
			QueueDepth     int64  `json:"queueDepth"`
		} `json:"projects"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}
	if memoryStatsProject != "" {
		// Client-side filter — the server already scanned every row
		// once, so this is a per-row comparison with no extra cost.
		filtered := parsed.Projects[:0]
		for _, p := range parsed.Projects {
			if p.ProjectID == memoryStatsProject {
				filtered = append(filtered, p)
			}
		}
		parsed.Projects = filtered
		parsed.Total = len(filtered)
	}
	if memoryStatsJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(parsed)
	}
	sort.Slice(parsed.Projects, func(i, j int) bool { return parsed.Projects[i].ProjectID < parsed.Projects[j].ProjectID })
	tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "PROJECT\tCHUNKS\tEMBEDDED\tCOVERAGE\tQUEUE")
	for _, p := range parsed.Projects {
		var coverage string
		if p.ChunksTotal == 0 {
			coverage = "—"
		} else {
			coverage = fmt.Sprintf("%.1f%%", 100*float64(p.ChunksEmbedded)/float64(p.ChunksTotal))
		}
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%d\t%s\t%d\n", p.ProjectID, p.ChunksTotal, p.ChunksEmbedded, coverage, p.QueueDepth)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Printf("\nTotal: %d\n", parsed.Total)
	return nil
}

func runMemoryReassign(cmd *cobra.Command, args []string) error {
	if memoryReassignFrom == "" || memoryReassignTo == "" {
		return fmt.Errorf("--from and --to are required")
	}
	if memoryReassignFrom == memoryReassignTo {
		return fmt.Errorf("--from and --to must differ")
	}

	cfg, _, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	backend, err := storage.Open(ctx, cfg.Database)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = backend.Close() }()
	db, err := requirePostgresDB(backend, "memory reassign")
	if err != nil {
		return err
	}

	// Count source rows and the subset that collides with destination.
	var (
		sourceCount    int
		collisionCount int
	)
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM project_memory_chunks WHERE project_id = $1`,
		memoryReassignFrom,
	).Scan(&sourceCount); err != nil {
		return fmt.Errorf("count source rows: %w", err)
	}
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM project_memory_chunks s
		WHERE s.project_id = $1
		  AND EXISTS (
		      SELECT 1 FROM project_memory_chunks d
		      WHERE d.project_id = $2 AND d.content_hash = s.content_hash
		  )`,
		memoryReassignFrom, memoryReassignTo,
	).Scan(&collisionCount); err != nil {
		return fmt.Errorf("count collisions: %w", err)
	}

	toMove := sourceCount - collisionCount

	fmt.Printf("source project %q:            %d rows\n", memoryReassignFrom, sourceCount)
	fmt.Printf("collisions at %q (drop):      %d rows\n", memoryReassignTo, collisionCount)
	fmt.Printf("rows to move:                  %d\n", toMove)

	if memoryReassignDryRun {
		fmt.Println("\n(dry-run: no changes made)")
		return nil
	}

	if sourceCount == 0 {
		fmt.Println("nothing to do.")
		return nil
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 1. Drop source rows whose hash already exists at the destination.
	//    The embed queue references project_memory_chunks.id via ON DELETE
	//    CASCADE, so this cleans up any pending jobs for dropped rows too.
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM project_memory_chunks s
		WHERE s.project_id = $1
		  AND EXISTS (
		      SELECT 1 FROM project_memory_chunks d
		      WHERE d.project_id = $2 AND d.content_hash = s.content_hash
		  )`,
		memoryReassignFrom, memoryReassignTo,
	); err != nil {
		return fmt.Errorf("delete collisions: %w", err)
	}

	// 2. Reassign the rest.
	res, err := tx.ExecContext(ctx,
		`UPDATE project_memory_chunks SET project_id = $1 WHERE project_id = $2`,
		memoryReassignTo, memoryReassignFrom,
	)
	if err != nil {
		return fmt.Errorf("update project_id: %w", err)
	}
	affected, _ := res.RowsAffected()

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	fmt.Printf("\nreassigned %d rows from %q to %q (%d duplicates dropped)\n",
		affected, memoryReassignFrom, memoryReassignTo, collisionCount)
	return nil
}
