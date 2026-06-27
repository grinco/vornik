package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/storage"
)

// `vornikctl memory scope {list,retag}` — operator surface for the
// per-deposit repo_scope partition (migration 75 / LLD migration-75 arc).
//
// scope list  — distinct scope tokens in a project, with chunk counts,
//               so operators can see what's accumulated and spot
//               typos ("github.com/foo/bar" vs "github.com/Foo/Bar").
// scope retag — bulk-promote chunks from one scope (or NULL /
//               uncategorized) to another. Lets a fleet that
//               accumulated pre-migration chunks tag them in one shot
//               instead of waiting for them to age out.

var memoryScopeCmd = &cobra.Command{
	Use:   "scope",
	Short: "Inspect and bulk-edit the per-chunk repo scope",
	Long: `Two subcommands for managing the per-chunk repo-scope tag. The
scope partitions a single project's RAG so one operator's many repos
don't cross-pollute each other's recall results.

  list   — distinct scopes + chunk counts (the namespace inventory)
  retag  — bulk update chunks from one scope (or NULL) to another

The chunk-side semantics are:
  NULL        = uncategorized (untagged chunks; visible under
                 every scoped recall during the transition window)
  "*"         = cross-cutting (surfaces in every scoped recall)
  any string  = repo token (typically a git remote, e.g.
                 "github.com/acme/myrepo")`,
}

var (
	memoryScopeListProject string
	memoryScopeListJSON    bool
)

var memoryScopeListCmd = &cobra.Command{
	Use:   "list",
	Short: "List distinct repo_scope values in a project, with chunk counts",
	RunE:  runMemoryScopeList,
}

func init() {
	memoryScopeListCmd.Flags().StringVarP(&memoryScopeListProject, "project", "p", "", "Project ID (required)")
	memoryScopeListCmd.Flags().BoolVar(&memoryScopeListJSON, "json", false, "Emit JSON instead of a table")
	_ = memoryScopeListCmd.MarkFlagRequired("project")

	memoryScopeCmd.AddCommand(memoryScopeListCmd, memoryScopeRetagCmd)
	memoryCmd.AddCommand(memoryScopeCmd)
}

type scopeRow struct {
	Scope  string `json:"scope"`
	Chunks int    `json:"chunks"`
}

