package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/postgres"
	"vornik.io/vornik/internal/secrets"
	"vornik.io/vornik/internal/storage"
)

var (
	secretsScanProject string
	secretsScanSince   string
	secretsScanApply   bool
	secretsScanJSON    bool
	secretsPatternJSON bool
)

var secretsCmd = &cobra.Command{
	Use:   "secrets",
	Short: "Inspect secret-leak detection patterns and scan historical rows",
	Long: `Operator surface for the secret-leak detection engine (secret-leak Phase 3).

  list-patterns   print the effective detection pattern corpus
  scan-history    retro-scan historical tool-audit rows for secret-shaped values
`,
}

var secretsListPatternsCmd = &cobra.Command{
	Use:   "list-patterns",
	Short: "Print the effective secret-detection pattern corpus",
	Long: `Print the patterns currently in force: the curated defaults, minus any
listed in secrets.patterns.disable, plus secrets.patterns.custom. This is the
exact set buildSecretsDetector compiles, so what you see here is what the
daemon actually scans with.`,
	RunE: runSecretsListPatterns,
}

var secretsScanHistoryCmd = &cobra.Command{
	Use:   "scan-history",
	Short: "Retro-scan historical tool-audit rows for secret-shaped values",
	Long: `Scan tool_audit_log rows for secret-shaped values and, with --apply, redact
them in place. The tool_audit checkpoint defaults to REDACT, so a current daemon
cleans rows as it writes them — but rows written before that default changed, or
under an explicit checkpoints.tool_audit: detect override, or by a writer that
predates the redaction seam, can still hold raw secrets. This is the retro-scan
that cleans them.

Other operational tables are NOT scanned: webhook_events stores only a payload
HASH, task_llm_usage stores token COUNTS, and artifacts are redacted at write
time — none persist raw secret-bearing text to retro-redact.

Defaults to a dry-run (counts findings, mutates nothing). --apply rewrites the
matching rows with typed [REDACTED:type] markers, records the counts to
secret_redaction_audit (source=scan), and writes one admin_audit row naming the
operator + scope. Re-running --apply is safe: already-redacted rows no longer
match, so a second pass is a no-op (though it writes a fresh admin_audit row —
run dry-run first). Postgres-only.

Examples:
  vornikctl secrets scan-history                          # dry-run, all projects
  vornikctl secrets scan-history --project janka --since 30d
  vornikctl secrets scan-history --project janka --apply  # redact in place
`,
	RunE: runSecretsScanHistory,
}

func init() {
	secretsListPatternsCmd.Flags().BoolVar(&secretsPatternJSON, "json", false, "emit machine-readable JSON")
	secretsScanHistoryCmd.Flags().StringVar(&secretsScanProject, "project", "", "operate on a single project (default: all)")
	secretsScanHistoryCmd.Flags().StringVar(&secretsScanSince, "since", "", "only scan rows newer than this (e.g. 30d, 720h); default: all history")
	secretsScanHistoryCmd.Flags().BoolVar(&secretsScanApply, "apply", false, "redact matching rows in place (default: dry-run)")
	secretsScanHistoryCmd.Flags().BoolVar(&secretsScanJSON, "json", false, "emit machine-readable JSON")
	secretsCmd.AddCommand(secretsListPatternsCmd)
	secretsCmd.AddCommand(secretsScanHistoryCmd)
	rootCmd.AddCommand(secretsCmd)
}

// effectivePatternsFromConfig resolves the in-force corpus the same way
// buildSecretsDetector does, via the shared secrets.EffectivePatterns
// helper so CLI + daemon never drift.
func effectivePatternsFromConfig(cfg config.SecretsConfig) []secrets.Pattern {
	custom := make([]secrets.Pattern, 0, len(cfg.Patterns.Custom))
	for _, c := range cfg.Patterns.Custom {
		custom = append(custom, secrets.Pattern{Name: c.Name, Regex: c.Regex, Description: c.Description})
	}
	return secrets.EffectivePatterns(cfg.Patterns.Disable, custom)
}

