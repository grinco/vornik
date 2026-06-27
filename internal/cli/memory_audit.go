package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

// `vornikctl memory audit` — list chunks that haven't been validated.
// Companion to the dispatcher's memory_correct tool: the chat tool is
// for in-the-moment "user just told me X is wrong" corrections; the
// audit CLI is for batch operator sweeps ("which chunks in the janka
// project are still on validation_status='legacy' from before the
// 2026-04 migration?"). Direct DB read, same pattern as memory dlq.

var (
	memoryAuditProject  string
	memoryAuditStatuses []string
	memoryAuditLimit    int
	memoryAuditJSON     bool
	memoryAuditFullText bool
)

var memoryAuditCmd = &cobra.Command{
	Use:   "audit",
	Short: "List unvalidated chunks (default: unverified + legacy)",
	Long: `Show chunks awaiting validation review. The default filter targets
the two states an operator cares about for content quality:
- unverified: freshly ingested chunks the system hasn't validated yet
- legacy:     chunks pre-dating the 2026.4 memory-hardening migration

Pass --status to narrow to a specific subset (e.g. --status legacy to
hand-audit the legacy backlog). The output is project-scoped so a typo
can't dump the whole deployment.

For per-chunk correction, use the dispatcher's memory_correct chat
tool (faster — embed similarity finds the wrong claim automatically).`,
	RunE: runMemoryAudit,
}

func init() {
	memoryAuditCmd.Flags().StringVarP(&memoryAuditProject, "project", "p", "", "Project ID (required)")
	memoryAuditCmd.Flags().StringSliceVar(&memoryAuditStatuses, "status", nil,
		"Filter by validation_status (repeatable). Default: unverified,legacy.")
	memoryAuditCmd.Flags().IntVarP(&memoryAuditLimit, "limit", "n", 100,
		"Max rows (1-500)")
	memoryAuditCmd.Flags().BoolVar(&memoryAuditJSON, "json", false, "Emit JSON instead of a table")
	memoryAuditCmd.Flags().BoolVar(&memoryAuditFullText, "full", false,
		"Show full chunk preview (~200 chars) instead of the one-line summary")
	_ = memoryAuditCmd.MarkFlagRequired("project")
	memoryCmd.AddCommand(memoryAuditCmd)
}

func runMemoryAudit(cmd *cobra.Command, args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	repo, cleanup, err := connectMemoryDB(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	rows, err := repo.ListUnverifiedChunks(ctx, memoryAuditProject, memoryAuditStatuses, memoryAuditLimit)
	if err != nil {
		return fmt.Errorf("list: %w", err)
	}
	if memoryAuditJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{"chunks": rows, "total": len(rows)})
	}
	if len(rows) == 0 {
		fmt.Printf("(no chunks for project %s match the filter)\n", memoryAuditProject)
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "CHUNK\tSTATUS\tCLASS\tROLE\tSOURCE\tCREATED\tPREVIEW")
	for _, r := range rows {
		preview := strings.ReplaceAll(r.Preview, "\n", " ⏎ ")
		if !memoryAuditFullText && len(preview) > 80 {
			preview = preview[:77] + "..."
		}
		role := r.ProducerRole
		if role == "" {
			role = "—"
		}
		title := r.ContentTitle
		if title == "" {
			title = r.SourceName
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			shortenID(r.ID), r.ValidationStatus, r.ContentClass, role,
			title, r.CreatedAt, preview)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Printf("\nShowing %d chunk(s). Use --json for full content_hash + ID; --full for un-truncated previews.\n", len(rows))
	fmt.Println("To refute a wrong fact via chat: tell the dispatcher \"X is wrong, it's actually Y\" — it'll call memory_correct.")
	return nil
}