func runMemoryScopeList(cmd *cobra.Command, args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// openVornikDB internally calls config.Load → flag.String("config")
	// which is a process-global. Calling connectMemoryDB AND
	// openVornikDB would re-register the flag and panic; one or
	// the other is the right shape per command.
	db, err := openVornikDB(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	rows, err := db.QueryContext(ctx, `
		SELECT COALESCE(repo_scope, '<uncategorized>') AS scope, COUNT(*) AS n
		FROM project_memory_chunks
		WHERE project_id = $1
		GROUP BY repo_scope
		ORDER BY n DESC, scope ASC
	`, memoryScopeListProject)
	if err != nil {
		return fmt.Errorf("scope list: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []scopeRow
	for rows.Next() {
		var r scopeRow
		if err := rows.Scan(&r.Scope, &r.Chunks); err != nil {
			return fmt.Errorf("scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	if memoryScopeListJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{"project": memoryScopeListProject, "scopes": out, "total": len(out)})
	}
	if len(out) == 0 {
		fmt.Printf("(no chunks for project %s)\n", memoryScopeListProject)
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "SCOPE\tCHUNKS")
	for _, r := range out {
		_, _ = fmt.Fprintf(w, "%s\t%d\n", r.Scope, r.Chunks)
	}
	return w.Flush()
}

var (
	memoryScopeRetagProject string
	memoryScopeRetagFrom    string
	memoryScopeRetagTo      string
	memoryScopeRetagPattern string
	memoryScopeRetagDryRun  bool
	memoryScopeRetagYes     bool
)

var memoryScopeRetagCmd = &cobra.Command{
	Use:   "retag",
	Short: "Bulk-promote chunks from one scope to another",
	Long: `Update the repo scope on every chunk in a project that matches the
selectors. Typical use is bulk-promoting a backlog of untagged chunks:

  # Tag every uncategorized chunk in a project as a given repo:
  vornikctl memory scope retag -p my-project --to=github.com/acme/myrepo

  # Fix a typo across a previously-scoped batch:
  vornikctl memory scope retag -p my-project --from=github.com/acme/old-repo --to=github.com/acme/myrepo

  # Narrow with a source_name LIKE filter (only re-tag a subset):
  vornikctl memory scope retag -p my-project \
      --from=                   \
      --to=github.com/acme/myrepo \
      --source-name-like='docs-%'

Defaults: --from is empty (NULL / uncategorized chunks). Pass --from=X
to migrate a specific scope; pass --source-name-like to narrow.

Always runs against the live daemon's chunk store.
Use --dry-run to preview row counts before commit.`,
	RunE: runMemoryScopeRetag,
}

func init() {
	memoryScopeRetagCmd.Flags().StringVarP(&memoryScopeRetagProject, "project", "p", "", "Project ID (required)")
	memoryScopeRetagCmd.Flags().StringVar(&memoryScopeRetagFrom, "from", "",
		"Source scope to promote from. Empty / unset = NULL (uncategorized chunks). Pass '*' to retag cross-cutting chunks.")
	memoryScopeRetagCmd.Flags().StringVar(&memoryScopeRetagTo, "to", "",
		"Target scope to stamp on matched chunks (required). '*' = cross-cutting; '' is rejected (would un-tag).")
	memoryScopeRetagCmd.Flags().StringVar(&memoryScopeRetagPattern, "source-name-like", "",
		"Optional source_name SQL LIKE pattern to narrow which chunks get retagged (e.g. 'lld-%').")
	memoryScopeRetagCmd.Flags().BoolVar(&memoryScopeRetagDryRun, "dry-run", false, "Show affected count without writing")
	memoryScopeRetagCmd.Flags().BoolVar(&memoryScopeRetagYes, "yes", false, "Skip the interactive confirmation prompt")
	_ = memoryScopeRetagCmd.MarkFlagRequired("project")
	_ = memoryScopeRetagCmd.MarkFlagRequired("to")
}

func runMemoryScopeRetag(cmd *cobra.Command, args []string) error {
	if strings.TrimSpace(memoryScopeRetagTo) == "" {
		return fmt.Errorf("--to must be non-empty (use '*' for cross-cutting; bare empty is rejected to avoid accidental un-tagging)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, err := openVornikDB(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	// Build the WHERE clause incrementally. project_id is always
	// bound; the from/source-name filters add params as needed.
	clauses := []string{"project_id = $1"}
	params := []any{memoryScopeRetagProject}
	if memoryScopeRetagFrom == "" {
		clauses = append(clauses, "repo_scope IS NULL")
	} else {
		params = append(params, memoryScopeRetagFrom)
		clauses = append(clauses, fmt.Sprintf("repo_scope = $%d", len(params)))
	}
	if memoryScopeRetagPattern != "" {
		params = append(params, memoryScopeRetagPattern)
		clauses = append(clauses, fmt.Sprintf("source_name LIKE $%d", len(params)))
	}
	whereSQL := strings.Join(clauses, " AND ")

	// Preview the affected count.
	countSQL := "SELECT COUNT(*) FROM project_memory_chunks WHERE " + whereSQL
	var n int
	if err := db.QueryRowContext(ctx, countSQL, params...).Scan(&n); err != nil {
		return fmt.Errorf("preview count: %w", err)
	}
	fromLabel := memoryScopeRetagFrom
	if fromLabel == "" {
		fromLabel = "<uncategorized / NULL>"
	}
	fmt.Printf("Project:           %s\n", memoryScopeRetagProject)
	fmt.Printf("From scope:        %s\n", fromLabel)
	fmt.Printf("To scope:          %s\n", memoryScopeRetagTo)
	if memoryScopeRetagPattern != "" {
		fmt.Printf("Source-name LIKE:  %s\n", memoryScopeRetagPattern)
	}
	fmt.Printf("Affected chunks:   %d\n", n)
	if memoryScopeRetagDryRun {
		fmt.Println("(dry run; no changes written)")
		return nil
	}
	if n == 0 {
		fmt.Println("(nothing to do)")
		return nil
	}
	if !memoryScopeRetagYes {
		fmt.Print("\nProceed? [y/N] ")
		var ans string
		_, _ = fmt.Fscanln(os.Stdin, &ans)
		ans = strings.ToLower(strings.TrimSpace(ans))
		if ans != "y" && ans != "yes" {
			return fmt.Errorf("aborted")
		}
	}

	// Append the new-scope param last; reuse the existing param list.
	updateParams := append(params, memoryScopeRetagTo)
	updateSQL := fmt.Sprintf(
		"UPDATE project_memory_chunks SET repo_scope = $%d WHERE %s",
		len(updateParams), whereSQL,
	)
	res, err := db.ExecContext(ctx, updateSQL, updateParams...)
	if err != nil {
		return fmt.Errorf("retag: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows-affected: %w", err)
	}
	fmt.Printf("Retagged %d chunks.\n", rows)
	return nil
}

// openVornikDB opens a direct *sql.DB connection for the scope
// subcommands' raw SQL queries (chunk-aggregate + bulk UPDATE).
// Mirrors connectMemoryDB's dial logic but returns the DB handle
// rather than wrapping it in memory.Repository — the scope ops
// don't fit the repository's per-chunk methods.
func openVornikDB(ctx context.Context) (*sql.DB, error) {
	cfg, _, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	backend, err := storage.Open(ctx, cfg.Database)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db, err := requirePostgresDB(backend, "memory scope")
	if err != nil {
		_ = backend.Close()
		return nil, err
	}
	return db, nil
}