func runSecretsListPatterns(_ *cobra.Command, _ []string) error {
	cfg, _, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	patterns := effectivePatternsFromConfig(cfg.Secrets)

	if secretsPatternJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(patterns)
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	_, _ = fmt.Fprintf(tw, "NAME\tDESCRIPTION\tREGEX\n")
	for _, p := range patterns {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", p.Name, p.Description, p.Regex)
	}
	_ = tw.Flush()
	if !cfg.Secrets.Entropy.Disabled {
		_, _ = fmt.Fprintf(os.Stdout, "\n(plus entropy detection: minLen=%d minBits=%g)\n",
			cfg.Secrets.Entropy.MinLen, cfg.Secrets.Entropy.MinBits)
	}
	_, _ = fmt.Fprintf(os.Stdout, "\n%d pattern(s) in force.\n", len(patterns))
	return nil
}

// scanHistoryResult is the per-run summary (dry-run and apply).
type scanHistoryResult struct {
	RowsScanned  int            `json:"rows_scanned"`
	RowsMatched  int            `json:"rows_matched"`
	Applied      bool           `json:"applied"`
	CountsByType map[string]int `json:"counts_by_type"`
}

func runSecretsScanHistory(_ *cobra.Command, _ []string) error {
	cfg, _, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if !cfg.Secrets.Enabled {
		return fmt.Errorf("secrets detection is disabled (secrets.enabled=false); enable it before scanning")
	}

	var since time.Time
	if secretsScanSince != "" {
		d, derr := time.ParseDuration(normalizeDuration(secretsScanSince))
		if derr != nil {
			return fmt.Errorf("invalid --since %q: %w", secretsScanSince, derr)
		}
		since = time.Now().Add(-d)
	}

	custom := make([]secrets.Pattern, 0, len(cfg.Secrets.Patterns.Custom))
	for _, c := range cfg.Secrets.Patterns.Custom {
		custom = append(custom, secrets.Pattern{Name: c.Name, Regex: c.Regex, Description: c.Description})
	}
	detector, err := secrets.NewMultiDetector(secrets.Config{
		Patterns:        secrets.EffectivePatterns(cfg.Secrets.Patterns.Disable, custom),
		Allowlist:       append(secrets.DefaultAllowlist(), cfg.Secrets.Allowlist...),
		EntropyDisabled: cfg.Secrets.Entropy.Disabled,
		EntropyMinLen:   cfg.Secrets.Entropy.MinLen,
		EntropyMinBits:  cfg.Secrets.Entropy.MinBits,
	})
	if err != nil {
		return fmt.Errorf("build detector: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	backend, err := storage.Open(ctx, cfg.Database)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = backend.Close() }()
	db, err := requirePostgresDB(backend, "secrets scan-history")
	if err != nil {
		return err
	}

	result, err := scanToolAuditHistory(ctx, db, detector, secretsScanProject, since, secretsScanApply)
	if err != nil {
		return err
	}

	if secretsScanApply && result.RowsMatched > 0 {
		// Accountability: who ran a privileged retro-scan that read raw
		// historical secrets, and over what scope (design §M3 / the
		// secret_view_audit reinterpretation).
		audit := postgres.NewAdminAuditRepository(db)
		_ = audit.Insert(ctx, &persistence.AdminAuditEntry{
			Principal: cliPrincipal(),
			Source:    "cli",
			Action:    "secrets.scan-history.apply",
			Target:    secretsScanProject,
			After: fmt.Sprintf(`{"rows_scanned":%d,"rows_matched":%d}`,
				result.RowsScanned, result.RowsMatched),
		})
	}

	if secretsScanJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	}
	mode := "DRY-RUN (nothing changed; re-run with --apply to redact)"
	if result.Applied {
		mode = "APPLIED (matching rows redacted in place)"
	}
	_, _ = fmt.Fprintf(os.Stdout, "tool_audit_log: scanned %d row(s), %d contained secret-shaped value(s)\n",
		result.RowsScanned, result.RowsMatched)
	for ft, n := range result.CountsByType {
		_, _ = fmt.Fprintf(os.Stdout, "  %-18s %d\n", ft, n)
	}
	_, _ = fmt.Fprintf(os.Stdout, "%s\n", mode)
	return nil
}

// scanToolAuditHistory scans (and with apply=true redacts) tool_input +
// tool_output on tool_audit_log. Each redacted row records its per-type
// counts to secret_redaction_audit with source="scan".
func scanToolAuditHistory(ctx context.Context, db *sql.DB, detector secrets.Detector, project string, since time.Time, apply bool) (scanHistoryResult, error) {
	res := scanHistoryResult{Applied: apply, CountsByType: map[string]int{}}

	q := `SELECT id, project_id, task_id, execution_id, tool_input, tool_output FROM tool_audit_log WHERE 1=1`
	args := []any{}
	if project != "" {
		args = append(args, project)
		q += fmt.Sprintf(" AND project_id = $%d", len(args))
	}
	if !since.IsZero() {
		args = append(args, since)
		q += fmt.Sprintf(" AND created_at >= $%d", len(args))
	}
	q += " ORDER BY created_at ASC"

	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return res, fmt.Errorf("query tool_audit_log: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var hits []toolAuditHit
	for rows.Next() {
		var id, projectID, taskID, execID, in, out string
		if err := rows.Scan(&id, &projectID, &taskID, &execID, &in, &out); err != nil {
			return res, fmt.Errorf("scan row: %w", err)
		}
		res.RowsScanned++
		inF := detector.Scan([]byte(in))
		outF := detector.Scan([]byte(out))
		if len(inF) == 0 && len(outF) == 0 {
			continue
		}
		res.RowsMatched++
		h := toolAuditHit{id: id, projectID: projectID, taskID: taskID, execID: execID, counts: map[string]int{}}
		for ft, n := range secrets.CountByType(append(append([]secrets.Finding{}, inF...), outF...)) {
			res.CountsByType[ft] += n
			h.counts[ft] += n
		}
		if len(inF) > 0 {
			h.newInput = string(secrets.Redact([]byte(in), inF))
			h.rewriteInput = true
		}
		if len(outF) > 0 {
			h.newOutput = string(secrets.Redact([]byte(out), outF))
			h.rewriteOutput = true
		}
		hits = append(hits, h)
	}
	if err := rows.Err(); err != nil {
		return res, err
	}
	if !apply {
		return res, nil
	}
	if err := applyToolAuditRedactions(ctx, db, hits); err != nil {
		return res, err
	}
	return res, nil
}

// toolAuditHit is one matched tool_audit_log row queued for redaction.
type toolAuditHit struct {
	id, projectID, taskID, execID string
	newInput, newOutput           string
	rewriteInput, rewriteOutput   bool
	counts                        map[string]int
}

// applyToolAuditRedactions rewrites each matched row in place and records
// its per-type counts to secret_redaction_audit (source="scan").
func applyToolAuditRedactions(ctx context.Context, db *sql.DB, hits []toolAuditHit) error {
	auditRepo := postgres.NewSecretRedactionAuditRepository(db)
	for _, h := range hits {
		var err error
		switch {
		case h.rewriteInput && h.rewriteOutput:
			_, err = db.ExecContext(ctx, `UPDATE tool_audit_log SET tool_input=$1, tool_output=$2 WHERE id=$3`, h.newInput, h.newOutput, h.id)
		case h.rewriteInput:
			_, err = db.ExecContext(ctx, `UPDATE tool_audit_log SET tool_input=$1 WHERE id=$2`, h.newInput, h.id)
		case h.rewriteOutput:
			_, err = db.ExecContext(ctx, `UPDATE tool_audit_log SET tool_output=$1 WHERE id=$2`, h.newOutput, h.id)
		}
		if err != nil {
			return fmt.Errorf("redact row %s: %w", h.id, err)
		}
		events := make([]persistence.SecretRedactionEvent, 0, len(h.counts))
		for ft, n := range h.counts {
			events = append(events, persistence.SecretRedactionEvent{
				ProjectID: h.projectID, TaskID: h.taskID, ExecutionID: h.execID,
				Checkpoint: secrets.CheckpointToolAudit, FindingType: ft, Count: n, Source: "scan",
			})
		}
		if err := auditRepo.Record(ctx, events); err != nil {
			return fmt.Errorf("record audit for row %s: %w", h.id, err)
		}
	}
	return nil
}

// normalizeDuration lets operators write "30d" as a shorthand for 720h;
// Go's time.ParseDuration has no day unit.
func normalizeDuration(s string) string {
	if n := len(s); n > 1 && s[n-1] == 'd' {
		if days, err := parsePositiveInt(s[:n-1]); err == nil {
			return fmt.Sprintf("%dh", days*24)
		}
	}
	return s
}

func parsePositiveInt(s string) (int, error) {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("not a number: %q", s)
		}
		n = n*10 + int(c-'0')
	}
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	return n, nil
}

// cliPrincipal identifies the operator running a CLI mutation for the
// admin_audit row. Best-effort: the OS user, prefixed to distinguish
// from UI/API principals.
func cliPrincipal() string {
	if u := os.Getenv("USER"); u != "" {
		return "cli:" + u
	}
	return "cli:unknown"
}
