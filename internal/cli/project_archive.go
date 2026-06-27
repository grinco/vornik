package cli

// Project archive / unarchive / delete-now CLI surface. Hits the
// daemon's /api/v1/projects/{id}/{archive,unarchive,delete-now}
// endpoints — same code path the UI archive button uses, same
// audit shape.
//
//   vornikctl project archive    <id> [--grace 7d] [--reason "..."] [--json]
//   vornikctl project unarchive  <id> [--json]
//   vornikctl project delete-now <id> [--json]
//
// See https://docs.vornik.io

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"

	"github.com/spf13/cobra"
)

var (
	projectArchiveCmd = &cobra.Command{
		Use:   "archive <projectId>",
		Short: "Archive a project (schedule it for deletion after a grace window)",
		Long: `Flip a project's lifecycle to archived. The daemon stops
dispatching new work for the project immediately. After the
grace window elapses the archive-sweeper hard-deletes the
project YAML, every project-scoped DB row, and every artifact
blob on disk.

Unarchive any time during the grace window to restore the
project. Default grace is 7 days.`,
		Args: cobra.ExactArgs(1),
		RunE: runProjectArchive,
	}
	projectUnarchiveCmd = &cobra.Command{
		Use:   "unarchive <projectId>",
		Short: "Restore an archived project to active",
		Long: `Clear the lifecycle block on a previously-archived project.
The grace window resets, new tasks can dispatch again, and the
sweeper stops tracking the project for deletion.`,
		Args: cobra.ExactArgs(1),
		RunE: runProjectUnarchive,
	}
	projectDeleteNowCmd = &cobra.Command{
		Use:   "delete-now <projectId>",
		Short: "Skip the grace window — wipe an archived project on the next sweeper tick",
		Long: `Rewind the scheduledDeleteAt timestamp to ~now and kick the
archive-sweeper. The project's YAML, DB rows, and artifact
blobs are wiped within seconds.

Requires the project to be archived first (use 'project
archive' if it isn't). Cannot be undone.`,
		Args: cobra.ExactArgs(1),
		RunE: runProjectDeleteNow,
	}

	projectArchiveGrace  string
	projectArchiveReason string
	projectArchiveJSON   bool
	projectUnarchiveJSON bool
	projectDeleteNowJSON bool
)

func init() {
	projectArchiveCmd.Flags().StringVar(&projectArchiveGrace, "grace", "7d", "Grace window before deletion (e.g. 1d, 7d, 30d, 90d, 12h)")
	projectArchiveCmd.Flags().StringVar(&projectArchiveReason, "reason", "", "Optional operator-visible reason recorded in the YAML")
	projectArchiveCmd.Flags().BoolVar(&projectArchiveJSON, "json", false, "Output JSON instead of human-readable")

	projectUnarchiveCmd.Flags().BoolVar(&projectUnarchiveJSON, "json", false, "Output JSON instead of human-readable")
	projectDeleteNowCmd.Flags().BoolVar(&projectDeleteNowJSON, "json", false, "Output JSON instead of human-readable")

	projectCmd.AddCommand(projectArchiveCmd)
	projectCmd.AddCommand(projectUnarchiveCmd)
	projectCmd.AddCommand(projectDeleteNowCmd)
}

// projectArchiveEntry mirrors api.ArchiveResponse.
type projectArchiveEntry struct {
	ProjectID         string `json:"project_id"`
	Status            string `json:"status"`
	ArchivedAt        string `json:"archived_at,omitempty"`
	ScheduledDeleteAt string `json:"scheduled_delete_at,omitempty"`
	Reason            string `json:"reason,omitempty"`
	ArchivedBy        string `json:"archived_by,omitempty"`
}

func runProjectArchive(_ *cobra.Command, args []string) error {
	projectID := args[0]
	body := map[string]string{}
	if projectArchiveGrace != "" {
		body["grace"] = projectArchiveGrace
	}
	if projectArchiveReason != "" {
		body["reason"] = projectArchiveReason
	}
	out, err := postArchiveAction("/api/v1/projects/"+url.PathEscape(projectID)+"/archive", body)
	if err != nil {
		return fmt.Errorf("archive: %w", err)
	}
	if projectArchiveJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	fmt.Printf("Archived %s.\n", out.ProjectID)
	fmt.Printf("  Status:          %s\n", out.Status)
	if out.ArchivedAt != "" {
		fmt.Printf("  Archived at:     %s\n", out.ArchivedAt)
	}
	if out.ScheduledDeleteAt != "" {
		fmt.Printf("  Delete after:    %s\n", out.ScheduledDeleteAt)
	}
	if out.Reason != "" {
		fmt.Printf("  Reason:          %s\n", out.Reason)
	}
	fmt.Printf("\nRun 'vornikctl project unarchive %s' to restore before the grace window elapses.\n", out.ProjectID)
	return nil
}

func runProjectUnarchive(_ *cobra.Command, args []string) error {
	projectID := args[0]
	out, err := postArchiveAction("/api/v1/projects/"+url.PathEscape(projectID)+"/unarchive", map[string]string{})
	if err != nil {
		return fmt.Errorf("unarchive: %w", err)
	}
	if projectUnarchiveJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	fmt.Printf("Restored %s to active.\n", out.ProjectID)
	return nil
}

func runProjectDeleteNow(_ *cobra.Command, args []string) error {
	projectID := args[0]
	out, err := postArchiveAction("/api/v1/projects/"+url.PathEscape(projectID)+"/delete-now", map[string]string{})
	if err != nil {
		return fmt.Errorf("delete-now: %w", err)
	}
	if projectDeleteNowJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	fmt.Printf("Scheduled %s for immediate deletion.\n", out.ProjectID)
	if out.ScheduledDeleteAt != "" {
		fmt.Printf("  Sweeper picks it up on the next tick (~1s when kicked synchronously).\n")
	}
	return nil
}

// postArchiveAction is the common POST-JSON helper for the
// three archive actions. The endpoints return the same
// archive-response shape (project_id + status + optional time
// fields).
func postArchiveAction(path string, body map[string]string) (*projectArchiveEntry, error) {
	client := ClientFromEnv()
	resp, err := client.Post(path, body)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, ParseAPIError(resp)
	}
	var out projectArchiveEntry
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &out, nil
}
